# Mandatory Protocol and Critical Guidelines for AI Agents

## Protocol

### 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

### 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

### 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it - don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

### 4. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

## Guidelines

### Commands

- `make fmt` — format all Go source files.
- `make lint` — run `golangci-lint` (must be installed separately).
- `make test` — run all tests with `-race`.
- `make build` — compile the `sortie` binary into the repo root.
- `make clean` — remove the compiled binary.

### Gotchas

- **Architecture doc is the spec.** `docs/architecture.md` (~3600 lines) defines every entity, state machine, algorithm, and validation rule. Read the relevant section before implementing anything. Drift from the spec is a bug.
- **Symphony is prior art, not a template.** Sortie derives from OpenAI Symphony but diverges intentionally (Go instead of Elixir, SQLite persistence, adapter interfaces). Do not copy Symphony patterns or Elixir idioms.
- **Workspace safety invariants are security boundaries.** Path containment under workspace root, sanitized workspace keys (`[A-Za-z0-9._-]` only), and cwd validation before agent launch are mandatory — not suggestions. See architecture Section 9.6.
- **Generic naming in core code.** Use `agent_*`, `tracker_*`, `session_*` in orchestrator core. Never `jira_*`, `claude_*`, `codex_*` outside their adapter packages.
- **Integration tests are env-gated.** `SORTIE_JIRA_TEST=1` for Jira, `SORTIE_GITHUB_TEST=1` for GitHub adapter integration tests, `SORTIE_GITHUB_E2E=1` for GitHub E2E orchestrator tests (also requires `SORTIE_GITHUB_TOKEN` and `SORTIE_GITHUB_PROJECT`), `SORTIE_CLAUDE_TEST=1` for Claude Code, `SORTIE_COPILOT_TEST=1` for Copilot. Without these vars, integration tests must skip cleanly — never fail.
- **SQLite library is `modernc.org/sqlite` only.** Never `mattn/go-sqlite3` — CGo breaks the single-binary zero-dependency deployment model.

### Boundaries

#### Always

- Read the relevant architecture doc section before implementing a feature.
- Implement adapter integrations as new packages behind the existing Go interface — additive only.
- Produce a statically-linked single binary with zero runtime dependencies.

#### Ask first

- Any change to `docs/architecture.md` or `docs/decisions/*.md`.
- Adding dependencies beyond what the architecture specifies.

#### Never

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

Last updated: 2026-04-16

Maintained by: AI Agents under human supervision
