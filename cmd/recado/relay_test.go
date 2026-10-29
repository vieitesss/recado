package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/relay"
	"github.com/vieitesss/recado/internal/store"
)

func TestRelayDrainsRewritesAndResumes(t *testing.T) {
	remote, _ := newTestApp(t, false)
	local, fake := newTestApp(t, true)
	startSession(t, local, "manager")
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, remote, "", "send", "recado+upstream://"+hostname(t)+"/manager", "answer")
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	local.Machines = remoteTestRegistry(t)
	mustExec(t, local, "", "relay", "rpi")
	t.Setenv("RECADO_ADDR", "manager")
	var got struct {
		Messages []message `json:"messages"`
	}
	got = decode[struct {
		Messages []message `json:"messages"`
	}](t, mustExec(t, local, "", "inbox", "--peek", "--json"))
	if len(got.Messages) != 1 || got.Messages[0].Envelope.From != "recado://rpi/worker" || got.Messages[0].Envelope.To != "recado://local/manager" || got.Messages[0].Envelope.Body != "answer" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	mustExec(t, local, "", "relay", "rpi")
	got = decode[struct {
		Messages []message `json:"messages"`
	}](t, mustExec(t, local, "", "inbox", "--peek", "--json"))
	if len(got.Messages) != 1 {
		t.Fatalf("restart delivered %d messages", len(got.Messages))
	}
	// Simulate a crash after mailbox delivery but before the cursor was saved.
	if err := os.WriteFile(filepath.Join(local.Root, "relay", "rpi.cursor"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustExec(t, local, "", "relay", "rpi")
	got = decode[struct {
		Messages []message `json:"messages"`
	}](t, mustExec(t, local, "", "inbox", "--peek", "--json"))
	if len(got.Messages) != 1 {
		t.Fatalf("replayed delivery stored %d messages", len(got.Messages))
	}
	pushes, err := fake.Pushes()
	if err != nil || len(pushes) != 1 {
		t.Fatalf("replayed delivery pushed %d times: %v", len(pushes), err)
	}
}

func TestRelayRepairsTornSenderAndKeepsLiveSender(t *testing.T) {
	remote, _ := newTestApp(t, false)
	local, _ := newTestApp(t, false)
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	t.Setenv("RECADO_ADDR", "manager")
	local.Machines = remoteTestRegistry(t)
	localStore, err := local.store()
	if err != nil {
		t.Fatal(err)
	}
	if err := localStore.Register(store.Agent{ID: "manager", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	remoteStore, err := remote.store()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteStore.Register(store.Agent{ID: "worker"}); err != nil {
		t.Fatal(err)
	}
	senders := filepath.Join(local.Root, "relay", "rpi.senders")
	if err := os.MkdirAll(filepath.Dir(senders), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(senders, []byte("man"), 0600); err != nil {
		t.Fatal(err)
	}
	mustExec(t, local, "", "send", "rpi/worker", "question")
	data, err := os.ReadFile(senders)
	if err != nil || string(data) != "manager\n" {
		t.Fatalf("senders after send = %q, %v; want manager\\n", data, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var relayErr error
	go func() {
		cmd := newRootCmd(local)
		cmd.SetArgs([]string{"relay", "rpi"})
		relayErr = cmd.ExecuteContext(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("relay did not stop after cancellation")
		}
	}()
	select {
	case <-done:
		t.Fatalf("relay exited while sender live: %v", relayErr)
	case <-time.After(600 * time.Millisecond):
	}

	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, remote, "", "send", "recado+upstream://"+hostname(t)+"/manager", "answer")
	deadline := time.Now().Add(4 * time.Second)
	for {
		messages, err := localStore.Messages("manager")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 1 && messages[0].Envelope.Body == "answer" {
			break
		}
		select {
		case <-done:
			t.Fatalf("relay exited while sender live: %v", relayErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("relay did not deliver answer while sender remained live")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRecordSenderRepairsBeforeDeduplication(t *testing.T) {
	for _, tc := range []struct{ initial, want string }{
		{"manager", "manager\n"},
		{"other\nman", "other\nmanager\n"},
		{"other\nmanager\nmanager", "other\nmanager\n"},
	} {
		t.Run(strings.ReplaceAll(tc.initial, "\n", "/"), func(t *testing.T) {
			root := t.TempDir()
			senders := filepath.Join(root, "relay", "rpi.senders")
			if err := os.MkdirAll(filepath.Dir(senders), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(senders, []byte(tc.initial), 0600); err != nil {
				t.Fatal(err)
			}
			if err := relay.RecordSender(root, "rpi", "manager", func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(senders)
			if err != nil || string(data) != tc.want {
				t.Fatalf("senders = %q, %v; want %q", data, err, tc.want)
			}
		})
	}
}

func TestRelayIgnoresUnterminatedSender(t *testing.T) {
	remote, _ := newTestApp(t, false)
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	for _, tail := range []string{"manager", "bad/id"} {
		t.Run(tail, func(t *testing.T) {
			local, _ := newTestApp(t, false)
			local.Machines = remoteTestRegistry(t)
			st, err := local.store()
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Register(store.Agent{ID: "manager", PID: os.Getpid()}); err != nil {
				t.Fatal(err)
			}
			senders := filepath.Join(local.Root, "relay", "rpi.senders")
			if err := os.MkdirAll(filepath.Dir(senders), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(senders, []byte("unknown\n"+tail), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				cmd := newRootCmd(local)
				cmd.SetArgs([]string{"relay", "rpi"})
				done <- cmd.ExecuteContext(ctx)
			}()
			select {
			case err := <-done:
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				cancel()
				<-done
				t.Fatal("relay did not exit when only an unterminated sender remained")
			}
		})
	}
}

func TestRelaySingleInstanceAndIdleExit(t *testing.T) {
	remote, _ := newTestApp(t, false)
	local, _ := newTestApp(t, false)
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	local.Machines = remoteTestRegistry(t)
	st, err := local.store()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Register(store.Agent{ID: "manager", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := relay.RecordSender(local.Root, "rpi", "manager", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := newRootCmd(local)
	cmd.SetArgs([]string{"relay", "rpi"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(filepath.Join(local.Root, "relay", "rpi.lock"), os.O_RDWR, 0600)
		if err == nil {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			_ = f.Close()
			if err != nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("first relay did not acquire lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err = exec(t, local, "", "relay", "rpi")
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second relay: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("relay exited while sender live: %v", err)
	default:
	}
	if err := st.Register(store.Agent{ID: "manager", PID: 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not exit after sender disappeared")
	}
}

func TestRelayRetriesUndeliverableMessageWithoutAdvancing(t *testing.T) {
	remote, _ := newTestApp(t, false)
	local, _ := newTestApp(t, false)
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, remote, "", "send", "recado+upstream://"+hostname(t)+"/manager", "retry me")
	t.Setenv("RECADO_TEST_REMOTE", "1")
	t.Setenv("RECADO_STATE_DIR", remote.Root)
	local.Machines = remoteTestRegistry(t)
	st, err := local.store()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Register(store.Agent{ID: "observer", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := relay.RecordSender(local.Root, "rpi", "observer", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := newRootCmd(local)
	cmd.SetArgs([]string{"relay", "rpi"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(local.Root, "relay", "rpi.log"))
		if strings.Contains(string(data), "unknown agent") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no delivery failure logged: %s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b, err := os.ReadFile(filepath.Join(local.Root, "relay", "rpi.cursor")); err == nil && strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("cursor advanced on failure: %s", b)
	}
	if err := st.Register(store.Agent{ID: "manager"}); err != nil {
		t.Fatal(err)
	}
	for {
		msgs, err := st.Messages("manager")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) == 1 {
			if msgs[0].Envelope.Body != "retry me" {
				t.Fatalf("message = %+v", msgs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("message was not retried")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := st.Register(store.Agent{ID: "observer"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not exit")
	}
}

func hostname(t *testing.T) string {
	t.Helper()
	s, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
