# Project Conventions for AI Agents

## Working Agreement

### 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask. This applies even when the confusion seems mild ("a bit confusing") or when you can imagine a reasonable resolution. Ambiguity that the agent silently resolves is a class of bug; ambiguity that the user resolves is not.

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

Read the project Makefile to discover available targets before running any Go toolchain commands directly.

### Gotchas

- Go is managed by asdf. The `go` binary resolves through `~/.asdf/shims/go`.
- **Architecture doc is the spec.** `docs/architecture.md` (~3600 lines) defines every entity, state machine, algorithm, and validation rule. Read the relevant section before implementing anything. Drift from the spec is a bug. The digest verson `docs/architecture-digest.md` is a 2-page map of the system for AI agents. Read it first, then consult the full doc as needed.
- **Symphony is prior art, not a template.** Sortie derives from OpenAI Symphony but diverges intentionally (Go instead of Elixir, SQLite persistence, adapter interfaces). Do NOT copy Symphony patterns or Elixir idioms.
- **Workspace safety invariants are security boundaries.** Path containment under workspace root, sanitized workspace keys (`[A-Za-z0-9._-]` only), and cwd validation before agent launch are mandatory — not suggestions. See architecture Section 9.6.
- **Generic naming in core code.** Use `agent_*`, `tracker_*`, `session_*` in orchestrator core. Never `jira_*`, `claude_*`, `codex_*` outside their adapter packages.
- **Integration tests are env-gated.** Each adapter has its own `SORTIE_<ADAPTER>_TEST=1` gate (e.g. `SORTIE_JIRA_TEST`, `SORTIE_LINEAR_TEST`, `SORTIE_CODEX_TEST`). GitHub end-to-end orchestrator tests also gate on `SORTIE_GITHUB_E2E=1` and require `SORTIE_GITHUB_TOKEN` and `SORTIE_GITHUB_PROJECT`. Without the gate set, integration tests must skip cleanly — never fail.
- **SQLite library is `modernc.org/sqlite` only.** Never `mattn/go-sqlite3` — CGo breaks the single-binary zero-dependency deployment model.
- **`internal/scm/` is an adapter family.** Apply the same boundary rules as `internal/tracker/*/`: no cross-adapter imports, no orchestrator imports, normalize external responses to domain types at the boundary. The coder agent's layer constraints enumerate trackers and agents but omit SCM — treat that gap as a drafting bug, not permission.
- **Orchestrator and `cmd/sortie` reach adapters via `internal/registry`.** Direct imports of `internal/tracker/<kind>` or `internal/agent/<kind>` from these layers are layering violations even though `go build` accepts them.
- **Shared adapter helpers go in `internal/httpkit`, `internal/issuekit`, `internal/trackermetrics`, `internal/typeutil`, `internal/registry`.** Do not duplicate the helper per adapter, and do not push it into `internal/domain/`.
- **`workflow.Manager.Reload()` is fail-safe.** On parse or validation error the previous `currentConfig` and `currentPrompt` are retained and `LastLoadError()` reports the failure. Preserve this invariant when modifying the loader; never `os.Exit` on a bad WORKFLOW.md.
- **Prompt templates render with `Option("missingkey=error")`.** Adding a new template variable without wiring the corresponding data field is a runtime error, not an empty string. Update the data map at the same time you add the variable.

### Boundaries

#### Always

- Read the relevant architecture doc section before implementing a feature.
- Implement adapter integrations as new packages behind the existing Go interface — additive only.
- Produce a statically-linked single binary with zero runtime dependencies.

#### Ask first

- Any change to `docs/decisions/*.md`.
- Adding dependencies beyond what the architecture specifies.

#### Never

- Modify accepted ADRs in `docs/decisions/*.md` without explicit instruction.
- Use CGo or any library requiring a C toolchain.
- Put integration-specific logic (Jira field names, Claude Code CLI flags) in orchestrator core packages.
- Weaken workspace path containment or sanitization rules.
- Do not reference `docs/architecture.md`, `docs/decisions/*.md`, section numbers, ADR numbers, or ticket IDs in any comment — godoc or inline. Those belong in specs and plans, not in source files.
- NEVER prefix commands with `GOPATH=...`, `GOMODCACHE=...`, or any Go environment overrides. The asdf shim configures everything.
- NEVER use `/usr/local/go/bin/go`, `/usr/bin/go`, or any absolute path to a Go binary.
- NEVER downgrade the `go` directive in `go.mod`. NEVER add or modify `toolchain` directives in `go.mod` unless explicitly asked.

## Reference docs

Consult these on demand for the area you are working on, not as a blanket prerequisite to read upfront:

- `docs/architecture-digest.md` - a 2-page map of the system for AI agents. Read this document as your first reference during specification, planning, and review. Open the full `docs/architecture.md` only when the feature you are working on touches one of the areas flagged in the "deep-read" section at the bottom.
- `docs/architecture.md` - the full specification (~3600 lines). Read the section that covers the entity, state machine, algorithm, or contract you are about to change. Do not read the document end-to-end before starting work.
- `docs/decisions/*.md` - accepted ADRs. Read when discussing or revising a prior design choice.
- `docs/workflow-reference.md` - WORKFLOW.md syntax reference.
- `docs/*-adapter-notes.md` - Adapter Research Notes with API details, response examples, and implementation tips for each integration. Read the relevant file when working on an adapter integration.
