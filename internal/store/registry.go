package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

// Agent states a shim records with `recado hook HARNESS turn-start|turn-end`.
// An absent state means the agent never reported one.
const (
	AgentBusy = "busy"
	AgentIdle = "idle"
)

// Agent is one registry entry: agents/<id>.json. The id is the file name, so
// it is never duplicated inside the file.
type Agent struct {
	ID       string    `json:"-"`
	Harness  string    `json:"harness"`
	Endpoint string    `json:"endpoint"`
	PID      int       `json:"pid"`
	Pane     string    `json:"pane,omitempty"`
	CWD      string    `json:"cwd,omitempty"`
	Started  time.Time `json:"started"`
	State    string    `json:"state,omitempty"`
	StateTS  string    `json:"state_ts,omitempty"`
}

// Live reports whether the agent's process is still running.
func (a Agent) Live() bool { return alive(a.PID) }

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
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
		name, ok := cutSuffix(e.Name(), ".json")
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
	return agents, nil
}

// SetState records whether the agent's harness is busy or idle, atomically.
func (s *Store) SetState(id, state string) error {
	a, err := s.Lookup(id)
	if err != nil {
		return err
	}
	a.State = state
	a.StateTS = envelope.FormatTime(time.Now())
	return s.Register(a)
}

func cutSuffix(s, suffix string) (string, bool) {
	if len(s) <= len(suffix) || s[len(s)-len(suffix):] != suffix {
		return s, false
	}
	return s[:len(s)-len(suffix)], true
}
