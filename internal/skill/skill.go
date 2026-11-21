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
const stubVersion = "2"

const stubMarker = "recado-stub:"

// Stub is the SKILL.md that setup installs.
func Stub() string {
	return `---
name: recado
description: Send and receive messages between agents running in different harnesses and on different machines with the recado CLI. Use when you need to report to, ask, or answer another agent.
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

// MarkerShim stamps the files that carry a harness shim, so recado can tell an
// outdated copy of its own (repairable) from a file someone else wrote.
const MarkerShim = "recado-shim:"

// File is one file recado installs for an agent harness: the skill stub, or a
// harness shim such as Pi's extension. Marker appears in every file recado
// writes.
type File struct {
	Path    string
	Content []byte
	Mode    os.FileMode
	Marker  string
}

// Inspect reports what is at f.Path.
func (f File) Inspect() (State, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Missing, nil
	}
	if err != nil {
		return 0, err
	}
	switch {
	case string(data) == string(f.Content):
		return Current, nil
	case strings.Contains(string(data), f.Marker):
		return Outdated, nil
	default:
		return Foreign, nil
	}
}

// Install writes f, replacing an outdated copy. It refuses to overwrite a file
// recado did not write.
func (f File) Install() error {
	state, err := f.Inspect()
	if err != nil {
		return err
	}
	if state == Foreign {
		return fmt.Errorf("%s was not installed by recado; remove it first", f.Path)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(f.Path, f.Content, f.Mode)
}

// Remove deletes f, leaving a file recado did not write. It reports whether it
// removed anything.
func (f File) Remove() (bool, error) {
	state, err := f.Inspect()
	if err != nil {
		return false, err
	}
	switch state {
	case Missing:
		return false, nil
	case Foreign:
		return false, fmt.Errorf("%s was not installed by recado; leaving it", f.Path)
	}
	return true, os.Remove(f.Path)
}

// Target is one place an agent harness looks for skills.
type Target struct {
	Agents string // the harnesses that read this directory
	Dir    string // the recado skill directory
}

// installed is the SKILL.md this target holds.
func (t Target) installed() File {
	return File{
		Path:    filepath.Join(t.Dir, "SKILL.md"),
		Content: []byte(Stub()),
		Mode:    0o644,
		Marker:  stubMarker,
	}
}

// Targets lists the skill directories under home: Claude Code and Antigravity
// read their own, and Codex, OpenCode and Pi share ~/.agents/skills.
func Targets(home string) []Target {
	return []Target{
		{Agents: "Claude Code", Dir: filepath.Join(home, ".claude", "skills", "recado")},
		{Agents: "Antigravity", Dir: filepath.Join(home, ".gemini", "config", "skills", "recado")},
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
func Inspect(t Target) (State, error) { return t.installed().Inspect() }

// Install writes the stub into t, replacing an outdated one. It refuses to
// overwrite a skill recado did not install.
func Install(t Target) error { return t.installed().Install() }

// Remove deletes the stub from t, leaving a skill recado did not install. It
// reports whether it removed anything.
func Remove(t Target) (bool, error) {
	removed, err := t.installed().Remove()
	if err != nil || !removed {
		return removed, err
	}
	// Leave the directory if the user keeps anything else in it.
	if entries, err := os.ReadDir(t.Dir); err != nil || len(entries) > 0 {
		return true, err
	}
	return true, os.Remove(t.Dir)
}
