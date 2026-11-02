// Package relay pulls remote outboxes into local mailboxes.
package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/store"
)

func paths(root, machine string) (string, error) {
	if _, err := addr.Parse("recado://" + machine + "/agent"); err != nil || machine == "local" {
		return "", fmt.Errorf("invalid machine %q", machine)
	}
	dir := filepath.Join(root, "relay")
	if err := os.MkdirAll(dir, store.DirMode); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, store.DirMode); err != nil {
		return "", err
	}
	return filepath.Join(dir, machine), nil
}

// RecordSender records an agent and starts a relay if its instance lock is free.
// The senders lock serializes this with the relay's final idle check.
func RecordSender(root, machine, id string, start func() error) error {
	if !addr.ValidID(id) {
		return fmt.Errorf("invalid sender %q", id)
	}
	base, err := paths(root, machine)
	if err != nil {
		return err
	}
	f, err := store.LockFile(base+".senders", os.O_RDWR|os.O_APPEND|os.O_CREATE, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer store.Unlock(f)
	if err := store.TrimTail(f); err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == id {
			found = true
			break
		}
	}
	if !found {
		if _, err = f.WriteString(id + "\n"); err != nil {
			return err
		}
	}
	lock, err := store.LockFile(base+".lock", os.O_RDWR|os.O_CREATE, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil
	}
	if err != nil {
		return err
	}
	store.Unlock(lock)
	return start()
}

func readCursor(base string) (int64, error) {
	b, err := os.ReadFile(base + ".cursor")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid relay cursor %q", b)
	}
	return n, nil
}
func writeCursor(base string, n int64) error {
	f, err := os.CreateTemp(filepath.Dir(base), ".cursor-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = fmt.Fprintf(f, "%d\n", n); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), base+".cursor")
}

// Run holds the instance lock and reconnects until the outbox is caught up and
// none of this machine's recorded senders is live.
func Run(ctx context.Context, svc *deliver.Service, machine string) (resultErr error) {
	if _, err := deliver.RemoteCommand(svc.Machines, machine, "outbox"); err != nil {
		return err
	}
	base, err := paths(svc.Store.Root(), machine)
	if err != nil {
		return err
	}
	lock, err := store.LockFile(base+".lock", os.O_RDWR|os.O_CREATE, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return fmt.Errorf("relay %s already running: %w", machine, err)
	}
	defer func() {
		if lock != nil {
			store.Unlock(lock)
		}
	}()
	log, err := os.OpenFile(base+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, store.FileMode)
	if err != nil {
		return err
	}
	defer log.Close()
	defer func() {
		if resultErr != nil {
			_, _ = fmt.Fprintf(log, "%s: %v\n", time.Now().UTC().Format(time.RFC3339), resultErr)
		}
	}()
	if err := log.Chmod(store.FileMode); err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	if _, err = addr.Parse("recado+upstream://" + host + "/agent"); err != nil {
		return fmt.Errorf("invalid hostname %q: %w", host, err)
	}
	cursor, err := readCursor(base)
	if err != nil {
		return err
	}
	delay := 200 * time.Millisecond
	for {
		idle, progressed, err := pull(ctx, svc, machine, host, base, &cursor, log, func() (bool, error) {
			f, err := store.LockFile(base+".senders", os.O_RDWR|os.O_APPEND|os.O_CREATE, syscall.LOCK_EX)
			if err != nil {
				return false, err
			}
			defer store.Unlock(f)
			live, err := liveSenders(f, svc.Store)
			if err != nil {
				return false, err
			}
			if !live {
				store.Unlock(lock)
				lock = nil
				return true, nil
			}
			return false, nil
		})
		if idle || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			_, _ = fmt.Fprintf(log, "%s: %v\n", time.Now().UTC().Format(time.RFC3339), err)
		}
		if progressed {
			delay = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
}

func liveSenders(f *os.File, st *store.Store) (bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return false, nil // a trailing partial record is not a sender
		}
		if err != nil {
			return false, err
		}
		id := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if !addr.ValidID(id) {
			return false, fmt.Errorf("invalid sender %q", id)
		}
		a, err := st.Lookup(id)
		if errors.Is(err, store.ErrUnknownAgent) {
			continue
		}
		if err != nil {
			return false, err
		}
		if a.Live() {
			return true, nil
		}
	}
}

type frame struct {
	Offset   int64           `json:"offset"`
	Envelope json.RawMessage `json:"envelope"`
	CaughtUp bool            `json:"caught_up"`
}

func pull(ctx context.Context, svc *deliver.Service, machine, host, base string, cursor *int64, log *os.File, idle func() (bool, error)) (bool, bool, error) {
	cmd, err := deliver.RemoteCommand(svc.Machines, machine, "outbox", "--follow", "--host", host, "--after", strconv.FormatInt(*cursor, 10), "--json")
	if err != nil {
		return false, false, err
	}
	cmd.Stderr = log
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, false, err
	}
	if err = cmd.Start(); err != nil {
		return false, false, err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	// Closing the pipe on cancellation also unblocks a stalled transport.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()
	reader := bufio.NewReader(stdout)
	progressed := false
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return false, progressed, fmt.Errorf("outbox connection: %w", err)
		}
		var f frame
		if err = json.Unmarshal(line, &f); err != nil {
			return false, progressed, fmt.Errorf("outbox frame: %w", err)
		}
		if f.Offset < *cursor {
			return false, progressed, fmt.Errorf("outbox offset moved backwards")
		}
		if f.Envelope != nil {
			env, err := envelope.Decode(f.Envelope)
			if err != nil {
				return false, progressed, err
			}
			if !env.From.IsLocal() {
				return false, progressed, fmt.Errorf("invalid remote sender %q", env.From)
			}
			if !env.To.Upstream || env.To.Machine != host {
				return false, progressed, fmt.Errorf("invalid upstream recipient %q", env.To)
			}
			env.From = addr.Address{Machine: machine, Agent: env.From.Agent}
			env.To = addr.Local(env.To.Agent)
			if _, err = svc.Deliver(env); err != nil {
				return false, progressed, err
			}
		} else if !f.CaughtUp {
			return false, progressed, fmt.Errorf("invalid outbox frame")
		}
		if f.Offset > *cursor {
			if err = writeCursor(base, f.Offset); err != nil {
				return false, progressed, err
			}
			*cursor = f.Offset
			progressed = true
		}
		if f.CaughtUp {
			yes, err := idle()
			if err != nil {
				return false, progressed, err
			}
			if yes {
				return true, progressed, nil
			}
		}
	}
}
