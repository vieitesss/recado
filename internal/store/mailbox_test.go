package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

// Two ULIDs that share their last four characters, so their short ids must be
// one character longer. Differing at index 21 keeps the rest identical.
const (
	ulidA = "01JB7Q4W2Z6ZC9M1Z3X4Y5V6T7"
	ulidB = "01JB7Q4W2Z6ZC9M1Z3X4Y4V6T7"
	ulidC = "01JB7Q4W2Z6ZC9M1Z3X4Y5W6T7"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func message(id, body string) envelope.Envelope {
	return envelope.Envelope{
		V:    1,
		ID:   id,
		TS:   time.Now().UTC().Truncate(time.Millisecond),
		From: addr.Local("asker"),
		To:   addr.Local("sub-1"),
		Kind: envelope.KindMessage,
		Body: body,
		Meta: map[string]string{},
	}
}

func TestAppendAndRead(t *testing.T) {
	s, root := open(t)
	if err := s.Append("sub-1", message(ulidA, "first")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("sub-1", message(ulidB, "second")); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Messages = %d messages, want 2", len(msgs))
	}
	if msgs[0].Envelope.Body != "first" || msgs[1].Envelope.Body != "second" {
		t.Errorf("order = %q, %q; want append order", msgs[0].Envelope.Body, msgs[1].Envelope.Body)
	}
	if msgs[0].State != StateStored {
		t.Errorf("state = %q, want %q", msgs[0].State, StateStored)
	}

	requireMode(t, root, DirMode)
	requireMode(t, filepath.Join(root, "mail"), DirMode)
	requireMode(t, filepath.Join(root, "mail", "sub-1"), DirMode)
	requireMode(t, filepath.Join(root, "mail", "sub-1", "inbox.jsonl"), FileMode)
}

func TestAppendIsIdempotentByID(t *testing.T) {
	s, _ := open(t)
	env := message(ulidA, "once")
	for range 3 {
		if err := s.Append("sub-1", env); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Messages = %d, want 1: a retried envelope is not stored twice", len(msgs))
	}
}

func TestOpenEnforcesModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("sub-1", message(ulidA, "hi")); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(Agent{ID: "sub-1", Harness: "fake", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	requireMode(t, root, DirMode)
	requireMode(t, filepath.Join(root, "mail"), DirMode)
	requireMode(t, filepath.Join(root, "mail", "sub-1"), DirMode)
	requireMode(t, filepath.Join(root, "mail", "sub-1", "inbox.jsonl"), FileMode)
	requireMode(t, filepath.Join(root, "agents"), DirMode)
	requireMode(t, filepath.Join(root, "agents", "sub-1.json"), FileMode)
}

func TestConcurrentAppendsDoNotInterleave(t *testing.T) {
	s, _ := open(t)
	const senders = 20

	var wg sync.WaitGroup
	for i := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := message(envelope.NewID(), fmt.Sprintf("message %d", i))
			if err := s.Append("sub-1", env); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()

	msgs, err := s.Messages("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != senders {
		t.Fatalf("Messages = %d, want %d intact lines", len(msgs), senders)
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		seen[m.Envelope.Body] = true
	}
	if len(seen) != senders {
		t.Errorf("%d distinct bodies, want %d", len(seen), senders)
	}
}

func TestShortIDIsShortestUniqueSuffix(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB, ulidC} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct{ id, want string }{
		{ulidA, "5v6t7"}, // ulidB shares the last four characters
		{ulidB, "4v6t7"},
		{ulidC, "w6t7"}, // unique in four
	} {
		got, err := s.ShortID("sub-1", c.id)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("ShortID(%s) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestResolveAcceptsUniqueSuffixAndFullID(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{ulidA, ulidB, ulidC} {
		if err := s.Append("sub-1", message(id, id)); err != nil {
			t.Fatal(err)
		}
	}

	for _, query := range []string{"5v6t7", "5V6T7", ulidA} {
		msg, err := s.Resolve("sub-1", query)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", query, err)
		}
		if msg.Envelope.ID != ulidA {
			t.Errorf("Resolve(%q) = %s, want %s", query, msg.Envelope.ID, ulidA)
		}
	}

	if _, err := s.Resolve("sub-1", "v6t7"); !errors.Is(err, ErrAmbiguousID) {
		t.Errorf("Resolve(shared suffix) error = %v, want ErrAmbiguousID", err)
	}
	if _, err := s.Resolve("sub-1", "zzzz"); !errors.Is(err, ErrUnknownID) {
		t.Errorf("Resolve(unknown suffix) error = %v, want ErrUnknownID", err)
	}
	if _, err := s.Resolve("sub-1", "01JB7"); !errors.Is(err, ErrUnknownID) {
		t.Errorf("Resolve(prefix) error = %v, want ErrUnknownID: a prefix is not a suffix", err)
	}
	if _, err := s.Resolve("sub-1", ""); !errors.Is(err, ErrUnknownID) {
		t.Errorf("Resolve(empty) error = %v, want ErrUnknownID", err)
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("RECADO_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	if got := os.Getenv("HOME"); got == "" {
		t.Skip("no HOME")
	}
	root, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(os.Getenv("HOME"), ".local", "state", "recado"); root != want {
		t.Errorf("DefaultRoot = %q, want %q", root, want)
	}

	t.Setenv("XDG_STATE_HOME", "/xdg")
	if root, _ := DefaultRoot(); root != filepath.Join("/xdg", "recado") {
		t.Errorf("DefaultRoot = %q, want /xdg/recado", root)
	}

	t.Setenv("RECADO_STATE_DIR", "/override")
	if root, _ := DefaultRoot(); root != "/override" {
		t.Errorf("DefaultRoot = %q, want /override", root)
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}
