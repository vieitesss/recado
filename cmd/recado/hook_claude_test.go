package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/harness"
)

// newClaudeApp is an app whose harnesses are the ones recado ships, with
// Claude Code's home under the test's control.
func newClaudeApp(t *testing.T) *app {
	t.Helper()
	return &app{
		Root:      filepath.Join(t.TempDir(), "state"),
		Harnesses: harness.Set{&harness.Claude{Home: t.TempDir()}},
	}
}

// listenInbox listens where a Claude Code session publishes its messaging
// socket and returns the lines of the first connection.
func listenInbox(t *testing.T) (string, <-chan []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "recado")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "inbox.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	lines := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			lines <- nil
			return
		}
		defer conn.Close()
		data, _ := io.ReadAll(conn)
		lines <- strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}()
	return sock, lines
}

// stopDecision is the Stop hook output that keeps Claude Code talking.
type stopDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func TestClaudeStopHookReopensTheTurnWithWhatWaited(t *testing.T) {
	a := newClaudeApp(t)
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "claude", "session-start")
	mustExec(t, a, "", "hook", "claude", "turn-start")

	t.Setenv("RECADO_ADDR", "asker")
	mustExec(t, a, "", "ask", "worker", "first question")
	mustExec(t, a, "", "send", "worker", "second message")

	t.Setenv("RECADO_ADDR", "worker")
	got := mustExec(t, a, "", "hook", "claude", "turn-end", "--hold")
	decision := decode[stopDecision](t, got)
	if decision.Decision != "block" {
		t.Fatalf("Stop decision = %q, want block so the turn continues", decision.Decision)
	}
	reason := decision.Reason
	if n := strings.Count(reason, "[recado] from asker"); n != 2 {
		t.Fatalf("reason holds %d messages, want both:\n%s", n, reason)
	}
	first, second := strings.Index(reason, "first question"), strings.Index(reason, "second message")
	if first < 0 || second < 0 || first > second {
		t.Errorf("reason is not in order:\n%s", reason)
	}
	if !strings.Contains(reason, "reply with: recado reply") {
		t.Errorf("reason dropped the question's reply hint:\n%s", reason)
	}

	// Claude Code starts the turn the held messages asked for. When that turn
	// ends with nothing waiting, the Stop hook lets it stop.
	mustExec(t, a, "", "hook", "claude", "turn-start")
	if got := mustExec(t, a, "", "hook", "claude", "turn-end", "--hold"); got != "" {
		t.Errorf("Stop with nothing waiting = %q, want no output, so nothing blocks the stop", got)
	}
}

func TestClaudeStopHookDeliversNothingWhenNothingWaited(t *testing.T) {
	a := newClaudeApp(t)
	sock, lines := listenInbox(t)
	t.Setenv("RECADO_ADDR", "worker")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sock)
	mustExec(t, a, "", "hook", "claude", "session-start")
	mustExec(t, a, "", "hook", "claude", "turn-start")
	if got := mustExec(t, a, "", "hook", "claude", "turn-end", "--hold"); got != "" {
		t.Fatalf("Stop = %q, want no output", got)
	}

	// The session is idle now, so a send is pushed through the socket and
	// wakes it, rather than waiting for another turn to end.
	t.Setenv("RECADO_ADDR", "asker")
	mustExec(t, a, "", "ask", "worker", "are you there?")

	got := <-lines
	if len(got) != 1 {
		t.Fatalf("socket received %q, want one frame", got)
	}
	frame := decode[struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}](t, got[0])
	if frame.Type != "user" || frame.Message.Role != "user" {
		t.Fatalf("frame = %+v, want a user turn", frame)
	}
	if !strings.HasPrefix(frame.Message.Content, "[recado] from asker") || !strings.Contains(frame.Message.Content, "are you there?") {
		t.Errorf("frame content = %q, want the rendered question", frame.Message.Content)
	}
}

func TestClaudeSessionStartKeepsABacklogItCannotPush(t *testing.T) {
	a := newClaudeApp(t)
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "claude", "session-start")

	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(strings.Fields(mustExec(t, a, "", "ask", "worker", "backlog"))[0])

	// The session start has no way to hand a message to the agent itself, so
	// a backlog it cannot push stays stored for the Stop hook.
	t.Setenv("RECADO_ADDR", "worker")
	out := decode[struct {
		Pushed  []string  `json:"pushed"`
		Pending []message `json:"pending"`
	}](t, mustExec(t, a, "", "hook", "claude", "session-start", "--store", "--json"))
	if len(out.Pending) != 0 {
		t.Errorf("session start handed back %d messages, want none", len(out.Pending))
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Errorf("status = %q, want the backlog still stored", got)
	}

	// The end of the first turn hands it over.
	mustExec(t, a, "", "hook", "claude", "turn-start")
	if got := mustExec(t, a, "", "hook", "claude", "turn-end", "--hold"); !strings.Contains(got, "backlog") {
		t.Errorf("Stop = %q, want the stored message as the next turn", got)
	}
}

func TestClaudeSessionStartPushesABacklogWhenTheSessionTakesMessages(t *testing.T) {
	a := newClaudeApp(t)
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, "", "hook", "claude", "session-start")

	t.Setenv("RECADO_ADDR", "asker")
	mustExec(t, a, "", "send", "worker", "backlog")

	// The session restarts with its inbox socket published, so the backlog
	// goes out through it instead of waiting for a turn to end.
	sock, lines := listenInbox(t)
	t.Setenv("RECADO_ADDR", "worker")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", sock)
	mustExec(t, a, "", "hook", "claude", "session-start")
	if got := <-lines; len(got) != 1 || !strings.Contains(got[0], "backlog") {
		t.Errorf("socket received %q, want the backlog", got)
	}
}

func TestClaudeSessionStartNamesTheAgentFromItsHookInput(t *testing.T) {
	a := newClaudeApp(t)
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("RECADO_ADDR", "")
	t.Setenv("CLAUDE_ENV_FILE", envFile)

	input := `{"session_id":"01a177db-2b83-77ac-9faa-a97df881c9ee","cwd":"/tmp","hook_event_name":"SessionStart","source":"startup"}`
	mustExec(t, a, input, "hook", "claude", "session-start")
	if got := mustExec(t, a, "", "agents"); !strings.Contains(got, "01a177db") {
		t.Errorf("agents = %q, want the session id as the agent id", got)
	}
	want := "export RECADO_ADDR=01a177db\nexport RECADO_HARNESS=claude\n"
	if got, err := os.ReadFile(envFile); err != nil || string(got) != want {
		t.Errorf("session env file = %q, %v; want %q", got, err, want)
	}

	// A user who names the session keeps that name.
	t.Setenv("RECADO_ADDR", "worker")
	mustExec(t, a, input, "hook", "claude", "session-start")
	if got := mustExec(t, a, "", "agents"); !strings.Contains(got, "worker") {
		t.Errorf("agents = %q, want the name the user gave the session", got)
	}
}

func TestClaudeHookNeedsAnAgentID(t *testing.T) {
	a := newClaudeApp(t)
	t.Setenv("RECADO_ADDR", "")
	if _, err := exec(t, a, "", "hook", "claude", "session-start"); exitCode(err) != 2 {
		t.Errorf("session-start with no id and no hook input: exit %d, want 2 (err %v)", exitCode(err), err)
	}
}

func TestClaudeHooksFindTheAgentInTheirOwnInput(t *testing.T) {
	a := newClaudeApp(t)
	t.Setenv("RECADO_ADDR", "")
	input := `{"session_id":"01a177db-2b83-77ac-9faa-a97df881c9ee","hook_event_name":"UserPromptSubmit"}`

	// Claude Code exports the session to the agent's shell, not to its later
	// hooks, so each hook names the agent the same way the session start did.
	mustExec(t, a, input, "hook", "claude", "session-start")
	mustExec(t, a, input, "hook", "claude", "turn-start")

	t.Setenv("RECADO_ADDR", "asker")
	mustExec(t, a, "", "send", "01a177db", "held while busy")
	t.Setenv("RECADO_ADDR", "") // the hook itself has no address of its own
	got := mustExec(t, a, input, "hook", "claude", "turn-end", "--hold")
	if !strings.Contains(got, "held while busy") {
		t.Errorf("Stop = %q, want the message waiting for the turn to end", got)
	}
}
