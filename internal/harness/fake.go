package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/store"
)

// Fake is a Harness for tests only: it appends every pushed envelope to
// <Dir>/pushes.jsonl, so a test can assert what was handed to the agent.
type Fake struct {
	Dir   string // where pushes.jsonl lives
	Queue bool   // report that the harness queues while busy
	Fail  bool   // reject every push
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

func (f *Fake) Push(_ store.Agent, msgs []envelope.Envelope) error {
	if f.Fail {
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
	for _, msg := range msgs {
		line, err := msg.Line()
		if err != nil {
			return err
		}
		if _, err := file.Write(line); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) Shims() []Shim { return nil }

func (f *Fake) Check() error { return nil }

// PushesPath is where the fake records pushes.
func (f *Fake) PushesPath() string { return filepath.Join(f.Dir, "pushes.jsonl") }

// Pushes returns the envelopes the fake was asked to hand over.
func (f *Fake) Pushes() ([]envelope.Envelope, error) {
	data, err := os.ReadFile(f.PushesPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []envelope.Envelope
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		env, err := envelope.Decode([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.PushesPath(), err)
		}
		out = append(out, env)
	}
	return out, nil
}
