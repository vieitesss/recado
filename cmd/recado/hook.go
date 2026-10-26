package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

func (a *app) newHookCmd() *cobra.Command {
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
			// Claude Code names the session in the hook's input and exports it to
			// the agent's shell, but not to later hooks: read it every time.
			var input []byte
			if _, ok := h.(harness.Namer); ok {
				data, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				input = data
			}
			id, err := a.hookAgent(h, input)
			if err != nil {
				return err
			}
			st, err := a.store()
			if err != nil {
				return err
			}
			svc := &deliver.Service{Store: st, Harnesses: a.Harnesses}
			switch args[1] {
			case "session-start":
				reg := a.registry(h, id)
				if reg.Endpoint, err = cmd.Flags().GetString("endpoint"); err != nil {
					return err
				}
				if starter, ok := h.(harness.Starter); ok {
					if err := starter.Start(&reg, id, os.Environ()); err != nil {
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
				keep, err := cmd.Flags().GetBool("store")
				if err != nil {
					return err
				}
				if keep && len(drained.Pending) > 0 {
					// A session start has no channel to the agent itself, so
					// what it cannot push waits for the turn to end.
					releasePending(st, id, drained.Pending)
					drained.Pending = nil
				}
				return reportDrained(cmd, st, id, drained)
			case "turn-start":
				if err := a.ensureAgent(st, h, id); err != nil {
					return err
				}
				return svc.TurnStart(id)
			case "turn-end":
				if err := a.ensureAgent(st, h, id); err != nil {
					return err
				}
				hold, err := cmd.Flags().GetBool("hold")
				if err != nil {
					return err
				}
				if hold {
					held, err := svc.Hold(id)
					if err != nil {
						return err
					}
					return holdTurn(cmd, st, id, held)
				}
				drained, err := svc.TurnEnd(id)
				if err != nil {
					return err
				}
				return reportDrained(cmd, st, id, drained)
			default:
				return usagef("unknown hook event %q: want session-start, turn-start or turn-end", args[1])
			}
		}),
	}
	cmd.Flags().String("endpoint", "", "where the harness accepts pushed messages (session-start)")
	cmd.Flags().Bool("store", false, "keep what this event cannot hand over for the next turn end (session-start)")
	cmd.Flags().Bool("hold", false, "return the waiting messages as the stop decision instead of pushing (turn-end)")
	return cmd
}

// hookAgent is the agent id a hook runs for: RECADO_ADDR, else the harness's
// own detection.
func (a *app) hookAgent(h harness.Harness, input []byte) (string, error) {
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
	if !ok {
		if namer, isNamer := h.(harness.Namer); isNamer {
			id, ok = namer.AgentID(input)
		}
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

// holdTurn answers a turn-end hook that runs inside the turn it reports on:
// the waiting messages must not be pushed into it, so they come back as the
// decision that continues the conversation. Nothing waiting means nothing
// blocks the stop.
func holdTurn(cmd *cobra.Command, st *store.Store, id string, held deliver.Drained) error {
	if len(held.Pending) == 0 {
		return nil
	}
	texts := make([]string, len(held.Pending))
	for i, msg := range held.Pending {
		texts[i] = envelope.Render(msg.Envelope, msg.Short)
	}
	out, err := json.Marshal(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{"block", strings.Join(texts, "\n\n")})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(out)); err != nil {
		releasePending(st, id, held.Pending)
		return err
	}
	return nil
}

// reportDrained prints what the shim must deliver itself. If the output fails,
// nothing reliable reached the shim: release every claimed message, so a later
// drain or session start can still deliver it.
func reportDrained(cmd *cobra.Command, st *store.Store, id string, drained deliver.Drained) error {
	if err := printDrained(cmd, drained); err != nil {
		releasePending(st, id, drained.Pending)
		return err
	}
	return nil
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
