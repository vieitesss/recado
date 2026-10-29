package harness

import (
	"io"

	"github.com/vieitesss/recado/internal/store"
)

// HookRequest is one shim invocation. Store and Hold retain the installed
// Claude hook flags; adapters decide what they mean for their hook dialect.
type HookRequest struct {
	Event string
	Input io.Reader
	Env   []string
	Store bool
	Hold  bool
}

// HookPlan describes how to handle an invocation without giving adapters
// access to the store. A nil WriteResponse uses the standard shim report.
type HookPlan struct {
	Event         string // session-start, turn-start or turn-end
	AgentID       string // fallback when neither RECADO_ADDR nor Detect names it
	Configure     func(*store.Agent) error
	KeepPending   bool // release messages a session start cannot hand over
	HoldTurn      bool // claim messages without pushing into the ending turn
	WriteResponse func(w io.Writer, pushed, pending []store.Message, json bool) error
}

// Hooks is the optional lifecycle contract for an adapter whose hook dialect
// needs its own input, session preparation, delivery policy or response.
// FailOpen reports whether hook errors need the non-blocking exit code.
type Hooks interface {
	PrepareHook(HookRequest) (HookPlan, error)
	FailOpen() bool
}
