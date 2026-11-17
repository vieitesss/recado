package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

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
	ID         string `json:"id"`
	Address    string `json:"address"`
	Name       string `json:"name,omitempty"`
	Harness    string `json:"harness"`
	PID        int    `json:"pid"`
	Live       bool   `json:"live"`
	State      string `json:"state,omitempty"` // busy or idle; only a live agent has one
	CWD        string `json:"cwd,omitempty"`
	RexSession string `json:"rex_session,omitempty"`
	RexBlock   string `json:"rex_block,omitempty"`
	RexLabel   string `json:"rex_label,omitempty"`
}

func (a *app) newAgentsCmd() *cobra.Command {
	var (
		machine    string
		rexSession string
		state      string
		all        bool
	)
	cmd := &cobra.Command{
		Use:   "agents [--all] [--state busy|idle] [--machine NAME] [--rex-session NAME]",
		Short: "List registered agents",
		Long: `List the agents whose session is live; --all also lists those whose session
is gone.

A live agent's state is busy or idle, as its harness last reported; - when it
has not reported yet. A gone agent has no state. --state busy|idle keeps the
live agents in that state.

Each entry shows the readable name it registered with: the repository or
work-tree base, and, when other live agents share it, a short suffix of the
id that tells them apart. A name is exact enough to send to when it names one
live agent. The cwd is the session-start snapshot. A local text listing
writes $HOME as ~; a remote listing and --json keep it absolute.

An entry whose session-start hook saw REX_SESSION records it, and the Rex
session's label is resolved when listing, because a label can be renamed.
--rex-session keeps the entries whose Rex session id or label is NAME.

--machine NAME lists the agents on a configured machine, through the same
transport send uses. That machine's registry decides liveness, names and Rex
labels; each entry's address is recado://NAME/<id>, ready for send.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if state != "" && !store.AgentState(state).Valid() {
				return usagef("unknown --state %q: want busy or idle", state)
			}
			list, err := a.listAgents(machine, all)
			if err != nil {
				return err
			}
			agents := make([]agent, 0, len(list))
			for _, ag := range list {
				if !all && !ag.Live {
					continue
				}
				if state != "" && ag.State != state {
					continue
				}
				if rexSession != "" && ag.RexSession != rexSession && ag.RexLabel != rexSession {
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
	cmd.Flags().BoolVar(&all, "all", false, "also list agents whose session is gone")
	cmd.Flags().StringVar(&state, "state", "", "only live agents in this state: busy or idle")
	cmd.Flags().StringVar(&machine, "machine", "", "list this machine's agents (default: local)")
	cmd.Flags().StringVar(&rexSession, "rex-session", "", "only agents in this Rex session (id or label)")
	return cmd
}

// listAgents is the registry's view: this machine's, or a remote machine's
// listing as that machine computed it. withGone asks a remote for its gone
// agents too; only then is --all forwarded, so the default listing still works
// against a recado that predates the flag.
//
// A registry entry keeps the last state its harness reported after the session
// ends, and an older remote reports it as well; a gone agent is neither busy
// nor idle, so its state is dropped here.
func (a *app) listAgents(machine string, withGone bool) ([]agent, error) {
	var (
		list []agent
		err  error
	)
	if machine == "" || machine == addr.LocalMachine {
		list, err = a.localAgents()
	} else {
		list, err = a.remoteAgents(machine, withGone)
	}
	for i := range list {
		if !list[i].Live {
			list[i].State = ""
		}
	}
	return list, err
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
			RexSession: ag.RexSession, RexBlock: ag.RexBlock, RexLabel: labels[ag.RexSession],
		})
	}
	return agents, nil
}

// remoteAgents runs `recado agents --json` on machine. The remote owns its
// registry, so it decides liveness, names and Rex labels; only the address is
// rewritten here, because only this side knows the machine's name.
func (a *app) remoteAgents(machine string, withGone bool) ([]agent, error) {
	registry, err := a.machineRegistry()
	if err != nil {
		return nil, err
	}
	args := []string{"agents", "--json"}
	if withGone {
		args = append(args, "--all")
	}
	remote, err := deliver.RemoteCommand(registry, machine, args...)
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

// printAgents writes a table with one row per agent, under a header row; no
// agents print nothing. Only a local listing abbreviates the home directory: a
// remote cwd is under that machine's home, not this one's.
func printAgents(w io.Writer, agents []agent, local bool) error {
	if len(agents) == 0 {
		return nil
	}
	tw := newTable(w, "ID", "NAME", "HARNESS", "PID", "SESSION", "STATE", "REX", "CWD")
	for _, ag := range agents {
		cwd := ag.CWD
		if local && cwd != "" {
			cwd = shortenHome(cwd)
		}
		session := "gone"
		if ag.Live {
			session = "live"
		}
		tw.row(ag.ID, ag.Name, ag.Harness, strconv.Itoa(ag.PID), session, ag.State, ag.RexLabel, cwd)
	}
	return tw.Flush()
}

// table writes aligned columns under a header row. An empty cell prints as -,
// so every row has the same number of fields.
type table struct{ *tabwriter.Writer }

func newTable(w io.Writer, header ...string) table {
	t := table{tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)}
	t.row(header...)
	return t
}

func (t table) row(cells ...string) {
	for i, c := range cells {
		if c == "" {
			cells[i] = "-"
		}
	}
	fmt.Fprintln(t, strings.Join(cells, "\t"))
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
