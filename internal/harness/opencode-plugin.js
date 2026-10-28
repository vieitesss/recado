/**
 * recado's OpenCode plugin: turns recado messages into queued user turns and
 * records the session and turn lifecycle with `recado hook opencode ...`.
 *
 * OpenCode runs one plugin instance per location, and one instance sees every
 * session in that location. The plugin therefore listens on one unix socket
 * per process and routes each frame to the session its agent id belongs to.
 * Every prompt it submits uses delivery "queue", so a message sent mid-turn
 * becomes its own turn after the running one and never steers it.
 *
 * Installed by "recado skill setup"; remove it with "recado skill remove".
 */
import { execFile } from "node:child_process"
import * as fs from "node:fs"
import * as net from "node:net"
import * as os from "node:os"
import * as path from "node:path"

// hookTimeout bounds one call to recado, so a wedged CLI cannot stall the
// server that runs every session in this process.
const hookTimeout = 10_000

// stateDir mirrors recado's state root: $RECADO_STATE_DIR, else XDG state.
function stateDir() {
	if (process.env.RECADO_STATE_DIR) return process.env.RECADO_STATE_DIR
	const base = process.env.XDG_STATE_HOME || path.join(os.homedir(), ".local", "state")
	return path.join(base, "recado")
}

// ownedDir creates a directory recado owns and keeps it at 0700: Node ignores
// the mode of a directory that already exists, so the mode is set again after
// the mkdir.
function ownedDir(dir) {
	fs.mkdirSync(dir, { recursive: true, mode: 0o700 })
	fs.chmodSync(dir, 0o700)
}

// deriveID turns an OpenCode session id into recado's agent id charset.
function deriveID(sessionID) {
	const id = sessionID.toLowerCase().replace(/[^a-z0-9._-]+/g, "-").replace(/^[^a-z0-9]+/, "")
	return id || `opencode-${process.pid}`
}

// launcherID is RECADO_ADDR when a launcher set one, as a bare agent id.
function launcherID() {
	const raw = process.env.RECADO_ADDR
	if (!raw) return ""
	const id = raw.slice(raw.lastIndexOf("/") + 1)
	return /^[a-z0-9][a-z0-9._-]*$/.test(id) ? id : ""
}

export default {
	id: "recado",
	async setup(ctx) {
		// sessionID <-> agent id, for every session this plugin has registered.
		const sessions = new Map() // agent id -> session id
		const owners = new Map() // session id -> agent id
		const registered = new Set() // sessions whose session-start hook succeeded
		const queued = [] // {agent, text, reply?}: messages waiting for a prompt
		// shellCalls are the shell tool calls a shell may be created for, in the
		// order their before-hooks ran.
		const shellCalls = [] // {sessionID, callID}
		let flushing = false
		let sock = ""
		let server
		let stopped = false

		// agentFor names the session's agent: the launcher's RECADO_ADDR for the
		// first session, else an id derived from the session id.
		function agentFor(sessionID) {
			const known = owners.get(sessionID)
			if (known) return known
			const id = launcherID()
			const agent = id && !sessions.has(id) ? id : deriveID(sessionID)
			owners.set(sessionID, agent)
			sessions.set(agent, sessionID)
			return agent
		}

		// hook runs one recado hook and returns the messages recado could not
		// push itself. undefined means the hook did not run.
		function hook(event, sessionID, endpoint) {
			const args = ["hook", "opencode", event, "--json"]
			if (endpoint) args.push("--endpoint", endpoint)
			const env = { ...process.env, RECADO_HARNESS: "opencode", RECADO_ADDR: agentFor(sessionID) }
			// A drain can push to this plugin's own socket. The event loop must
			// remain free to acknowledge that push before the hook can exit.
			return new Promise((resolve) => {
				execFile("recado", args, { encoding: "utf8", env, timeout: hookTimeout }, (err, stdout) => {
					if (err || !stdout) return resolve(undefined)
					try {
						resolve((JSON.parse(stdout).pending ?? []).map((msg) => ({ agent: agentFor(sessionID), text: msg.text })))
					} catch {
						resolve(undefined)
					}
				})
			})
		}

		// dropShellCalls forgets a session's shell tool calls: a turn that ends
		// without creating one leaves nothing to attribute a later shell to.
		function dropShellCalls(sessionID) {
			for (let i = shellCalls.length - 1; i >= 0; i--) {
				if (shellCalls[i].sessionID === sessionID) shellCalls.splice(i, 1)
			}
		}

		// flush submits queued messages as queued prompts, in order, one turn
		// each. A message is acknowledged only after session.prompt admitted it.
		// A refused prompt returns its frame to recado's store, which retries it
		// at the next turn end or session start, and it never holds the queue:
		// the frames after it, for this session or any other, still go out.
		async function flush() {
			if (flushing) return
			flushing = true
			try {
				while (queued.length > 0) {
					const msg = queued[0]
					const sessionID = sessions.get(msg.agent)
					if (!sessionID) {
						queued.shift()
						msg.reply?.(false, `no OpenCode session for agent ${msg.agent}`)
						console.error(`recado: no OpenCode session for agent ${msg.agent}; returning its message`)
						continue
					}
					try {
						await ctx.session.prompt({ sessionID, text: msg.text, delivery: "queue" })
					} catch (err) {
						console.error(`recado: cannot prompt session ${sessionID}: ${err}`)
						if (!msg.reply) {
							// The store already claimed this one: it waits for the next event.
							return
						}
						queued.shift()
						msg.reply(false, String(err))
						// Later messages for the same session follow it back to the
						// store, so that session still sees them in order.
						for (let i = queued.length - 1; i >= 0; i--) {
							if (queued[i].agent !== msg.agent) continue
							queued[i].reply?.(false, `waiting behind a refused prompt: ${err}`)
							queued.splice(i, 1)
						}
						continue
					}
					queued.shift()
					msg.reply?.(true)
				}
			} finally {
				flushing = false
			}
		}

		// ensure registers the session once: it records the socket as the
		// agent's endpoint and drains whatever waited in its mailbox.
		async function ensure(sessionID) {
			if (!sessionID || registered.has(sessionID)) return
			const pending = await hook("session-start", sessionID, sock)
			if (pending === undefined) {
				console.error(`recado: session-start failed for ${sessionID}; is recado on PATH?`)
				return
			}
			registered.add(sessionID)
			queued.push(...pending)
			void flush()
		}

		// report runs a lifecycle hook and queues what the drain could not push.
		async function report(sessionID, event) {
			await ensure(sessionID)
			const pending = await hook(event, sessionID)
			if (pending === undefined) {
				console.error(`recado: hook ${event} failed for ${sessionID}`)
				return
			}
			queued.push(...pending)
			void flush()
		}

		// The shell tool's environment carries the address the agent sends from.
		// OpenCode 2.0.1's shell create event carries neither a session id nor a
		// call id, so a shell belongs to a session only while exactly one shell
		// tool call is waiting for one. Two sessions interleaved would otherwise
		// be handed each other's address, so an ambiguous shell gets none and
		// recado reports that it cannot tell who is sending.
		try {
			await ctx.tool.hook("execute.before", (event) => {
				if (event.tool !== "bash" && event.tool !== "shell") return
				shellCalls.push({ sessionID: event.sessionID, callID: event.callID ?? event.id })
			})
			await ctx.tool.hook("execute.after", (event) => {
				const callID = event.callID ?? event.id
				const i = shellCalls.findIndex((call) => call.callID === callID)
				if (i >= 0) shellCalls.splice(i, 1)
			})
			await ctx.shell.hook("create.before", (event) => {
				let owner = ""
				if (shellCalls.length === 1) owner = shellCalls.shift().sessionID
				else if (shellCalls.length === 0 && sessions.size === 1) owner = sessions.values().next().value
				event.env.RECADO_HARNESS = "opencode"
				if (!owner) {
					delete event.env.RECADO_ADDR
					return
				}
				event.env.RECADO_ADDR = agentFor(owner)
			})
		} catch (err) {
			console.error(`recado: cannot install shell env hooks: ${err}`)
		}

		// One socket per plugin instance, mode 0600; the registry records it as
		// each session's endpoint.
		const dir = path.join(stateDir(), "run")
		try {
			ownedDir(dir)
		} catch (err) {
			console.error(`recado: cannot keep ${dir} at 0700: ${err}`)
		}
		const serial = (globalThis.__recadoOpenCodeSockets = (globalThis.__recadoOpenCodeSockets ?? 0) + 1)
		sock = path.join(dir, `opencode-${process.pid}-${serial}.sock`)
		try {
			fs.unlinkSync(sock)
		} catch {}
		server = net.createServer((conn) => {
			conn.on("error", () => {})
			let buf = ""
			conn.on("data", (chunk) => {
				buf += chunk.toString("utf8")
				let nl
				while ((nl = buf.indexOf("\n")) >= 0) {
					const line = buf.slice(0, nl)
					buf = buf.slice(nl + 1)
					if (!line.trim()) continue
					let frame
					try {
						frame = JSON.parse(line)
					} catch {
						frame = undefined
					}
					if (typeof frame?.agent !== "string" || typeof frame.text !== "string") {
						conn.end(JSON.stringify({ ok: false, error: "not a recado frame" }) + "\n")
						continue
					}
					queued.push({
						agent: frame.agent,
						text: frame.text,
						reply: (ok, err) =>
							conn.end(JSON.stringify(ok ? { ok: true } : { ok: false, error: err ?? "refused" }) + "\n"),
					})
				}
				void flush()
			})
		})
		try {
			await new Promise((resolve, reject) => {
				server.once("error", reject)
				server.listen(sock, resolve)
			})
			fs.chmodSync(sock, 0o600)
		} catch (err) {
			console.error(`recado: cannot listen on ${sock}: ${err}`)
			server = undefined
			return
		}

		// Session and turn lifecycle. A session is registered the first time it
		// is seen; turn-start marks it busy, every way a turn can end drains it.
		const controller = new AbortController()
		void (async () => {
			try {
				for await (const event of ctx.event.subscribe({ signal: controller.signal })) {
					const sessionID = event?.data?.sessionID
					if (!sessionID) continue
					switch (event.type) {
						case "session.created":
						case "session.inbox.enqueued":
							await ensure(sessionID)
							break
						case "session.execution.started":
							await report(sessionID, "turn-start")
							break
						case "session.execution.succeeded":
						case "session.execution.failed":
						case "session.execution.interrupted":
							dropShellCalls(sessionID)
							await report(sessionID, "turn-end")
							break
						case "session.deleted": {
							const agent = owners.get(sessionID)
							owners.delete(sessionID)
							if (agent) sessions.delete(agent)
							registered.delete(sessionID)
							dropShellCalls(sessionID)
							break
						}
					}
				}
			} catch (err) {
				if (!stopped) console.error(`recado: event stream failed: ${err}`)
			}
		})()

		return () => {
			stopped = true
			controller.abort()
			server?.close()
			server = undefined
			try {
				fs.unlinkSync(sock)
			} catch {}
		}
	},
}
