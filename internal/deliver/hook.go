package deliver

import (
	"errors"
	"fmt"
	"io"

	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

// UnknownHookEvent is a lifecycle event the delivery service does not handle.
type UnknownHookEvent string

func (e UnknownHookEvent) Error() string {
	return fmt.Sprintf("unknown hook event %q: want session-start, turn-start or turn-end", string(e))
}

// ForeignAgent is a hook for one harness naming an agent whose live session
// another harness owns. Handling it would take that session's registry entry
// and mailbox over, which is what a harness spawning another one produces when
// the child inherits the parent's address.
type ForeignAgent struct {
	ID      string // the agent the hook names
	Harness string // the harness the hook runs for
	Owner   string // the harness whose live session owns the id
}

func (e ForeignAgent) Error() string {
	return fmt.Sprintf("agent %s is live in harness %s: a %s hook cannot handle it", e.ID, e.Owner, e.Harness)
}

// Hook handles a lifecycle plan and writes its response. Agent supplies the
// session-start registration and the fallback if a turn hook fires first. The
// standard writer must be non-nil; it is used when the adapter has not supplied
// its own response format. The delivery lock is not held while writing. On
// output failure, release of unhanded claims is best-effort, as for KeepPending.
func (s *Service) Hook(agent store.Agent, plan harness.HookPlan, out io.Writer, json bool, standard func(io.Writer, []store.Message, []store.Message, bool) error) error {
	id := agent.ID
	if err := s.refuseForeignAgent(agent); err != nil {
		return err
	}
	var drained drainResult
	var err error
	switch plan.Event {
	case "session-start":
		if plan.Configure != nil {
			if err := plan.Configure(&agent); err != nil {
				return err
			}
		}
		if err := s.Store.Register(agent); err != nil {
			return err
		}
		if err := s.retirePrior(agent); err != nil {
			return err
		}
		drained, err = s.drain(id)
		if err == nil && plan.KeepPending && len(drained.Pending) > 0 {
			_ = s.release(id, drained.Pending)
			drained.Pending = nil
		}
	case "turn-start":
		if err := s.ensureHookAgent(agent, plan.Configure); err != nil {
			return err
		}
		// A turn start drains nothing, but the shim still reads its answer.
		if err := s.turnStart(id); err != nil {
			return err
		}
	case "turn-end":
		if err := s.ensureHookAgent(agent, plan.Configure); err != nil {
			return err
		}
		switch {
		case plan.SkipDrain:
			err = s.markIdle(id)
		case plan.HoldTurn:
			drained, err = s.hold(id)
		default:
			drained, err = s.turnEnd(id)
		}
	default:
		return UnknownHookEvent(plan.Event)
	}
	if err != nil {
		return err
	}
	write := plan.WriteResponse
	if write == nil {
		write = standard
	}
	if err := write(out, drained.Pushed, drained.Pending, json); err != nil {
		_ = s.release(id, drained.Pending)
		return err
	}
	return nil
}

// refuseForeignAgent rejects a hook naming an id another harness's live session
// owns. The harness that owns the live session is the only one that may handle
// its events, so a child harness can neither register over it, nor report its
// turns, nor drain its mailbox.
func (s *Service) refuseForeignAgent(agent store.Agent) error {
	if agent.Harness == "" {
		return nil // a caller that names no harness claims nothing
	}
	owner, err := s.Store.Lookup(agent.ID)
	if errors.Is(err, store.ErrUnknownAgent) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner.Harness == agent.Harness || !owner.Live() {
		return nil
	}
	return ForeignAgent{ID: agent.ID, Harness: agent.Harness, Owner: owner.Harness}
}

// retirePrior marks the ids this agent's process registered before retired,
// for a harness that runs one session at a time: the session that just started
// supersedes them, so an idle push or a relay sender check never reaches an old
// id through the shared pid. A harness that serves several sessions from one
// process retires nothing.
func (s *Service) retirePrior(agent store.Agent) error {
	h, ok := s.Harnesses.Lookup(agent.Harness)
	if !ok {
		return nil
	}
	if single, ok := h.(harness.SingleSession); !ok || !single.SingleSession() {
		return nil
	}
	return s.Store.RetireOthers(agent.PID, agent.Harness, agent.ID)
}

// ensureHookAgent registers the agent if this is the first hook it fired.
func (s *Service) ensureHookAgent(agent store.Agent, configure func(*store.Agent) error) error {
	_, err := s.Store.Lookup(agent.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrUnknownAgent) {
		return err
	}
	if configure != nil {
		if err := configure(&agent); err != nil {
			return err
		}
	}
	return s.Store.Register(agent)
}
