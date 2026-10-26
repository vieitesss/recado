package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/skill"
	"github.com/vieitesss/recado/internal/store"
)

// claudeLines collects the lines of one connection to a fresh socket.
func claudeLines(t *testing.T) (string, <-chan []string) {
	t.Helper()
	sock := filepath.Join(socketDir(t), "cc.sock")
	return sock, collect(t, sock)
}

// decodeLine decodes one socket line, failing the test when it is not a JSON
// object.
func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("socket line %q is not JSON: %v", line, err)
	}
	return got
}

func TestClaudePushWritesOneUserTurnFrame(t *testing.T) {
	sock, lines := claudeLines(t)
	want := "[recado] from asker · message (id v6t7)\nline one\nline two"
	agent := store.Agent{ID: "worker", Harness: "claude", Endpoint: sock}
	if err := (&Claude{}).Push(agent, Outgoing{Text: want}); err != nil {
		t.Fatal(err)
	}

	got := <-lines
	if len(got) != 1 {
		t.Fatalf("socket received %d lines, want one frame per push: %q", len(got), got)
	}
	frame := decodeLine(t, got[0])
	if len(frame) != 2 || frame["type"] != "user" {
		t.Fatalf("frame = %v, want {type: user, message: ...}", frame)
	}
	message, ok := frame["message"].(map[string]any)
	if !ok || len(message) != 2 {
		t.Fatalf("frame message = %v, want a role and a content", frame["message"])
	}
	if message["role"] != "user" || message["content"] != want {
		t.Errorf("message = %v, want role user and the rendered text", message)
	}
}

func TestClaudePushAuthenticatesWhenTheSessionPublishedAToken(t *testing.T) {
	sock, lines := claudeLines(t)
	agent := store.Agent{ID: "worker", Harness: "claude", Endpoint: sock, Token: "s3cret"}
	if err := (&Claude{}).Push(agent, Outgoing{Text: "hi"}); err != nil {
		t.Fatal(err)
	}

	got := <-lines
	if len(got) != 2 {
		t.Fatalf("socket received %d lines, want the auth line and the frame: %q", len(got), got)
	}
	if auth := decodeLine(t, got[0]); len(auth) != 2 || auth["type"] != "auth" || auth["token"] != "s3cret" {
		t.Errorf("first line = %v, want the session's auth token", auth)
	}
	if frame := decodeLine(t, got[1]); frame["type"] != "user" {
		t.Errorf("second line = %v, want the user frame", frame)
	}
}

func TestClaudePushFailsInsteadOfDropping(t *testing.T) {
	if err := (&Claude{}).Push(store.Agent{ID: "worker", Harness: "claude"}, Outgoing{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "messaging socket") {
		t.Fatalf("Push without an endpoint = %v, want an error naming the messaging socket", err)
	}

	sock := filepath.Join(socketDir(t), "gone.sock")
	agent := store.Agent{ID: "worker", Harness: "claude", Endpoint: sock}
	if err := (&Claude{}).Push(agent, Outgoing{Text: "hi"}); err == nil {
		t.Fatal("Push to a socket nobody listens on succeeded")
	}
}

func TestClaudeDetect(t *testing.T) {
	claude := &Claude{}
	cases := []struct {
		name string
		env  []string
		want string
		ok   bool
	}{
		{"nothing", []string{"HOME=/tmp"}, "", false},
		{"address only", []string{"RECADO_ADDR=worker"}, "", false},
		{"marker only", []string{"RECADO_HARNESS=claude"}, "", false},
		{"bare address", []string{"RECADO_HARNESS=claude", "RECADO_ADDR=worker"}, "worker", true},
		{"full address", []string{"RECADO_HARNESS=claude", "RECADO_ADDR=recado://local/sub-7"}, "sub-7", true},
		{"other harness", []string{"RECADO_HARNESS=pi", "RECADO_ADDR=worker"}, "", false},
		{"bad address", []string{"RECADO_HARNESS=claude", "RECADO_ADDR=Nope!"}, "", false},
		{"remote address", []string{"RECADO_HARNESS=claude", "RECADO_ADDR=rpi/worker"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := claude.Detect(c.env)
			if id != c.want || ok != c.ok {
				t.Errorf("Detect(%q) = %q, %v; want %q, %v", c.env, id, ok, c.want, c.ok)
			}
		})
	}
}

func TestDefaultRegistersClaude(t *testing.T) {
	h, ok := Default().Lookup("claude")
	if !ok {
		t.Fatal("Default does not register claude")
	}
	if h.QueuesWhileBusy() {
		t.Error("Claude Code must never be pushed to mid-turn: its socket delivers inside the running turn")
	}
	if shims := h.Shims(); len(shims) != 0 {
		t.Errorf("Shims = %d files, want none: Claude Code is installed through its settings file", len(shims))
	}
}

func TestClaudeHookSettings(t *testing.T) {
	home := t.TempDir()
	hooks, err := (&Claude{Home: home}).HookSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude", "settings.json")
	if hooks.Path != want {
		t.Errorf("settings path = %s, want %s", hooks.Path, want)
	}
	wantEntries := []skill.HookEntry{
		{Event: "SessionStart", Command: "recado hook claude session-start --store"},
		{Event: "UserPromptSubmit", Command: "recado hook claude turn-start"},
		{Event: "Stop", Command: "recado hook claude turn-end --hold"},
	}
	if len(hooks.Entries) != len(wantEntries) {
		t.Fatalf("Entries = %+v, want %+v", hooks.Entries, wantEntries)
	}
	for i, want := range wantEntries {
		if hooks.Entries[i] != want {
			t.Errorf("entry %d = %+v, want %+v", i, hooks.Entries[i], want)
		}
		if !strings.HasPrefix(want.Command, hooks.Prefix) {
			t.Errorf("%q does not start with the prefix %q, so remove could not recognise it", want.Command, hooks.Prefix)
		}
	}
}

// fakePS puts a ps on PATH that reports the test's own parent chain with a
// claude process at the top, and returns that process's pid.
func fakePS(t *testing.T) int {
	t.Helper()
	const claudePIDinTable = 999999
	parent := os.Getppid()
	bin := t.TempDir()
	script := "#!/bin/sh\n" + fmt.Sprintf("echo '%d %d /bin/sh'\necho '%d 1 claude'\n", parent, claudePIDinTable, claudePIDinTable)
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return claudePIDinTable
}

func TestClaudeStartReadsTheSessionsOwnEnvironment(t *testing.T) {
	wantPID := fakePS(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	env := []string{
		"CLAUDE_CODE_MESSAGING_SOCKET=/tmp/cc-socks/1.sock",
		"CLAUDE_CODE_MESSAGING_TOKEN=s3cret",
		"CLAUDE_ENV_FILE=" + envFile,
	}
	agent := store.Agent{ID: "sub-1", Harness: "claude", PID: 1}
	if err := (&Claude{}).Start(&agent, "sub-1", env); err != nil {
		t.Fatal(err)
	}
	if agent.Endpoint != "/tmp/cc-socks/1.sock" {
		t.Errorf("endpoint = %q, want the session's messaging socket", agent.Endpoint)
	}
	if agent.Token != "s3cret" {
		t.Errorf("token = %q, want the session's token", agent.Token)
	}
	if agent.State != store.AgentIdle || agent.StateTS == "" {
		t.Errorf("state = %q at %q, want the fresh session recorded idle", agent.State, agent.StateTS)
	}
	if agent.PID != wantPID {
		t.Errorf("pid = %d, want the claude process %d, not the hook's parent", agent.PID, wantPID)
	}
	want := "export RECADO_ADDR=sub-1\nexport RECADO_HARNESS=claude\n"
	if got, err := os.ReadFile(envFile); err != nil || string(got) != want {
		t.Errorf("session env file = %q, %v; want %q", got, err, want)
	}
}

func TestClaudeStartWithoutAMessagingSocket(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no ps: the pid walk finds nothing
	agent := store.Agent{ID: "sub-1", Harness: "claude", PID: 1}
	if err := (&Claude{}).Start(&agent, "sub-1", []string{"HOME=/tmp"}); err != nil {
		t.Fatal(err)
	}
	if agent.Endpoint != "" {
		t.Errorf("endpoint = %q, want none: the session published no socket", agent.Endpoint)
	}
	if agent.PID != 1 {
		t.Errorf("pid = %d, want the pid it had: there was no claude process to follow", agent.PID)
	}
}

// fakeClaude puts a claude binary that prints version on PATH.
func fakeClaude(t *testing.T, version string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\necho '" + version + " (Claude Code)'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestClaudeCheck(t *testing.T) {
	t.Run("claude not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := (&Claude{Home: t.TempDir()}).Check()
		if err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Fatalf("Check = %v, want a missing-binary error", err)
		}
	})

	t.Run("older than the first release with messaging", func(t *testing.T) {
		fakeClaude(t, "2.1.210")
		err := (&Claude{Home: t.TempDir()}).Check()
		if err == nil || !strings.Contains(err.Error(), "older than 2.1.224") {
			t.Fatalf("Check = %v, want a too-old error", err)
		}
	})

	t.Run("hooks missing, then current", func(t *testing.T) {
		fakeClaude(t, "2.1.310")
		claude := &Claude{Home: t.TempDir()}
		if err := claude.Check(); err == nil || !strings.Contains(err.Error(), "recado skill setup") {
			t.Fatalf("Check with no hooks = %v, want an install hint", err)
		}
		hooks, err := claude.HookSettings()
		if err != nil {
			t.Fatal(err)
		}
		if err := hooks.Install(); err != nil {
			t.Fatal(err)
		}
		if err := claude.Check(); err != nil {
			t.Fatalf("Check with current hooks = %v, want nil", err)
		}
	})

	t.Run("newer than the release the frame is proven on", func(t *testing.T) {
		fakeClaude(t, "2.1.400")
		claude := &Claude{Home: t.TempDir()}
		hooks, err := claude.HookSettings()
		if err != nil {
			t.Fatal(err)
		}
		if err := hooks.Install(); err != nil {
			t.Fatal(err)
		}
		err = claude.Check()
		if err == nil || !strings.Contains(err.Error(), "2.1.310") {
			t.Fatalf("Check = %v, want the proven version named", err)
		}
	})
}

func TestClaudeAncestorSkipsTheShell(t *testing.T) {
	table := map[int]process{
		100: {parent: 1, command: "claude"},
		200: {parent: 100, command: "/bin/sh"},
		300: {parent: 200, command: "recado"},
	}
	if got := claudeAncestor(300, table); got != 100 {
		t.Errorf("claudeAncestor = %d, want the claude process 100 behind the shell", got)
	}
	if got := claudeAncestor(200, table); got != 100 {
		t.Errorf("claudeAncestor = %d, want 100", got)
	}
	// A hook whose chain has no claude process reports none, so the caller
	// keeps the pid it had.
	noClaude := map[int]process{300: {parent: 200, command: "recado"}, 200: {parent: 1, command: "/bin/sh"}}
	if got := claudeAncestor(300, noClaude); got != 0 {
		t.Errorf("claudeAncestor = %d, want none", got)
	}
	// A loop in a broken table must not spin.
	loop := map[int]process{300: {parent: 300, command: "recado"}}
	if got := claudeAncestor(300, loop); got != 0 {
		t.Errorf("claudeAncestor = %d on a loop, want none", got)
	}
}

func TestClaudeAgentIDFromHookInput(t *testing.T) {
	claude := &Claude{}
	input := []byte(`{"session_id":"01a177db-2b83-77ac-9faa-a97df881c9ee","cwd":"/tmp","hook_event_name":"SessionStart"}`)
	if id, ok := claude.AgentID(input); !ok || id != "01a177db" {
		t.Errorf("AgentID = %q, %v; want the session id without dashes, shortened to eight characters", id, ok)
	}
	if _, ok := claude.AgentID([]byte(`{"cwd":"/tmp"}`)); ok {
		t.Error("AgentID named a session that reported no session_id")
	}
	if _, ok := claude.AgentID(nil); ok {
		t.Error("AgentID named a session from empty input")
	}
}
