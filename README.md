# recado

Messages between coding agents, delivered as their next turn, across harnesses
and machines. No daemon.

**Status:** usable, not stable. The CLI, the envelope format and the state
layout may still change before 1.0; breaking changes are called out in the
release notes.

## Why

- **A message arrives as a turn.** recado hands it to the recipient's harness
  through the harness's own API. Nothing is typed into a terminal, and nothing
  lands in the middle of a running turn: a busy agent gets it when its turn
  ends, an idle one is woken.
- **Send and forget.** `recado send` returning 0 means the message is stored
  and recado owns its delivery. The sender never polls or waits.
- **Any machine you can reach.** An agent on your laptop can talk to one over
  SSH, in a Docker or Podman container, or behind NAT; replies find their way
  back.

## Install

macOS and Linux (Windows via WSL), amd64 and arm64:

```sh
curl -fsSL https://recado.vieitesss.eu/install | sh
recado skill setup
recado --version
```

The script installs to `~/.local/bin` and verifies the download against the
release's `checksums.txt`. Set variables on `sh` to change where it installs or
to pin a release:

```sh
curl -fsSL https://recado.vieitesss.eu/install | RECADO_INSTALL_DIR=~/bin RECADO_VERSION=v0.1.0 sh
```

`recado skill setup` installs the skill agents read and the shim each
receiving harness needs: hooks merged into `~/.claude/settings.json`, an
extension in `~/.pi/agent/extensions/`, a plugin in
`~/.config/opencode/plugins/`. `recado skill status` shows what is installed.
Start a new agent session afterwards so the harness loads them.

Other ways: `go install github.com/vieitesss/recado/cmd/recado@latest`.

## Quick look

Every agent has an address. Inside a supported harness recado derives it from
the session; set `RECADO_ADDR` to choose one yourself.

```sh
recado whoami                       # recado://local/manager-4123
recado agents                       # every registered agent, live or gone
```

`recado agents` also shows each agent's name: the repository or directory its
session started in. You can send to a name instead of an id when it names one
live agent; if several match, recado lists them and exits 2.

Agents use three commands:

```sh
recado send manager-4123 "Login stage is built; tests pass."
recado ask  manager-4123 "Reuse the session helper or add a new one?"
recado reply 7k2q "Reuse the session helper."
```

`ask` does not block: the asking agent ends its turn and the answer arrives as
its next one. The recipient sees each message as a new turn:

```
[recado] from subagent-build-4188 · question (id 7k2q)
Reuse the session helper or add a new one?
reply with: recado reply 7k2q "…"
```

You rarely type these yourself. Agents learn them from the guide `recado
skill` prints; a typical setup is a manager agent that starts subagents and
tells each one which `recado` line to report back with.

## Supported harnesses

| Harness | Receives | Sends | Notes |
| --- | --- | --- | --- |
| Claude Code ≥ 2.1.224 | yes | yes | woken only while idle; otherwise delivered when its turn ends |
| Pi ≥ 1.2.0 | yes | yes | queued after the running tool calls |
| OpenCode ≥ 2.0.0 | yes | yes | queued while busy |
| Codex | no | yes | set `RECADO_ADDR` to give it an address; replies to it fail, so it can report but not `ask` |

Any other program can send: `recado send` is a shell command.

## Rex

recado supports the Rex terminal out of the box, with nothing to configure. An
agent started in Rex records its Rex session and block, and `recado agents`
shows the session's current label:

```sh
recado agents                       # ... rex=login
recado agents --rex-session login   # only the agents in that Rex session
```

`--rex-session` takes a session id or label, and also works with
`--machine`. The label is looked up through `rex ls` when listing, so a renamed
session shows its new label; without `rex` on `PATH` the listing just omits it.

## Across machines

Register a machine with how to run a command there, then address agents on it:

```sh
recado machine add rpi ssh rpi          # NAME KIND TARGET
recado machine add web docker web       # also: podman
recado agents --machine rpi             # the agents running there
recado send rpi/sub-1 "Deploy the staging build."
```

recado must be installed on both ends. Only your side runs commands, so it
works into containers and behind NAT: replies from `rpi` are pulled back by a
relay that runs while a local agent that sent there is alive. The registry is
`${XDG_CONFIG_HOME:-~/.config}/recado/machines.toml`.

## For scripts and tools

The CLI is the interface. Every command takes `--json`; exit codes are `0`
success, `1` failure, `2` usage error or unknown address.

- `RECADO_ADDR` sets the address of the agent you start, so you can hand it
  exact lines to run, e.g.
  `recado send manager-4123 --kind complete --meta report=out.md "summary"`.
- `--kind` is free text (`message` by default; only `question` changes how the
  message is shown). `--meta k=v` attaches string metadata recado passes
  through untouched.
- `recado deliver` reads finished envelopes, one JSON object per line, on
  stdin (wrapped here):

  ```json
  {"v":1,"id":"01JB...","ts":"2026-10-26T10:00:00.123Z",
   "from":"recado://local/manager-4123","to":"recado://local/subagent-build-4188",
   "kind":"question","body":"...","meta":{"item":"login"}}
  ```

- `recado inbox [--peek] --as ID` lists an agent's mailbox and `recado status
  ID` prints a message's delivery state (`stored`, `queued`, `read`). Both are
  for recovery and debugging; delivery never needs them.

`recado <command> --help` documents the rest. Design decisions and their
reasons are in [docs/adr](docs/adr); the vocabulary is in
[GLOSSARY.md](GLOSSARY.md).

## Update and uninstall

```sh
recado update          # install the latest release over this binary, then rerun skill setup
recado skill remove    # remove the skill, shims and hooks
rm "$(command -v recado)"
```

State lives in `${XDG_STATE_HOME:-~/.local/state}/recado` (override with
`RECADO_STATE_DIR`); delete it to forget every agent and message.

## Development

```sh
git config core.hooksPath .githooks   # once per clone
scripts/check                         # gofmt, vet, staticcheck, shuffled tests
```

Pushing a `v*` tag builds a release with goreleaser.

## License

[MIT](LICENSE)
