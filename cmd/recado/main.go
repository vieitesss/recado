package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/deliver"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/machine"
	"github.com/vieitesss/recado/internal/store"
)

func main() {
	err := newRootCmd(newApp()).Execute()
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "recado:", err)
	os.Exit(exitCode(err))
}

// exitError carries the exit code a failure asks for.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

// exitCode is the design's contract: 0 success, 1 failure, 2 usage error or
// unknown address. Untyped errors are cobra's own (unknown command, bad flag,
// wrong arguments), which are usage errors.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var e exitError
	if errors.As(err, &e) {
		return e.code
	}
	return 2
}

func usage(err error) error { return exitError{2, err} }

func usagef(format string, args ...any) error { return usage(fmt.Errorf(format, args...)) }

// classify turns invalid input and unknown names into usage errors; other
// domain errors are failures.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var e exitError
	if errors.As(err, &e) {
		return err
	}
	switch {
	case errors.Is(err, deliver.ErrRemoteUsage),
		errors.Is(err, deliver.ErrNonLocalSender),
		errors.Is(err, store.ErrInvalidOutboxOffset),
		errors.Is(err, machine.ErrUnknownMachine),
		errors.Is(err, machine.ErrInvalidMachine),
		errors.Is(err, store.ErrUnknownAgent),
		errors.Is(err, store.ErrUnknownID),
		errors.Is(err, store.ErrAmbiguousID):
		return usage(err)
	}
	return exitError{1, err}
}

// run wraps a command body with exit-code classification.
func run(fn func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error { return classify(fn(cmd, args)) }
}

// app is the command tree's dependencies. Tests build one with a temporary
// state root and a fake harness.
type app struct {
	Root       string // state root; empty means store.DefaultRoot()
	Harnesses  harness.Set
	Machines   *machine.Registry
	startRelay func(string) error // tests can avoid starting a subprocess
}

func newApp() *app { return &app{Harnesses: harness.Default()} }

func newRootCmd(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:           "recado",
		Short:         "Carry messages between agents across harnesses, multiplexers and machines",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Carry messages between agents: a Manager and the Subagents it launched,
whichever harness (Claude Code, Codex, Pi, ...) and terminal multiplexer each
runs in.

State lives in $RECADO_STATE_DIR, else ${XDG_STATE_HOME:-~/.local/state}/recado.
The caller's address is $RECADO_ADDR, else what the harness adapter detects.`,
	}
	root.PersistentFlags().Bool("json", false, "print machine-readable JSON")
	root.AddCommand(
		a.newSkillCmd(),
		a.newMachineCmd(),
		a.newSendCmd(),
		a.newAskCmd(),
		a.newReplyCmd(),
		a.newDeliverCmd(),
		a.newOutboxCmd(),
		a.newRelayCmd(),
		a.newInboxCmd(),
		a.newStatusCmd(),
		a.newAgentsCmd(),
		a.newWhoamiCmd(),
		a.newHookCmd(),
	)
	return root
}

func (a *app) store() (*store.Store, error) {
	root := a.Root
	if root == "" {
		var err error
		root, err = store.DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	return store.Open(root)
}

func (a *app) service() (*deliver.Service, error) {
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	r, err := a.machineRegistry()
	if err != nil {
		return nil, err
	}
	return &deliver.Service{Store: st, Harnesses: a.Harnesses, Machines: r}, nil
}

// whoami is RECADO_ADDR, else what a harness adapter detects.
func (a *app) whoami() (addr.Address, error) {
	self, ok, err := envAddr()
	if err != nil || ok {
		return self, err
	}
	if _, id, ok := a.Harnesses.Detect(os.Environ()); ok {
		return addr.Local(id), nil
	}
	return addr.Address{}, usagef("cannot tell which agent this is: set RECADO_ADDR or run inside a supported harness")
}

// envAddr is RECADO_ADDR parsed, when it is set: the caller's own address, or
// the agent a hook runs for.
func envAddr() (addr.Address, bool, error) {
	v := os.Getenv("RECADO_ADDR")
	if v == "" {
		return addr.Address{}, false, nil
	}
	self, err := addr.Parse(v)
	if err != nil {
		return addr.Address{}, false, usagef("RECADO_ADDR: %v", err)
	}
	return self, true, nil
}

// target is the mailbox a listing command works on: --as ADDR, else the
// caller.
func (a *app) target(as string) (addr.Address, error) {
	if as == "" {
		return a.whoami()
	}
	t, err := addr.Parse(as)
	if err != nil {
		return addr.Address{}, usage(err)
	}
	if !t.IsLocal() {
		return addr.Address{}, usagef("%s: machines not supported yet", t)
	}
	return t, nil
}

// registry is the entry a session-start hook writes.
func (a *app) registry(h harness.Harness, id string) store.Agent {
	cwd, _ := os.Getwd()
	return store.Agent{
		ID:      id,
		Harness: h.Name(),
		PID:     agentPID(),
		Pane:    os.Getenv("TMUX_PANE"),
		CWD:     cwd,
		Started: time.Now().UTC(),
	}
}

// agentPID is the process liveness is checked against. A hook exits at once,
// so its own pid would make every agent look gone: use the launcher's
// RECADO_PID when it knows it, else the hook's parent process.
func agentPID() int {
	if v := os.Getenv("RECADO_PID"); v != "" {
		if pid, err := strconv.Atoi(v); err == nil && pid > 0 {
			return pid
		}
	}
	return os.Getppid()
}

// metaFlag collects repeated --meta k=v pairs.
type metaFlag map[string]string

func (m metaFlag) String() string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + m[k]
	}
	return strings.Join(pairs, ",")
}

func (m metaFlag) Type() string { return "k=v" }

func (m metaFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("meta wants k=v, got %q", s)
	}
	if m == nil {
		return errors.New("meta flag is not initialized")
	}
	m[k] = v
	return nil
}

// readBody is TEXT, or stdin when TEXT is "-".
func readBody(cmd *cobra.Command, text string) (string, error) {
	if text != "-" {
		return text, nil
	}
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

func jsonFlag(cmd *cobra.Command) bool {
	on, err := cmd.Flags().GetBool("json")
	return err == nil && on
}

func printJSON(cmd *cobra.Command, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}

// sent is what send, ask, reply and deliver print.
type sent struct {
	ID     string `json:"id"`
	Short  string `json:"short"`
	To     string `json:"to"`
	Pushed bool   `json:"pushed"`
}

func view(res deliver.Result) sent {
	return sent{ID: res.Message.Envelope.ID, Short: res.Message.Short, To: res.Message.Envelope.To, Pushed: res.Pushed}
}

func printSent(cmd *cobra.Command, r sent) error {
	if jsonFlag(cmd) {
		return printJSON(cmd, r)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), r.Short)
	return err
}

// printMessages prints how each message reads in its recipient's timeline,
// with a blank line between messages.
func printMessages(w io.Writer, msgs []message) error {
	for i, msg := range msgs {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, msg.Text); err != nil {
			return err
		}
	}
	return nil
}

// message is one mailbox entry, rendered for a person or a script. Text is
// how the agent sees it in its timeline: what a shim sends as a user turn.
type message struct {
	ID       string            `json:"id"`
	Short    string            `json:"short"`
	State    string            `json:"state"`
	Text     string            `json:"text"`
	Envelope envelope.Envelope `json:"envelope"`
}

// viewMessage renders one stored message as a mailbox entry.
func viewMessage(msg store.Message) message {
	return message{
		ID:       msg.Envelope.ID,
		Short:    msg.Short,
		State:    string(msg.State),
		Text:     envelope.Render(msg.Envelope, msg.Short),
		Envelope: msg.Envelope,
	}
}

func matchMeta(meta, want map[string]string) bool {
	for k, v := range want {
		if meta[k] != v {
			return false
		}
	}
	return true
}
