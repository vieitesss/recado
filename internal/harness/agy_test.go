package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/skill"
	"github.com/vieitesss/recado/internal/store"
)

func TestAgyDetect(t *testing.T) {
	agy := &Agy{}
	cases := []struct {
		name string
		env  []string
		want string
		ok   bool
	}{
		{"nothing", []string{"HOME=/tmp"}, "", false},
		{"conversation uuid", []string{"ANTIGRAVITY_CONVERSATION_ID=1208f8ee-309e-4f0b-bf0c-6a0aa1262060"}, "1208f8ee", true},
		{"explicit harness and addr", []string{"RECADO_HARNESS=agy", "RECADO_ADDR=subagent-build-1", "ANTIGRAVITY_CONVERSATION_ID=1208f8ee-309e-4f0b-bf0c-6a0aa1262060"}, "subagent-build-1", true},
		{"other harness ignored when no conv id", []string{"RECADO_HARNESS=pi", "RECADO_ADDR=worker"}, "", false},
		{"invalid conv id", []string{"ANTIGRAVITY_CONVERSATION_ID=!!!"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := agy.Detect(c.env)
			if id != c.want || ok != c.ok {
				t.Errorf("Detect(%q) = %q, %v; want %q, %v", c.env, id, ok, c.want, c.ok)
			}
		})
	}
}

func TestDefaultRegistersAgy(t *testing.T) {
	h, ok := Default().Lookup("agy")
	if !ok {
		t.Fatal("Default does not register agy")
	}
	if h.QueuesWhileBusy() {
		t.Error("agy must never be pushed to mid-turn: agentapi delivers inside the running turn")
	}
	if shims := h.Shims(); len(shims) != 0 {
		t.Errorf("Shims = %d files, want none: agy is installed through hooks.json", len(shims))
	}
}

func TestAgyHookSettings(t *testing.T) {
	home := t.TempDir()
	hooks, err := (&Agy{Home: home}).HookSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".gemini", "config", "hooks.json")
	if hooks.Path != want {
		t.Errorf("hooks path = %s, want %s", hooks.Path, want)
	}
	if hooks.Section != "recado" || !hooks.Flat {
		t.Errorf("hooks section=%q flat=%v, want recado/true", hooks.Section, hooks.Flat)
	}
	wantEntries := []skill.HookEntry{
		{Event: "SessionStart", Command: "recado hook agy session-start --store"},
		{Event: "PreInvocation", Command: "recado hook agy turn-start"},
		{Event: "Stop", Command: "recado hook agy turn-end --hold"},
	}
	if len(hooks.Entries) != len(wantEntries) {
		t.Fatalf("Entries = %+v, want %+v", hooks.Entries, wantEntries)
	}
	for i, want := range wantEntries {
		if hooks.Entries[i] != want {
			t.Errorf("entry %d = %+v, want %+v", i, hooks.Entries[i], want)
		}
		if !strings.HasPrefix(want.Command, hooks.Prefix) {
			t.Errorf("%q does not start with %q", want.Command, hooks.Prefix)
		}
	}
}

func fakeAgyPS(t *testing.T) int {
	t.Helper()
	const agyPIDInTable = 888888
	parent := os.Getppid()
	bin := t.TempDir()
	script := "#!/bin/sh\n" + fmt.Sprintf("echo '%d %d /bin/sh'\necho '%d 1 antigravity'\n", parent, agyPIDInTable, agyPIDInTable)
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return agyPIDInTable
}

func TestAgyStartRecordsEndpointTokenCWDAndPID(t *testing.T) {
	wantPID := fakeAgyPS(t)
	env := []string{
		"ANTIGRAVITY_LS_ADDRESS=localhost:64177",
		"ANTIGRAVITY_CSRF_TOKEN=tok-123",
	}
	in := agyHookInput{
		ConversationID: "1208f8ee-309e-4f0b-bf0c-6a0aa1262060",
		CWD:            "/tmp/my-project",
	}
	agent := store.Agent{ID: "1208f8ee", Harness: "agy", PID: 1, CWD: "/Users/me/.gemini/config", Name: "config"}
	if err := (&Agy{}).start(&agent, in, env); err != nil {
		t.Fatal(err)
	}
	if agent.Endpoint != "localhost:64177/1208f8ee-309e-4f0b-bf0c-6a0aa1262060" {
		t.Errorf("endpoint = %q", agent.Endpoint)
	}
	if agent.Token != "tok-123" {
		t.Errorf("token = %q", agent.Token)
	}
	if agent.CWD != "/tmp/my-project" || agent.Name != "my-project" {
		t.Errorf("cwd=%q name=%q, want /tmp/my-project / my-project", agent.CWD, agent.Name)
	}
	if agent.PID != wantPID {
		t.Errorf("pid = %d, want %d", agent.PID, wantPID)
	}
	if agent.State != store.AgentIdle || agent.StateTS == "" {
		t.Errorf("state = %q at %q, want idle", agent.State, agent.StateTS)
	}
}

func TestAgyPushInvokesAgentAPIWithRecipientEnv(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "call.log")
	stub := filepath.Join(dir, "agentapi")
	script := fmt.Sprintf("#!/bin/sh\nprintf 'ADDR=%%s\\nTOKEN=%%s\\nARGS=%%s\\n' \"$ANTIGRAVITY_LS_ADDRESS\" \"$ANTIGRAVITY_CSRF_TOKEN\" \"$*\" > %q\nprintf '{\"response\":{}}\\n'\n", logFile)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate another agy session sending to this recipient: its own env must
	// not override the recipient's LS address or CSRF token.
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:11111")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "sender-token")

	agent := store.Agent{
		ID:       "1208f8ee",
		Harness:  "agy",
		Endpoint: "localhost:64177/1208f8ee-309e-4f0b-bf0c-6a0aa1262060",
		Token:    "recipient-token",
	}
	if err := (&Agy{AgentAPI: stub}).Push(agent, Outgoing{Text: "hello from recado"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"ADDR=localhost:64177\n",
		"TOKEN=recipient-token\n",
		"ARGS=send-message --title recado 1208f8ee-309e-4f0b-bf0c-6a0aa1262060 hello from recado\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("agentapi call = %q, want %q", text, want)
		}
	}
}

func TestAgyPushSurfacesJSONErrorAndExitFailure(t *testing.T) {
	if err := (&Agy{}).Push(store.Agent{ID: "worker", Harness: "agy"}, Outgoing{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "language server endpoint") {
		t.Fatalf("Push without endpoint = %v", err)
	}

	dir := t.TempDir()
	rpcErrStub := filepath.Join(dir, "agentapi-rpc-err")
	if err := os.WriteFile(rpcErrStub, []byte("#!/bin/sh\nprintf '{\"error\":\"rpc error: unavailable\"}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := store.Agent{ID: "worker", Harness: "agy", Endpoint: "localhost:64177/conv-1"}
	if err := (&Agy{AgentAPI: rpcErrStub}).Push(agent, Outgoing{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "rpc error: unavailable") {
		t.Fatalf("Push with JSON error = %v, want RPC error surfaced", err)
	}

	failStub := filepath.Join(dir, "agentapi-fail")
	if err := os.WriteFile(failStub, []byte("#!/bin/sh\necho boom >&2; exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&Agy{AgentAPI: failStub}).Push(agent, Outgoing{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Push with exit 1 = %v, want stderr surfaced", err)
	}
}

func TestAgyCheck(t *testing.T) {
	t.Run("agy not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if err := (&Agy{Home: t.TempDir()}).Check(); err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Fatalf("Check = %v, want PATH error", err)
		}
	})

	t.Run("hooks missing, then current", func(t *testing.T) {
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "agy"), []byte("#!/bin/sh\necho 1.5.13\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)

		agy := &Agy{Home: t.TempDir()}
		if err := agy.Check(); err == nil || !strings.Contains(err.Error(), "recado skill setup") {
			t.Fatalf("Check with no hooks = %v, want setup hint", err)
		}
		hooks, err := agy.HookSettings()
		if err != nil {
			t.Fatal(err)
		}
		if err := hooks.Install(); err != nil {
			t.Fatal(err)
		}
		if err := agy.Check(); err != nil {
			t.Fatalf("Check with current hooks = %v, want nil", err)
		}
	})
}

func TestAgyAncestorSkipsTheShell(t *testing.T) {
	table := map[int]process{
		100: {parent: 1, command: "/opt/homebrew/Caskroom/antigravity-cli/1.5.13/antigravity"},
		200: {parent: 100, command: "/bin/sh"},
		300: {parent: 200, command: "recado"},
	}
	if got := agyAncestor(300, table); got != 100 {
		t.Errorf("agyAncestor = %d, want 100", got)
	}
}
