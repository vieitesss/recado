package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vieitesss/recado/internal/harness"
	"github.com/vieitesss/recado/internal/store"
)

func newAgyApp(t *testing.T) (*app, string, string) {
	t.Helper()
	for _, name := range []string{
		"ANTIGRAVITY_CONVERSATION_ID",
		"ANTIGRAVITY_LS_ADDRESS",
		"ANTIGRAVITY_CSRF_TOKEN",
		"RECADO_HARNESS",
		"RECADO_ADDR",
		"RECADO_PID",
	} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	pushLog := filepath.Join(dir, "push.log")
	stub := filepath.Join(dir, "agentapi")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s|%%s|%%s\\n' \"$ANTIGRAVITY_LS_ADDRESS\" \"$ANTIGRAVITY_CSRF_TOKEN\" \"$*\" >> %q\nprintf '{\"response\":{}}\\n'\n", pushLog)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{
		Root:      filepath.Join(t.TempDir(), "state"),
		Harnesses: harness.Set{&harness.Agy{Home: home, AgentAPI: stub}},
	}
	return a, filepath.Join(home, ".gemini", "config", "hooks.json"), pushLog
}

func TestAgyStopHookContinuesWithWaitingMessagesAndPushesWhileIdle(t *testing.T) {
	a, _, pushLog := newAgyApp(t)
	convID := "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"

	// SessionStart runs without LS env (agy never exports it to hooks).
	startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/my-repo","hookEventName":"SessionStart"}`, convID)
	if got := strings.TrimSpace(mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")); got != "{}" {
		t.Fatalf("SessionStart = %q, want {}", got)
	}
	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	ag, err := st.Lookup("1208f8ee")
	if err != nil {
		t.Fatal(err)
	}
	if ag.Endpoint != "" || ag.Token != "" {
		t.Fatalf("after SessionStart: endpoint=%q token=%q, want both empty", ag.Endpoint, ag.Token)
	}

	// whoami inside run_command detects the agy session and passively captures
	// the LS endpoint + CSRF token.
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
	if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/1208f8ee" {
		t.Fatalf("whoami = %q, want recado://local/1208f8ee", got)
	}
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")

	// PreInvocation marks the agent busy.
	if got := strings.TrimSpace(mustExec(t, a, startInput, "hook", "agy", "turn-start")); got != "{}" {
		t.Fatalf("PreInvocation = %q, want {}", got)
	}

	// Messages sent while busy wait for Stop and are not pushed.
	t.Setenv("RECADO_ADDR", "subagent-build-1")
	mustExec(t, a, "", "ask", "1208f8ee", "reuse helper?")
	mustExec(t, a, "", "send", "1208f8ee", "second note")
	t.Setenv("RECADO_ADDR", "")
	if _, err := os.Stat(pushLog); !os.IsNotExist(err) {
		t.Fatalf("agentapi was called while busy: %v", err)
	}

	// Stop holds waiting messages and asks Antigravity to continue.
	stopInput := fmt.Sprintf(`{"conversationId":%q,"terminationReason":"NO_TOOL_CALL","fullyIdle":true}`, convID)
	got := mustExec(t, a, stopInput, "hook", "agy", "turn-end", "--hold")
	decision := decode[stopDecision](t, got)
	if decision.Decision != "continue" {
		t.Fatalf("Stop decision = %q, want continue", decision.Decision)
	}
	if !strings.Contains(decision.Reason, "reuse helper?") || !strings.Contains(decision.Reason, "second note") {
		t.Fatalf("Stop reason = %q, want both messages", decision.Reason)
	}

	// When the continuation turn finishes with nothing waiting, Stop prints {}
	// and marks the agent idle.
	mustExec(t, a, startInput, "hook", "agy", "turn-start")
	if got := strings.TrimSpace(mustExec(t, a, stopInput, "hook", "agy", "turn-end", "--hold")); got != "{}" {
		t.Fatalf("empty Stop = %q, want {}", got)
	}

	// Now idle: a send pushes immediately via agentapi send-message.
	t.Setenv("RECADO_ADDR", "subagent-build-1")
	mustExec(t, a, "", "send", "1208f8ee", "wake up")
	data, err := os.ReadFile(pushLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "localhost:64177|csrf-tok|send-message --title recado "+convID) || !strings.Contains(string(data), "wake up") {
		t.Errorf("push log = %q, want agentapi send-message with rendered message", string(data))
	}
}

func TestAgySessionStartIgnoresInheritedEndpoint(t *testing.T) {
	a, _, _ := newAgyApp(t)
	convID := "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"
	// Simulate hook env inheriting a parent agy session's LS vars.
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:54299")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "parent-csrf")

	startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/child-repo","hookEventName":"SessionStart"}`, convID)
	if got := strings.TrimSpace(mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")); got != "{}" {
		t.Fatalf("SessionStart = %q, want {}", got)
	}

	st, err := a.store()
	if err != nil {
		t.Fatal(err)
	}
	ag, err := st.Lookup("1208f8ee")
	if err != nil {
		t.Fatal(err)
	}
	if ag.Endpoint != "" || ag.Token != "" {
		t.Fatalf("SessionStart captured inherited endpoint=%q token=%q, want both empty", ag.Endpoint, ag.Token)
	}
}

func TestAgyIdleSendBeforeCaptureStaysStoredAndDrainsAtStop(t *testing.T) {
	a, _, pushLog := newAgyApp(t)
	convID := "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"
	startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/my-repo","hookEventName":"SessionStart"}`, convID)
	mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")

	// Agent is idle, but no non-hook recado CLI call has captured its endpoint yet.
	t.Setenv("RECADO_ADDR", "asker")
	out := mustExec(t, a, "", "send", "--json", "1208f8ee", "wait for next turn")
	res := decode[sent](t, out)
	if res.Pushed {
		t.Fatalf("send before capture reported pushed=true, want false")
	}
	if _, err := os.Stat(pushLog); !os.IsNotExist(err) {
		t.Fatalf("agentapi should not be called without endpoint: %v", err)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", res.Short)); got != res.Short+" stored" {
		t.Fatalf("status = %q, want %q", got, res.Short+" stored")
	}
	t.Setenv("RECADO_ADDR", "")

	// Next turn drains the stored message through Stop --hold.
	mustExec(t, a, startInput, "hook", "agy", "turn-start")
	stopInput := fmt.Sprintf(`{"conversationId":%q,"terminationReason":"NO_TOOL_CALL","fullyIdle":true}`, convID)
	got := mustExec(t, a, stopInput, "hook", "agy", "turn-end", "--hold")
	decision := decode[stopDecision](t, got)
	if decision.Decision != "continue" || !strings.Contains(decision.Reason, "wait for next turn") {
		t.Fatalf("Stop decision = %+v, want continue with stored message", decision)
	}
}

func TestAgyPassiveCaptureArmsIdlePush(t *testing.T) {
	convID := "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"
	cases := []struct {
		name string
		args []string
	}{
		{"whoami", []string{"whoami"}},
		{"agents", []string{"agents"}},
		{"send", []string{"send", "peer", "hello peer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, pushLog := newAgyApp(t)
			// Register a peer agent for the `send` case.
			st, err := a.store()
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Register(store.Agent{ID: "peer", Harness: "agy", PID: os.Getpid(), State: store.AgentBusy}); err != nil {
				t.Fatal(err)
			}

			startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/my-repo","hookEventName":"SessionStart"}`, convID)
			mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")

			// Run the non-hook CLI command inside agy's run_command env.
			t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
			t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
			t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
			mustExec(t, a, "", tc.args...)

			ag, err := st.Lookup("1208f8ee")
			if err != nil {
				t.Fatal(err)
			}
			wantEndpoint := "localhost:64177/" + convID
			if ag.Endpoint != wantEndpoint || ag.Token != "csrf-tok" {
				t.Fatalf("after %v: endpoint=%q token=%q, want %q / %q", tc.args, ag.Endpoint, ag.Token, wantEndpoint, "csrf-tok")
			}

			// Repeating the command with unchanged values does not rewrite the file.
			regPath := filepath.Join(a.Root, "agents", "1208f8ee.json")
			oldTime := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(regPath, oldTime, oldTime); err != nil {
				t.Fatal(err)
			}
			mustExec(t, a, "", "whoami")
			info, err := os.Stat(regPath)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(oldTime) {
				t.Errorf("unchanged passive refresh rewrote %s (mtime %v, want %v)", regPath, info.ModTime(), oldTime)
			}

			// Clear caller env and send to the idle agy agent: must push via agentapi.
			t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
			t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
			t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")
			t.Setenv("RECADO_ADDR", "asker")
			res := decode[sent](t, mustExec(t, a, "", "send", "--json", "1208f8ee", "wake up"))
			if !res.Pushed {
				t.Fatalf("send after passive capture: pushed=false, want true")
			}
			data, err := os.ReadFile(pushLog)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "localhost:64177|csrf-tok|send-message --title recado "+convID) {
				t.Errorf("push log = %q, want agentapi call for %s", string(data), convID)
			}
		})
	}
}

func TestAgyPassiveCaptureLauncherOverrideAndNestedChildGuard(t *testing.T) {
	parentConv := "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"
	childConv := "96ab425f-1111-4f0b-bf0c-6a0aa1262060"

	for _, marker := range []string{"", "agy"} {
		t.Run("marker="+marker, func(t *testing.T) {
			a, _, _ := newAgyApp(t)
			t.Setenv("RECADO_ADDR", "subagent-1")
			t.Setenv("RECADO_HARNESS", marker)
			t.Setenv("ANTIGRAVITY_CONVERSATION_ID", parentConv)

			startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/parent-repo","hookEventName":"SessionStart"}`, parentConv)
			mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")

			// Parent runs a non-hook command: captures its endpoint on subagent-1.
			t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
			t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "parent-tok-1")
			mustExec(t, a, "", "whoami")

			st, err := a.store()
			if err != nil {
				t.Fatal(err)
			}
			ag, err := st.Lookup("subagent-1")
			if err != nil {
				t.Fatal(err)
			}
			if ag.Endpoint != "localhost:64177/"+parentConv || ag.Token != "parent-tok-1" {
				t.Fatalf("initial capture = (%q, %q), want localhost:64177/%s / parent-tok-1", ag.Endpoint, ag.Token, parentConv)
			}

			// Same conversation updating its LS port/token is allowed.
			t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64200")
			t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "parent-tok-2")
			mustExec(t, a, "", "whoami")
			ag, err = st.Lookup("subagent-1")
			if err != nil {
				t.Fatal(err)
			}
			if ag.Endpoint != "localhost:64200/"+parentConv || ag.Token != "parent-tok-2" {
				t.Fatalf("same-conversation refresh = (%q, %q), want localhost:64200/%s / parent-tok-2", ag.Endpoint, ag.Token, parentConv)
			}

			// Nested child agy with a different conversation id inheriting RECADO_ADDR
			// must NOT overwrite the parent's already-captured endpoint.
			t.Setenv("ANTIGRAVITY_CONVERSATION_ID", childConv)
			t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:99999")
			t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "child-tok")
			mustExec(t, a, "", "whoami")

			ag, err = st.Lookup("subagent-1")
			if err != nil {
				t.Fatal(err)
			}
			if ag.Endpoint != "localhost:64200/"+parentConv || ag.Token != "parent-tok-2" {
				t.Fatalf("nested child overwrote parent endpoint = (%q, %q), want localhost:64200/%s / parent-tok-2", ag.Endpoint, ag.Token, parentConv)
			}
		})
	}
}

func TestAgyPassiveCaptureNoOps(t *testing.T) {
	convID := "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"
	startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/my-repo","hookEventName":"SessionStart"}`, convID)

	t.Run("missing csrf token", func(t *testing.T) {
		a, _, _ := newAgyApp(t)
		mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")

		t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")
		mustExec(t, a, "", "whoami")

		st, err := a.store()
		if err != nil {
			t.Fatal(err)
		}
		ag, err := st.Lookup("1208f8ee")
		if err != nil {
			t.Fatal(err)
		}
		if ag.Endpoint != "" || ag.Token != "" {
			t.Fatalf("captured without CSRF token: endpoint=%q token=%q", ag.Endpoint, ag.Token)
		}
	})

	t.Run("unregistered agent does not create state dir", func(t *testing.T) {
		a, _, _ := newAgyApp(t)
		t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")

		if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/1208f8ee" {
			t.Fatalf("whoami = %q, want recado://local/1208f8ee", got)
		}
		if _, err := os.Stat(a.Root); !os.IsNotExist(err) {
			t.Fatalf("passive refresh created state dir %s for unregistered agent (err=%v)", a.Root, err)
		}
	})

	t.Run("dead agent", func(t *testing.T) {
		a, _, _ := newAgyApp(t)
		st, err := a.store()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Register(store.Agent{ID: "1208f8ee", Harness: "agy", PID: deadPID, State: store.AgentIdle}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
		mustExec(t, a, "", "whoami")

		ag, err := st.Lookup("1208f8ee")
		if err != nil {
			t.Fatal(err)
		}
		if ag.Endpoint != "" || ag.Token != "" {
			t.Fatalf("captured on dead agent: endpoint=%q token=%q", ag.Endpoint, ag.Token)
		}
	})

	t.Run("retired agent", func(t *testing.T) {
		a, _, _ := newAgyApp(t)
		st, err := a.store()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Register(store.Agent{ID: "1208f8ee", Harness: "agy", PID: os.Getpid(), Retired: true, State: store.AgentIdle}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
		mustExec(t, a, "", "whoami")

		ag, err := st.Lookup("1208f8ee")
		if err != nil {
			t.Fatal(err)
		}
		if ag.Endpoint != "" || ag.Token != "" {
			t.Fatalf("captured on retired agent: endpoint=%q token=%q", ag.Endpoint, ag.Token)
		}
	})

	t.Run("foreign harness agent", func(t *testing.T) {
		for _, marker := range []string{"", "pi"} {
			t.Run("marker="+marker, func(t *testing.T) {
				a, _ := newForeignApp(t)
				t.Setenv("RECADO_PID", strconv.Itoa(os.Getpid()))
				t.Setenv("RECADO_HARNESS", "pi")
				t.Setenv("RECADO_ADDR", "pi-worker")
				mustExec(t, a, "", "hook", "pi", "session-start", "--endpoint", "http://127.0.0.1:4321")

				// Simulate pi running inside an agy session with inherited ANTIGRAVITY_* env.
				t.Setenv("RECADO_HARNESS", marker)
				t.Setenv("RECADO_ADDR", "pi-worker")
				t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
				t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
				t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
				mustExec(t, a, "", "whoami")

				st, err := a.store()
				if err != nil {
					t.Fatal(err)
				}
				ag, err := st.Lookup("pi-worker")
				if err != nil {
					t.Fatal(err)
				}
				if ag.Harness != "pi" || ag.Endpoint != "http://127.0.0.1:4321" || ag.Token != "" {
					t.Fatalf("foreign pi agent was mutated: %+v", ag)
				}
			})
		}
	})

	t.Run("hook commands never trigger passive capture", func(t *testing.T) {
		a, _, _ := newAgyApp(t)
		mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")

		t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
		mustExec(t, a, startInput, "hook", "agy", "turn-start")
		stopInput := fmt.Sprintf(`{"conversationId":%q,"terminationReason":"NO_TOOL_CALL","fullyIdle":true}`, convID)
		mustExec(t, a, stopInput, "hook", "agy", "turn-end", "--hold")

		st, err := a.store()
		if err != nil {
			t.Fatal(err)
		}
		ag, err := st.Lookup("1208f8ee")
		if err != nil {
			t.Fatal(err)
		}
		if ag.Endpoint != "" || ag.Token != "" {
			t.Fatalf("hook command triggered passive capture: endpoint=%q token=%q", ag.Endpoint, ag.Token)
		}
	})

	t.Run("help and version skip passive capture", func(t *testing.T) {
		a, _, _ := newAgyApp(t)
		mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")

		t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
		mustExec(t, a, "", "help")
		mustExec(t, a, "", "--version")

		st, err := a.store()
		if err != nil {
			t.Fatal(err)
		}
		ag, err := st.Lookup("1208f8ee")
		if err != nil {
			t.Fatal(err)
		}
		if ag.Endpoint != "" || ag.Token != "" {
			t.Fatalf("help/version triggered passive capture: endpoint=%q token=%q", ag.Endpoint, ag.Token)
		}
	})

	t.Run("corrupt registry does not break CLI command", func(t *testing.T) {
		a, _, _ := newAgyApp(t)
		regDir := filepath.Join(a.Root, "agents")
		if err := os.MkdirAll(regDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(regDir, "1208f8ee.json"), []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
		t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
		t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")
		if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/1208f8ee" {
			t.Fatalf("whoami with corrupt registry = %q, want recado://local/1208f8ee", got)
		}
	})
}

func TestAgyStopDoesNotReopenUserCanceledTurn(t *testing.T) {
	a, _, _ := newAgyApp(t)
	convID := "1208f8ee-309e-4f0b-bf0c-6a0aa1262060"
	startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/my-repo"}`, convID)
	mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")
	mustExec(t, a, startInput, "hook", "agy", "turn-start")

	t.Setenv("RECADO_ADDR", "asker")
	short := strings.TrimSpace(mustExec(t, a, "", "send", "1208f8ee", "waiting"))
	t.Setenv("RECADO_ADDR", "")

	cancelInput := fmt.Sprintf(`{"conversationId":%q,"terminationReason":"USER_CANCELED"}`, convID)
	if got := strings.TrimSpace(mustExec(t, a, cancelInput, "hook", "agy", "turn-end", "--hold")); got != "{}" {
		t.Fatalf("USER_CANCELED Stop = %q, want {}", got)
	}
	if got := strings.TrimSpace(mustExec(t, a, "", "status", short)); got != short+" stored" {
		t.Errorf("status after USER_CANCELED = %q, want stored", got)
	}
}

func TestAgyFailedHooksFailOpen(t *testing.T) {
	for _, event := range []string{"session-start", "turn-start", "turn-end"} {
		t.Run(event, func(t *testing.T) {
			a, _, _ := newAgyApp(t)
			_, err := exec(t, a, "", "hook", "agy", event)
			if code := exitCode(err); code != 1 {
				t.Fatalf("failed %s: exit %d, want 1 (%v)", event, code, err)
			}
		})
	}
}

func TestSkillInstallsAgyHooksAndSkill(t *testing.T) {
	a, hooksPath, _ := newAgyApp(t)
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	userHooks := "{\n  \"mine\": {\n    \"Stop\": [{\"type\": \"command\", \"command\": \"./my-stop.sh\"}]\n  }\n}\n"
	if err := os.WriteFile(hooksPath, []byte(userHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	if out := mustExec(t, a, "", "skill", "setup"); !strings.Contains(lineWith(out, hooksPath), "installed") || !strings.Contains(out, filepath.Join(".gemini", "config", "skills", "recado")) {
		t.Errorf("skill setup = %q, want agy hooks and skill installed", out)
	}
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"mine"`, "./my-stop.sh", `"recado"`, "recado hook agy session-start --store", "recado hook agy turn-start", "recado hook agy turn-end --hold"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("hooks.json missing %q:\n%s", want, data)
		}
	}
	if out := mustExec(t, a, "", "skill", "status"); !strings.Contains(lineWith(out, hooksPath), "current") {
		t.Errorf("skill status = %q, want agy hooks current", out)
	}

	if out := mustExec(t, a, "", "skill", "remove"); !strings.Contains(lineWith(out, hooksPath), "removed") {
		t.Errorf("skill remove = %q, want agy hooks removed", out)
	}
	data, err = os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "recado hook agy") {
		t.Errorf("recado hooks still present:\n%s", data)
	}
	if !strings.Contains(string(data), "./my-stop.sh") {
		t.Errorf("user hook was lost:\n%s", data)
	}
}
