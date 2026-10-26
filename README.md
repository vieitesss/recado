# recado

Carry messages between coding agents across harnesses (Claude Code, Codex,
Pi, ...), terminal multiplexers (Rex, tmux, ...) and machines, without a
background daemon. See [docs/design.md](docs/design.md).

Early development: only the agent skill exists so far.

```sh
GOPRIVATE='github.com/vieitesss/*' go install github.com/vieitesss/recado/cmd/recado@latest
recado skill setup    # install the skill into Claude Code, Codex, OpenCode, Pi
recado skill          # print the guide agents follow
```
