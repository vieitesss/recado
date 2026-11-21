package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/envelope"
	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

// A second dialect exercises the hook boundary without a special case in the
// command: it names the session on stdin, prepares its registry entry, keeps a
// start backlog and returns a different turn-end response.
type dialectHarness struct{ *harness.Fake }

func (*dialectHarness) Name() string { return "dialect" }

func (*dialectHarness) PrepareHook(req harness.HookRequest) (harness.HookPlan, error) {
	input, err := io.ReadAll(req.Input)
	if err != nil {
		return harness.HookPlan{}, err
	}
	event := req.Event
	switch event {
	case "open":
		event = "session-start"
	case "begin":
		event = "turn-start"
	case "finish":
		event = "turn-end"
	case "invalid-plan":
		event = "invalid-normalized-event"
	}
	plan := harness.HookPlan{
		Event:       event,
		AgentID:     strings.TrimSpace(string(input)),
		KeepPending: event == "session-start",
		HoldTurn:    event == "turn-end",
		Configure: func(agent *store.Agent) error {
			agent.Endpoint = "dialect endpoint"
			agent.State = store.AgentBusy
			return nil
		},
	}
	if event == "turn-start" {
		plan.WriteResponse = func(w io.Writer, _, _ []store.Message, _ bool) error {
			_, err := fmt.Fprintln(w, "BEGAN")
			return err
		}
	} else if plan.HoldTurn {
		plan.WriteResponse = func(w io.Writer, _ []store.Message, pending []store.Message, _ bool) error {
			for _, msg := range pending {
				if _, err := fmt.Fprintf(w, "NEXT: %s\n", envelope.Render(msg.Envelope, msg.Short)); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return plan, nil
}

func (*dialectHarness) FailOpen() bool { return false }

// TestHookJSONAnswersEveryEvent: shims parse each hook's stdout, and "--json"
// promises a document, so an event with nothing to hand over still prints one.
func TestHookJSONAnswersEveryEvent(t *testing.T) {
	a, _ := newTestApp(t, false)
	t.Setenv("RECADO_ADDR", "worker")
	for _, event := range []string{"session-start", "turn-start", "turn-end"} {
		out := decode[struct {
			Pushed  []string  `json:"pushed"`
			Pending []message `json:"pending"`
		}](t, mustExec(t, a, "", "hook", "fake", event, "--json"))
		if out.Pushed == nil || out.Pending == nil {
			t.Errorf("%s --json = %+v, want empty lists", event, out)
		}
		if got := mustExec(t, a, "", "hook", "fake", event); got != "" {
			t.Errorf("%s without --json printed %q, want nothing", event, got)
		}
	}
}

func TestUnknownHookEventKeepsErrorAndExitCode(t *testing.T) {
	fake, _ := newTestApp(t, false)
	claude := newClaudeApp(t)
	t.Setenv("RECADO_ADDR", "worker")
	for _, tc := range []struct {
		name string
		app  *app
		code int
	}{
		{"fake", fake, 2},
		{"claude", claude, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec(t, tc.app, "", "hook", tc.name, "unknown")
			if code := exitCode(err); code != tc.code {
				t.Fatalf("exit code = %d, want %d (error %v)", code, tc.code, err)
			}
			const want = `unknown hook event "unknown": want session-start, turn-start or turn-end`
			if err == nil || err.Error() != want || out != "" {
				t.Fatalf("output = %q, error = %v; want no output and %q", out, err, want)
			}
		})
	}
}

func TestUnknownHookEventReportsOriginalDialectEvent(t *testing.T) {
	fake := &harness.Fake{Dir: t.TempDir()}
	a := &app{Root: t.TempDir(), Harnesses: harness.Set{&dialectHarness{fake}}}
	t.Setenv("RECADO_HARNESS", "")
	t.Setenv("RECADO_ADDR", "worker")
	out, err := exec(t, a, "", "hook", "dialect", "invalid-plan")
	const want = `unknown hook event "invalid-plan": want session-start, turn-start or turn-end`
	if out != "" || err == nil || err.Error() != want || exitCode(err) != 2 {
		t.Fatalf("output = %q, error = %v, exit = %d; want %q and exit 2", out, err, exitCode(err), want)
	}
}

func TestPrintDrainedUsesResponseWriterAndFormat(t *testing.T) {
	var out bytes.Buffer
	pushed := []store.Message{{Short: "abcd"}}
	if err := printDrained(&out, pushed, nil, true); err != nil {
		t.Fatal(err)
	}
	const want = "{\n  \"pushed\": [\n    \"abcd\"\n  ],\n  \"pending\": []\n}\n"
	if out.String() != want {
		t.Fatalf("JSON response = %q, want %q", out.String(), want)
	}
	out.Reset()
	if err := printDrained(&out, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	const empty = "{\n  \"pushed\": [],\n  \"pending\": []\n}\n"
	if out.String() != empty {
		t.Fatalf("empty JSON response = %q, want %q", out.String(), empty)
	}
	out.Reset()
	if err := printDrained(&out, pushed, nil, false); err != nil || out.Len() != 0 {
		t.Fatalf("text response = %q, %v; want empty", out.String(), err)
	}
}

func TestAdapterOwnsHookDialect(t *testing.T) {
	fake := &harness.Fake{Dir: t.TempDir()}
	a := &app{Root: filepath.Join(t.TempDir(), "state"), Harnesses: harness.Set{&dialectHarness{fake}}}
	t.Setenv("RECADO_ADDR", "")
	t.Setenv("FAKE_AGENT_ID", "")

	mustExec(t, a, "worker", "hook", "dialect", "open")
	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.Lookup("worker")
	if err != nil || agent.Endpoint != "dialect endpoint" || agent.State != store.AgentBusy {
		t.Fatalf("prepared agent = %+v, %v", agent, err)
	}
	if got := mustExec(t, a, "worker", "hook", "dialect", "begin"); got != "BEGAN\n" {
		t.Fatalf("turn-start response = %q", got)
	}

	t.Setenv("RECADO_ADDR", "sender")
	short := strings.TrimSpace(mustExec(t, a, "", "send", "worker", "wait for the next turn"))
	t.Setenv("RECADO_ADDR", "")
	out := decode[struct {
		Pending []message `json:"pending"`
	}](t, mustExec(t, a, "worker", "hook", "dialect", "open", "--json"))
	if len(out.Pending) != 0 {
		t.Fatalf("start returned %d pending messages; the adapter cannot hand them over", len(out.Pending))
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Fatalf("after start, status = %q; want stored", got)
	}

	// A failed adapter response must return its claimed messages for retry.
	root := newRootCmd(a)
	root.SetIn(strings.NewReader("worker"))
	root.SetOut(brokenWriter{errors.New("stdout closed")})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"hook", "dialect", "finish", "--json"})
	if err := root.Execute(); err == nil {
		t.Fatal("turn-end succeeded with a broken response")
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Fatalf("after failed response, status = %q; want stored", got)
	}

	got := mustExec(t, a, "worker", "hook", "dialect", "finish", "--json")
	if !strings.HasPrefix(got, "NEXT: [recado] from sender") || !strings.Contains(got, "wait for the next turn") {
		t.Fatalf("adapter response = %q", got)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" queued" {
		t.Fatalf("after response, status = %q; want queued", got)
	}
	if got := mustExec(t, a, "worker", "hook", "dialect", "finish"); got != "" {
		t.Errorf("empty continuation response = %q", got)
	}
	agent, err = st.Lookup("worker")
	if err != nil || agent.State != store.AgentIdle {
		t.Errorf("agent after an empty hand-over = %+v, %v; want idle", agent, err)
	}
	if pushes, err := fake.Pushes(); err != nil || len(pushes) != 0 {
		t.Errorf("pushed %d messages, %v; the hook must hand them over", len(pushes), err)
	}
}

func TestStandardHookRetainsInstalledFlagBehavior(t *testing.T) {
	a, _ := newTestApp(t, false)
	startSession(t, a, "worker")
	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(mustExec(t, a, "", "send", "worker", "waiting"))
	t.Setenv("RECADO_PID", "4194304") // above the supported platforms' PID ceilings
	t.Setenv("RECADO_ADDR", "worker")
	out := decode[struct {
		Pending []message `json:"pending"`
	}](t, mustExec(t, a, "", "hook", "fake", "session-start", "--store", "--json"))
	if len(out.Pending) != 0 {
		t.Fatalf("session-start returned %d messages; want none", len(out.Pending))
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Fatalf("after session-start, status = %q; want stored", got)
	}
	decision := decode[stopDecision](t, mustExec(t, a, "", "hook", "fake", "turn-end", "--hold", "--json"))
	if decision.Decision != "block" || !strings.Contains(decision.Reason, "waiting") {
		t.Errorf("turn-end decision = %+v; want the waiting message", decision)
	}
}
