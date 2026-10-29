package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

func (a *app) newHookCmd() *cobra.Command {
	var options harness.HookRequest
	cmd := &cobra.Command{
		Use:    "hook HARNESS EVENT",
		Short:  "Record a harness lifecycle event",
		Hidden: true,
		Long: `Convert a harness lifecycle event into recado state. Harness shims call it.

session-start registers the agent and drains its mailbox, recording the
--endpoint the harness accepts pushed messages on; turn-start records busy and
marks messages delivered this turn read; turn-end records idle and drains.
Drain output is what recado could not hand over itself: the shim delivers
those messages as user turns.

--store keeps what a session start cannot hand over for the end of the next
turn. --hold returns the waiting messages as the decision that starts a new
turn, because a turn-end hook runs inside the turn it would push into.`,
		Args: cobra.ExactArgs(2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			h, ok := a.Harnesses.Lookup(args[0])
			if !ok {
				return usagef("unknown harness %q", args[0])
			}
			options.Event, options.Input, options.Env = args[1], cmd.InOrStdin(), os.Environ()
			err := a.runHook(cmd, h, options)
			if err == nil {
				return nil
			}
			if hooks, ok := h.(harness.Hooks); ok && hooks.FailOpen() {
				return exitError{1, err}
			}
			return err
		}),
	}
	cmd.Flags().String("endpoint", "", "where the harness accepts pushed messages (session-start)")
	cmd.Flags().BoolVar(&options.Store, "store", false, "keep what this event cannot hand over for the next turn end (session-start)")
	cmd.Flags().BoolVar(&options.Hold, "hold", false, "return the waiting messages as the stop decision instead of pushing (turn-end)")
	return cmd
}

// runHook turns one harness lifecycle event into recado state.
func (a *app) runHook(cmd *cobra.Command, h harness.Harness, req harness.HookRequest) error {
	plan := harness.DefaultHook(req)
	if hooks, ok := h.(harness.Hooks); ok {
		var err error
		plan, err = hooks.PrepareHook(req)
		if err != nil {
			return err
		}
	}
	id, err := a.hookAgent(h, plan.AgentID)
	if err != nil {
		return err
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	svc := &deliver.Service{Store: st, Harnesses: a.Harnesses}
	switch plan.Event {
	case "session-start":
		reg := a.registry(h, id)
		if reg.Endpoint, err = cmd.Flags().GetString("endpoint"); err != nil {
			return err
		}
		if plan.Configure != nil {
			if err := plan.Configure(&reg); err != nil {
				return err
			}
		}
		if err := st.Register(reg); err != nil {
			return err
		}
		drained, err := svc.Drain(id)
		if err != nil {
			return err
		}
		if plan.KeepPending && len(drained.Pending) > 0 {
			releasePending(st, id, drained.Pending)
			drained.Pending = nil
		}
		return reportDrained(cmd, st, id, drained, plan)
	case "turn-start":
		if err := a.ensureAgent(st, h, id); err != nil {
			return err
		}
		if err := svc.TurnStart(id); err != nil {
			return err
		}
		if plan.WriteResponse != nil {
			return reportDrained(cmd, st, id, deliver.Drained{}, plan)
		}
		return nil
	case "turn-end":
		if err := a.ensureAgent(st, h, id); err != nil {
			return err
		}
		var drained deliver.Drained
		if plan.HoldTurn {
			drained, err = svc.Hold(id)
		} else {
			drained, err = svc.TurnEnd(id)
		}
		if err != nil {
			return err
		}
		return reportDrained(cmd, st, id, drained, plan)
	default:
		return usagef("unknown hook event %q: want session-start, turn-start or turn-end", req.Event)
	}
}

// hookAgent is the agent id a hook runs for: RECADO_ADDR, the harness's
// detection, or the id its hook input supplied.
func (a *app) hookAgent(h harness.Harness, fallback string) (string, error) {
	self, ok, err := envAddr()
	if err != nil {
		return "", err
	}
	if ok {
		if !self.IsLocal() {
			return "", usagef("%s: machines not supported yet", self)
		}
		return self.Agent, nil
	}
	id, ok := h.Detect(os.Environ())
	if !ok && fallback != "" {
		id, ok = fallback, true
	}
	if !ok {
		return "", usagef("harness %s did not report an agent id; set RECADO_ADDR", h.Name())
	}
	if !addr.ValidID(id) {
		return "", usagef("harness %s detected a bad agent id %q", h.Name(), id)
	}
	return id, nil
}

// ensureAgent registers the agent if this is the first hook it fired.
func (a *app) ensureAgent(st *store.Store, h harness.Harness, id string) error {
	_, err := st.Lookup(id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrUnknownAgent) {
		return err
	}
	return st.Register(a.registry(h, id))
}

// reportDrained prints what the shim must deliver itself. If the output fails,
// nothing reliable reached the shim: release every claimed message, so a later
// drain or session start can still deliver it.
func reportDrained(cmd *cobra.Command, st *store.Store, id string, drained deliver.Drained, plan harness.HookPlan) error {
	var err error
	if plan.WriteResponse != nil {
		err = plan.WriteResponse(cmd.OutOrStdout(), drained.Pushed, drained.Pending, jsonFlag(cmd))
	} else {
		err = printDrained(cmd, drained)
	}
	if err != nil {
		releasePending(st, id, drained.Pending)
	}
	return err
}

// printDrained writes the drain report: the pushed short ids and the messages
// the shim must hand over, as JSON or as the text the shim reads.
func printDrained(cmd *cobra.Command, drained deliver.Drained) error {
	pending := make([]message, len(drained.Pending))
	for i, msg := range drained.Pending {
		pending[i] = viewMessage(msg)
	}
	if jsonFlag(cmd) {
		pushed := make([]string, len(drained.Pushed))
		for i, msg := range drained.Pushed {
			pushed[i] = msg.Short
		}
		return printJSON(cmd, struct {
			Pushed  []string  `json:"pushed"`
			Pending []message `json:"pending"`
		}{pushed, pending})
	}
	return printMessages(cmd.OutOrStdout(), pending)
}

// releasePending returns a failed hook's claimed messages to stored.
func releasePending(st *store.Store, id string, msgs []store.Message) {
	for _, msg := range msgs {
		_ = st.Release(id, msg.Envelope.ID)
	}
}
