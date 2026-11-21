# Antigravity is pushed to only while idle

Antigravity CLI (`agy`) provides `agentapi send-message <conversationId> <text>`,
which injects a message into the session's language server (using
`ANTIGRAVITY_LS_ADDRESS` and `ANTIGRAVITY_CSRF_TOKEN` captured at
`SessionStart`). When the session is busy, incoming messages are delivered at
the start of the next model invocation inside the running turn rather than
waiting for the turn to end.

To preserve [ADR 0002](0002-messages-arrive-as-turns.md), recado therefore
pushes to an `agy` session only while its hooks report it idle, and holds
messages that arrive mid-turn for the `Stop` hook, which hands them back with
`{"decision": "continue", "reason": "..."}` (`recado hook agy turn-end --hold`).
When `terminationReason` is `USER_CANCELED`, `Stop` records the session idle
without reopening the turn. Because one `antigravity` language-server process
can host multiple conversations (such as built-in subagents), `agy` reports
`SingleSession() == false` so starting a second conversation does not retire
earlier ids sharing the PID.
