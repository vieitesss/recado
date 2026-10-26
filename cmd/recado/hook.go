package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

func (a *app) newHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook HARNESS EVENT",
		Short:  "Record a harness lifecycle event",
		Hidden: true,
		Long: `Convert a harness lifecycle event into recado state. Harness shims call it.

session-start registers the agent and drains its mailbox; turn-start records
busy and marks messages delivered this turn read; turn-end records idle and
drains. Drain output is what recado could not hand over itself: the shim
delivers those messages.`,
		Args: cobra.ExactArgs(2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			h, ok := a.Harnesses.Lookup(args[0])
			if !ok {
				return usagef("unknown harness %q", args[0])
			}
			id, err := a.hookAgent(h)
			if err != nil {
				return err
			}
			st, err := a.store()
			if err != nil {
				return err
			}
			switch args[1] {
			case "session-start":
				if err := st.Register(a.registry(h, id)); err != nil {
					return err
				}
				return a.drain(cmd, st, id)
			case "turn-start":
				if err := a.ensureAgent(st, h, id); err != nil {
					return err
				}
				if err := st.SetState(id, store.AgentBusy); err != nil {
					return err
				}
				return st.MarkQueuedRead(id)
			case "turn-end":
				if err := a.ensureAgent(st, h, id); err != nil {
					return err
				}
				if err := st.SetState(id, store.AgentIdle); err != nil {
					return err
				}
				return a.drain(cmd, st, id)
			default:
				return usagef("unknown hook event %q: want session-start, turn-start or turn-end", args[1])
			}
		}),
	}
}

// hookAgent is the agent id a hook runs for: RECADO_ADDR, else the harness's
// own detection.
func (a *app) hookAgent(h harness.Harness) (string, error) {
	if v := os.Getenv("RECADO_ADDR"); v != "" {
		self, err := addr.Parse(v)
		if err != nil {
			return "", usagef("RECADO_ADDR: %v", err)
		}
		if !self.IsLocal() {
			return "", usagef("%s: machines not supported yet", self)
		}
		return self.Agent, nil
	}
	id, ok := h.Detect(os.Environ())
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

// drain hands the agent's pending messages to its harness, and prints the ones
// the calling shim must deliver itself.
func (a *app) drain(cmd *cobra.Command, st *store.Store, id string) error {
	svc := &deliver.Service{Store: st, Harnesses: a.Harnesses}
	drained, err := svc.Drain(id)
	if err != nil {
		return err
	}
	pushed := make([]string, 0, len(drained.Pushed))
	for _, msg := range drained.Pushed {
		short, err := st.ShortID(id, msg.Envelope.ID)
		if err != nil {
			return err
		}
		pushed = append(pushed, short)
	}
	pending := make([]message, 0, len(drained.Pending))
	for _, msg := range drained.Pending {
		view, err := a.message(st, id, msg)
		if err != nil {
			return err
		}
		pending = append(pending, view)
	}

	if jsonFlag(cmd) {
		return printJSON(cmd, struct {
			Pushed  []string  `json:"pushed"`
			Pending []message `json:"pending"`
		}{pushed, pending})
	}
	w := cmd.OutOrStdout()
	for i, msg := range pending {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, envelope.Render(msg.Envelope, msg.Short)); err != nil {
			return err
		}
	}
	return nil
}
