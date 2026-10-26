package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/store"
)

// Fake is a Harness for tests only: it appends every pushed message to
// <Dir>/pushes.jsonl, so a test can assert what was handed to the agent.
type Fake struct {
	Dir       string // where pushes.jsonl lives
	Queue     bool   // report that the harness queues while busy
	Fail      bool   // reject every push
	FailAfter int    // reject the push call with this 1-based number and every later one

	mu    sync.Mutex
	calls int
}

func (f *Fake) Name() string { return "fake" }

// Detect reports the agent id in FAKE_AGENT_ID, the environment variable a
// test uses in place of a real harness's session id.
func (f *Fake) Detect(env []string) (string, bool) {
	for _, kv := range env {
		if id, ok := strings.CutPrefix(kv, "FAKE_AGENT_ID="); ok && id != "" {
			return id, true
		}
	}
	return "", false
}

func (f *Fake) QueuesWhileBusy() bool { return f.Queue }

func (f *Fake) Push(_ store.Agent, msg Outgoing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.Fail || (f.FailAfter > 0 && f.calls >= f.FailAfter) {
		return errors.New("fake harness rejected the push")
	}
	if f.Dir == "" {
		return errors.New("fake harness has no Dir")
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(f.PushesPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	line, err := json.Marshal(pushed(msg))
	if err != nil {
		return err
	}
	_, err = file.Write(append(line, '\n'))
	return err
}

func (f *Fake) Shims() []Shim { return nil }

func (f *Fake) Check() error { return nil }

// PushesPath is where the fake records pushes.
func (f *Fake) PushesPath() string { return filepath.Join(f.Dir, "pushes.jsonl") }

// pushed is one line of pushes.jsonl: the envelope and the text the harness
// was asked to hand the agent.
type pushed struct {
	Envelope envelope.Envelope `json:"envelope"`
	Text     string            `json:"text"`
}

// Pushes returns the envelopes the fake was asked to hand over.
func (f *Fake) Pushes() ([]envelope.Envelope, error) {
	var out []envelope.Envelope
	err := f.each(func(p pushed) { out = append(out, p.Envelope) })
	return out, err
}

// Texts returns the rendered text of each push, in order.
func (f *Fake) Texts() ([]string, error) {
	var out []string
	err := f.each(func(p pushed) { out = append(out, p.Text) })
	return out, err
}

func (f *Fake) each(visit func(pushed)) error {
	data, err := os.ReadFile(f.PushesPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var p pushed
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return fmt.Errorf("%s: %w", f.PushesPath(), err)
		}
		visit(p)
	}
	return nil
}
