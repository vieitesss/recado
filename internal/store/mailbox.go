// Package store owns recado's on-disk state: the agent registry and the
// mailboxes. Every mailbox write happens under an flock on its inbox file, so
// concurrent senders never interleave a message or deliver one twice.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

// Modes recado creates its state with.
const (
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600
)

// Message states. A message is stored until someone claims it for push or
// drain (queued), and read once its harness reports the turn started.
const (
	StateStored = "stored"
	StateQueued = "queued"
	StateRead   = "read"
)

// Errors callers map to exit code 2: the address or id they named does not
// exist or is not enough to identify one message.
var (
	ErrUnknownAgent = errors.New("unknown agent")
	ErrUnknownID    = errors.New("no such message")
	ErrAmbiguousID  = errors.New("ambiguous message id")
)

// Store is a recado state directory.
type Store struct {
	root string
}

// DefaultRoot is ${RECADO_STATE_DIR}, else
// ${XDG_STATE_HOME:-~/.local/state}/recado.
func DefaultRoot() (string, error) {
	if dir := os.Getenv("RECADO_STATE_DIR"); dir != "" {
		return dir, nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "recado"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "recado"), nil
}

// Open returns the store rooted at root, creating it if needed and enforcing
// the 0700 directory mode the design promises.
func Open(root string) (*Store, error) {
	if err := ensureDir(root); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// ensureDir creates a directory recado owns and keeps its mode at DirMode.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, DirMode); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() == DirMode {
		return nil
	}
	return os.Chmod(path, DirMode)
}

// Message is one stored envelope and its current state.
type Message struct {
	Envelope envelope.Envelope
	State    string
}

// Append stores env in agent id's mailbox, under the mailbox lock. Storing an
// envelope whose id is already in the mailbox changes nothing, so a retried
// deliver from the relay cannot deliver the same message twice.
func (s *Store) Append(id string, env envelope.Envelope) error {
	line, err := env.Line()
	if err != nil {
		return err
	}
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock(f)
	envs, err := readInbox(f)
	if err != nil {
		return err
	}
	for _, existing := range envs {
		if existing.ID == env.ID {
			return nil
		}
	}
	if _, err := f.Write(line); err != nil {
		return err
	}
	return nil
}

// Messages returns agent id's mailbox in append order, with each message's
// current state.
func (s *Store) Messages(id string) ([]Message, error) {
	f, err := s.lockMailbox(id, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer unlock(f)
	envs, err := readInbox(f)
	if err != nil {
		return nil, err
	}
	states, err := s.states(id)
	if err != nil {
		return nil, err
	}
	return join(envs, states), nil
}

// ShortID is the shortest suffix of msgID, at least four characters, that
// names exactly one message in agent id's mailbox.
func (s *Store) ShortID(id, msgID string) (string, error) {
	msgs, err := s.Messages(id)
	if err != nil {
		return "", err
	}
	return shortID(msgs, msgID), nil
}

// Resolve finds the one message in agent id's mailbox whose id ends with
// query, which may be a short id or a full ULID.
func (s *Store) Resolve(id, query string) (Message, error) {
	msgs, err := s.Messages(id)
	if err != nil {
		return Message{}, err
	}
	return resolve(msgs, query)
}

// ResolveAny finds the one message in any mailbox whose id ends with query.
// It is what `recado status` uses: the short id send printed belongs to the
// recipient's mailbox, not the sender's.
func (s *Store) ResolveAny(query string) (Message, string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "mail"))
	if errors.Is(err, os.ErrNotExist) {
		return Message{}, "", ErrUnknownID
	}
	if err != nil {
		return Message{}, "", err
	}
	var found []Message
	var mailbox string
	for _, e := range entries {
		if !e.IsDir() || !addr.ValidID(e.Name()) {
			continue
		}
		msg, err := s.Resolve(e.Name(), query)
		switch {
		case err == nil:
			found = append(found, msg)
			mailbox = e.Name()
		case errors.Is(err, ErrUnknownID):
		case errors.Is(err, ErrAmbiguousID):
			return Message{}, "", err
		default:
			return Message{}, "", err
		}
	}
	switch len(found) {
	case 0:
		return Message{}, "", ErrUnknownID
	case 1:
		return found[0], mailbox, nil
	default:
		return Message{}, "", ErrAmbiguousID
	}
}

// Claim marks msgID queued if it is still stored, under the mailbox lock. It
// reports false when someone else claimed it first.
func (s *Store) Claim(id, msgID string) (Message, bool, error) {
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return Message{}, false, err
	}
	defer unlock(f)
	msgs, err := s.messagesLocked(f, id)
	if err != nil {
		return Message{}, false, err
	}
	msg, err := find(msgs, msgID)
	if err != nil {
		return Message{}, false, err
	}
	if msg.State != StateStored {
		return msg, false, nil
	}
	if err := s.appendReceipt(id, msgID, StateQueued); err != nil {
		return Message{}, false, err
	}
	msg.State = StateQueued
	return msg, true, nil
}

// ClaimPending marks every stored message queued, in append order, and
// returns them. A second call returns nothing: a message is delivered once.
func (s *Store) ClaimPending(id string) ([]Message, error) {
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	defer unlock(f)
	msgs, err := s.messagesLocked(f, id)
	if err != nil {
		return nil, err
	}
	var claimed []Message
	for _, msg := range msgs {
		if msg.State != StateStored {
			continue
		}
		if err := s.appendReceipt(id, msg.Envelope.ID, StateQueued); err != nil {
			return nil, err
		}
		msg.State = StateQueued
		claimed = append(claimed, msg)
	}
	return claimed, nil
}

// Mark records state for msgID if it differs from the message's current one.
func (s *Store) Mark(id, msgID, state string) error {
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock(f)
	msgs, err := s.messagesLocked(f, id)
	if err != nil {
		return err
	}
	msg, err := find(msgs, msgID)
	if err != nil {
		return err
	}
	if msg.State == state {
		return nil
	}
	return s.appendReceipt(id, msgID, state)
}

// Release returns a claimed message to stored, for example after a rejected
// push, so a later drain retries it.
func (s *Store) Release(id, msgID string) error { return s.Mark(id, msgID, StateStored) }

// MarkQueuedRead records read for every queued message: the harness reported
// the turn started.
func (s *Store) MarkQueuedRead(id string) error {
	f, err := s.lockMailbox(id, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock(f)
	msgs, err := s.messagesLocked(f, id)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		if msg.State != StateQueued {
			continue
		}
		if err := s.appendReceipt(id, msg.Envelope.ID, StateRead); err != nil {
			return err
		}
	}
	return nil
}

// Render is how agent id sees env in its timeline.
func (s *Store) Render(id string, env envelope.Envelope) (string, error) {
	short, err := s.ShortID(id, env.ID)
	if err != nil {
		return "", err
	}
	return envelope.Render(env, short), nil
}

func shortID(msgs []Message, msgID string) string {
	target := strings.ToLower(msgID)
	for n := 4; n < len(target); n++ {
		suffix := target[len(target)-n:]
		matches := 0
		for _, m := range msgs {
			if strings.HasSuffix(strings.ToLower(m.Envelope.ID), suffix) {
				matches++
			}
		}
		if matches == 1 {
			return suffix
		}
	}
	return target
}

func resolve(msgs []Message, query string) (Message, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Message{}, ErrUnknownID
	}
	var found []Message
	for _, m := range msgs {
		if strings.HasSuffix(strings.ToLower(m.Envelope.ID), q) {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return Message{}, ErrUnknownID
	case 1:
		return found[0], nil
	default:
		return Message{}, ErrAmbiguousID
	}
}

// lockMailbox opens id's inbox (creating the mailbox) and takes the mailbox
// lock: every read and write of the mailbox goes through it.
func (s *Store) lockMailbox(id string, how int) (*os.File, error) {
	if !addr.ValidID(id) {
		return nil, fmt.Errorf("bad agent id %q", id)
	}
	path := s.inboxPath(id)
	if err := ensureDir(filepath.Dir(filepath.Dir(path))); err != nil {
		return nil, err
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, FileMode)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(FileMode); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// readInbox reads complete lines from the locked inbox. A torn trailing line
// is what a crashed writer leaves behind; it is not a message.
func readInbox(f *os.File) ([]envelope.Envelope, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	var envs []envelope.Envelope
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		env, err := envelope.Decode(sc.Bytes())
		if err != nil {
			return nil, fmt.Errorf("inbox line %d: %w", n, err)
		}
		envs = append(envs, env)
	}
	return envs, sc.Err()
}

// states reads the receipts of id's mailbox: the last state recorded per
// message wins.
func (s *Store) states(id string) (map[string]string, error) {
	data, err := os.ReadFile(s.receiptsPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	// A crashed writer leaves a torn last line; it is not a receipt.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	states := map[string]string{}
	for n, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r receipt
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("receipts line %d: %w", n+1, err)
		}
		states[r.ID] = r.State
	}
	return states, nil
}

// receipt is one state change in receipts.jsonl.
type receipt struct {
	ID    string `json:"id"`
	State string `json:"state"`
	TS    string `json:"ts"`
}

// appendReceipt records a state change. The caller holds the mailbox lock.
func (s *Store) appendReceipt(id, msgID, state string) error {
	data, err := json.Marshal(receipt{ID: msgID, State: state, TS: envelope.FormatTime(time.Now())})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.receiptsPath(id), os.O_WRONLY|os.O_APPEND|os.O_CREATE, FileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(FileMode); err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

func join(envs []envelope.Envelope, states map[string]string) []Message {
	msgs := make([]Message, len(envs))
	for i, env := range envs {
		state, ok := states[env.ID]
		if !ok {
			state = StateStored
		}
		msgs[i] = Message{Envelope: env, State: state}
	}
	return msgs
}

// messagesLocked reads a mailbox the caller has already locked.
func (s *Store) messagesLocked(f *os.File, id string) ([]Message, error) {
	envs, err := readInbox(f)
	if err != nil {
		return nil, err
	}
	states, err := s.states(id)
	if err != nil {
		return nil, err
	}
	return join(envs, states), nil
}

// find returns the message with exactly this id.
func find(msgs []Message, msgID string) (Message, error) {
	for _, m := range msgs {
		if m.Envelope.ID == msgID {
			return m, nil
		}
	}
	return Message{}, fmt.Errorf("%w: %s", ErrUnknownID, msgID)
}

func (s *Store) inboxPath(id string) string {
	return filepath.Join(s.root, "mail", id, "inbox.jsonl")
}

func (s *Store) receiptsPath(id string) string {
	return filepath.Join(s.root, "mail", id, "receipts.jsonl")
}
