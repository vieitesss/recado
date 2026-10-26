package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/skill"
	"github.com/vieitesss/recado/internal/store"
)

// claudeTimeout bounds how long a push may wait on the Claude Code process: a
// wedged agent must not hang the sender, and a failed push leaves the message
// stored for the turn-end hook.
const claudeTimeout = 2 * time.Second

const (
	// minClaudeVersion is the first Claude Code release with cross-session
	// messaging, which is what binds the inbox socket a push writes to.
	minClaudeVersion = "2.1.224"
	// provenClaudeVersion is the newest release recado's socket frame is proven
	// on. The frame is undocumented, so Check reports anything outside that.
	provenClaudeVersion = "2.1.310"
	// hookCommandPrefix starts every hook command recado installs in Claude
	// Code's settings file, so setup, status and remove recognise their own.
	hookCommandPrefix = "recado hook claude"
)

// Claude is the Claude Code adapter. Claude Code binds a per-session inbox
// socket that starts a turn when the session is idle, but delivers a message
// inside the running turn when it is busy, so recado pushes only while idle: a
// message that arrives mid-turn waits for the Stop hook, which hands it back as
// the continuation prompt of a new turn.
type Claude struct {
	// Home is where the user's .claude lives; empty means the real home.
	Home string
}

func (c *Claude) Name() string { return "claude" }

// Detect reports the agent id when a Claude Code session prepared this process.
func (c *Claude) Detect(env []string) (string, bool) {
	if v, ok := lookupEnv(env, HarnessEnv); !ok || v != c.Name() {
		return "", false
	}
	raw, ok := lookupEnv(env, "RECADO_ADDR")
	if !ok {
		return "", false
	}
	self, err := addr.Parse(raw)
	if err != nil || !self.IsLocal() {
		return "", false
	}
	return self.Agent, true
}

// AgentID names the session the way Claude Code does: its hook input carries
// the session id, shortened to eight characters because it is also what a
// person types as a recipient.
func (c *Claude) AgentID(hookInput []byte) (string, bool) {
	var input struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(hookInput, &input); err != nil {
		return "", false
	}
	id := strings.ReplaceAll(input.SessionID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	if !addr.ValidID(id) {
		return "", false
	}
	return id, true
}

// QueuesWhileBusy reports Claude Code's limit: its socket hands a message to
// the running turn at the next step, so recado never pushes mid-turn.
func (c *Claude) QueuesWhileBusy() bool { return false }

// Shims is empty: Claude Code is installed by merging hook entries into its
// settings file, not by copying a file. See HookSettings.
func (c *Claude) Shims() []Shim { return nil }

// HookSettings is the hook configuration `recado skill setup` merges into
// ~/.claude/settings.json.
func (c *Claude) HookSettings() (skill.Hooks, error) {
	home := c.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return skill.Hooks{}, err
		}
	}
	return skill.Hooks{
		Path:   filepath.Join(home, ".claude", "settings.json"),
		Prefix: hookCommandPrefix,
		Entries: []skill.HookEntry{
			// session-start registers the agent and pushes any backlog, and
			// --store keeps what it cannot push for the Stop hook: a session
			// start has no channel to hand a message back to the agent.
			{Event: "SessionStart", Command: hookCommandPrefix + " session-start --store"},
			{Event: "UserPromptSubmit", Command: hookCommandPrefix + " turn-start"},
			// turn-end runs while the turn is still running, so --hold claims
			// the pending messages and prints them as the continuation prompt
			// instead of pushing them into the running turn.
			{Event: "Stop", Command: hookCommandPrefix + " turn-end --hold"},
		},
	}, nil
}

// Check reports whether this machine can receive messages in Claude Code: the
// release carries the inbox socket, and recado's hooks are installed and
// current.
func (c *Claude) Check() error {
	path, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude is not on PATH: %w", err)
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return fmt.Errorf("claude --version: %w", err)
	}
	version := ""
	if fields := strings.Fields(string(out)); len(fields) > 0 {
		version = fields[0]
	}
	if !versionAtLeast(version, minClaudeVersion) {
		return fmt.Errorf("claude %s is older than %s, which first binds the messaging socket", version, minClaudeVersion)
	}
	if !versionAtLeast(provenClaudeVersion, version) {
		return fmt.Errorf("claude %s is newer than %s, the newest release recado's messaging socket frame is proven on; a push it rejects leaves the message for the Stop hook", version, provenClaudeVersion)
	}
	hooks, err := c.HookSettings()
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

// Start records what only the session-start hook can see: where this session
// accepts pushes, the token that authenticates them, and the Claude Code
// process liveness follows. It also tells the session's own shell which agent
// it is, and records the fresh session idle, so a message sent to it is pushed
// at once instead of waiting for a first turn to end.
func (c *Claude) Start(agent *store.Agent, id string, env []string) error {
	if endpoint, ok := lookupEnv(env, "CLAUDE_CODE_MESSAGING_SOCKET"); ok {
		agent.Endpoint = endpoint
	}
	if token, ok := lookupEnv(env, "CLAUDE_CODE_MESSAGING_TOKEN"); ok {
		agent.Token = token
	}
	if pid := claudePID(); pid > 0 {
		agent.PID = pid
	}
	agent.State = store.AgentIdle
	agent.StateTS = envelope.FormatTime(time.Now())
	return c.exportSessionEnv(env, id)
}

// exportSessionEnv writes the agent's address into the file Claude Code sources
// for the session's own shell commands, so `recado send` from the Bash tool
// knows who is sending.
func (c *Claude) exportSessionEnv(env []string, id string) error {
	file, ok := lookupEnv(env, "CLAUDE_ENV_FILE")
	if !ok {
		return nil
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("claude: session env file: %w", err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "export RECADO_ADDR=%s\nexport %s=%s\n", id, HarnessEnv, c.Name())
	return err
}

// Push writes the session's inbox one frame, which Claude Code turns into a
// user turn. An idle session starts the turn at once.
func (c *Claude) Push(agent store.Agent, msg Outgoing) error {
	if agent.Endpoint == "" {
		return errors.New("claude: the session published no messaging socket at its start")
	}
	conn, err := net.DialTimeout("unix", agent.Endpoint, claudeTimeout)
	if err != nil {
		return fmt.Errorf("claude: dial %s: %w", agent.Endpoint, err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(claudeTimeout)); err != nil {
		return err
	}
	frames := make([]any, 0, 2)
	if agent.Token != "" {
		frames = append(frames, authFrame{"auth", agent.Token})
	}
	frames = append(frames, userFrame{"user", userMessage{"user", msg.Text}})
	var buf []byte
	for _, frame := range frames {
		line, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("claude: write %s: %w", agent.Endpoint, err)
	}
	return nil
}

// authFrame is the optional first line of a session inbox connection: the
// session's own token, which it needs to verify a message when it cannot see
// the process that posted it.
type authFrame struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// userFrame is the one frame that arrives as a user turn.
type userFrame struct {
	Type    string      `json:"type"`
	Message userMessage `json:"message"`
}

type userMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// claudePID is the process liveness follows: a hook is spawned by Claude Code,
// sometimes through a shell, so walk up to the first process named claude.
// Zero means the walk found none and the caller keeps the pid it had.
func claudePID() int {
	return claudeAncestor(os.Getppid(), processTable())
}

// claudeAncestor is the pid of the nearest claude process at or above start.
func claudeAncestor(start int, table map[int]process) int {
	for i, pid := 0, start; pid > 1 && i < 32; i++ {
		p, ok := table[pid]
		if !ok || p.parent == pid {
			return 0
		}
		if isClaudeProcess(p.command) {
			return pid
		}
		pid = p.parent
	}
	return 0
}

// process is one line of the process table.
type process struct {
	parent  int
	command string
}

// processTable reads the parents of every process with ps(1), which every
// unix recado supports.
func processTable() map[int]process {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil
	}
	table := make(map[int]process)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		table[pid] = process{parent: parent, command: strings.Join(fields[2:], " ")}
	}
	return table
}

// isClaudeProcess reports whether a process table entry is Claude Code.
func isClaudeProcess(command string) bool {
	base := filepath.Base(strings.TrimSpace(command))
	return base == "claude" || strings.HasPrefix(base, "claude-")
}
