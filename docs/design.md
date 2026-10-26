# recado design

recado is a library and CLI that carries messages between coding agents
running in different harnesses (Claude Code, Codex, Pi, ...), terminal
multiplexers (Rex, tmux, herdr, ...) and machines, with no background daemon.
A future TUI and the `subagents` skill are both clients of it.

## Why

The `subagents` skill delivers reports by pasting text into the Manager's
pane. That works, but:

- a message pasted mid-turn mixes into the agent's current turn;
- "delivered" means "pasted", not "read";
- the message kind and tag are parsed out of free text;
- nothing structured is left for a TUI to show.

Each host extension (`rex/host.py`, `tmux/host.py`, ...) also reimplements
launch, notify and close.

## Model

- **Address**: a URI that names an agent from any machine, e.g.
  `rex://local/block:…`, `tmux+ssh://rpi/%12`.
- **Envelope**: `{id, from, to, kind, item, stage, axis, body, report, reply_to}`,
  one JSON line per message.
- **Kinds**: `complete`, `question`, `answer`, `followup` (maybe `progress`).
  Communication is hierarchical: Manager ↔ Subagent; peers talk through
  their Manager.
- **Mailbox**: an append-only JSONL inbox per agent, on that agent's machine.
  Sending is appending; the message is durable once written.
- **Wake**: a separate, per-harness step that makes the recipient read its
  inbox. The fallback pastes a short "you have N messages" line into the pane,
  only while the agent is idle. Harness hooks can do better (to verify:
  Claude Code's Stop hook returning `decision: block` with the messages).
- **Host adapters**: launch, close, capture, inject and list panes, per
  multiplexer and transport.

## Agent-facing CLI

`recado send | ask | reply | inbox`, used through each harness's shell tool,
so it works everywhere and fits `--allowedTools` allowlists. The skill
(`recado skill`) documents these commands as they land.

## Daemonless, with one exception

Agents live in the multiplexer, which already runs. The client is stateless.
Cross-machine delivery while no client is open still needs something on the
Manager's machine to pull from the remote mailbox; as in the skill's
`rex-remote` relay, that process is scoped to one Manager and exits with it.

## First target

Rex locally, tmux on a remote machine over SSH: the route `rex-remote.md`
already proves. Port it onto recado first, then build the TUI on the same
adapters. To check: what `rex events` and `rex api` expose.
