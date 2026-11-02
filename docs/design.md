# recado design

recado carries messages between coding agents running in different harnesses
(Pi, Claude Code, OpenCode, ...), terminal multiplexers and machines, with no
background daemon. It is a messaging tool only: launching and closing agents
stays with whoever launches them (the `subagents` skill's host extensions, a
future TUI, a script).

## Why

The `subagents` skill delivers reports by typing text into the Manager's
pane. That works, but:

- text typed into a pane mixes with whatever the human is typing there;
- a message pasted mid-turn mixes into the agent's current turn;
- "delivered" means "pasted", not "read";
- the message kind and tag are parsed out of free text;
- nothing structured is left for a TUI to show.

Each host extension (`rex/host.py`, `tmux/host.py`, ...) also reimplements
notify, post and relay.

## Principles

- **A message arrives as a turn.** It reaches the recipient through its
  harness's own API as a new user turn. recado never types into a terminal,
  and never splices a message into a running turn: a busy agent gets it after
  the turn ends; an idle one is woken.
- **Agents send and forget.** `send` returning 0 means the message is stored
  and recado owns its delivery. Agents never poll, wait, or check delivery.
- **Agents use three commands**: `send`, `ask`, `reply`. Everything else is for
  launchers, scripts and the TUI.
- **The CLI is the contract.** Other software shells out to `recado` with
  `--json` and stdin; the Go packages stay `internal/` until the TUI needs a
  library.
- **Daemonless.** Long-lived pieces live inside the harness process (an
  extension, a plugin) or only as long as someone waits on them (the relay).

## Addresses

`recado://<machine>/<agent-id>`

- `<machine>` is `local` or a name from the machine registry (see Machines).
- `<agent-id>` matches `[a-z0-9][a-z0-9._-]*`. It is `RECADO_ADDR` when the
  launcher set it (e.g. `subagent-build-4123`), otherwise derived from the
  harness session id.
- On the command line a bare `<agent-id>` means `recado://local/<agent-id>`,
  and `<machine>/<agent-id>` is accepted too.
- An address names the agent, not its pane: it survives a resumed session or a
  moved pane. The pane is registry metadata.
- `recado whoami` prints the caller's address: `RECADO_ADDR`, else what the
  harness adapter detects.
- `recado+upstream://<host>/<agent-id>` only exists on a remote machine: it
  names an agent on the machine that reached this one (see Return routes).

## Envelope

One JSON object per line:

```json
{"v":1,"id":"01JB...","ts":"2026-10-26T10:00:00.123Z",
 "from":"recado://local/manager-4123","to":"recado://local/subagent-build-4188",
 "kind":"question","body":"...","reply_to":"01JA...",
 "meta":{"item":"login","stage":"build"}}
```

- `id` is a ULID. Humans and agents see its **short id**: the shortest suffix,
  at least 4 lowercase characters, unique within the recipient's mailbox
  (suffix, not prefix, because a ULID's prefix is its timestamp). Every
  command taking an id accepts a unique suffix or the full id.
- `kind` is free text. Well-known kinds: `message` (default), `question`,
  `answer`, `complete`, `followup`. recado gives meaning to `question` only
  (the reply hint below).
- `meta` is a flat string map. The `subagents` conventions are `item`,
  `stage`, `axis` and `report`; recado does not interpret them.
- `v` versions the format.

## Agent-facing commands

```sh
recado send TO "text"  [--kind K] [--meta k=v ...]    # text may be - for stdin
recado ask  TO "question" [--meta k=v ...]             # send --kind question
recado reply ID "text"                                  # answer message ID
```

- `send` and `ask` print the short id and return. `ask` does not block; the
  agent ends its turn and the answer arrives as its next turn. A blocking
  `ask --wait` for scripts and hosts without a wake is not yet implemented.
- `reply ID` sends kind `answer` to the original sender, with `reply_to` set
  and the original `meta` copied. It always names the message: a Manager
  answering two Subagents at once never sends to the wrong one.
- There is no `--from`: the sender is `whoami`.
- A launcher hands the agent the exact lines to run, e.g.
  `recado send manager-4123 --kind complete --meta report=.agents/reports/x.md "summary"`.

A delivered message appears in the recipient's timeline as:

```
[recado] from manager-4123 · question · item=login stage=build (id 7k2q)
<body>
reply with: recado reply 7k2q "…"
```

The reply hint is added to `question` only. Remote senders show as
`rpi/subagent-build-4188`.

## Other commands

For launchers, scripts and the TUI, not documented in the agent guide:

| Command | Does |
| --- | --- |
| `recado deliver` | reads finished envelopes (JSON lines) on stdin and delivers each; the one entry point for remote sends, the relay and other software |
| `recado inbox [--peek] [--all] [--meta k=v] [--as ADDR]` | lists messages for an agent; recovery and debugging, never the normal path |
| `recado status ID` | prints a message's delivery state |
| `recado agents` | lists registered agents, live or gone |
| `recado whoami` | prints the caller's address |
| `recado machine add NAME KIND TARGET`, `rm`, `ls` | manages the machine registry |
| `recado hook HARNESS EVENT [--endpoint PATH]` | called by harness shims; session start records where the harness accepts pushes |
| `recado relay MACHINE`, `recado outbox` | cross-machine plumbing |
| `recado skill [setup\|status\|remove]` | installs the skill stub and harness shims |

Every command takes `--json`. Exit codes: `0` success, `1` failure, `2` usage
error or unknown address.

## State

Everything lives under `${XDG_STATE_HOME:-~/.local/state}/recado`, mode `0700`
for directories and `0600` for files:

```
agents/<id>.json        registry: harness, endpoint, pid, pane, cwd, started,
                        state, state_ts
mail/<id>/inbox.jsonl   append-only envelopes for that agent
mail/<id>/receipts.jsonl  append-only state changes {id, state, ts}
run/pi-<pid>.sock       the Pi extension's socket, while its session lives
run/opencode-<pid>-<n>.sock  the OpenCode plugin's socket, while its process lives
outbox.jsonl            messages for upstream addresses (remote machines only)
relay/<machine>.{lock,cursor,log,senders}
```

Writers append with `O_APPEND` under `flock`, so concurrent senders never
interleave. Messages never carry environment variables or secrets; OpenCode
delivery needs no server password because it goes through the plugin's `0600`
socket.

## Registry

A harness shim registers its agent on session start (`recado hook HARNESS
session-start`), writing `agents/<id>.json`. An entry stays after the agent
exits; liveness is its pid. `send` to an id that was never registered fails
at once (exit 2), so a typo never vanishes silently. Messages to a gone agent
wait in its mailbox and arrive when a session with that id starts again.

`recado hook HARNESS turn-start|turn-end` rewrites the entry atomically
(temp file and rename, `0600`) with `state` (`busy` or `idle`) and `state_ts`
(RFC 3339). An entry without them has never reported a turn, which a harness
that cannot queue mid-turn treats as not idle. A turn-end hook that
finds nothing waiting records the agent idle, including at the end of the
turn recado's own hand-over started (Claude Code's Stop with
`stop_hook_active`): that turn fires no prompt hook, so no later end would
come to settle the agent, and every message sent meanwhile would wait. A
different Stop hook that blocks again can make idle wrong, and a socket push
then enters the running turn.

`cwd` is the directory the session-start hook ran in, a snapshot taken at
registration and not the session's live directory. `recado agents --json`
prints it for scripts and omits the key when an entry has none. It can be
stale or too broad: Claude Code's goes stale after `cd` or entering a
worktree, and OpenCode's is the shared server's directory. Treat it as a hint
and let a person confirm anything chosen from it.

## Delivery

Delivering is one operation, whoever runs it (`send` locally, `deliver` on a
remote machine, the relay):

1. **Store**: append the envelope to the recipient's inbox. The message is
   durable; `send` may return.
2. **Push**: if the recipient is live and its harness can take the message
   now, hand it over through the harness adapter and record `queued`.
   - A harness that queues while busy (Pi `steer`, OpenCode
     `delivery: "queue"`) is pushed to whenever it is live.
   - A harness that cannot (Claude Code's socket delivers mid-turn) is pushed
     to only while idle, as recorded by its shim.
3. **Drain**: anything still `stored` is delivered by the shim at the end of
   the recipient's turn and at session start, so a busy or gone agent still
   gets every message, in order. A refused idle push waits for the next turn
   or session event before it is retried.

The shims recado ships answer every frame with `{"ok":true}` when they accept
it, or `{"ok":false,"error":"…"}` when they refuse, and the push waits for
that answer under a timeout. A refusal or a silence releases the message to
`stored`. OpenCode confirms that its harness admitted the message; Pi's
extension API returns void and hides asynchronous failures, while Claude
Code's socket has no answer at all, so either can report `queued` for a message
that never became a turn.

Claiming a message for push or drain records `queued` under the mailbox lock,
preventing simultaneous claims. A lost acknowledgement can cause a retry of an
already accepted message. States: `stored → queued → read`; `read` is
recorded when the harness reports the turn started, where it can. States are
for the TUI and debugging; senders never see them.

One per-recipient delivery lock covers the whole step: reading the recipient's
state, claiming and pushing. A push goes out only behind any older stored
messages, never overtaking them. The turn-start/turn-end hooks take the same
lock around recording `busy`/`idle`, so a delivery cannot slip across a turn
boundary, and a turn end that finds a backlog drains it before an idle agent
can be left with stored mail. A claim that fails partway records nothing and
leaves the messages `stored`; a push that fails releases only the messages it
did not hand over. A known refusal cannot strand a message. The delivery layer renders
the text (with the recipient's short id) and hands the adapter an
`Outgoing{Envelope, Text}`; adapters never read the store.

## Harness adapters

Each receiving harness needs explicit support: there is no standard for
"start a turn with this text". The logic lives once, in Go; each harness adds
a thin shim, embedded in the binary and installed by `recado skill setup`
with a version stamp. Sending needs nothing per harness: it is a shell
command.

```go
type Outgoing struct {
	Envelope Envelope // the message
	Text     string   // rendered with the recipient's short id
}

type Harness interface {
	Name() string
	Detect(env []string) (agentID string, ok bool) // running inside this harness?
	QueuesWhileBusy() bool                          // safe to push mid-turn?
	Push(agent Agent, msg Outgoing) error           // hand over as a new turn
	Shims() []Shim                                  // files `skill setup` installs
	Check() error                                   // version and install health
}
```

| Harness | Push | Busy / turn end | Shim |
| --- | --- | --- | --- |
| Pi ≥ 1.2 | extension's unix socket → `sendUserMessage(text, {deliverAs: "steer"})` | lands after the running tool calls | TypeScript extension in `~/.pi/agent/extensions/` |
| Claude Code ≥ 2.1.224 | built-in messaging socket (`--messaging-socket-path`), only while idle | `Stop` hook returns `decision: "block"` with pending messages | hooks in `~/.claude/settings.json` calling `recado hook claude …` |
| OpenCode ≥ 2.0 | plugin's unix socket → `session.prompt({delivery: "queue"})` | queues while busy; `session.execution.*` events report turns | plugin in `~/.config/opencode/plugins/` |
| Codex | none: hooks cannot wake an idle session | — | send-only until its app-server path is proven |

Pi refuses pushes while busy outside a known turn, or idle without a model or
configured authentication; the latter can conservatively refuse dynamically
supplied credentials. Its API gives no admission result, so races, input
interceptors or other asynchronous errors can still discard an acknowledged
message.

Claude Code's socket frame is undocumented (recovered from the binary, proven
on 2.1.310), and the socket acknowledges neither admission nor refusal: a
message it discards is indistinguishable from one it took, so `queued` here
means "written", not "a turn started". The `Stop` hook still drains what
stays `stored`. Evidence and sources:
`.agents/reports/harness-delivery-research.md` (local) and the research it
cites.

OpenCode loads the plugin inside its server process, and one instance can see
every session it is allowed to access, so it listens on one socket per
process. The registry records that same socket as each session's endpoint; the
frame names the agent (`{"agent":"<id>","text":"..."}`) and the plugin routes
it to that agent's session. A session's agent id is `RECADO_ADDR` for the first
session the process serves, else the session id lowercased into recado's id
charset. The plugin reports `turn-start` on `session.execution.started` and
`turn-end` on every way an execution can end, and gives its shell tool
`RECADO_ADDR` and `RECADO_HARNESS=opencode` through OpenCode's shell env hook,
so `recado send` finds the right sender even when one process holds several
sessions. That hook carries no session of its own, so a shell belongs to the
session whose shell tool call it is starting; when two sessions have one in
flight the shell gets no address, and `recado send` reports that it cannot tell
who is sending rather than guessing.

An adapter can implement the optional `Hooks` contract for its lifecycle
protocol: it translates hook events, prepares the input and session, chooses
how to handle pending messages, formats its response, and says whether failures
must be non-blocking.
Otherwise the standard shim report is used. The separate `Settings` capability
supplies the hook entries `skill setup` merges into a settings file, recognised
by their command prefix and removed without touching the rest of the file.
Claude's hook input names the session; session start records the socket, token
and process liveness and exports `RECADO_ADDR` through `CLAUDE_ENV_FILE`. Its
turn-end hook runs inside the turn it reports on, so it cannot push:
`recado hook claude turn-end --hold` claims what waits and prints it as the
`decision: "block"` reason, which starts a new turn; a session start has no
channel to the agent at all, so `--store` leaves an unpushable backlog for
that turn end instead of handing it to a shim that only Claude Code would
ignore. The messages a held turn delivered stay `queued`: the continuation
turn fires no prompt hook to report its start.

Claude Code blocks the prompt or the stop when a hook exits `2`, so a recado
failure there answers with exit `1` (non-blocking: the notice reaches the
human, the prompt goes through) instead of its usual usage error. A shim also
bounds every call it makes to `recado`, so a wedged CLI costs a notice rather
than the session; it fails open, because a message that waits for the next
event beats a human who cannot type.

The interface is shaped so a later plugin contract can wrap it: an executable
`recado-harness-<name>` on `PATH` answering `detect`, `push` and `check`, with
its own shim calling `recado hook <name> …`. More harnesses are expected.

## Machines

```sh
recado machine add rpi ssh rpi        # NAME KIND TARGET
recado machine add web docker web
recado machine add db  podman db-dev
recado machine ls
```

The registry is `${XDG_CONFIG_HOME:-~/.config}/recado/machines.toml`, editable
by hand. The kind says how to run a command there; it is a command prefix
(`ssh -o BatchMode=yes rpi`, `docker exec -i web`, `podman exec -i db-dev`), so a new
kind is a few lines, plugin-shaped like harnesses. An unknown machine name
fails with the configured list; there is no silent SSH fallback. recado must
be installed on both ends; `send` says so clearly when it is not.

### Return routes

Only the side that knows the machine runs commands, so the route works into
containers and behind NAT:

- **Out**: sending to `recado://rpi/sub-1` runs `recado deliver` on `rpi`
  with the envelope on stdin, `to` rewritten to `recado://local/sub-1` and
  `from` to `recado+upstream://<this-host>/manager-4123`.
- **Back**: on `rpi`, a message to a `+upstream` address is appended to
  `outbox.jsonl`. The relay pulls it (`recado outbox` over the same
  transport), rewrites `from` to `recado://rpi/sub-1` and `to` to
  `recado://local/manager-4123`, and delivers it locally.

Agents never choose a route: they reply by id, and the id's envelope carries
the right address. An upstream send or reply prints the full lowercase message
id: its recipient's shortest unique suffix cannot be known until the relay
delivers it. Local sends and outward remote sends still print the recipient's
short id.

### Relay

One relay per (this host, remote machine), not per agent: several local
agents talking to the same remote share it. The cursor is the byte offset
immediately after the last handled outbox line; delivery is idempotent by
message id, including if a relay stops between delivery and cursor update.
`relay/<machine>.senders` is an append-only, deduplicated list of local agent
ids that have sent to that machine. An agent is live when its registry pid is
live, regardless of when it sent. The first `send` to a machine starts a relay
if its lock is free. It pulls only messages addressed to this host, survives
dropped connections, and exits once no recorded sender is live and the outbox
has been drained.

`recado outbox --host H --json --after N` emits JSON lines with `offset` (the
next byte offset) and `envelope`, filtering for upstream host H. `N` must be
zero or the byte offset after a complete line, not beyond the end of the file.
With `--follow`, it keeps the connection open and emits
`{"offset":N,"caught_up":true}` after reaching the end of the current complete
lines, initially and on each subsequent poll. A watermark includes skipped lines for other hosts; it lets the relay
advance its cursor and know that the outbox has been drained before exiting.

A later transport may share mailbox files directly (a synced directory, a
bind mount into a container) instead of relaying.

## Clients

- **`subagents` skill**: its launcher sets `RECADO_ADDR` and puts the exact
  `recado send … --kind complete` and `recado ask` lines in the prompt; its
  host extensions keep launch, follow-up and close and drop their notify,
  post and relay code. The old reporting path stays until a full Stage has
  run over recado.
- **TUI** (later): reads the registry, mailboxes and receipts; same adapters.
