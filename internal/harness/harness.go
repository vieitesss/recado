// Package harness adapts recado's delivery to a specific agent harness.
package harness

import (
	"os"

	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/skill"
	"github.com/vieitesss/recado/internal/store"
)

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

// Starter is the optional capability of a harness whose session start must
// configure the session from the hook process itself: where the session
// accepts pushes, any credential, which process liveness follows, and the
// variables the agent's own shell needs. Only the hook can see that.
type Starter interface {
	Start(agent *store.Agent, id string, env []string) error
}

// Namer is the optional capability of a harness that names the agent in the
// hook's own input rather than in the environment.
type Namer interface {
	AgentID(hookInput []byte) (string, bool)
}

// Settings is the optional capability of a harness installed by merging hook
// entries into a JSON settings file rather than by copying a shim file.
type Settings interface {
	HookSettings() (skill.Hooks, error)
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
