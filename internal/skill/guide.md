# recado

recado carries messages between agents, across harnesses and machines. You
send a message; recado stores it and delivers it to the recipient as its next
turn.

## Commands

```sh
recado send TO "text" [--kind K] [--meta k=v ...]   # tell an agent something
recado ask TO "question" [--meta k=v ...]           # ask a question
recado reply ID "text"                              # answer message ID
```

Examples, one per command:

```sh
recado send manager-4123 "Login stage is built; tests pass."
recado ask manager-4123 "Reuse the session helper or add a new one?"
recado reply 7k2q "Reuse the session helper."
```

- `TO` is an agent id, or the name `recado agents` shows for it, such as
  `sb` or `sb-f4`. A bare id or name is on this machine. Addresses look like
  `recado://<machine>/<agent-id>`.
- `recado agents` lists the agents whose session is live; `--all` adds the
  gone ones and `--state idle` keeps the live agents that are idle.
- `recado agents --machine NAME` lists the agents on a configured machine;
  their addresses work in `TO`.
- `text` may be `-` to read the body from stdin.
- `--kind` names the message type; the default is `message`. Use the kind
  the agent or script that started you gives you.
- Every incoming message names its sender and its id, as `(id 7k2q)`. Use
  that id in `recado reply`.

## Rules

- Send and forget. Exit 0 means the message is stored and recado owns its
  delivery: carry on with your work. Do not poll, wait for, or check delivery.
- `ask` does not block. End your turn; the answer arrives as your next turn.
- Answer a recado message only with `recado reply ID "..."`, never with your
  harness's own messaging tool.
- Every reply names the message it answers, by id.
- The agent or script that started you may hand you exact `recado` lines to
  run. Run them verbatim.

## Exit codes

- `0`: stored.
- `1`: recado failed. Report the failure; do not work around it.
- `2`: usage error or unknown address. Fix the command or the address; do
  not retry it unchanged.
