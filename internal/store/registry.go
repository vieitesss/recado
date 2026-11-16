package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

// AgentState is what an agent's harness last reported: busy while a turn
// runs, idle between them. An absent state means it never reported one.
type AgentState string

const (
	AgentBusy AgentState = "busy"
	AgentIdle AgentState = "idle"
)

// Valid reports whether s is a state a harness reports.
func (s AgentState) Valid() bool { return s == AgentBusy || s == AgentIdle }

// Agent is one registry entry: agents/<id>.json. The id is the file name, so
// it is never duplicated inside the file.
type Agent struct {
	ID         string     `json:"-"`
	Harness    string     `json:"harness"`
	Endpoint   string     `json:"endpoint"`
	Token      string     `json:"token,omitempty"` // a session credential, never printed
	PID        int        `json:"pid"`
	Pane       string     `json:"pane,omitempty"`
	RexSession string     `json:"rex_session,omitempty"` // Rex session id, resolved to a label when listed
	RexBlock   string     `json:"rex_block,omitempty"`
	CWD        string     `json:"cwd,omitempty"`
	Name       string     `json:"name,omitempty"` // readable name: the repository or directory base
	Display    string     `json:"-"`              // Name plus a telling id suffix, filled by List
	Started    time.Time  `json:"started"`
	State      AgentState `json:"state,omitempty"`
	StateTS    string     `json:"state_ts,omitempty"`
	Retired    bool       `json:"retired,omitempty"` // a later id took this process's session over
}

// Live reports whether the agent's session is still there: its process runs and
// an id it registered later did not supersede it. A retired id keeps its
// mailbox, so messages still wait for it and arrive if it resumes.
func (a Agent) Live() bool {
	if a.Retired || a.PID <= 0 {
		return false
	}
	err := syscall.Kill(a.PID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Register writes a's registry entry, creating or replacing it atomically.
func (s *Store) Register(a Agent) error {
	if !addr.ValidID(a.ID) {
		return fmt.Errorf("bad agent id %q", a.ID)
	}
	dir := filepath.Join(s.root, "agents")
	if err := ensureDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, "."+a.ID+".*")
	if err != nil {
		return err
	}
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, a.ID+".json")); err != nil {
		return err
	}
	tmp = nil
	return nil
}

// RetireOthers marks every entry of pid and harnessName other than keepID
// retired, with the same atomic rewrite Register uses. It is for a harness that
// runs one session per process: a session start with a new id supersedes the
// ids that process registered before, so no idle push or relay sender reaches
// the old ones. Entries of another pid or harness are untouched, and an entry
// already retired is left alone.
func (s *Store) RetireOthers(pid int, harnessName, keepID string) error {
	if pid <= 0 {
		return nil // no process identity, so nothing is superseded
	}
	agents, err := s.List()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.ID == keepID || a.PID != pid || a.Harness != harnessName || a.Retired {
			continue
		}
		a.Retired = true
		if err := s.Register(a); err != nil {
			return err
		}
	}
	return nil
}

// Lookup returns the registry entry for id.
func (s *Store) Lookup(id string) (Agent, error) {
	if !addr.ValidID(id) {
		return Agent{}, fmt.Errorf("%w: %q", ErrUnknownAgent, id)
	}
	data, err := os.ReadFile(filepath.Join(s.root, "agents", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Agent{}, fmt.Errorf("%w: %s", ErrUnknownAgent, id)
	}
	if err != nil {
		return Agent{}, err
	}
	var a Agent
	if err := json.Unmarshal(data, &a); err != nil {
		return Agent{}, fmt.Errorf("agents/%s.json: %w", id, err)
	}
	a.ID = id
	return a, nil
}

// List returns every registered agent, sorted by id, live or gone.
func (s *Store) List() ([]Agent, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "agents"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	agents := make([]Agent, 0, len(entries))
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok || !addr.ValidID(name) {
			continue
		}
		a, err := s.Lookup(name)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	nameAgents(agents)
	return agents, nil
}

// SetState records whether the agent's harness is busy or idle, atomically.
func (s *Store) SetState(id string, state AgentState) error {
	if !state.Valid() {
		return fmt.Errorf("bad agent state %q", state)
	}
	a, err := s.Lookup(id)
	if err != nil {
		return err
	}
	a.State = state
	a.StateTS = envelope.FormatTime(time.Now())
	return s.Register(a)
}
