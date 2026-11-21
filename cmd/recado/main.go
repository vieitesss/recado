package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
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

// exitCode is the CLI's contract: 0 success, 1 failure, 2 usage error or
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
		errors.Is(err, store.ErrAmbiguousName),
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
	startRelay func(string) error      // tests can avoid starting a subprocess
	execCmd    func(*osexec.Cmd) error // tests can avoid running update's commands
	executable func() (string, error)  // tests can stub the running binary's path
	rexList    func() ([]byte, error)  // tests can stub `rex ls --json`
}

func newApp() *app { return &app{Harnesses: harness.Default()} }

func newRootCmd(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:           "recado",
		Short:         "Carry messages between coding agents across harnesses and machines",
		Version:       buildVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Carry messages between coding agents, whichever harness (Claude Code, Pi,
OpenCode, ...) and machine each runs on. A message arrives as the recipient's
next turn.

State lives in $RECADO_STATE_DIR, else ${XDG_STATE_HOME:-~/.local/state}/recado.
The caller's address is $RECADO_ADDR, else what the harness adapter detects.`,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			if skipEndpointCapture(cmd) {
				return
			}
			a.refreshSessionEndpoint(os.Environ())
		},
	}
	root.SetVersionTemplate("recado {{.Version}}\n")
	root.PersistentFlags().Bool("json", false, "print machine-readable JSON")
	root.AddCommand(
		a.newSkillCmd(),
		a.newUpdateCmd(),
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

func (a *app) storeRoot() (string, error) {
	if a.Root != "" {
		return a.Root, nil
	}
	return store.DefaultRoot()
}

func (a *app) store() (*store.Store, error) {
	root, err := a.storeRoot()
	if err != nil {
		return nil, err
	}
	return store.Open(root)
}

// skipEndpointCapture reports commands that must never trigger passive
// endpoint refresh: lifecycle hooks, help, completion, version, and bare root.
func skipEndpointCapture(cmd *cobra.Command) bool {
	if v, err := cmd.Flags().GetBool("version"); err == nil && v {
		return true
	}
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		switch c.Name() {
		case "hook", "help", "completion":
			return true
		}
	}
	return !cmd.HasParent()
}

// refreshSessionEndpoint passively captures a harness's push endpoint and
// token when a non-hook CLI command runs inside a session whose hooks do not
// receive them. All errors are ignored: a read-only environment or missing
// registry must never break a CLI command.
func (a *app) refreshSessionEndpoint(env []string) {
	for _, h := range a.Harnesses {
		cap, ok := h.(harness.EndpointCapturer)
		if !ok {
			continue
		}
		endpoint, token, ok := cap.SessionEndpoint(env)
		if !ok {
			continue
		}
		id, ok := captureAgentID(h, env)
		if !ok {
			continue
		}
		a.updateSessionEndpoint(h.Name(), id, endpoint, token)
	}
}

// captureAgentID picks the local agent id to refresh for h: RECADO_ADDR when
// it is local and RECADO_HARNESS does not name a different harness, otherwise
// h.Detect(env).
func captureAgentID(h harness.Harness, env []string) (string, bool) {
	marker := harness.EnvHarness(env)
	if marker == "" || marker == h.Name() {
		if raw := envLookup(env, "RECADO_ADDR"); raw != "" {
			if self, err := addr.Parse(raw); err == nil && self.IsLocal() {
				return self.Agent, true
			}
		}
	}
	id, ok := h.Detect(env)
	if !ok || !addr.ValidID(id) {
		return "", false
	}
	return id, true
}

func (a *app) updateSessionEndpoint(harnessName, id, endpoint, token string) {
	root, err := a.storeRoot()
	if err != nil {
		return
	}
	// Never create the state directory or a phantom registry entry.
	if _, err := os.Stat(filepath.Join(root, "agents", id+".json")); err != nil {
		return
	}
	st, err := store.Open(root)
	if err != nil {
		return
	}
	ag, err := st.Lookup(id)
	if err != nil || !canRefreshEndpoint(ag, harnessName, endpoint, token) {
		return
	}
	release, err := st.LockDelivery(id)
	if err != nil {
		return
	}
	defer release()
	ag, err = st.Lookup(id)
	if err != nil || !canRefreshEndpoint(ag, harnessName, endpoint, token) {
		return
	}
	ag.Endpoint = endpoint
	ag.Token = token
	_ = st.Register(ag)
}

// canRefreshEndpoint enforces the passive-refresh safety guards: the entry
// must be live and owned by harnessName, must not already hold a different
// conversation's endpoint (protecting a launcher-named parent from a nested
// child session), and must actually differ from the incoming values.
func canRefreshEndpoint(ag store.Agent, harnessName, endpoint, token string) bool {
	if ag.Harness != harnessName || !ag.Live() {
		return false
	}
	if ag.Endpoint != "" {
		_, oldConv, hasOld := strings.Cut(ag.Endpoint, "/")
		_, newConv, _ := strings.Cut(endpoint, "/")
		if hasOld && oldConv != "" && oldConv != newConv {
			return false
		}
	}
	return ag.Endpoint != endpoint || ag.Token != token
}

func envLookup(env []string, name string) string {
	prefix := name + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
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

// whoami is RECADO_ADDR, else what a harness adapter detects. RECADO_ADDR was
// exported by the harness that spawned this process when RECADO_HARNESS names
// it, so a harness that natively recognises the session outranks the address
// the process inherited.
func (a *app) whoami() (addr.Address, error) {
	env := os.Environ()
	if marker := harness.EnvHarness(env); marker != "" {
		if _, id, found := a.Harnesses.DetectOther(env, marker); found {
			return addr.Local(id), nil
		}
	}
	self, ok, err := envAddr()
	if err != nil {
		return self, err
	}
	if ok {
		return self, nil
	}
	if _, id, found := a.Harnesses.Detect(env); found {
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
// caller. ADDR names an agent the way send's TO does: an id, or the name of
// one live agent. An id nobody registered still names its mailbox.
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
	st, err := a.store()
	if err != nil {
		return addr.Address{}, err
	}
	ag, err := st.ResolveRecipient(t.Agent)
	switch {
	case errors.Is(err, store.ErrUnknownAgent):
		return t, nil
	case err != nil:
		return addr.Address{}, err
	}
	return addr.Local(ag.ID), nil
}

// registry is the entry a session-start hook writes.
func (a *app) registry(h harness.Harness, id string) store.Agent {
	cwd, _ := os.Getwd()
	return store.Agent{
		ID:         id,
		Harness:    h.Name(),
		PID:        agentPID(),
		Pane:       os.Getenv("TMUX_PANE"),
		RexSession: os.Getenv("REX_SESSION"),
		RexBlock:   os.Getenv("REX_BLOCK"),
		CWD:        cwd,
		Name:       store.DeriveName(cwd),
		Started:    time.Now().UTC(),
	}
}

// agentPID is the process liveness is checked against. A hook exits at once,
// so its own pid would make every agent look gone: use the starting
// process's RECADO_PID when it knows it, else the hook's parent process.
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

func printJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
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
	return sent{ID: res.Message.Envelope.ID, Short: res.Message.Short, To: res.Message.Envelope.To.String(), Pushed: res.Pushed}
}

func printSent(cmd *cobra.Command, r sent) error {
	if jsonFlag(cmd) {
		return printJSON(cmd.OutOrStdout(), r)
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
