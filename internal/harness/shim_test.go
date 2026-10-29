package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The shims are JavaScript, and some of what they promise exists only there:
// the acknowledgement a push waits for, which session a shell belongs to, which
// identity a replacement session gets, and the mode of recado's run directory.
// These tests run the installed shim under node against a mocked harness object
// and check what it does. node strips the extension's TypeScript types, so no
// separate TypeScript runner is needed; they skip when node is missing.
func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: the shim tests run the installed shim with it")
	}
	return node
}

// fakeRecado writes a recado the shim can call: it answers a hook with the
// pending messages the scenario asks for, fails, or never returns.
func fakeRecado(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
case "$FAKE_RECADO" in
pending) printf '{"pushed":[],"pending":[{"text":"handed over by the hook"}]}\n' ;;
turnend) if [ "$3" = "turn-end" ]; then printf '{"pushed":[],"pending":[{"text":"handed over at turn end"}]}\n'; else printf '{"pushed":[],"pending":[]}\n'; fi ;;
fail) echo "recado: the state directory is not writable" >&2; exit 2 ;;
hang) exec sleep 60 ;;
*) printf '{"pushed":[],"pending":[]}\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "recado"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// shimRoot is a short temporary workspace: the shim's unix socket lives under
// its state directory, and a socket path is limited to about a hundred bytes.
func shimRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "recado-shim")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// runShim runs the installed shim under node with a throwaway state root and
// home, and returns what its runner printed on the RESULT line. A run that
// outlives timeout fails the test: a shim that waits on recado forever freezes
// the human's session.
func runShim(t *testing.T, shimName string, shim []byte, runner string, env map[string]string, timeout time.Duration) map[string]any {
	t.Helper()
	root := shimRoot(t)
	return runShimIn(t, root, shimName, shim, runner, env, timeout)
}

// runShimIn is runShim in a directory the test prepared, for the cases that
// need recado's state tree to exist before the shim starts.
func runShimIn(t *testing.T, root, shimName string, shim []byte, runner string, env map[string]string, timeout time.Duration) map[string]any {
	t.Helper()
	node := requireNode(t)
	if err := os.WriteFile(filepath.Join(root, shimName), shim, 0o644); err != nil {
		t.Fatal(err)
	}
	// node reads a .js file as CommonJS unless the directory says otherwise.
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"type":"module"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runner.mjs"), []byte(runner), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "runner.mjs")
	cmd.Dir = root
	cmd.Env = []string{
		"SHIM=" + filepath.Join(root, shimName),
		"HOME=" + filepath.Join(root, "home"),
		"PATH=" + fakeRecado(t) + ":/usr/bin:/bin",
		"FAKE_RECADO=ok",
	}
	if _, ok := env["RECADO_STATE_DIR"]; !ok {
		cmd.Env = append(cmd.Env, "RECADO_STATE_DIR="+filepath.Join(root, "state"))
	}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the shim was still running after %s, so it waits on recado forever:\n%s", timeout, out)
	}
	if err != nil {
		t.Fatalf("the shim's runner failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		payload, ok := strings.CutPrefix(line, "RESULT ")
		if !ok {
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(payload), &got); err != nil {
			t.Fatalf("result %q is not JSON: %v", line, err)
		}
		return got
	}
	t.Fatalf("the runner printed no RESULT line:\n%s", out)
	return nil
}

// piRunner drives the Pi extension with a mocked Pi: the events it registers,
// sendUserMessage, and a session manager that names the session.
const piRunner = `
import { pathToFileURL } from "node:url";
import * as fs from "node:fs";
import * as net from "node:net";

const extension = (await import(pathToFileURL(process.env.SHIM).href)).default;
const scenario = process.env.SCENARIO;
const handlers = {};
const sends = [];
const errors = [];
let compacting = process.env.ASYNC_REFUSE === "1";
let streaming = false;
const pi = {
	on(event, fn) { handlers[event] = fn; },
	sendUserMessage(text, opts) {
		if (process.env.REFUSE === "1") throw new Error("the session is not taking a turn");
		// Pi's extension API returns void and catches the session's asynchronous
		// rejection itself, so the caller cannot catch a compaction refusal.
		if (compacting) {
			Promise.reject(new Error("Cannot submit a prompt while compaction is in progress")).catch(() => {});
			return;
		}
		sends.push({ text, deliverAs: opts?.deliverAs });
	},
};
const ctxFor = (session) => ({
	sessionManager: { getSessionId: () => session },
	ui: { notify: (message) => errors.push(message) },
	isIdle: () => !compacting && !streaming,
	model: process.env.NO_MODEL === "1" ? undefined : { provider: "fake" },
	modelRegistry: { hasConfiguredAuth: () => process.env.NO_AUTH !== "1" },
});
const fire = async (event, ctx) => { await handlers[event]({}, ctx); };
const result = (value) => console.log("RESULT " + JSON.stringify(value));

// push writes one frame the way recado's Pi.Push does and reads the answer.
function push(text) {
	return new Promise((resolve) => {
		const sock = net.createConnection(process.env.RECADO_STATE_DIR + "/run/pi-" + process.pid + ".sock", () =>
			sock.write(JSON.stringify({ text }) + "\n"));
		let buf = "";
		sock.setTimeout(4000, () => sock.destroy());
		sock.on("data", (chunk) => { buf += chunk.toString("utf8"); });
		sock.on("close", () => resolve(buf.trim()));
	});
}

extension(pi);
const runDir = process.env.RECADO_STATE_DIR + "/run";
const mode = () => (fs.statSync(runDir).mode & 0o777).toString(8);

switch (scenario) {
	case "identity": {
		await fire("session_start", ctxFor("first-session"));
		const first = process.env.RECADO_ADDR;
		await fire("session_start", ctxFor("second-session"));
		const second = process.env.RECADO_ADDR;
		await fire("session_shutdown", ctxFor("second-session"));
		result({ first, second });
		break;
	}
	case "reload-identity": {
		await fire("session_start", ctxFor("first-session"));
		const first = process.env.RECADO_ADDR;
		await fire("session_shutdown", ctxFor("first-session"));
		const reloaded = (await import(pathToFileURL(process.env.SHIM).href + "?reload=1")).default;
		reloaded(pi);
		await fire("session_start", ctxFor("first-session"));
		const afterReload = process.env.RECADO_ADDR;
		await fire("session_shutdown", ctxFor("first-session"));
		await fire("session_start", ctxFor("second-session"));
		const second = process.env.RECADO_ADDR;
		await fire("session_shutdown", ctxFor("second-session"));
		result({ first, afterReload, second });
		break;
	}
	case "backlog": {
		await fire("session_start", ctxFor("worker"));
		await new Promise((resolve) => setTimeout(resolve, 300));
		await fire("session_shutdown", ctxFor("worker"));
		result({ sends });
		break;
	}
	case "dir": {
		await fire("session_start", ctxFor("a-session"));
		const observed = mode();
		await fire("session_shutdown", ctxFor("a-session"));
		result({ mode: observed });
		break;
	}
	case "ack": {
		await fire("session_start", ctxFor("a-session"));
		const reply = await push("hello from recado");
		await fire("session_shutdown", ctxFor("a-session"));
		result({ reply, sends });
		break;
	}
	case "compaction": {
		const ctx = ctxFor("a-session");
		await fire("session_start", ctx);
		compacting = true;
		const refused = await push("during compaction");
		compacting = false;
		const accepted = await push("after compaction");
		await fire("session_shutdown", ctx);
		result({ refused, accepted, sends });
		break;
	}
	case "busy": {
		const ctx = ctxFor("a-session");
		await fire("session_start", ctx);
		streaming = true;
		await fire("agent_start", ctx);
		const reply = await push("hello from recado");
		await fire("agent_end", ctx);
		streaming = false;
		await fire("session_shutdown", ctx);
		result({ reply, sends });
		break;
	}
	case "timeout": {
		const started = Date.now();
		await fire("session_start", ctxFor("a-session"));
		result({ elapsed: Date.now() - started, errors });
		break;
	}
	case "pending": {
		await fire("session_start", ctxFor("a-session"));
		result({ sends });
		break;
	}
}
process.exit(0);
`

// TestPiExtensionKeepsTheLauncherAddressOutOfTheDerivedIDs: /new and /resume
// keep the Pi process, so the address the extension writes for the agent's
// shell must follow the session instead of repeating the first one, and must
// repeat the launcher's choice instead of replacing it.
func TestPiExtensionKeepsTheLauncherAddressOutOfTheDerivedIDs(t *testing.T) {
	t.Run("derived from each session id", func(t *testing.T) {
		got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{"SCENARIO": "identity"}, 60*time.Second)
		for name, want := range map[string]string{"first": "first-session", "second": "second-session"} {
			if got[name] != want {
				t.Errorf("the %s session address = %v, want %q", name, got[name], want)
			}
		}
	})

	t.Run("the launcher's address outlives every session", func(t *testing.T) {
		got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{
			"SCENARIO":    "identity",
			"RECADO_ADDR": "recado://local/boss",
		}, 60*time.Second)
		for _, name := range []string{"first", "second"} {
			if got[name] != "boss" {
				t.Errorf("the %s session address = %v, want the launcher's boss", name, got[name])
			}
		}
	})
}

// Reload reevaluates the extension in the same process, then a later session
// must still derive its own address unless the launcher supplied one.
func TestPiExtensionReloadKeepsLauncherAndSessionAddressesSeparate(t *testing.T) {
	for _, c := range []struct {
		name     string
		launcher string
		first    string
		second   string
	}{
		{"derived", "", "first-session", "second-session"},
		{"launcher", "recado://local/boss", "boss", "boss"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"SCENARIO": "reload-identity"}
			if c.launcher != "" {
				env["RECADO_ADDR"] = c.launcher
			}
			got := runShim(t, "recado.ts", Extension(), piRunner, env, 10*time.Second)
			for name, want := range map[string]string{"first": c.first, "afterReload": c.first, "second": c.second} {
				if got[name] != want {
					t.Errorf("the %s session address = %v, want %q", name, got[name], want)
				}
			}
		})
	}
}

// realRecado builds the CLI for tests that exercise a shim's own socket.
func realRecado(t *testing.T, root string) (string, func(string, string, ...string) string) {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "recado"), "../../cmd/recado")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recado: %v\n%s", err, out)
	}
	state := filepath.Join(root, "state")
	run := func(agent, harness string, args ...string) string {
		t.Helper()
		cmd := exec.Command(filepath.Join(bin, "recado"), args...)
		cmd.Env = append(os.Environ(), "RECADO_STATE_DIR="+state, "RECADO_ADDR="+agent, "RECADO_HARNESS="+harness)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("recado %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	return bin, run
}

// TestPiSessionStartDeliversBacklogOnce exercises the real CLI and socket on
// the same event loop: a synchronous hook cannot service its own push.
func TestPiSessionStartDeliversBacklogOnce(t *testing.T) {
	requireNode(t)
	root := shimRoot(t)
	bin, run := realRecado(t, root)
	state := filepath.Join(root, "state")
	run("worker", "pi", "hook", "pi", "session-start")
	run("sender", "pi", "send", "worker", "waiting before start")
	got := runShimIn(t, root, "recado.ts", Extension(), piRunner, map[string]string{
		"SCENARIO":         "backlog",
		"RECADO_ADDR":      "worker",
		"RECADO_STATE_DIR": state,
		"PATH":             bin + ":/usr/bin:/bin",
	}, 10*time.Second)
	sends, _ := got["sends"].([]any)
	if len(sends) != 1 {
		t.Fatalf("the session received %d turns, want the backlog exactly once: %v", len(sends), sends)
	}
	if msg, _ := sends[0].(map[string]any); !strings.Contains(msg["text"].(string), "waiting before start") {
		t.Errorf("the session received %v, want the backlog", sends[0])
	}
}

// A refused push, including one attempted by a lifecycle drain, remains
// stored and can be admitted on a later session start.
func TestPiRefusedBacklogRemainsStoredUntilRetry(t *testing.T) {
	requireNode(t)
	root := shimRoot(t)
	bin, run := realRecado(t, root)
	state := filepath.Join(root, "state")
	run("worker", "pi", "hook", "pi", "session-start")
	short := strings.Fields(run("sender", "pi", "send", "worker", "refused backlog"))[0]
	env := map[string]string{
		"SCENARIO":         "backlog",
		"RECADO_ADDR":      "worker",
		"RECADO_STATE_DIR": state,
		"PATH":             bin + ":/usr/bin:/bin",
		"REFUSE":           "1",
	}
	got := runShimIn(t, root, "recado.ts", Extension(), piRunner, env, 10*time.Second)
	if sends, _ := got["sends"].([]any); len(sends) != 0 {
		t.Errorf("the refusing session received %v, want no turns", sends)
	}
	if status := strings.TrimSpace(run("worker", "pi", "status", short)); status != short+" stored" {
		t.Errorf("status = %q, want the refused backlog stored", status)
	}
	delete(env, "REFUSE")
	got = runShimIn(t, root, "recado.ts", Extension(), piRunner, env, 10*time.Second)
	if sends, _ := got["sends"].([]any); len(sends) != 1 {
		t.Errorf("the retry received %v, want the backlog exactly once", sends)
	}
	if status := strings.TrimSpace(run("worker", "pi", "status", short)); status != short+" queued" {
		t.Errorf("status = %q, want the accepted backlog queued", status)
	}
}

// Pi's extension API swallows a manual-compaction rejection asynchronously.
// A backlog pushed during compaction must remain stored for the next start.
func TestPiAsyncCompactionRefusalRemainsStoredUntilRetry(t *testing.T) {
	requireNode(t)
	root := shimRoot(t)
	bin, run := realRecado(t, root)
	state := filepath.Join(root, "state")
	run("worker", "pi", "hook", "pi", "session-start")
	short := strings.Fields(run("sender", "pi", "send", "worker", "compaction backlog"))[0]
	env := map[string]string{
		"SCENARIO":         "backlog",
		"RECADO_ADDR":      "worker",
		"RECADO_STATE_DIR": state,
		"PATH":             bin + ":/usr/bin:/bin",
		"ASYNC_REFUSE":     "1",
	}
	got := runShimIn(t, root, "recado.ts", Extension(), piRunner, env, 10*time.Second)
	if sends, _ := got["sends"].([]any); len(sends) != 0 {
		t.Errorf("the compacting session received %v, want no turns", sends)
	}
	if status := strings.TrimSpace(run("worker", "pi", "status", short)); status != short+" stored" {
		t.Errorf("status = %q, want the asynchronously refused backlog stored", status)
	}
	delete(env, "ASYNC_REFUSE")
	got = runShimIn(t, root, "recado.ts", Extension(), piRunner, env, 10*time.Second)
	if sends, _ := got["sends"].([]any); len(sends) != 1 {
		t.Errorf("the retry received %v, want the backlog exactly once", sends)
	}
	if status := strings.TrimSpace(run("worker", "pi", "status", short)); status != short+" queued" {
		t.Errorf("status = %q, want the accepted backlog queued", status)
	}
}

// TestPiExtensionAnswersEachPush: recado records a message queued only when the
// session admitted it, so the extension answers every frame, and a frame it
// could not turn into a turn comes back as a refusal recado can retry.
func TestPiExtensionAnswersEachPush(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{"SCENARIO": "ack"}, 60*time.Second)
		if got["reply"] != `{"ok":true}` {
			t.Errorf("the answer to an accepted push = %v, want an acceptance", got["reply"])
		}
		sends, _ := got["sends"].([]any)
		if len(sends) != 1 {
			t.Fatalf("the session took %d messages, want 1", len(sends))
		}
		if msg, _ := sends[0].(map[string]any); msg["text"] != "hello from recado" || msg["deliverAs"] != "steer" {
			t.Errorf("the session was given %v, want the pushed text as a follow-up turn", sends[0])
		}
	})

	t.Run("refused", func(t *testing.T) {
		got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{
			"SCENARIO": "ack",
			"REFUSE":   "1",
		}, 60*time.Second)
		reply, _ := got["reply"].(string)
		if !strings.Contains(reply, `"ok":false`) || !strings.Contains(reply, "not taking a turn") {
			t.Errorf("the answer to a refused push = %v, want a refusal carrying the reason", got["reply"])
		}
	})

	t.Run("compaction refuses before admitting the next push", func(t *testing.T) {
		got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{"SCENARIO": "compaction"}, 10*time.Second)
		if reply, _ := got["refused"].(string); !strings.Contains(reply, `"ok":false`) {
			t.Errorf("the answer during compaction = %v, want a refusal", got["refused"])
		}
		if got["accepted"] != `{"ok":true}` {
			t.Errorf("the answer after compaction = %v, want an acceptance", got["accepted"])
		}
		sends, _ := got["sends"].([]any)
		if len(sends) != 1 {
			t.Fatalf("the session received %v, want only the post-compaction message", sends)
		}
		if msg, _ := sends[0].(map[string]any); msg["text"] != "after compaction" {
			t.Errorf("the session received %v, want the post-compaction message", msg)
		}
	})

	t.Run("busy turn accepts a follow-up", func(t *testing.T) {
		got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{"SCENARIO": "busy"}, 10*time.Second)
		if got["reply"] != `{"ok":true}` {
			t.Errorf("the answer while the agent is busy = %v, want an acceptance", got["reply"])
		}
		if sends, _ := got["sends"].([]any); len(sends) != 1 {
			t.Errorf("the busy session received %v, want one follow-up", sends)
		}
	})

	t.Run("turn-end drain accepts a follow-up", func(t *testing.T) {
		got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{
			"SCENARIO": "busy", "FAKE_RECADO": "turnend",
		}, 10*time.Second)
		sends, _ := got["sends"].([]any)
		if len(sends) != 2 {
			t.Fatalf("the session received %v, want the push and turn-end backlog", sends)
		}
		if msg, _ := sends[1].(map[string]any); msg["text"] != "handed over at turn end" || msg["deliverAs"] != "steer" {
			t.Errorf("the turn-end backlog = %v, want a follow-up", sends[1])
		}
	})

	for _, c := range []struct{ name, env, reason string }{
		{"no model", "NO_MODEL", "no model"},
		{"no authentication", "NO_AUTH", "no configured model authentication"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{
				"SCENARIO": "ack", c.env: "1",
			}, 10*time.Second)
			reply, _ := got["reply"].(string)
			if !strings.Contains(reply, `"ok":false`) || !strings.Contains(reply, c.reason) {
				t.Errorf("the answer = %v, want a refusal explaining %s", got["reply"], c.reason)
			}
			if sends, _ := got["sends"].([]any); len(sends) != 0 {
				t.Errorf("the session received %v, want no turns", sends)
			}
		})
	}
}

// TestPiExtensionHandsOverWhatTheHookCouldNotPush: the messages a hook returns
// as pending have to reach the session, because recado no longer pushes them.
func TestPiExtensionHandsOverWhatTheHookCouldNotPush(t *testing.T) {
	got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{
		"SCENARIO":    "pending",
		"FAKE_RECADO": "pending",
	}, 60*time.Second)
	sends, _ := got["sends"].([]any)
	if len(sends) != 1 {
		t.Fatalf("the session took %d messages, want the 1 the hook handed over: %v", len(sends), got["sends"])
	}
	if msg, _ := sends[0].(map[string]any); msg["text"] != "handed over by the hook" {
		t.Errorf("the session was given %v, want the hook's pending message", sends[0])
	}
}

// TestPiExtensionGivesUpOnAWedgeRecado: a hook runs on the thread the human
// types on, so a recado that never answers must cost a notice, not the session.
func TestPiExtensionGivesUpOnAWedgeRecado(t *testing.T) {
	got := runShim(t, "recado.ts", Extension(), piRunner, map[string]string{
		"SCENARIO":    "timeout",
		"FAKE_RECADO": "hang",
	}, 30*time.Second)
	elapsed, _ := got["elapsed"].(float64)
	if elapsed > 15_000 {
		t.Errorf("a wedge recado held the session for %.0fms, want the hook bounded", elapsed)
	}
	if errors, _ := got["errors"].([]any); len(errors) == 0 {
		t.Error("a wedge recado left no notice for the human")
	}
}

// TestShimsEnforceTheRunDirectoryMode: Node ignores the mode of a directory that
// already exists, and recado promises its state directories are 0700, so a run/
// left at 0755 by an earlier install has to be corrected.
func TestShimsEnforceTheRunDirectoryMode(t *testing.T) {
	cases := []struct {
		name, shim, runner string
		content            []byte
	}{
		{"pi", "recado.ts", piRunner, Extension()},
		{"opencode", "recado-plugin.js", openCodeRunner, Plugin()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := shimRoot(t)
			run := filepath.Join(root, "state", "run")
			if err := os.MkdirAll(run, 0o755); err != nil {
				t.Fatal(err)
			}
			// MkdirAll's mode is subject to the umask, so set it explicitly.
			if err := os.Chmod(run, 0o755); err != nil {
				t.Fatal(err)
			}
			got := runShimIn(t, root, c.shim, c.content, c.runner, map[string]string{
				"SCENARIO":         "dir",
				"RECADO_STATE_DIR": filepath.Join(root, "state"),
			}, 60*time.Second)
			if got["mode"] != "700" {
				t.Errorf("run/ mode = %v, want 700", got["mode"])
			}
		})
	}
}
