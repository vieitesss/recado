package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/harness"
)

// claudeApp is an app whose only harness is Claude Code, with its settings
// file under a home of the test's own.
func claudeApp(t *testing.T) (*app, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := &app{Root: filepath.Join(t.TempDir(), "state"), Harnesses: harness.Set{&harness.Claude{Home: home}}}
	return a, filepath.Join(home, ".claude", "settings.json")
}

const userSettings = `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Grep",
        "hooks": [{"type": "command", "command": "~/.claude/hooks/gate"}]
      }
    ]
  }
}
`

func TestSkillInstallsClaudeHooks(t *testing.T) {
	a, settings := claudeApp(t)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(userSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	if out := mustExec(t, a, "", "skill", "setup"); !strings.Contains(lineWith(out, settings), "installed") {
		t.Errorf("skill setup = %q, want Claude Code's hooks installed", out)
	}
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"model": "opus"`, "~/.claude/hooks/gate", "recado hook claude session-start --store", "recado hook claude turn-start", "recado hook claude turn-end --hold"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("settings do not hold %q:\n%s", want, data)
		}
	}
	if out := mustExec(t, a, "", "skill", "status"); !strings.Contains(lineWith(out, settings), "current") {
		t.Errorf("skill status = %q, want Claude Code's hooks current", out)
	}

	if out := mustExec(t, a, "", "skill", "remove"); !strings.Contains(lineWith(out, settings), "removed") {
		t.Errorf("skill remove = %q, want Claude Code's hooks removed", out)
	}
	data, err = os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "recado hook claude") {
		t.Errorf("recado's hooks are still there:\n%s", data)
	}
	if !strings.Contains(string(data), "~/.claude/hooks/gate") || !strings.Contains(string(data), `"model": "opus"`) {
		t.Errorf("removing recado's hooks changed the user's settings:\n%s", data)
	}
}

func TestSkillRefusesToTouchASettingsFileItCannotRead(t *testing.T) {
	a, settings := claudeApp(t)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := `{"model": "opus",`
	if err := os.WriteFile(settings, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"skill", "setup"}, {"skill", "status"}, {"skill", "remove"}} {
		if _, err := exec(t, a, "", args...); err == nil {
			t.Errorf("recado %s succeeded on a settings file that is not JSON", strings.Join(args, " "))
		}
		if got, err := os.ReadFile(settings); err != nil || string(got) != broken {
			t.Errorf("recado %s changed the settings file to %q, %v", strings.Join(args, " "), got, err)
		}
	}
}
