# Sortie

Spec-first, agent-developed orchestration service. The architecture document is the authoritative specification — every implementation detail has been decided there. Do not invent behavior; conform to the spec.

## Commands

- `make fmt` — format all Go source files.
- `make lint` — run `golangci-lint` (must be installed separately).
- `make test` — run all tests with `-race`.
- `make build` — compile the `sortie` binary into the repo root.
- `make clean` — remove the compiled binary.

## Gotchas

- **Architecture doc is the spec.** `docs/architecture.md` (~2600 lines) defines every entity, state machine, algorithm, and validation rule. Read the relevant section before implementing anything. Drift from the spec is a bug.
- **Symphony is prior art, not a template.** Sortie derives from OpenAI Symphony but diverges intentionally (Go instead of Elixir, SQLite persistence, adapter interfaces). Do not copy Symphony patterns or Elixir idioms.
- **Workspace safety invariants are security boundaries.** Path containment under workspace root, sanitized workspace keys (`[A-Za-z0-9._-]` only), and cwd validation before agent launch are mandatory — not suggestions. See architecture Section 9.5.
- **Generic naming in core code.** Use `agent_*`, `tracker_*`, `session_*` in orchestrator core. Never `jira_*`, `claude_*`, `codex_*` outside their adapter packages.
- **Integration tests are env-gated.** `SORTIE_JIRA_TEST=1` for Jira, `SORTIE_GITHUB_TEST=1` for GitHub adapter integration tests, `SORTIE_GITHUB_E2E=1` for GitHub E2E orchestrator tests (also requires `SORTIE_GITHUB_TOKEN` and `SORTIE_GITHUB_PROJECT`), `SORTIE_CLAUDE_TEST=1` for Claude Code. Without these vars, integration tests must skip cleanly — never fail.
- **SQLite library is `modernc.org/sqlite` only.** Never `mattn/go-sqlite3` — CGo breaks the single-binary zero-dependency deployment model.

## Boundaries

### Always

- Read the relevant architecture doc section before implementing a feature.
- Implement adapter integrations as new packages behind the existing Go interface — additive only.
- Produce a statically-linked single binary with zero runtime dependencies.

### Ask first

- Any change to `docs/architecture.md` or `docs/decisions/*.md`.
- Adding dependencies beyond what the architecture specifies.

### Never

- Modify accepted ADRs in `docs/decisions/` without explicit instruction.
- Use CGo or any library requiring a C toolchain.
- Put integration-specific logic (Jira field names, Claude Code CLI flags) in orchestrator core packages.
- Weaken workspace path containment or sanitization rules.
- Edit `LICENSE` or `README.md` without explicit instruction.
- Do not reference `docs/architecture.md`, `docs/decisions/`, section numbers, ADR numbers, or ticket IDs in any comment — godoc or inline. Those belong in specs and plans, not in source files.

## Reference docs

Read whichever of these are relevant before starting work:

- `docs/architecture.md` — the full specification: domain model, state machine, algorithms, adapter contracts, persistence schema, test matrix
- `docs/decisions/` — accepted ADRs for Go runtime, SQLite persistence, and adapter-based extensibility
- `docs/workflow-reference.md` - WORKFLOW.md Syntax Reference

---

Last updated: 2026-03-27

Maintained by: AI Agents under human supervision
