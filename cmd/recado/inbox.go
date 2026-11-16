package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
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
	cmd.Flags().StringVar(&as, "as", "", "list this agent's mailbox, by id or name (default: whoami)")
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

// agent is one entry in an agents listing. Address is the recipient form of the
// entry, so whichever machine listed it, it can go straight into send.
// A listing from a newer remote may carry keys this struct does not know;
// encoding/json ignores them.
type agent struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
	Harness string `json:"harness"`
	PID     int    `json:"pid"`
	Live    bool   `json:"live"`
	State   string `json:"state"`
	CWD     string `json:"cwd,omitempty"`
	RexID   string `json:"rex_session,omitempty"`
	RexBlk  string `json:"rex_block,omitempty"`
	RexName string `json:"rex_label,omitempty"`
}

func (a *app) newAgentsCmd() *cobra.Command {
	var (
		machine    string
		rexSession string
	)
	cmd := &cobra.Command{
		Use:   "agents [--machine NAME] [--rex-session NAME]",
		Short: "List registered agents, live or gone",
		Long: `List every agent in the registry, live or gone.

Each entry shows the readable name it registered with: the repository or
work-tree base, and, when other live agents share it, a short suffix of the
id that tells them apart. A name is exact enough to send to when it names one
live agent. The cwd is the session-start snapshot, with $HOME written as ~.

An entry whose session-start hook saw REX_SESSION records it, and the Rex
session's label is resolved when listing, because a label can be renamed.
--rex-session keeps the entries whose Rex session id or label is NAME.

--machine NAME lists the agents on a configured machine, through the same
transport send uses. That machine's registry decides liveness, names and Rex
labels; each entry's address is recado://NAME/<id>, ready for send.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			list, err := a.listAgents(machine)
			if err != nil {
				return err
			}
			agents := make([]agent, 0, len(list))
			for _, ag := range list {
				if rexSession != "" && ag.RexID != rexSession && ag.RexName != rexSession {
					continue
				}
				agents = append(agents, ag)
			}
			if jsonFlag(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					Agents []agent `json:"agents"`
				}{agents})
			}
			return printAgents(cmd.OutOrStdout(), agents, machine == "" || machine == addr.LocalMachine)
		}),
	}
	cmd.Flags().StringVar(&machine, "machine", "", "list this machine's agents (default: local)")
	cmd.Flags().StringVar(&rexSession, "rex-session", "", "only agents in this Rex session (id or label)")
	return cmd
}

// listAgents is the registry's view: this machine's, or a remote machine's
// listing as that machine computed it.
func (a *app) listAgents(machine string) ([]agent, error) {
	if machine == "" || machine == addr.LocalMachine {
		return a.localAgents()
	}
	return a.remoteAgents(machine)
}

func (a *app) localAgents() ([]agent, error) {
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	list, err := st.List()
	if err != nil {
		return nil, err
	}
	labels := a.rexLabels(list)
	agents := make([]agent, 0, len(list))
	for _, ag := range list {
		agents = append(agents, agent{
			ID: ag.ID, Address: addr.Local(ag.ID).String(), Name: ag.Display, Harness: ag.Harness,
			PID: ag.PID, Live: ag.Live(), State: string(ag.State), CWD: ag.CWD,
			RexID: ag.RexSession, RexBlk: ag.RexBlock, RexName: labels[ag.RexSession],
		})
	}
	return agents, nil
}

// remoteAgents runs `recado agents --json` on machine. The remote owns its
// registry, so it decides liveness, names and Rex labels; only the address is
// rewritten here, because only this side knows the machine's name.
func (a *app) remoteAgents(machine string) ([]agent, error) {
	registry, err := a.machineRegistry()
	if err != nil {
		return nil, err
	}
	remote, err := deliver.RemoteCommand(registry, machine, "agents", "--json")
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	remote.Stderr = &stderr
	out, err := remote.Output()
	if err != nil {
		return nil, deliver.RemoteFailure(machine, err, stderr.String())
	}
	var response struct {
		Agents []agent `json:"agents"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, fmt.Errorf("machine %q: invalid agents response: %w", machine, err)
	}
	for i := range response.Agents {
		if id := response.Agents[i].ID; !addr.ValidID(id) {
			return nil, fmt.Errorf("machine %q: invalid agents response: agent id %q", machine, id)
		}
		// Also fills the address in when the remote runs an older recado that
		// does not report one.
		response.Agents[i].Address = addr.Address{Machine: machine, Agent: response.Agents[i].ID}.String()
	}
	return response.Agents, nil
}

// printAgents writes one line per agent. Only a local listing abbreviates the
// home directory: a remote cwd is under that machine's home, not this one's.
func printAgents(w io.Writer, agents []agent, local bool) error {
	for _, ag := range agents {
		life := "gone"
		if ag.Live {
			life = "live"
		}
		state := ag.State
		if state == "" {
			state = "unknown"
		}
		if _, err := fmt.Fprintf(w, "%-28s %-5s %-10s pid=%-6d state=%s", ag.ID, life, ag.Harness, ag.PID, state); err != nil {
			return err
		}
		if ag.Name != "" {
			if _, err := fmt.Fprintf(w, " name=%s", ag.Name); err != nil {
				return err
			}
		}
		if ag.CWD != "" {
			cwd := ag.CWD
			if local {
				cwd = shortenHome(cwd)
			}
			if _, err := fmt.Fprintf(w, " cwd=%s", cwd); err != nil {
				return err
			}
		}
		if ag.RexName != "" {
			if _, err := fmt.Fprintf(w, " rex=%s", ag.RexName); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
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

// shortenHome writes the part of path under the user's home as ~/..., the way a
// person reads a path.
func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return path
}
