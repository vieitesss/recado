# recado

recado carries messages between agents: a Manager and the Subagents it
launched, whichever harness (Claude Code, Codex, Pi, ...) and terminal
multiplexer (Rex, tmux, ...) each runs in, on this machine or over SSH.

## Status

This version has no messaging commands yet. Until it does, keep reporting
the way your prompt tells you to, and do not invent `recado` subcommands.
Run `recado --help` to see what this version offers.

## What is coming

- `recado send`, `recado ask`, `recado reply`: write a message to another
  agent's address.
- `recado inbox`: read the messages waiting for you.

Each command will be documented here once it exists.
