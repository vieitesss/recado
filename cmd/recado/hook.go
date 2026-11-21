package main

import (
	"errors"
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
	if plan.Event == "session-start" {
		if reg.Endpoint, err = cmd.Flags().GetString("endpoint"); err != nil {
			return err
		}
	}
	svc := &deliver.Service{Store: st, Harnesses: a.Harnesses}
	err = svc.Hook(reg, plan, cmd.OutOrStdout(), jsonFlag(cmd), printDrained)
	var unknown deliver.UnknownHookEvent
	if errors.As(err, &unknown) {
		return usage(deliver.UnknownHookEvent(req.Event))
	}
	return err
}

// hookAgent is the agent id a hook runs for: RECADO_ADDR, the harness's
// detection, or the id its hook input supplied. A RECADO_ADDR set for another
// harness is the parent's address, inherited by the harness it spawned, so only
// this harness's own declared address overrides its session id.
func (a *app) hookAgent(h harness.Harness, fallback string) (string, error) {
	if !foreignHarness(h) {
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

// foreignHarness reports whether the environment declares a harness other than
// h: this process inherited the identity of the harness that spawned it.
func foreignHarness(h harness.Harness) bool {
	name := harness.EnvHarness(os.Environ())
	return name != "" && name != h.Name()
}

// printDrained writes the drain report: the pushed short ids and the messages
// the shim must hand over, as JSON or as the text the shim reads.
func printDrained(w io.Writer, pushed, pending []store.Message, json bool) error {
	messages := make([]message, len(pending))
	for i, msg := range pending {
		messages[i] = viewMessage(msg)
	}
	if json {
		shorts := make([]string, len(pushed))
		for i, msg := range pushed {
			shorts[i] = msg.Short
		}
		return printJSON(w, struct {
			Pushed  []string  `json:"pushed"`
			Pending []message `json:"pending"`
		}{shorts, messages})
	}
	return printMessages(w, messages)
}
