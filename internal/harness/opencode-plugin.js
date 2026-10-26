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
import { spawnSync } from "node:child_process"
import * as fs from "node:fs"
import * as net from "node:net"
import * as os from "node:os"
import * as path from "node:path"

// stateDir mirrors recado's state root: $RECADO_STATE_DIR, else XDG state.
function stateDir() {
	if (process.env.RECADO_STATE_DIR) return process.env.RECADO_STATE_DIR
	const base = process.env.XDG_STATE_HOME || path.join(os.homedir(), ".local", "state")
	return path.join(base, "recado")
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
		const queued = [] // {agent, text}: messages waiting for a prompt
		let current = "" // session of the shell command being prepared
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
			const run = spawnSync("recado", args, { encoding: "utf8", env, timeout: 10_000 })
			if (run.error || run.status !== 0 || !run.stdout) return undefined
			try {
				return (JSON.parse(run.stdout).pending ?? []).map((msg) => ({ agent: agentFor(sessionID), text: msg.text }))
			} catch {
				return undefined
			}
		}

		// flush submits queued messages as queued prompts, one turn each. A
		// message OpenCode will not admit stays queued for the next event.
		async function flush() {
			if (flushing) return
			flushing = true
			try {
				while (queued.length > 0) {
					const msg = queued[0]
					const sessionID = sessions.get(msg.agent)
					if (!sessionID) {
						console.error(`recado: no OpenCode session for agent ${msg.agent}; dropping its message`)
						queued.shift()
						continue
					}
					try {
						await ctx.session.prompt({ sessionID, text: msg.text, delivery: "queue" })
						queued.shift()
					} catch (err) {
						console.error(`recado: cannot prompt session ${sessionID}: ${err}`)
						return
					}
				}
			} finally {
				flushing = false
			}
		}

		// ensure registers the session once: it records the socket as the
		// agent's endpoint and drains whatever waited in its mailbox.
		function ensure(sessionID) {
			if (!sessionID || registered.has(sessionID)) return
			const pending = hook("session-start", sessionID, sock)
			if (pending === undefined) {
				console.error(`recado: session-start failed for ${sessionID}; is recado on PATH?`)
				return
			}
			registered.add(sessionID)
			queued.push(...pending)
			void flush()
		}

		// report runs a lifecycle hook and queues what the drain could not push.
		function report(sessionID, event) {
			ensure(sessionID)
			const pending = hook(event, sessionID)
			if (pending === undefined) {
				console.error(`recado: hook ${event} failed for ${sessionID}`)
				return
			}
			queued.push(...pending)
			void flush()
		}

		// The shell tool's environment carries the address the agent sends from.
		// tool.execute.before names the session; shell.create.before lets a
		// plugin add environment variables to that shell.
		try {
			await ctx.tool.hook("execute.before", (event) => {
				if (event.tool === "bash" || event.tool === "shell") current = event.sessionID
			})
			await ctx.shell.hook("create.before", (event) => {
				const sessionID = current || (sessions.size === 1 ? sessions.values().next().value : "")
				if (!sessionID) return
				event.env.RECADO_ADDR = agentFor(sessionID)
				event.env.RECADO_HARNESS = "opencode"
			})
		} catch (err) {
			console.error(`recado: cannot install shell env hooks: ${err}`)
		}

		// One socket per plugin instance, mode 0600; the registry records it as
		// each session's endpoint.
		const dir = path.join(stateDir(), "run")
		fs.mkdirSync(dir, { recursive: true, mode: 0o700 })
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
					try {
						const frame = JSON.parse(line)
						if (frame?.agent && typeof frame.text === "string") {
							queued.push({ agent: frame.agent, text: frame.text })
						}
					} catch {}
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
							ensure(sessionID)
							break
						case "session.execution.started":
							ensure(sessionID)
							report(sessionID, "turn-start")
							break
						case "session.execution.succeeded":
						case "session.execution.failed":
						case "session.execution.interrupted":
							report(sessionID, "turn-end")
							break
						case "session.deleted": {
							const agent = owners.get(sessionID)
							owners.delete(sessionID)
							if (agent) sessions.delete(agent)
							registered.delete(sessionID)
							if (current === sessionID) current = ""
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
