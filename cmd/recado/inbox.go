package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/store"
)

func (a *app) newInboxCmd() *cobra.Command {
	var (
		peek bool
		all  bool
		as   string
	)
	meta := metaFlag{}
	cmd := &cobra.Command{
		Use:   "inbox [--peek] [--all] [--meta k=v] [--as ADDR]",
		Short: "List an agent's messages",
		Long: `List the messages waiting for an agent, in arrival order.

By default inbox claims pending messages (records them queued), taking them out
of recado's delivery path: recovery and debugging, never the normal path.
--peek lists them without claiming; --all also lists messages already read.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			st, err := a.store()
			if err != nil {
				return err
			}
			target, err := a.target(as)
			if err != nil {
				return err
			}
			msgs, err := st.Mailbox(target.Agent, store.MailboxOptions{
				Meta: meta, ExcludeRead: !all, Claim: !peek,
			})
			if err != nil {
				return err
			}
			var out []message
			for _, msg := range msgs {
				view := viewMessage(msg)
				out = append(out, view)
			}

			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					Messages []message `json:"messages"`
				}{out})
			}
			return printMessages(cmd.OutOrStdout(), out)
		}),
	}
	cmd.Flags().BoolVar(&peek, "peek", false, "list without claiming the messages")
	cmd.Flags().BoolVar(&all, "all", false, "include messages already read")
	cmd.Flags().Var(meta, "meta", "only messages carrying this metadata (repeatable)")
	cmd.Flags().StringVar(&as, "as", "", "list this agent's mailbox (default: whoami)")
	return cmd
}

func (a *app) newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status ID",
		Short: "Print a message's delivery state",
		Long: `Print where message ID is: stored, queued or read.

ID may be a short id or a full ULID; recado looks in every mailbox, because the
short id send printed belongs to the recipient's mailbox.`,
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			st, err := a.store()
			if err != nil {
				return err
			}
			msg, mailbox, err := st.ResolveAny(args[0])
			if err != nil {
				return err
			}
			view := viewMessage(msg)
			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					ID      string `json:"id"`
					Short   string `json:"short"`
					State   string `json:"state"`
					Mailbox string `json:"mailbox"`
				}{ID: view.ID, Short: view.Short, State: view.State, Mailbox: mailbox})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", view.Short, view.State)
			return err
		}),
	}
}

func (a *app) newAgentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agents",
		Short: "List registered agents, live or gone",
		Args:  cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			st, err := a.store()
			if err != nil {
				return err
			}
			list, err := st.List()
			if err != nil {
				return err
			}
			type agent struct {
				ID      string `json:"id"`
				Harness string `json:"harness"`
				PID     int    `json:"pid"`
				Live    bool   `json:"live"`
				State   string `json:"state"`
				CWD     string `json:"cwd,omitempty"`
			}
			agents := make([]agent, len(list))
			for i, ag := range list {
				agents[i] = agent{ID: ag.ID, Harness: ag.Harness, PID: ag.PID, Live: ag.Live(), State: string(ag.State), CWD: ag.CWD}
			}
			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					Agents []agent `json:"agents"`
				}{agents})
			}
			w := cmd.OutOrStdout()
			for _, ag := range agents {
				life := "gone"
				if ag.Live {
					life = "live"
				}
				state := ag.State
				if state == "" {
					state = "unknown"
				}
				if _, err := fmt.Fprintf(w, "%-28s %-5s %-10s pid=%-6d state=%s\n", ag.ID, life, ag.Harness, ag.PID, state); err != nil {
					return err
				}
			}
			return nil
		}),
	}
}

func (a *app) newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Print the caller's address",
		Args:  cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			self, err := a.whoami()
			if err != nil {
				return err
			}
			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					Address string `json:"address"`
					Agent   string `json:"agent"`
				}{self.String(), self.Agent})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), self.String())
			return err
		}),
	}
}
