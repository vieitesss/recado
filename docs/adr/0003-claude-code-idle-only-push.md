# Claude Code is pushed to only while idle

Claude Code's messaging socket is undocumented, may break on Claude Code
updates, and delivers a message into the running turn instead of queueing it.
It also acknowledges nothing, so recado cannot tell a message it admitted from
one it dropped. recado therefore pushes to a Claude Code session only while its
hooks report it idle, and at the end of every turn the `Stop` hook claims
whatever is still stored and returns it as the decision that starts the next
turn (`recado hook claude turn-end --hold`). The hook path is the one recado
relies on; the socket only wakes an idle session. For Claude Code, `queued`
means "written to the socket", not "a turn started".
