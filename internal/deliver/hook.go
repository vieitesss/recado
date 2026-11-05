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

// Hook handles a lifecycle plan and writes its response. Agent supplies the
// session-start registration and the fallback if a turn hook fires first. The
// standard writer must be non-nil; it is used when the adapter has not supplied
// its own response format. The delivery lock is not held while writing. On
// output failure, release of unhanded claims is best-effort, as for KeepPending.
func (s *Service) Hook(agent store.Agent, plan harness.HookPlan, out io.Writer, json bool, standard func(io.Writer, []store.Message, []store.Message, bool) error) error {
	id := agent.ID
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
		drained, err = s.drain(id)
		if err == nil && plan.KeepPending && len(drained.Pending) > 0 {
			_ = s.release(id, drained.Pending)
			drained.Pending = nil
		}
	case "turn-start":
		if err := s.ensureHookAgent(agent); err != nil {
			return err
		}
		// A turn start drains nothing, but the shim still reads its answer.
		if err := s.turnStart(id); err != nil {
			return err
		}
	case "turn-end":
		if err := s.ensureHookAgent(agent); err != nil {
			return err
		}
		if plan.HoldTurn {
			drained, err = s.hold(id)
		} else {
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

// ensureHookAgent registers the agent if this is the first hook it fired.
func (s *Service) ensureHookAgent(agent store.Agent) error {
	_, err := s.Store.Lookup(agent.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrUnknownAgent) {
		return err
	}
	return s.Store.Register(agent)
}
