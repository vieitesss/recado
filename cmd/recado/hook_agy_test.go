package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/harness"
)

func newAgyApp(t *testing.T) (*app, string, string) {
	t.Helper()
	for _, name := range []string{"ANTIGRAVITY_CONVERSATION_ID", "ANTIGRAVITY_LS_ADDRESS", "ANTIGRAVITY_CSRF_TOKEN", "RECADO_HARNESS", "RECADO_ADDR"} {
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
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "localhost:64177")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-tok")

	startInput := fmt.Sprintf(`{"conversationId":%q,"cwd":"/tmp/my-repo","hookEventName":"SessionStart"}`, convID)
	if got := strings.TrimSpace(mustExec(t, a, startInput, "hook", "agy", "session-start", "--store")); got != "{}" {
		t.Fatalf("SessionStart = %q, want {}", got)
	}

	// whoami detects the agy session automatically from ANTIGRAVITY_CONVERSATION_ID.
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", convID)
	if got := strings.TrimSpace(mustExec(t, a, "", "whoami")); got != "recado://local/1208f8ee" {
		t.Fatalf("whoami = %q, want recado://local/1208f8ee", got)
	}
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")

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
