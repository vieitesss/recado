package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

// foreignConvID is the conversation id Antigravity exports to hook input.
const foreignConvID = "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"

// newForeignApp is an app holding Pi, Claude Code and Antigravity, so a test can
// run one harness as the child of a parent that exported its own identity.
func newForeignApp(t *testing.T) (*app, string) {
	t.Helper()
	for _, name := range []string{"ANTIGRAVITY_CONVERSATION_ID", "ANTIGRAVITY_LS_ADDRESS", "ANTIGRAVITY_CSRF_TOKEN", "RECADO_HARNESS", "RECADO_ADDR"} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	dir := t.TempDir()
	pushLog := filepath.Join(dir, "push.log")
	stub := filepath.Join(dir, "agentapi")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nprintf '{\"response\":{}}\\n'\n", pushLog)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &app{
		Root: filepath.Join(t.TempDir(), "state"),
		Harnesses: harness.Set{
			&harness.Pi{Home: home},
			&harness.Claude{Home: home},
			&harness.Agy{Home: home, AgentAPI: stub},
		},
	}, pushLog
}

// TestForeignHarnessHookLeavesTheParentAgentAlone: a harness exports
// RECADO_HARNESS and RECADO_ADDR to everything it spawns, so a harness started
// as a child inherits the parent's identity. Its hooks must register and drain
// the child's own session instead of overwriting the parent's entry and taking
// the parent's messages.
func TestForeignHarnessHookLeavesTheParentAgentAlone(t *testing.T) {
	a, pushLog := newForeignApp(t)
	t.Setenv("RECADO_PID", strconv.Itoa(os.Getpid()))

	// A live Pi Manager reports itself busy and has a message waiting.
	t.Setenv("RECADO_HARNESS", "pi")
	t.Setenv("RECADO_ADDR", "pi-manager")
	mustExec(t, a, "", "hook", "pi", "session-start")
	mustExec(t, a, "", "hook", "pi", "turn-start")
	t.Setenv("RECADO_HARNESS", "")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(strings.Fields(mustExec(t, a, "", "ask", "pi-manager", "still there?"))[0])

	// The Manager runs `agy`, which inherits the Pi identity and reports its own
	// conversation.
	t.Setenv("RECADO_HARNESS", "pi")
	t.Setenv("RECADO_ADDR", "pi-manager")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", foreignConvID)
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
	start := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/child-repo"}`, foreignConvID)
	mustExec(t, a, start, "hook", "agy", "session-start", "--store")
	mustExec(t, a, start, "hook", "agy", "turn-start")
	stop := fmt.Sprintf(`{"conversationId":%q,"terminationReason":"NO_TOOL_CALL"}`, foreignConvID)
	mustExec(t, a, stop, "hook", "agy", "turn-end", "--hold")

	// The child registered under its own id and harness.
	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.Lookup("1208f8ee")
	if err != nil {
		t.Fatalf("the agy session registered no agent of its own: %v", err)
	}
	if child.Harness != "agy" {
		t.Errorf("child harness = %q, want agy", child.Harness)
	}

	// The Manager's entry still belongs to Pi and is still busy.
	mgr, err := st.Lookup("pi-manager")
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Harness != "pi" {
		t.Errorf("the agy hook rewrote the Pi agent's harness to %q", mgr.Harness)
	}
	if mgr.State != store.AgentBusy {
		t.Errorf("the agy hook rewrote the Pi agent's state to %q, want busy", mgr.State)
	}

	// The Manager's question is still waiting for the Manager.
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Errorf("the Manager's message after the agy hooks = %q, want it still stored", got)
	}

	// Nothing was pushed into the parent's session on the child's behalf.
	if data, err := os.ReadFile(pushLog); err == nil {
		t.Errorf("the agy hooks pushed through agentapi: %q", data)
	}
}

// TestForeignHarnessClaudeHookUsesItsOwnSessionID: Claude Code names its session
// in the hook input, so an inherited address from a parent does not become the
// agent its hooks register and drain.
func TestForeignHarnessClaudeHookUsesItsOwnSessionID(t *testing.T) {
	a, _ := newForeignApp(t)
	t.Setenv("RECADO_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("RECADO_HARNESS", "pi")
	t.Setenv("RECADO_ADDR", "pi-manager")
	mustExec(t, a, "", "hook", "pi", "session-start")
	t.Setenv("RECADO_HARNESS", "")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(strings.Fields(mustExec(t, a, "", "ask", "pi-manager", "still there?"))[0])

	// Claude Code starts as the Manager's child and inherits its identity, but
	// its hook input names the session it really runs.
	input := `{"session_id":"01a177db-2b83-77ac-9faa-a97df881c9ee","cwd":"/tmp/child-repo"}`
	t.Setenv("RECADO_HARNESS", "pi")
	t.Setenv("RECADO_ADDR", "pi-manager")
	mustExec(t, a, input, "hook", "claude", "session-start")

	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.Lookup("01a177db")
	if err != nil {
		t.Fatalf("the Claude Code session registered no agent of its own: %v", err)
	}
	if child.Harness != "claude" {
		t.Errorf("child harness = %q, want claude", child.Harness)
	}
	mgr, err := st.Lookup("pi-manager")
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Harness != "pi" {
		t.Errorf("the claude hook rewrote the Pi agent's harness to %q", mgr.Harness)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Errorf("the Manager's message after the claude start = %q, want it still stored", got)
	}
}

// TestLauncherAddressOverridesTheHarnessSession: a launcher that names the
// agent keeps that name, whether or not it also declares the harness it runs.
func TestLauncherAddressOverridesTheHarnessSession(t *testing.T) {
	for _, marker := range []string{"", "agy"} {
		t.Run("marker="+marker, func(t *testing.T) {
			a, _ := newForeignApp(t)
			t.Setenv("ANTIGRAVITY_CONVERSATION_ID", foreignConvID)
			t.Setenv("RECADO_HARNESS", marker)
			t.Setenv("RECADO_ADDR", "subagent-1")
			start := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/child-repo"}`, foreignConvID)
			mustExec(t, a, start, "hook", "agy", "session-start", "--store")
			st, err := a.store()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Lookup("subagent-1"); err != nil {
				t.Errorf("the launcher's id was not registered: %v", err)
			}
			if _, err := st.Lookup("1208f8ee"); err == nil {
				t.Error("the hook registered the conversation id over the launcher's id")
			}
		})
	}
}

// TestWhoamiPrefersAHarnessThatSeesTheSession: RECADO_HARNESS names the harness
// that exported the environment, not necessarily the one hosting this process.
// When another harness natively reports the session, that id is ours.
func TestWhoamiPrefersAHarnessThatSeesTheSession(t *testing.T) {
	a, _ := newForeignApp(t)

	t.Setenv("RECADO_HARNESS", "pi")
	t.Setenv("RECADO_ADDR", "pi-manager")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", foreignConvID)
	if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/1208f8ee" {
		t.Errorf("whoami inside a child agy = %q, want the conversation id", got)
	}

	// The parent's own shell has no other harness to ask, so the exported
	// address stays.
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/pi-manager" {
		t.Errorf("whoami outside agy = %q, want the exported address", got)
	}

	// A launcher's address needs no harness to confirm it.
	t.Setenv("RECADO_HARNESS", "")
	t.Setenv("RECADO_ADDR", "subagent-1")
	if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/subagent-1" {
		t.Errorf("whoami with a launcher address = %q, want subagent-1", got)
	}
}

// TestHookRefusesAnotherHarnessLiveAgent: a hook may not handle an agent id a
// different harness's live session owns: registering would take that session's
// entry and mailbox over.
func TestHookRefusesAnotherHarnessLiveAgent(t *testing.T) {
	a, _ := newForeignApp(t)
	t.Setenv("RECADO_PID", strconv.Itoa(os.Getpid()))

	t.Setenv("RECADO_HARNESS", "pi")
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "pi", "session-start")
	mustExec(t, a, "", "hook", "pi", "turn-start")
	t.Setenv("RECADO_HARNESS", "")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(strings.Fields(mustExec(t, a, "", "ask", "worker", "waiting"))[0])

	// A Claude Code hook claims the live Pi id; Claude's hooks fail open, so the
	// session keeps going while recado refuses the takeover.
	t.Setenv("RECADO_HARNESS", "claude")
	t.Setenv("RECADO_ADDR", "worker")
	for _, args := range [][]string{
		{"session-start"},
		{"turn-start"},
		{"turn-end", "--hold"},
	} {
		ev := args[0]
		_, err := exec(t, a, "", append([]string{"hook", "claude"}, args...)...)
		if code := exitCode(err); code != 1 {
			t.Fatalf("a %s hook for another harness's live agent: exit %d, want 1 (err %v)", ev, code, err)
		}
		if err == nil || !strings.Contains(err.Error(), "worker") || !strings.Contains(err.Error(), "pi") {
			t.Errorf("the %s failure = %v, want a notice naming the agent and the harness that owns it", ev, err)
		}
	}

	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := st.Lookup("worker")
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Harness != "pi" || mgr.State != store.AgentBusy {
		t.Errorf("after the refused hooks, the agent = %+v; want it still a busy Pi session", mgr)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Errorf("after the refused hooks, status = %q; want the message still stored", got)
	}

	// Once the owning session is gone, its id belongs to whoever claims it next.
	mgr.PID = 1 << 22 // above the supported platforms' PID ceilings
	if err := st.Register(mgr); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "", "hook", "claude", "session-start")
	mgr, err = st.Lookup("worker")
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Harness != "claude" {
		t.Errorf("harness after the Pi session left = %q, want claude", mgr.Harness)
	}
}
