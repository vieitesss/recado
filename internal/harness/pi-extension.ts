/**
 * recado's Pi extension: turns recado messages into user turns and reports the
 * session and turn lifecycle to `recado hook pi ...`.
 *
 * Installed by "recado skill setup"; remove it with "recado skill remove".
 */
import { spawnSync } from "node:child_process";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

// stateDir mirrors recado's state root: $RECADO_STATE_DIR, else XDG state.
function stateDir(): string {
	if (process.env.RECADO_STATE_DIR) return process.env.RECADO_STATE_DIR;
	const base = process.env.XDG_STATE_HOME || path.join(os.homedir(), ".local", "state");
	return path.join(base, "recado");
}

// agentID is the launcher's RECADO_ADDR when it set one, else the session id
// lowercased into recado's id charset.
function agentID(ctx: ExtensionContext): string {
	const set = process.env.RECADO_ADDR;
	if (set) return set.slice(set.lastIndexOf("/") + 1);
	const raw = ctx.sessionManager.getSessionId() || `pi-${process.pid}`;
	const id = raw.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "");
	return id || `pi-${process.pid}`;
}

// hook runs one recado hook and returns the messages recado could not push
// itself, or undefined when the hook did not run.
function hook(event: string, sock?: string): string[] | undefined {
	const args = ["hook", "pi", event, "--json"];
	if (sock) args.push("--endpoint", sock);
	const run = spawnSync("recado", args, { encoding: "utf8" });
	if (run.status !== 0) return undefined;
	try {
		return (JSON.parse(run.stdout).pending ?? []).map((msg: { text: string }) => msg.text);
	} catch {
		return undefined;
	}
}

export default function (pi: ExtensionAPI) {
	let sock = "";
	let server: net.Server | undefined;
	const queued: string[] = [];

	// flush hands the queue over, one user turn per message. A message the
	// session cannot take yet stays queued for the next event.
	function flush() {
		while (queued.length > 0) {
			try {
				pi.sendUserMessage(queued[0], { deliverAs: "followUp" });
				queued.shift();
			} catch {
				return;
			}
		}
	}

	// report runs a hook and queues whatever the shim must deliver itself.
	function report(ctx: ExtensionContext, event: string, endpoint?: string) {
		const pending = hook(event, endpoint);
		if (pending === undefined) ctx.ui.notify(`recado: hook ${event} failed; is recado on PATH?`, "error");
		queued.push(...(pending ?? []));
		flush();
	}

	pi.on("session_start", async (_event, ctx) => {
		process.env.RECADO_ADDR = agentID(ctx);
		process.env.RECADO_HARNESS = "pi";
		const dir = path.join(stateDir(), "run");
		fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
		sock = path.join(dir, `pi-${process.pid}.sock`);
		try {
			fs.unlinkSync(sock);
		} catch {}
		server = net.createServer((conn) => {
			conn.on("error", () => {});
			let buf = "";
			conn.on("data", (chunk) => {
				buf += chunk.toString("utf8");
				let nl: number;
				while ((nl = buf.indexOf("\n")) >= 0) {
					const line = buf.slice(0, nl);
					buf = buf.slice(nl + 1);
					if (!line.trim()) continue;
					try {
						queued.push(JSON.parse(line).text);
					} catch {}
				}
				flush();
			});
		});
		try {
			await new Promise<void>((resolve, reject) => {
				server!.on("error", reject);
				server!.listen(sock, resolve);
			});
			fs.chmodSync(sock, 0o600);
		} catch (err) {
			ctx.ui.notify(`recado: cannot listen on ${sock}: ${err}`, "error");
			return;
		}
		report(ctx, "session-start", sock);
	});

	pi.on("agent_start", (_event, ctx) => report(ctx, "turn-start"));
	pi.on("agent_end", (_event, ctx) => report(ctx, "turn-end"));

	pi.on("session_shutdown", () => {
		server?.close();
		server = undefined;
		try {
			fs.unlinkSync(sock);
		} catch {}
	});
}
