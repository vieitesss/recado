# A message arrives as a turn

recado hands a message to the recipient's harness through that harness's own
API, as a new user turn. It never types into a terminal pane and never splices
text into a running turn: a busy agent gets the message after its turn ends,
and an idle one is woken. Typing into a pane (what multiplexer-based tools do)
needs no per-harness support, but it mixes with whatever the human is typing,
lands mid-turn, and only proves the text was pasted, not read. The cost is that
every receiving harness needs an adapter and a shim, so a harness without a way
to start a turn from outside (Codex today) can send but not receive.
