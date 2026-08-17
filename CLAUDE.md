# sortie

An agent orchestrator that ships as one statically-linked binary with no runtime dependencies. Most of its structure is enforced by convention rather than by the compiler: `go build` accepts every layering violation listed below.

## Commands

Read the Makefile to discover available targets before running any Go toolchain command directly.

## Gotchas

- **Go resolves through the asdf shim** at `~/.asdf/shims/go`. Overriding `GOPATH`, `GOMODCACHE` or the binary path breaks the pinned toolchain; `GOFLAGS` is not such an override and is allowed.
- **Symphony is prior art, not a template.** Sortie derives from OpenAI Symphony but diverges intentionally. Do not port Symphony patterns or Elixir idioms.
- **Generic naming in core code.** Use `agent_*`, `tracker_*`, `session_*` in orchestrator core. Never `jira_*`, `claude_*`, `codex_*` outside their adapter packages.
- **Orchestrator and `cmd/sortie` reach adapters via `internal/registry`.** Direct imports of a package that registers a kind — under `internal/tracker/`, `internal/agent/`, or `internal/scm/` — from these layers are layering violations even though `go build` accepts them. A package under one of those roots that registers no kind and holds no adapter is not a kind package and may be imported directly.
- **`internal/scm/` is an adapter family.** Apply the same boundary rules as `internal/tracker/*/`: no cross-adapter imports, must not import the orchestrator, normalize external responses to domain types at the boundary. The coder agent's layer constraints enumerate trackers and agents but omit SCM — treat that gap as a drafting bug, not permission.
- **Shared adapter helpers go in an `internal/` package named for the concern it serves**, never duplicated per adapter and never in `internal/domain/`. Logic scoped to one adapter family nests under that family instead.
- **Integration tests are env-gated** by a per-adapter `SORTIE_<ADAPTER>_TEST=1`. Without the gate set they must skip cleanly — never fail.
- **`workflow.Manager.Reload()` is fail-safe.** On parse or validation error the previous `currentConfig` and `currentPrompt` are retained and `LastLoadError()` reports the failure. Preserve that invariant; never `os.Exit` on a bad WORKFLOW.md.
- **Prompt templates render with `Option("missingkey=error")`.** Adding a template variable without wiring its data field is a runtime error, not an empty string.

## Boundaries

### Always

- Read the architecture section your task touches before implementing. Drift from the spec is a bug.
- Implement adapter integrations as new packages behind the existing Go interface — additive only.

### Ask first

- Any change to `docs/decisions/*.md`.
- Adding a dependency beyond what the architecture specifies.

### Never

- Discard, revert, reset, stash, or reformat uncommitted changes outside your current task's file set - the working tree may hold the user's or a parallel agent's work (see the working-agreement rule).
- Use CGo or any library requiring a C toolchain. It breaks the single-binary deployment model.
- Put integration-specific logic (Jira field names, Claude Code CLI flags) in orchestrator core packages.
- Weaken workspace path containment, workspace-key sanitization, or cwd validation before agent launch. These are security boundaries.
- Reference `docs/architecture.md`, `docs/decisions/*.md`, section numbers, ADR numbers, or ticket IDs in any comment, godoc or inline. Those belong in specs and plans.
- Downgrade the `go` directive in `go.mod`, or add or modify `toolchain` directives, unless explicitly asked.

## Reference docs

Consult these for the area you are working on, not as a blanket prerequisite:

- `docs/architecture.md` - the index. A system-at-a-glance plus a routing table mapping each task to the one section it needs. It is a map, not a second source of truth.
- `docs/architecture/NN-<slug>.md` - the specification, one file per section. Open only the section the index routes you to; on conflict the section file wins.
- `docs/decisions/*.md` - accepted ADRs. Read when discussing or revising a prior design choice.
- `docs/workflow-reference.md` - WORKFLOW.md syntax.
- `docs/*-adapter-notes.md` - API details, response examples and implementation tips per integration.
