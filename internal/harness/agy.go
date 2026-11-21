package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/skill"
	"github.com/vieitesss/recado/internal/store"
)

// agyTimeout bounds how long a push may wait on agentapi: a wedged language
// server must not hang the sender, and a failed push leaves the message stored
// for the Stop hook.
const agyTimeout = 5 * time.Second

const agyHookCommandPrefix = "recado hook agy"

// Agy is the Antigravity CLI (agy) adapter. Its lifecycle hooks register the
// session's language-server address, CSRF token and conversation id, mark
// turns busy/idle, and hand waiting messages back through Stop's continue
// decision. While idle, Push wakes the session with `agentapi send-message`.
type Agy struct {
	// Home is where the user's .gemini lives; empty means the real home.
	Home string
	// AgentAPI overrides the agentapi executable (tests).
	AgentAPI string
}

func (a *Agy) Name() string { return "agy" }

// Detect reports the agent id when recado runs inside an Antigravity session.
// Antigravity exports ANTIGRAVITY_CONVERSATION_ID to every shell command and
// hook it spawns, so no session env file is needed.
func (a *Agy) Detect(env []string) (string, bool) {
	if v, ok := lookupEnv(env, HarnessEnv); ok && v == a.Name() {
		if raw, ok := lookupEnv(env, "RECADO_ADDR"); ok {
			if self, err := addr.Parse(raw); err == nil && self.IsLocal() {
				return self.Agent, true
			}
		}
	}
	convID, ok := lookupEnv(env, "ANTIGRAVITY_CONVERSATION_ID")
	if !ok {
		return "", false
	}
	id := shortSessionID(convID)
	if !addr.ValidID(id) {
		return "", false
	}
	return id, true
}

// QueuesWhileBusy reports Antigravity's limit: `agentapi send-message` delivers
// at the next model invocation inside a running turn, so recado pushes only
// while idle and lets Stop hand over messages that arrived mid-turn.
func (a *Agy) QueuesWhileBusy() bool { return false }

// SingleSession reports false: one Antigravity language-server process can host
// multiple conversations (such as built-in subagents or side questions), so a
// session start must not retire other ids sharing its PID.
func (a *Agy) SingleSession() bool { return false }

// FailOpen reports that a broken hook must not block the user's Antigravity
// session.
func (a *Agy) FailOpen() bool { return true }

// Shims is empty: Antigravity is installed by merging a "recado" section into
// ~/.gemini/config/hooks.json. See HookSettings.
func (a *Agy) Shims() []Shim { return nil }

// HookSettings is the hook configuration `recado skill setup` merges into
// ~/.gemini/config/hooks.json.
func (a *Agy) HookSettings() (skill.Hooks, error) {
	home, err := a.homeDir()
	if err != nil {
		return skill.Hooks{}, err
	}
	return skill.Hooks{
		Path:    filepath.Join(home, ".gemini", "config", "hooks.json"),
		Prefix:  agyHookCommandPrefix,
		Section: "recado",
		Flat:    true,
		Entries: []skill.HookEntry{
			{Event: "SessionStart", Command: agyHookCommandPrefix + " session-start --store"},
			{Event: "PreInvocation", Command: agyHookCommandPrefix + " turn-start"},
			{Event: "Stop", Command: agyHookCommandPrefix + " turn-end --hold"},
		},
	}, nil
}

type agyHookInput struct {
	ConversationID    string `json:"conversationId"`
	CWD               string `json:"cwd"`
	TerminationReason string `json:"terminationReason"`
}

// PrepareHook owns Antigravity's camelCase hook input, session configuration,
// delivery policy and JSON stdout contract (every Antigravity hook must print a
// JSON object on stdout).
func (a *Agy) PrepareHook(req HookRequest) (HookPlan, error) {
	input, err := io.ReadAll(req.Input)
	if err != nil {
		return HookPlan{}, err
	}
	var in agyHookInput
	_ = json.Unmarshal(input, &in)

	plan := HookPlan{
		Event:       req.Event,
		KeepPending: req.Event == "session-start" && req.Store,
		Configure:   func(agent *store.Agent) error { return a.start(agent, in, req.Env) },
	}
	if id := shortSessionID(in.ConversationID); addr.ValidID(id) {
		plan.AgentID = id
	}
	switch req.Event {
	case "turn-end":
		if in.TerminationReason == "USER_CANCELED" {
			plan.SkipDrain = true
			plan.WriteResponse = writeAgyEmpty
		} else if req.Hold {
			plan.HoldTurn = true
			plan.WriteResponse = writeAgyStop
		} else {
			plan.WriteResponse = writeAgyEmpty
		}
	case "session-start", "turn-start":
		plan.WriteResponse = writeAgyEmpty
	}
	return plan, nil
}

// writeAgyEmpty writes the empty JSON object Antigravity expects from hooks
// that take no action.
func writeAgyEmpty(w io.Writer, _, _ []store.Message, _ bool) error {
	_, err := fmt.Fprintln(w, "{}")
	return err
}

// writeAgyStop hands waiting messages back through Antigravity's Stop contract
// (`{"decision":"continue","reason":"..."}`). With nothing waiting it outputs
// `{}` so the session can stop cleanly.
func writeAgyStop(w io.Writer, _ []store.Message, pending []store.Message, _ bool) error {
	if len(pending) == 0 {
		return writeAgyEmpty(w, nil, nil, false)
	}
	texts := make([]string, len(pending))
	for i, msg := range pending {
		texts[i] = envelope.Render(msg.Envelope, msg.Short)
	}
	out, err := json.Marshal(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{"continue", strings.Join(texts, "\n\n")})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(out))
	return err
}

// start records the Antigravity session's language-server endpoint and
// conversation id, CSRF token, workspace CWD (Antigravity runs hooks with cwd
// set to the hooks.json directory), and the antigravity process PID.
func (a *Agy) start(agent *store.Agent, in agyHookInput, env []string) error {
	convID := in.ConversationID
	if convID == "" {
		convID, _ = lookupEnv(env, "ANTIGRAVITY_CONVERSATION_ID")
	}
	if lsAddr, ok := lookupEnv(env, "ANTIGRAVITY_LS_ADDRESS"); ok && convID != "" {
		agent.Endpoint = lsAddr + "/" + convID
	}
	if token, ok := lookupEnv(env, "ANTIGRAVITY_CSRF_TOKEN"); ok {
		agent.Token = token
	}
	if in.CWD != "" {
		agent.CWD = in.CWD
		agent.Name = store.DeriveName(in.CWD)
	}
	if pid := agyPID(); pid > 0 {
		agent.PID = pid
	}
	agent.State = store.AgentIdle
	agent.StateTS = envelope.FormatTime(time.Now())
	return nil
}

// Push wakes an idle Antigravity conversation by calling `agentapi
// send-message` against the session's recorded language server.
func (a *Agy) Push(agent store.Agent, msg Outgoing) error {
	lsAddr, convID, ok := strings.Cut(agent.Endpoint, "/")
	if !ok || lsAddr == "" || convID == "" {
		return errors.New("agy: the session published no language server endpoint at its start")
	}
	exe, prefixArgs, err := a.agentAPICommand()
	if err != nil {
		return err
	}
	args := append(prefixArgs, "send-message", "--title", "recado", convID, msg.Text)
	ctx, cancel := context.WithTimeout(context.Background(), agyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = withAgyEnv(os.Environ(), lsAddr, agent.Token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("agy: agentapi send-message: %w: %s", err, detail)
		}
		return fmt.Errorf("agy: agentapi send-message: %w", err)
	}
	// agentapi prints a JSON object with a non-empty "error" field on RPC
	// failures even when the process exits 0.
	var resp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(out, &resp) == nil && resp.Error != "" {
		return fmt.Errorf("agy: agentapi send-message: %s", resp.Error)
	}
	return nil
}

// Check reports whether this machine can receive messages in Antigravity: `agy`
// is on PATH and recado's hooks are installed and current.
func (a *Agy) Check() error {
	path, err := exec.LookPath("agy")
	if err != nil {
		return fmt.Errorf("agy is not on PATH: %w", err)
	}
	if _, err := exec.Command(path, "--version").Output(); err != nil {
		return fmt.Errorf("agy --version: %w", err)
	}
	hooks, err := a.HookSettings()
	if err != nil {
		return err
	}
	state, err := hooks.Inspect()
	if err != nil {
		return err
	}
	if state != skill.Current {
		return fmt.Errorf("%s: recado's hooks are %s; run recado skill setup", hooks.Path, state)
	}
	return nil
}

func (a *Agy) homeDir() (string, error) {
	if a.Home != "" {
		return a.Home, nil
	}
	return os.UserHomeDir()
}

// agentAPICommand locates the agentapi CLI wrapper, falling back to
// ~/.gemini/antigravity-cli/bin/agentapi and then `agy agentapi`.
func (a *Agy) agentAPICommand() (string, []string, error) {
	if a.AgentAPI != "" {
		return a.AgentAPI, nil, nil
	}
	if path, err := exec.LookPath("agentapi"); err == nil {
		return path, nil, nil
	}
	if home, err := a.homeDir(); err == nil {
		candidate := filepath.Join(home, ".gemini", "antigravity-cli", "bin", "agentapi")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil, nil
		}
	}
	if path, err := exec.LookPath("agy"); err == nil {
		return path, []string{"agentapi"}, nil
	}
	return "", nil, errors.New("agy: agentapi is not on PATH")
}

// withAgyEnv returns env with the recipient's ANTIGRAVITY_LS_ADDRESS and
// ANTIGRAVITY_CSRF_TOKEN replacing any values inherited from a sending agy
// session.
func withAgyEnv(env []string, lsAddr, token string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTIGRAVITY_LS_ADDRESS=") || strings.HasPrefix(kv, "ANTIGRAVITY_CSRF_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "ANTIGRAVITY_LS_ADDRESS="+lsAddr)
	if token != "" {
		out = append(out, "ANTIGRAVITY_CSRF_TOKEN="+token)
	}
	return out
}

// shortSessionID turns a UUID conversation id into the 8-character id people
// type as a recipient, matching Claude Code's convention.
func shortSessionID(raw string) string {
	id := strings.ReplaceAll(strings.TrimSpace(raw), "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

func agyPID() int {
	return agyAncestor(os.Getppid(), processTable())
}

func agyAncestor(start int, table map[int]process) int {
	return processAncestor(start, table, isAgyProcess)
}

func isAgyProcess(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	return base == "antigravity" || base == "agy" || strings.HasPrefix(base, "antigravity-")
}
