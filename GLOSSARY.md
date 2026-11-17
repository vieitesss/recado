# recado

recado carries messages between coding agents running in different harnesses
and on different machines, delivering each one as the recipient's next turn.

## Participants

**Agent**:
An addressable participant that sends and receives messages. It keeps its
address across sessions.
_Avoid_: session (when you mean the recipient), pane, bot, launcher

**Name**:
An agent's readable label, taken from the repository or directory its session
started in. A hint for people, not part of its address.
_Avoid_: alias, label, nickname

**Session**:
One run of a harness currently hosting an agent. An agent has at most one live
session at a time and may have many over its life.
_Avoid_: agent

**Live / Gone**:
Whether an agent's session is still running. A gone agent keeps its address
and mailbox, and its messages wait until a new session takes it up.
_Avoid_: dead, offline, active

**State**:
What a live agent's harness last reported: busy while a turn runs, idle
between turns. A gone agent has no state.
_Avoid_: status (for liveness)

**Harness**:
The coding-agent program a session runs in, such as Claude Code, Pi or
OpenCode.
_Avoid_: host, runtime, client

**Shim**:
The small piece recado installs into a harness so it can receive messages and
report its turns.
_Avoid_: plugin, extension, hook (each is only one harness's form of a shim)

## Places

**Machine**:
Any place recado runs, named by the side that reaches it. The machine you are
on is `local`.
_Avoid_: host, server, node

**Address**:
The name of an agent on a machine, `recado://<machine>/<agent-id>`.
_Avoid_: endpoint (that is where a session accepts pushes), pane

**Relay**:
The short-lived process that brings messages back from a remote machine to the
agents on this one.
_Avoid_: daemon, proxy

## Messages

**Message**:
One thing an agent sends to another: a body, a kind and optional metadata.
_Avoid_: notification, event

**Envelope**:
The recorded form of a message, as stored and as passed between machines.
_Avoid_: message (in design and launcher docs)

**Kind**:
A message's free-text type; `message` is the default and `question` is the
only kind recado gives meaning to.
_Avoid_: type, tag

**Short id**:
The shortest unique suffix of a message id within the recipient's mailbox,
used to reply.
_Avoid_: prefix, handle

**Mailbox**:
Every message sent to one agent, kept until delivered and afterwards. `inbox`
is only the name of the command that lists it.
_Avoid_: inbox, queue
