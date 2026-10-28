package harness

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openCodeRunner drives the OpenCode plugin with a mocked OpenCode: one plugin
// instance, its event stream, the session prompt it submits turns through, and
// the tool and shell hooks it installs.
const openCodeRunner = `
import { pathToFileURL } from "node:url";
import * as fs from "node:fs";
import * as net from "node:net";

const plugin = (await import(pathToFileURL(process.env.SHIM).href)).default;
const scenario = process.env.SCENARIO;
const prompts = [];
const refused = new Set((process.env.REFUSE ?? "").split(",").filter(Boolean));
const tools = {};
const shells = {};
const events = [];

const ctx = {
	session: {
		async prompt({ sessionID, text, delivery }) {
			if (refused.has(sessionID)) throw new Error("session " + sessionID + " is busy");
			prompts.push({ sessionID, text, delivery });
		},
	},
	tool: { async hook(name, fn) { tools[name] = fn; } },
	shell: { async hook(name, fn) { shells[name] = fn; } },
	event: {
		async *subscribe({ signal }) {
			for (;;) {
				if (events.length) {
					const event = events.shift();
					yield event.value;
					event.done(); // the plugin has finished handling this event
					continue;
				}
				if (signal.aborted) return;
				await new Promise((resolve) => setTimeout(resolve, 5));
			}
		},
	},
};
const result = (value) => console.log("RESULT " + JSON.stringify(value));
const stop = await plugin.setup(ctx);
const emit = (type, sessionID) => new Promise((done) =>
	events.push({ value: { type, data: { sessionID } }, done }));

// push writes one frame the way recado's OpenCode.Push does and reads the answer.
function push(frame) {
	return new Promise((resolve) => {
		const sock = net.createConnection(process.env.RECADO_STATE_DIR + "/run/opencode-" + process.pid + "-1.sock", () =>
			sock.write(JSON.stringify(frame) + "\n"));
		let buf = "";
		sock.setTimeout(4000, () => sock.destroy());
		sock.on("data", (chunk) => { buf += chunk.toString("utf8"); });
		sock.on("close", () => resolve(buf.trim()));
	});
}

const cases = {
	async backlog() {
		await emit("session.created", "worker");
		const end = Date.now() + 4000;
		while (prompts.length === 0 && Date.now() < end) await new Promise((resolve) => setTimeout(resolve, 10));
		await new Promise((resolve) => setTimeout(resolve, 300));
		result({ prompts });
	},

	async refusedBacklog() {
		await emit("session.created", "worker");
		await emit("session.created", "ses_b");
		result({ b: await push({ agent: "ses_b", text: "for B" }), prompts });
	},

	// Two sessions, two frames: the first prompt is refused and the second must
	// still go out. A refused prompt that stalls the queue starves every other
	// session the plugin serves.
	async flush() {
		await emit("session.created", "ses_a");
		await emit("session.created", "ses_b");
		const [a, b] = await Promise.all([
			push({ agent: "ses_a", text: "for A" }),
			push({ agent: "ses_b", text: "for B" }),
		]);
		result({ a, b, prompts });
	},

	// A shell belongs to the session whose shell tool call is waiting for it.
	// OpenCode 2.0.1's create event names neither, so two waiting calls are
	// ambiguous and must get no address at all.
	async shells() {
		await emit("session.created", "ses_a");
		await emit("session.created", "ses_b");
		tools["execute.before"]({ tool: "bash", sessionID: "ses_a", id: "call1" });
		tools["execute.before"]({ tool: "bash", sessionID: "ses_b", callID: "call2" });
		const ambiguous = { RECADO_ADDR: "launcher", RECADO_HARNESS: "opencode" };
		shells["create.before"]({ command: "recado send", cwd: "/tmp", env: ambiguous });
		tools["execute.after"]({ id: "call1", sessionID: "ses_a" });
		tools["execute.after"]({ callID: "call2", sessionID: "ses_b" });
		tools["execute.before"]({ tool: "bash", sessionID: "ses_a", id: "call3" });
		const single = {};
		shells["create.before"]({ command: "recado send", cwd: "/tmp", env: single });
		result({ ambiguous, single });
	},

	async dir() {
		await emit("session.created", "ses_a");
		result({ mode: (fs.statSync(process.env.RECADO_STATE_DIR + "/run").mode & 0o777).toString(8) });
	},

	async unknown() {
		result({ reply: await push({ agent: "ses_gone", text: "for a session that is gone" }) });
	},
};

await cases[scenario]();
stop();
process.exit(0);
`

// TestOpenCodeSessionStartDeliversBacklogOnce uses the real CLI so a hook's
// push must be acknowledged by the plugin while the hook is still running.
func TestOpenCodeSessionStartDeliversBacklogOnce(t *testing.T) {
	requireNode(t)
	root := shimRoot(t)
	bin, run := realRecado(t, root)
	state := filepath.Join(root, "state")
	run("worker", "opencode", "hook", "opencode", "session-start")
	run("sender", "opencode", "send", "worker", "waiting before start")
	got := runShimIn(t, root, "recado-plugin.js", Plugin(), openCodeRunner, map[string]string{
		"SCENARIO":         "backlog",
		"RECADO_ADDR":      "worker",
		"RECADO_STATE_DIR": state,
		"PATH":             bin + ":/usr/bin:/bin",
	}, 10*time.Second)
	prompts, _ := got["prompts"].([]any)
	if len(prompts) != 1 {
		t.Fatalf("the session received %d turns, want the backlog exactly once: %v", len(prompts), prompts)
	}
	if msg, _ := prompts[0].(map[string]any); !strings.Contains(msg["text"].(string), "waiting before start") {
		t.Errorf("the session received %v, want the backlog", prompts[0])
	}
}

// A refused backlog must return to the store rather than block the plugin's
// queue, including frames addressed to other sessions.
func TestOpenCodeRefusedBacklogRemainsStoredWithoutBlockingOtherSessions(t *testing.T) {
	requireNode(t)
	root := shimRoot(t)
	bin, run := realRecado(t, root)
	state := filepath.Join(root, "state")
	run("worker", "opencode", "hook", "opencode", "session-start")
	short := strings.Fields(run("sender", "opencode", "send", "worker", "refused backlog"))[0]
	got := runShimIn(t, root, "recado-plugin.js", Plugin(), openCodeRunner, map[string]string{
		"SCENARIO":         "refusedBacklog",
		"REFUSE":           "worker",
		"RECADO_ADDR":      "worker",
		"RECADO_STATE_DIR": state,
		"PATH":             bin + ":/usr/bin:/bin",
	}, 10*time.Second)
	if got["b"] != `{"ok":true}` {
		t.Errorf("reply for B = %v, want an acceptance despite worker's refusal", got["b"])
	}
	if status := strings.TrimSpace(run("worker", "opencode", "status", short)); status != short+" stored" {
		t.Errorf("status = %q, want the refused backlog stored", status)
	}
}

// TestOpenCodePluginKeepsDeliveringAfterARefusedPrompt: a prompt OpenCode
// refuses returns to recado's store, and the frames behind it, for this session
// or another, still go out. A refusal used to end the drain and leave every
// later message waiting for a turn that never came.
func TestOpenCodePluginKeepsDeliveringAfterARefusedPrompt(t *testing.T) {
	got := runShim(t, "recado-plugin.js", Plugin(), openCodeRunner, map[string]string{
		"SCENARIO": "flush",
		"REFUSE":   "ses_a",
	}, 60*time.Second)
	if reply, _ := got["a"].(string); !strings.Contains(reply, `"ok":false`) || !strings.Contains(reply, "busy") {
		t.Errorf("the answer to the refused frame = %v, want a refusal carrying the reason", got["a"])
	}
	if got["b"] != `{"ok":true}` {
		t.Errorf("the answer to the frame behind it = %v, want an acceptance", got["b"])
	}
	prompts, _ := got["prompts"].([]any)
	if len(prompts) != 1 {
		t.Fatalf("the sessions were prompted %d times, want only the accepted one: %v", len(prompts), prompts)
	}
	if prompt, _ := prompts[0].(map[string]any); prompt["sessionID"] != "ses_b" || prompt["text"] != "for B" || prompt["delivery"] != "queue" {
		t.Errorf("the session was given %v, want B's text as a queued turn", prompts[0])
	}
}

// TestOpenCodePluginAttributesAShellToItsSession: the shell environment carries
// RECADO_ADDR, so a shell started for one session must not be handed another
// session's address, and a shell whose owner recado cannot tell must get none
// rather than the last session that happened to start a turn.
func TestOpenCodePluginAttributesAShellToItsSession(t *testing.T) {
	got := runShim(t, "recado-plugin.js", Plugin(), openCodeRunner, map[string]string{"SCENARIO": "shells"}, 60*time.Second)
	ambiguous, _ := got["ambiguous"].(map[string]any)
	if addr, ok := ambiguous["RECADO_ADDR"]; ok {
		t.Errorf("an ambiguous shell was given RECADO_ADDR %q, want none", addr)
	}
	single, _ := got["single"].(map[string]any)
	if single["RECADO_ADDR"] != "ses_a" || single["RECADO_HARNESS"] != "opencode" {
		t.Errorf("the shell's environment = %v, want ses_a's address", single)
	}
}

// TestOpenCodePluginRefusesAFrameForAnUnknownSession: a frame for a session the
// plugin does not know is refused, so recado keeps the message stored instead of
// recording a delivery nothing can confirm.
func TestOpenCodePluginRefusesAFrameForAnUnknownSession(t *testing.T) {
	got := runShim(t, "recado-plugin.js", Plugin(), openCodeRunner, map[string]string{"SCENARIO": "unknown"}, 60*time.Second)
	reply, _ := got["reply"].(string)
	if !strings.Contains(reply, `"ok":false`) || !strings.Contains(reply, "ses_gone") {
		t.Errorf("the answer = %v, want a refusal naming the unknown session", got["reply"])
	}
}
