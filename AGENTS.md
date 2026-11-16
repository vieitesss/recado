# AGENTS.md

- Gate: `scripts/check` (gofmt, vet, staticcheck, shuffled tests). Pre-commit hook: `git config core.hooksPath .githooks` once per clone.
- Vocabulary: `GLOSSARY.md`. Use its terms in code, docs and messages.
- Decisions and why: `docs/adr/`. Read before changing delivery, harness adapters or machine routing; add one for a decision that is hard to reverse and surprising.
- The agent guide printed by `recado skill` lives in `internal/skill/guide.md`; document a command there only once it exists. Bump `stubVersion` in `internal/skill/skill.go` whenever the installed stub's text changes.
- Releases: pushing a `v*` tag runs goreleaser (`.goreleaser.yaml`). `install.sh` depends on its asset names.
