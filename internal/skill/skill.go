// Package skill installs recado's agent skill. The installed SKILL.md is a
// stub that tells the agent to run `recado skill`, so the guide it follows
// always comes from the recado binary on PATH.
package skill

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Guide is the text `recado skill` prints for agents.
//
//go:embed guide.md
var Guide string

// stubVersion changes whenever Stub's text does, so setup can tell an
// outdated stub of ours from a skill someone else wrote.
const stubVersion = "1"

const stubMarker = "recado-stub:"

// Stub is the SKILL.md that setup installs.
func Stub() string {
	return `---
name: recado
description: Send and receive messages between agents running in different harnesses, terminals and machines with the recado CLI. Use when you need to report to, ask, or answer another agent.
metadata:
  ` + stubMarker + ` "` + stubVersion + `"
---
<!-- Installed by "recado skill setup". Remove it with "recado skill remove". -->

Run ` + "`recado skill`" + ` and follow the guide it prints. The guide comes from
the installed recado CLI, so it describes the commands available in that
version.

If the command fails, run ` + "`recado --help`" + `.
`
}

// Target is one place an agent harness looks for skills.
type Target struct {
	Agents string // the harnesses that read this directory
	Dir    string // the recado skill directory
}

func (t Target) file() string { return filepath.Join(t.Dir, "SKILL.md") }

// Targets lists the skill directories under home: Claude Code reads its own,
// and Codex, OpenCode and Pi share ~/.agents/skills.
func Targets(home string) []Target {
	return []Target{
		{Agents: "Claude Code", Dir: filepath.Join(home, ".claude", "skills", "recado")},
		{Agents: "Codex, OpenCode, Pi", Dir: filepath.Join(home, ".agents", "skills", "recado")},
	}
}

// State is what a Target holds.
type State int

const (
	Missing  State = iota // no SKILL.md
	Current               // this binary's stub
	Outdated              // an older or edited recado stub
	Foreign               // a SKILL.md recado did not install
)

func (s State) String() string {
	return [...]string{"missing", "current", "outdated", "foreign"}[s]
}

// Inspect reports what t holds.
func Inspect(t Target) (State, error) {
	data, err := os.ReadFile(t.file())
	if errors.Is(err, os.ErrNotExist) {
		return Missing, nil
	}
	if err != nil {
		return 0, err
	}
	switch {
	case string(data) == Stub():
		return Current, nil
	case strings.Contains(string(data), stubMarker):
		return Outdated, nil
	default:
		return Foreign, nil
	}
}

// Install writes the stub into t, replacing an outdated one. It refuses to
// overwrite a skill recado did not install.
func Install(t Target) error {
	state, err := Inspect(t)
	if err != nil {
		return err
	}
	if state == Foreign {
		return fmt.Errorf("%s holds a skill recado did not install; remove it first", t.file())
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(t.file(), []byte(Stub()), 0o644)
}

// Remove deletes the stub from t, leaving a skill recado did not install.
// It reports whether it removed anything.
func Remove(t Target) (bool, error) {
	state, err := Inspect(t)
	if err != nil {
		return false, err
	}
	switch state {
	case Missing:
		return false, nil
	case Foreign:
		return false, fmt.Errorf("%s holds a skill recado did not install; leaving it", t.file())
	}
	if err := os.Remove(t.file()); err != nil {
		return false, err
	}
	// Leave the directory if the user keeps anything else in it.
	if entries, err := os.ReadDir(t.Dir); err != nil || len(entries) > 0 {
		return true, err
	}
	return true, os.Remove(t.Dir)
}
