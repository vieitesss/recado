package main

import (
	"io"
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
	reg := a.registry(h, id)
	switch plan.Event {
	case "session-start":
		if reg.Endpoint, err = cmd.Flags().GetString("endpoint"); err != nil {
			return err
		}
	case "turn-start", "turn-end":
	default:
		return usagef("unknown hook event %q: want session-start, turn-start or turn-end", req.Event)
	}
	svc := &deliver.Service{Store: st, Harnesses: a.Harnesses}
	return svc.Hook(reg, plan, cmd.OutOrStdout(), jsonFlag(cmd), func(_ io.Writer, pushed, pending []store.Message, _ bool) error {
		return printDrained(cmd, pushed, pending)
	})
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

// printDrained writes the drain report: the pushed short ids and the messages
// the shim must hand over, as JSON or as the text the shim reads.
func printDrained(cmd *cobra.Command, pushed, pending []store.Message) error {
	messages := make([]message, len(pending))
	for i, msg := range pending {
		messages[i] = viewMessage(msg)
	}
	if jsonFlag(cmd) {
		shorts := make([]string, len(pushed))
		for i, msg := range pushed {
			shorts[i] = msg.Short
		}
		return printJSON(cmd, struct {
			Pushed  []string  `json:"pushed"`
			Pending []message `json:"pending"`
		}{shorts, messages})
	}
	return printMessages(cmd.OutOrStdout(), messages)
}
