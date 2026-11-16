# No background daemon

recado runs no long-lived service of its own. A send stores the message and
pushes it if the recipient can take it now; everything else happens inside a
process that already exists: the harness shim delivers what is still stored at
the end of a turn and at session start, and the relay lives only while a local
agent that sent to a remote machine is still alive. A daemon would make
delivery simpler to reason about, but it must be installed, started, upgraded
and kept alive on every machine, including containers and machines reached
only over SSH, and a dead daemon silently stops all delivery. We accepted
per-harness shims and file locking as the price of having nothing to keep
running.
