# AGENTS.md

- Gate: `scripts/check` (gofmt, vet, staticcheck, shuffled tests). Pre-commit hook: `git config core.hooksPath .githooks` once per clone.
- Design and decisions so far: `docs/design.md`.
- The agent guide printed by `recado skill` lives in `internal/skill/guide.md`; document a command there only once it exists. Bump `stubVersion` in `internal/skill/skill.go` whenever the installed stub's text changes.
