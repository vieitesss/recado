# An inherited identity is not yours

A harness's shim exports `RECADO_HARNESS` and `RECADO_ADDR` to everything the
agent spawns, so a shell command that starts another harness hands the child the
parent's identity. The child's hooks then registered the parent's id under the
child's harness and drained the parent's mailbox, and commands the child ran
reported the parent's address.

Two rules keep an agent's identity with the harness that hosts its session:

1. A hook named for harness H ignores `RECADO_ADDR` when `RECADO_HARNESS` names
   another harness. It falls through to H's own detection (Antigravity's
   `ANTIGRAVITY_CONVERSATION_ID`) or to the id H's hook input carries.
   `RECADO_ADDR` without `RECADO_HARNESS`, or with H's own name, still names the
   agent: a launcher keeps the power to choose the address.
2. `recado whoami` asks the harnesses that natively recognise the session
   first, leaving out the harness the environment names. When one of them
   reports the session, its id is the answer; otherwise `RECADO_ADDR` stands.

As a second line of defence, `deliver.Service.Hook` refuses any lifecycle event
naming an agent whose registry entry is live under a different harness. The
glossary gives an agent at most one live session at a time, so a second harness
handling its events is a takeover, not a second session. Refusing leaves the
owning entry and its mailbox untouched and fails the child's hook; for the
harnesses whose hooks fail open (`claude`, `agy`) the child's session carries on
without recado. Once the owning session is gone, the id is free again.

The cost: nesting harnesses in the direction their ids already allow — starting
`pi` from an `agy` shell — identifies that process as `agy`, because
`ANTIGRAVITY_CONVERSATION_ID` is still exported to it. Accepted, because a
harness that natively sees the session is stronger evidence than a variable any
parent can leave behind, and trusting the marker a child cannot verify is the
bug this decision removes.
