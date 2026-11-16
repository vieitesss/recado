package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/envelope"
)

// deadPID is above Linux's and macOS's pid ceilings, so it is never live.
const deadPID = 1 << 22

func register(t *testing.T, s *Store, id string, pid int) Agent {
	t.Helper()
	a := Agent{
		ID:       id,
		Harness:  "fake",
		Endpoint: "/tmp/" + id + ".sock",
		PID:      pid,
		Pane:     "%1",
		CWD:      "/tmp",
		Started:  time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.Register(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRegisterLookupList(t *testing.T) {
	s, root := open(t)
	want := register(t, s, "sub-1", os.Getpid())

	got, err := s.Lookup("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Harness != "fake" || got.Endpoint != want.Endpoint ||
		got.PID != want.PID || got.Pane != want.Pane || got.CWD != want.CWD || !got.Started.Equal(want.Started) {
		t.Errorf("Lookup = %+v, want %+v", got, want)
	}
	if got.State != "" || got.StateTS != "" {
		t.Errorf("state = %q/%q, want never reported", got.State, got.StateTS)
	}
	if !got.Live() {
		t.Error("agent with this process's pid reported gone")
	}

	agents, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ID != "sub-1" {
		t.Fatalf("List = %+v, want sub-1", agents)
	}

	requireMode(t, filepath.Join(root, "agents"), DirMode)
	requireMode(t, filepath.Join(root, "agents", "sub-1.json"), FileMode)

	if _, err := s.Lookup("nobody"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("Lookup(unknown) error = %v, want ErrUnknownAgent", err)
	}
}

func TestRegistryFileKeepsDocumentedFields(t *testing.T) {
	s, root := open(t)
	register(t, s, "sub-1", os.Getpid())

	data, err := os.ReadFile(filepath.Join(root, "agents", "sub-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"harness", "endpoint", "pid", "pane", "cwd", "started"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("registry %s has no %q field", data, key)
		}
	}
	for _, key := range []string{"state", "rex_session", "rex_block"} {
		if _, ok := fields[key]; ok {
			t.Errorf("registry %s has a %s field on a plain entry", data, key)
		}
	}
}

func TestRegistryKeepsRexMetadata(t *testing.T) {
	s, root := open(t)
	a := Agent{ID: "sub-1", Harness: "fake", PID: os.Getpid(), RexSession: "session:abc", RexBlock: "block-1"}
	if err := s.Register(a); err != nil {
		t.Fatal(err)
	}

	got, err := s.Lookup("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RexSession != a.RexSession || got.RexBlock != a.RexBlock {
		t.Errorf("Lookup = %+v, want Rex session %q block %q", got, a.RexSession, a.RexBlock)
	}

	data, err := os.ReadFile(filepath.Join(root, "agents", "sub-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if got := string(fields["rex_session"]); got != `"session:abc"` {
		t.Errorf("rex_session = %s, want \"session:abc\"", got)
	}
	if got := string(fields["rex_block"]); got != `"block-1"` {
		t.Errorf("rex_block = %s, want \"block-1\"", got)
	}
}

func TestLivenessByPID(t *testing.T) {
	s, _ := open(t)
	register(t, s, "gone", deadPID)
	got, err := s.Lookup("gone")
	if err != nil {
		t.Fatal(err)
	}
	if got.Live() {
		t.Errorf("agent with pid %d reported live", deadPID)
	}
}

func TestSetState(t *testing.T) {
	s, root := open(t)
	register(t, s, "sub-1", os.Getpid())

	if err := s.SetState("sub-1", AgentBusy); err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != AgentBusy {
		t.Errorf("state = %q, want %q", got.State, AgentBusy)
	}
	if _, err := envelope.ParseTime(got.StateTS); err != nil {
		t.Errorf("state_ts = %q: %v", got.StateTS, err)
	}

	data, err := os.ReadFile(filepath.Join(root, "agents", "sub-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["state"]; !ok {
		t.Errorf("registry %s has no state field after a turn", data)
	}
	if _, ok := fields["state_ts"]; !ok {
		t.Errorf("registry %s has no state_ts field after a turn", data)
	}

	if err := s.SetState("nobody", AgentIdle); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("SetState(unknown) error = %v, want ErrUnknownAgent", err)
	}
	if err := s.SetState("sub-1", "sleepy"); err == nil {
		t.Error("SetState accepted a state no harness reports")
	}
}

func TestRegisterRejectsBadID(t *testing.T) {
	s, _ := open(t)
	if err := s.Register(Agent{ID: "../escape", Harness: "fake"}); err == nil {
		t.Fatal("Register accepted an id with path separators")
	}
}
