# Antigravity is pushed to only while idle

Antigravity CLI (`agy`) provides `agentapi send-message <conversationId> <text>`,
which injects a message into the session's language server using
`ANTIGRAVITY_LS_ADDRESS` and `ANTIGRAVITY_CSRF_TOKEN`. When the session is
busy, incoming messages are delivered at the start of the next model invocation
inside the running turn rather than waiting for the turn to end.

## Idle-only push and Stop hand-over

To preserve [ADR 0002](0002-messages-arrive-as-turns.md), recado pushes to an
`agy` session only while its hooks report it idle, and holds messages that
arrive mid-turn for the `Stop` hook, which hands them back with
`{"decision": "continue", "reason": "..."}` (`recado hook agy turn-end --hold`).
When `terminationReason` is `USER_CANCELED`, `Stop` records the session idle
without reopening the turn. Because one `antigravity` language-server process
can host multiple conversations (such as built-in subagents), `agy` reports
`SingleSession() == false` so starting a second conversation does not retire
earlier ids sharing the PID.

## Passive endpoint capture (why hooks cannot capture it)

As of `agy` 1.5.13, Antigravity passes only `ANTIGRAVITY_CONVERSATION_ID` to
lifecycle hooks (`SessionStart`, `PreInvocation`, `Stop`). It never passes
`ANTIGRAVITY_LS_ADDRESS` or `ANTIGRAVITY_CSRF_TOKEN` to hooks: any values
present in a hook's environment were inherited from a parent `agy` session's
`run_command` and point at the parent's language server. `SessionStart`
therefore leaves the agent's endpoint and token empty.

Those two variables are exported to tool commands (`run_command`) spawned
inside a session. Non-hook `recado` CLI commands (`whoami`, `send`, `ask`,
`reply`, `agents`, and so on) passively capture `ANTIGRAVITY_LS_ADDRESS` and
`ANTIGRAVITY_CSRF_TOKEN` into the live `agy` agent's registry entry
(`agents/<id>.json`), arming idle pushes via `agentapi send-message`. Passive
capture only updates an existing live `agy` entry and refuses to overwrite a
different conversation's already-captured endpoint. Before the session runs its
first non-hook `recado` command, an idle push fails cleanly because no endpoint
is recorded yet; the message stays `stored` and is handed over at the end of
the session's next turn.

## Workspace `cwd` with `--add-dir`

Antigravity invokes hooks with `PWD` set to the `hooks.json` directory, so
`SessionStart` reads the workspace from the hook payload's `cwd` field. When a
session is launched with `--add-dir`, `agy` sorts `workspacePaths` and puts the
sorted-first path in `cwd` rather than the launch directory; the derived agent
name reflects that sorted-first workspace.
