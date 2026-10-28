package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/store"
)

// frame decodes one socket line the OpenCode plugin accepts.
type frame struct {
	Agent string `json:"agent"`
	Text  string `json:"text"`
}

func TestOpenCodePushNamesTheSessionItIsFor(t *testing.T) {
	sock := filepath.Join(socketDir(t), "opencode.sock")
	lines := collect(t, sock)
	agent := store.Agent{ID: "ses_abc", Harness: "opencode", Endpoint: sock}
	want := "[recado] from asker · message (id v6t7)\nhello"
	if err := (&OpenCode{}).Push(agent, Outgoing{Text: want}); err != nil {
		t.Fatal(err)
	}

	got := <-lines
	if len(got) != 1 {
		t.Fatalf("socket received %d lines, want one per push: %q", len(got), got)
	}
	var f frame
	if err := json.Unmarshal([]byte(got[0]), &f); err != nil {
		t.Fatalf("socket line %q is not JSON: %v", got[0], err)
	}
	if f.Agent != agent.ID || f.Text != want {
		t.Errorf("frame = %+v, want agent %q and the rendered text", f, agent.ID)
	}
}

func TestOpenCodePushFailsInsteadOfDropping(t *testing.T) {
	oc := &OpenCode{}
	if err := oc.Push(store.Agent{ID: "ses_abc", Harness: "opencode"}, Outgoing{Text: "hi"}); err == nil {
		t.Fatal("Push without an endpoint succeeded")
	}
	gone := store.Agent{ID: "ses_abc", Harness: "opencode", Endpoint: filepath.Join(socketDir(t), "gone.sock")}
	if err := oc.Push(gone, Outgoing{Text: "hi"}); err == nil {
		t.Fatal("Push to a socket nobody listens on succeeded")
	}
}

func TestOpenCodeDetect(t *testing.T) {
	oc := &OpenCode{}
	cases := []struct {
		name string
		env  []string
		want string
		ok   bool
	}{
		{"nothing", []string{"HOME=/tmp"}, "", false},
		{"address only", []string{"RECADO_ADDR=worker"}, "", false},
		{"marker only", []string{"RECADO_HARNESS=opencode"}, "", false},
		{"bare address", []string{"RECADO_HARNESS=opencode", "RECADO_ADDR=worker"}, "worker", true},
		{"full address", []string{"RECADO_HARNESS=opencode", "RECADO_ADDR=recado://local/ses_7"}, "ses_7", true},
		{"other harness", []string{"RECADO_HARNESS=pi", "RECADO_ADDR=worker"}, "", false},
		{"bad address", []string{"RECADO_HARNESS=opencode", "RECADO_ADDR=Nope!"}, "", false},
		{"remote address", []string{"RECADO_HARNESS=opencode", "RECADO_ADDR=rpi/worker"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := oc.Detect(c.env)
			if id != c.want || ok != c.ok {
				t.Errorf("Detect(%q) = %q, %v; want %q, %v", c.env, id, ok, c.want, c.ok)
			}
		})
	}
}

func TestOpenCodeShims(t *testing.T) {
	home := t.TempDir()
	shims := (&OpenCode{Home: home}).Shims()
	if len(shims) != 1 {
		t.Fatalf("Shims() = %d files, want one plugin", len(shims))
	}
	s := shims[0]
	want := filepath.Join(home, ".config", "opencode", "plugins", "recado.js")
	if s.Path != want {
		t.Errorf("shim path = %s, want %s", s.Path, want)
	}
	if s.Mode != 0o644 {
		t.Errorf("shim mode = %v, want 0644", s.Mode)
	}
	body := string(s.Content)
	for _, part := range []string{
		`// recado-shim: "2"`,
		`hook", "opencode"`,
		`delivery: "queue"`,
		`RECADO_ADDR`,
		`RECADO_HARNESS: "opencode"`,
		`ctx.session.prompt`,
		`JSON.stringify(ok ? { ok: true }`,
	} {
		if !strings.Contains(body, part) {
			t.Errorf("plugin does not mention %q", part)
		}
	}
	if strings.Contains(body, `"steer"`) {
		t.Error("plugin can steer a running turn; recado never wants that")
	}
}

// fakeOpenCode puts an opencode binary that prints version on PATH.
func fakeOpenCode(t *testing.T, version string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\necho " + version + "\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestOpenCodeCheck(t *testing.T) {
	t.Run("opencode not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := (&OpenCode{Home: t.TempDir()}).Check()
		if err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Fatalf("Check = %v, want a missing-binary error", err)
		}
	})

	t.Run("opencode too old", func(t *testing.T) {
		fakeOpenCode(t, "opencode v1.9.9")
		err := (&OpenCode{Home: t.TempDir()}).Check()
		if err == nil || !strings.Contains(err.Error(), "older than 2.0.0") {
			t.Fatalf("Check = %v, want a too-old error", err)
		}
	})

	t.Run("plugin missing, outdated, then current", func(t *testing.T) {
		home := t.TempDir()
		fakeOpenCode(t, "opencode v2.0.1")
		oc := &OpenCode{Home: home}
		if err := oc.Check(); err == nil || !strings.Contains(err.Error(), "recado skill setup") {
			t.Fatalf("Check with no plugin = %v, want an install hint", err)
		}

		shim := oc.Shims()[0]
		if err := os.MkdirAll(filepath.Dir(shim.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shim.Path, []byte("// recado-shim: \"0\"\nold\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := oc.Check(); err == nil || !strings.Contains(err.Error(), "outdated") {
			t.Fatalf("Check with an outdated plugin = %v, want an outdated error", err)
		}

		if err := os.WriteFile(shim.Path, shim.Content, shim.Mode); err != nil {
			t.Fatal(err)
		}
		if err := oc.Check(); err != nil {
			t.Fatalf("Check with a current plugin = %v, want nil", err)
		}
	})
}

func TestDefaultRegistersOpenCode(t *testing.T) {
	h, ok := Default().Lookup("opencode")
	if !ok {
		t.Fatal("Default does not register opencode")
	}
	if !h.QueuesWhileBusy() {
		t.Error("OpenCode must be pushed to mid-turn: delivery \"queue\" parks the prompt")
	}
}

func TestOpenCodeShimFollowsXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	shims := (&OpenCode{}).Shims()
	if len(shims) != 1 {
		t.Fatalf("Shims() = %d files, want one plugin", len(shims))
	}
	want := filepath.Join(dir, "opencode", "plugins", "recado.js")
	if shims[0].Path != want {
		t.Errorf("shim path = %s, want %s", shims[0].Path, want)
	}
}
