package harness

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vieitesss/recado/internal/store"
)

// socketDir is a short temporary directory: a unix socket path is limited to
// about a hundred bytes, which t.TempDir() can exceed.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "recado")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// collect listens on sock and acknowledges every line it receives, the way an
// installed shim does, so a push that waits for an acknowledgement gets one. It
// reports the lines of the first connection once that connection closes.
func collect(t *testing.T, sock string) <-chan []string {
	t.Helper()
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
		var got []string
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if line = strings.TrimSuffix(line, "\n"); line != "" {
				got = append(got, line)
				_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
			}
			if err != nil {
				break
			}
		}
		lines <- got
	}()
	return lines
}

// onlyText decodes one socket line and reports the text it carries.
func onlyText(t *testing.T, line string) string {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("socket line %q is not JSON: %v", line, err)
	}
	if len(got) != 1 {
		t.Fatalf("socket line %q carries %d fields, want only text", line, len(got))
	}
	text, ok := got["text"].(string)
	if !ok {
		t.Fatalf("socket line %q has no text", line)
	}
	return text
}

func TestPiPushWritesTheTextItIsGiven(t *testing.T) {
	sock := filepath.Join(socketDir(t), "pi.sock")
	lines := collect(t, sock)
	agent := store.Agent{ID: "worker", Harness: "pi", Endpoint: sock}
	want := "[recado] from asker · message (id v6t7)\nline one\nline two"
	if err := (&Pi{}).Push(agent, Outgoing{Text: want}); err != nil {
		t.Fatal(err)
	}

	got := <-lines
	if len(got) != 1 {
		t.Fatalf("socket received %d lines, want one per push: %q", len(got), got)
	}
	if text := onlyText(t, got[0]); text != want {
		t.Errorf("socket text =\n%s\nwant\n%s", text, want)
	}
}

func TestPiPushFailsInsteadOfDropping(t *testing.T) {
	if err := (&Pi{}).Push(store.Agent{ID: "worker", Harness: "pi"}, Outgoing{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("Push without an endpoint = %v, want an error mentioning the endpoint", err)
	}

	sock := filepath.Join(socketDir(t), "gone.sock")
	agent := store.Agent{ID: "worker", Harness: "pi", Endpoint: sock}
	if err := (&Pi{}).Push(agent, Outgoing{Text: "hi"}); err == nil {
		t.Fatal("Push to a socket nobody listens on succeeded")
	}
}

func TestPiDetect(t *testing.T) {
	pi := &Pi{}
	cases := []struct {
		name string
		env  []string
		want string
		ok   bool
	}{
		{"nothing", []string{"HOME=/tmp"}, "", false},
		{"address only", []string{"RECADO_ADDR=worker"}, "", false},
		{"marker only", []string{"RECADO_HARNESS=pi"}, "", false},
		{"bare address", []string{"RECADO_HARNESS=pi", "RECADO_ADDR=worker"}, "worker", true},
		{"full address", []string{"RECADO_HARNESS=pi", "RECADO_ADDR=recado://local/sub-7"}, "sub-7", true},
		{"other harness", []string{"RECADO_HARNESS=claude", "RECADO_ADDR=worker"}, "", false},
		{"bad address", []string{"RECADO_HARNESS=pi", "RECADO_ADDR=Nope!"}, "", false},
		{"remote address", []string{"RECADO_HARNESS=pi", "RECADO_ADDR=rpi/worker"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := pi.Detect(c.env)
			if id != c.want || ok != c.ok {
				t.Errorf("Detect(%q) = %q, %v; want %q, %v", c.env, id, ok, c.want, c.ok)
			}
		})
	}
}

func TestDefaultRegistersPi(t *testing.T) {
	h, ok := Default().Lookup("pi")
	if !ok {
		t.Fatal("Default does not register pi")
	}
	if !h.QueuesWhileBusy() {
		t.Error("Pi must be pushed to mid-turn: it queues follow-ups")
	}
}

func TestPiShims(t *testing.T) {
	home := t.TempDir()
	shims := (&Pi{Home: home}).Shims()
	if len(shims) != 1 {
		t.Fatalf("Shims = %d files, want the one extension", len(shims))
	}
	shim := shims[0]
	want := filepath.Join(home, ".pi", "agent", "extensions", "recado.ts")
	if shim.Path != want {
		t.Errorf("shim path = %s, want %s", shim.Path, want)
	}
	if shim.Mode != 0o644 {
		t.Errorf("shim mode = %v, want 0644", shim.Mode)
	}
	text := string(shim.Content)
	for _, wantPart := range []string{
		"recado-shim:",
		"session_start",
		"session_shutdown",
		`deliverAs: "steer"`,
		"RECADO_ADDR",
		"RECADO_HARNESS",
		"hook",
	} {
		if !strings.Contains(text, wantPart) {
			t.Errorf("the extension does not mention %q", wantPart)
		}
	}
}

// fakePi puts a pi binary that prints version on PATH.
func fakePi(t *testing.T, version string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\necho " + version + "\n"
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestPiCheck(t *testing.T) {
	t.Run("pi not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := (&Pi{Home: t.TempDir()}).Check()
		if err == nil || !strings.Contains(err.Error(), "PATH") {
			t.Fatalf("Check = %v, want a missing-binary error", err)
		}
	})

	t.Run("pi too old", func(t *testing.T) {
		fakePi(t, "1.1.9")
		err := (&Pi{Home: t.TempDir()}).Check()
		if err == nil || !strings.Contains(err.Error(), "older than 1.2.0") {
			t.Fatalf("Check = %v, want a too-old error", err)
		}
	})

	t.Run("shim missing, outdated, then current", func(t *testing.T) {
		home := t.TempDir()
		fakePi(t, "1.4.0")
		pi := &Pi{Home: home}
		if err := pi.Check(); err == nil || !strings.Contains(err.Error(), "recado skill setup") {
			t.Fatalf("Check with no extension = %v, want an install hint", err)
		}

		shim := pi.Shims()[0]
		if err := os.MkdirAll(filepath.Dir(shim.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shim.Path, []byte("// recado-shim: \"0\"\nold\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := pi.Check(); err == nil || !strings.Contains(err.Error(), "outdated") {
			t.Fatalf("Check with an outdated extension = %v, want an outdated error", err)
		}

		if err := os.WriteFile(shim.Path, shim.Content, shim.Mode); err != nil {
			t.Fatal(err)
		}
		if err := pi.Check(); err != nil {
			t.Fatalf("Check with a current extension = %v, want nil", err)
		}
	})
}
