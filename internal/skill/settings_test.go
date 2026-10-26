package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// claudeSettings is the hook configuration the Claude Code adapter installs.
func claudeSettings(path string) Hooks {
	return Hooks{
		Path:   path,
		Prefix: "recado hook claude",
		Entries: []HookEntry{
			{Event: "SessionStart", Command: "recado hook claude session-start --store"},
			{Event: "UserPromptSubmit", Command: "recado hook claude turn-start"},
			{Event: "Stop", Command: "recado hook claude turn-end --hold"},
		},
	}
}

// usersSettings is a settings file recado does not own: its own keys, its own
// hook events and, in SessionStart, a group with a matcher.
const usersSettings = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "~/.claude/hooks/reminder"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Grep|Glob",
        "hooks": [
          {
            "type": "command",
            "command": "~/.claude/hooks/gate",
            "timeout": 5
          }
        ]
      }
    ]
  },
  "attribution": {
    "commit": ""
  }
}
`

// writeSettings writes data to a fresh settings path.
func writeSettings(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// readSettings decodes a settings file as a document, whatever its layout.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", path, err, data)
	}
	return doc
}

// commands lists every hook command in a settings document, in order.
func commands(t *testing.T, doc map[string]any) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	hooks, _ := doc["hooks"].(map[string]any)
	for event, groups := range hooks {
		list, _ := groups.([]any)
		for _, group := range list {
			group, _ := group.(map[string]any)
			list, _ := group["hooks"].([]any)
			for _, hook := range list {
				hook, _ := hook.(map[string]any)
				if command, ok := hook["command"].(string); ok {
					out[event] = append(out[event], command)
				}
			}
		}
	}
	return out
}

func TestHooksInstallMergesIntoTheUsersSettings(t *testing.T) {
	path := writeSettings(t, usersSettings)
	want := readSettings(t, path)
	h := claudeSettings(path)

	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The user's keys keep their order; recado appends to what is there.
	text := string(data)
	model, hooks, attribution := strings.Index(text, `"model"`), strings.Index(text, `"hooks"`), strings.Index(text, `"attribution"`)
	if model < 0 || hooks < 0 || attribution < 0 || !(model < hooks && hooks < attribution) {
		t.Errorf("the settings keys are out of order: model=%d hooks=%d attribution=%d\n%s", model, hooks, attribution, text)
	}

	got := readSettings(t, path)
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		if len(commands(t, got)[event]) == 0 {
			t.Errorf("no %s hook was installed", event)
		}
	}
	installed := commands(t, got)
	for _, entry := range h.Entries {
		if !contains(installed[entry.Event], entry.Command) {
			t.Errorf("%s hooks = %q, want %q", entry.Event, installed[entry.Event], entry.Command)
		}
	}
	// Foreign hooks are untouched, matcher and all.
	if wantHooks, gotHooks := want["hooks"].(map[string]any), got["hooks"].(map[string]any); !reflect.DeepEqual(wantHooks["PreToolUse"], gotHooks["PreToolUse"]) {
		t.Errorf("PreToolUse changed:\n got %v\nwant %v", gotHooks["PreToolUse"], wantHooks["PreToolUse"])
	}
	if !contains(installed["SessionStart"], "~/.claude/hooks/reminder") {
		t.Errorf("SessionStart hooks = %q, want the user's reminder kept", installed["SessionStart"])
	}

	// Installing again changes nothing.
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != text {
		t.Errorf("a second install changed the file:\n%s", again)
	}
}

func TestHooksRemoveLeavesTheUserAsTheyWere(t *testing.T) {
	path := writeSettings(t, usersSettings)
	original := readSettings(t, path)
	h := claudeSettings(path)
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}

	removed, err := h.Remove()
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("Remove reported nothing removed")
	}
	if got := readSettings(t, path); !reflect.DeepEqual(got, original) {
		t.Errorf("after remove the settings are\n%v\nwant\n%v", got, original)
	}

	if removed, err := h.Remove(); err != nil || removed {
		t.Errorf("a second remove = %v, %v; want nothing removed", removed, err)
	}
}

func TestHooksRemoveDeletesAFileThatHeldOnlyRecadosHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	h := claudeSettings(path)
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the settings file recado created is still there: %v", err)
	}
}

func TestHooksInspectReportsWhatIsInstalled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	h := claudeSettings(path)
	if state, err := h.Inspect(); err != nil || state != Missing {
		t.Fatalf("Inspect before install = %v, %v; want missing", state, err)
	}

	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	if state, err := h.Inspect(); err != nil || state != Current {
		t.Fatalf("Inspect after install = %v, %v; want current", state, err)
	}

	// An entry from an older recado makes the install outdated, not current.
	stale := strings.Replace(readFile(t, path), "turn-end --hold", "turn-end", 1)
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if state, err := h.Inspect(); err != nil || state != Outdated {
		t.Fatalf("Inspect with an old command = %v, %v; want outdated", state, err)
	}
}

func TestHooksRefusesAFileThatIsNotJSON(t *testing.T) {
	path := writeSettings(t, "{\"model\": \"opus\",\n")
	h := claudeSettings(path)
	if _, err := h.Inspect(); err == nil {
		t.Error("Inspect accepted a settings file that is not JSON")
	}
	if err := h.Install(); err == nil {
		t.Error("Install wrote into a settings file that is not JSON")
	}
	if _, err := h.Remove(); err == nil {
		t.Error("Remove wrote into a settings file that is not JSON")
	}
	if got := readFile(t, path); got != "{\"model\": \"opus\",\n" {
		t.Errorf("the invalid settings file changed: %q", got)
	}
}

func TestHooksInstallCreatesAnEmptySettingsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	if err := claudeSettings(path).Install(); err != nil {
		t.Fatal(err)
	}
	if got := commands(t, readSettings(t, path)); len(got["Stop"]) != 1 {
		t.Errorf("Stop hooks = %q, want recado's one entry", got["Stop"])
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}
