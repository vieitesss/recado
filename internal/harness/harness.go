// Package harness adapts recado's delivery to a specific agent harness.
package harness

import (
	"os"
	"strconv"
	"strings"

	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/skill"
	"github.com/vieitesss/recado/internal/store"
)

// HarnessEnv is how a shim tells recado's commands which harness they run in,
// and RECADO_ADDR which agent they run for. The shim sets both for the
// children of the agent process, so `recado send` knows its sender.
const HarnessEnv = "RECADO_HARNESS"

// versionAtLeast reports whether v is at least want. Only the leading
// major.minor.patch matters.
func versionAtLeast(v, want string) bool {
	got, ok := parseVersion(v)
	if !ok {
		return false
	}
	min, ok := parseVersion(want)
	if !ok {
		return false
	}
	for i := range min {
		if got[i] != min[i] {
			return got[i] > min[i]
		}
	}
	return true
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > len(out) {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// lookupEnv reads one variable out of an environment list.
func lookupEnv(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v, v != ""
		}
	}
	return "", false
}

// Outgoing is one message the delivery layer hands to a harness: the envelope
// itself and the text the agent reads, rendered with the recipient's own short
// id. The delivery layer renders, so an adapter never reads the store.
type Outgoing struct {
	Envelope envelope.Envelope
	Text     string
}

// Harness delivers messages into one kind of agent harness: it holds the only
// per-harness delivery logic, and each harness adds a thin shim that calls
// `recado hook <name> ...`.
type Harness interface {
	Name() string
	// Detect reports the agent id when recado runs inside this harness.
	Detect(env []string) (agentID string, ok bool)
	// QueuesWhileBusy reports whether Push may run mid-turn.
	QueuesWhileBusy() bool
	// Push hands one message to the agent as a new user turn.
	Push(agent store.Agent, msg Outgoing) error
	// Shims are the files `recado skill setup` installs for this harness.
	Shims() []Shim
	// Check reports version and install health.
	Check() error
}

// Shim is one file a harness needs installed.
type Shim struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// Settings is the optional capability of a harness installed by merging hook
// entries into a JSON settings file rather than by copying a shim file.
type Settings interface {
	HookSettings() (skill.Hooks, error)
}

// SingleSession is the optional capability of a harness whose process runs one
// session at a time. Claude Code and Pi open /clear, /new or /resume as a new
// session id in the same process, so the ids that process registered before
// belong to no session any more. OpenCode's shared server runs several at once
// and reports false.
type SingleSession interface {
	// SingleSession reports whether a process of this harness runs one session.
	SingleSession() bool
}

// Set is the harnesses this binary knows.
type Set []Harness

// Default is the harnesses built into recado.
func Default() Set { return Set{&Pi{}, &Claude{}, &OpenCode{}} }

// Lookup finds the harness with this name.
func (s Set) Lookup(name string) (Harness, bool) {
	for _, h := range s {
		if h.Name() == name {
			return h, true
		}
	}
	return nil, false
}

// Detect asks each harness in order whether recado runs inside it.
func (s Set) Detect(env []string) (Harness, string, bool) {
	for _, h := range s {
		if id, ok := h.Detect(env); ok {
			return h, id, true
		}
	}
	return nil, "", false
}
