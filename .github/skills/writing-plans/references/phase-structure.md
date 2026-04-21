# Phase Structure — Dependency-Ordered Plan Layout

## Contents

1. [The Eight Phases](#the-eight-phases)
2. [Dependency Graph](#dependency-graph)
3. [Phase 1 — Domain Model](#phase-1--domain-model)
4. [Phase 2 — Configuration Layer](#phase-2--configuration-layer)
5. [Phase 3 — Persistence Layer](#phase-3--persistence-layer)
6. [Phase 4 — Integration Adapters](#phase-4--integration-adapters)
7. [Phase 5 — Workspace Manager](#phase-5--workspace-manager)
8. [Phase 6 — Orchestrator](#phase-6--orchestrator)
9. [Phase 7 — CLI and Observability](#phase-7--cli-and-observability)
10. [Phase 8 — Verification and Cleanup](#phase-8--verification-and-cleanup)
11. [Milestone Sequencing](#milestone-sequencing)
12. [Partial-Scope Plans](#partial-scope-plans)

---

## The Eight Phases

Every plan organizes work into a subset of these eight phases, in this order. Phases map one-to-one to the project's layered architecture (Section 3.2 of the architecture doc) plus two bookend phases (CLI/Observability at the top, Verification at the end).

Only include phases that are actually touched. A spec that modifies one interface produces a two-phase plan (Domain + Verification). A spec that threads through every layer produces an eight-phase plan. The number of phases is driven by the work, not by the template.

Each phase in the plan uses a header of the form:

```markdown
## Phase N: <Phase Name>

*<One-sentence description of what this phase produces.>*

- [ ] <step 1>
- [ ] <step 2>
- ...
- [ ] **Constraint Check:** <layer-boundary assertion>
```

The italic description is not decorative — it is the phase's contract. If a step inside the phase does not contribute to producing what the description promises, it belongs in a different phase.

---

## Dependency Graph

```
Phase 1: Domain
    └─> Phase 2: Configuration
            └─> Phase 3: Persistence
                    └─> Phase 4: Integration Adapters
                            └─> Phase 5: Workspace Manager
                                    └─> Phase 6: Orchestrator
                                            └─> Phase 7: CLI & Observability
                                                    └─> Phase 8: Verification
```

Imports flow downward only. A package in Phase N may import from Phases 1 through N-1, never from N+1 or above. The orchestrator imports the domain; the domain does not import the orchestrator.

Phase 8 is not an architectural layer — it is the verification gate that confirms the work built on top of every prior phase still passes `make lint`, `make test`, and `make build`.

---

## Phase 1 — Domain Model

**Purpose:** Pure type definitions — Go interfaces, normalized entity structs, error categories. Zero external dependencies, zero business logic.

**Packages:**
- `internal/domain/` — everything in one place. Entities, adapter interfaces, error types.

**Typical steps:**
- Define entity structs with field types per the architecture doc.
- Define adapter interface method signatures (no bodies, no default implementations).
- Define normalized error categories (`Kind` values, whether each is retryable).

**Allowed signatures in plan text:**
```go
type TrackerAdapter interface {
    FetchIssueByID(ctx context.Context, id string) (*Issue, error)
    // other methods with short doc comments
}

type Issue struct {
    ID         string
    Identifier string
    State      IssueState
}
```

**Constraint Check:** No import of any package outside `internal/domain/`. Domain types are pure data — no methods with side effects, no database handles, no adapter imports. Validate by mental grep: the domain package, built in isolation, must still compile against `go.mod` with every other `internal/*` package removed.

---

## Phase 2 — Configuration Layer

**Purpose:** Typed config structs, YAML parsing, environment variable resolution, path expansion, defaults, and validation.

**Packages:**
- `internal/workflow/` — WORKFLOW.md front matter parsing and prompt body split, filesystem watcher for dynamic reload.
- `internal/config/` — typed config structs, `$VAR` resolution, `~` path expansion, prompt template rendering.

**Typical steps:**
- Implement workflow file loader (front-matter + prompt body split).
- Implement filesystem watcher for dynamic reload. Invalid reloads keep last-known-good config — never crash.
- Implement typed config structs with defaults.
- Implement `$VAR` resolution and `~` path expansion.
- Implement prompt template rendering with strict mode (unknown variables raise an error).

**Constraint Check:** Config layer accepts `map[string]any` from workflow loader, returns typed config. Must not import orchestrator, persistence, adapter, or workspace packages. Workflow reload failures must not crash the process.

---

## Phase 3 — Persistence Layer

**Purpose:** SQLite schema, migrations, and CRUD operations. Uses `modernc.org/sqlite` (pure Go) exclusively — never `mattn/go-sqlite3` or any CGo driver.

**Packages:**
- `internal/persistence/` — store, migrations, CRUD per table.

**Typical steps:**
- Implement schema migration runner with `schema_migrations` tracking table.
- Create a new migration file: e.g. `retry_entries`, `run_history`, `session_metadata`, `aggregate_metrics`.
- Implement CRUD methods for the table (`Save<Entity>`, `Load<Entity>`, `Update<Entity>`, `Delete<Entity>`).
- Implement startup recovery: load persisted entries, reconstruct derived state from timestamps.

**Constraint Check:** SQLite in WAL mode, single-writer only. `modernc.org/sqlite` is the only allowed driver (pure Go — CGo breaks the single-binary deployment model). No concurrent write paths, no transaction nesting. Empty-result queries return empty non-nil slices, never `nil`.

---

## Phase 4 — Integration Adapters

**Purpose:** Tracker and agent adapter implementations. Each adapter is a separate package behind the domain interface. Additive only — no core changes.

**Packages:**
- `internal/tracker/file/` — file-based tracker for dev/test.
- `internal/tracker/jira/` — Jira HTTP client, JQL queries, response normalization.
- `internal/tracker/github/` — GitHub Issues/Projects v2 adapter.
- `internal/agent/mock/` — canned events, configurable outcomes.
- `internal/agent/claude/` — Claude Code subprocess, stdio parsing, event normalization.
- `internal/agent/codex/` — Codex CLI adapter.
- `internal/agent/copilot/` — GitHub Copilot CLI adapter.

**Typical steps:**
- Implement the adapter constructor and config struct.
- Implement each domain interface method. For HTTP adapters: one method per endpoint, plus a shared request/response normalization pass.
- Implement error normalization: map adapter-specific errors into domain error `Kind` values.
- Add integration test skeleton guarded by the adapter's env var (`SORTIE_JIRA_TEST=1`, `SORTIE_GITHUB_TEST=1`, `SORTIE_CLAUDE_TEST=1`, `SORTIE_COPILOT_TEST=1`). Integration tests must skip cleanly without the env var — never fail.

**Isolation Rule:** Adapter packages must NOT import from `internal/orchestrator/`, `internal/workspace/`, or other adapter packages. All data flows through `internal/domain` types at the boundary. Integration-specific identifiers (`jira_*`, `claude_*`, `codex_*`, `copilot_*`) live only inside their adapter package — never in core.

---

## Phase 5 — Workspace Manager

**Purpose:** Filesystem workspace lifecycle — path computation, sanitization, containment validation, creation and reuse, hook execution.

**Packages:**
- `internal/workspace/` — path, sanitization, creation, hooks.

**Typical steps:**
- Implement workspace key sanitization — only `[A-Za-z0-9._-]` allowed in directory names.
- Implement workspace path computation with root-containment check.
- Implement workspace creation and reuse logic.
- Implement hook execution — shell scripts with timeout, env var injection, output truncation.
- Implement the full lifecycle: `after_create` → `before_run` → `after_run` → `before_remove`.

**Constraint Check:** Path containment is a **security boundary**, not a best-effort. The computed workspace path MUST be under the workspace root, verified by absolute path prefix. Reject symlink escapes. Agent launch MUST pass `cwd == workspace_path`, validated before `exec`. These invariants are non-negotiable — if the plan weakens any of them, reject the plan.

---

## Phase 6 — Orchestrator

**Purpose:** The coordination core — poll loop, dispatch, reconciliation, retry scheduling, state machine transitions. Single-authority mutable state.

**Packages:**
- `internal/orchestrator/` — runtime state, dispatch, worker attempt, reconciliation.

**Typical steps:**
- Implement runtime state struct (`running`, `claimed`, `retry_attempts`, `completed`, `agent_totals`).
- Implement poll-and-dispatch tick (reads tracker → filters candidates → dispatches up to slot capacity).
- Implement candidate selection and dispatch sort order.
- Implement active-run reconciliation (stall detection + tracker state refresh).
- Implement worker attempt: workspace → prompt → agent session → turn loop.
- Implement worker exit and retry scheduling with exponential backoff.
- Implement startup recovery sequence (load persisted retries, reconstruct timers).
- Implement terminal-workspace cleanup on startup.

**Isolation Rule:** The orchestrator is the **single authority** for all scheduling-state mutations. No other package writes to `running`, `claimed`, or `retry_attempts`. All adapter and workspace results flow back as return values or channel messages — never as callbacks that mutate state directly.

---

## Phase 7 — CLI and Observability

**Purpose:** Entry point, structured logging, optional HTTP server, metrics.

**Packages:**
- `cmd/sortie/main.go` — entry point, positional workflow arg, CLI flags, startup validation.
- `internal/logging/` — structured logging with `issue_id`, `issue_identifier`, `session_id` context fields.
- `internal/httpapi/` — HTTP server with `/`, `/api/v1/state`, `/api/v1/<issue>`, `/api/v1/refresh`.
- `internal/metrics/` — Prometheus counters, gauges, histograms.

**Typical steps:**
- Wire new CLI flags or positional args, with defaults and validation at startup.
- Propagate context fields through `slog` handlers.
- Add runtime snapshot functions for monitoring.
- Add HTTP handlers — extensions, not required for spec conformance.

**Constraint Check:** Observability failures must never crash the orchestrator. Log sink failures are warnings, not fatals. A broken metrics backend degrades to no-op, it does not halt work.

---

## Phase 8 — Verification and Cleanup

**Purpose:** Confirm the plan's cumulative output compiles, lints, and tests green.

**Typical steps:**
- [ ] Run `make lint` — all packages pass with zero warnings.
- [ ] Run `make test` — all tests pass with `-race`.
- [ ] Run `make build` — produces a single static binary.
- [ ] Manual smoke check: `go run ./cmd/sortie /path/to/WORKFLOW.md` starts, validates config, enters poll loop.
- [ ] Confirm integration tests skip cleanly without their env-var guard variable.

**Note:** Phase 8 does not list `make fmt` because formatting is enforced by pre-commit hooks — if formatting breaks, earlier phases failed their own verify conditions. Do not pad Phase 8 with commands that should have been enforced upstream.

---

## Milestone Sequencing

The project ships in milestones (M1, M2, …). A plan must respect milestone order: steps from Milestone N cannot depend on work from an incomplete Milestone N-1. If a spec sits on top of unbuilt foundations, flag it at Step 1 (spec analysis) and either (a) add the prerequisite work to the plan, (b) split the plan, or (c) stop and ask the user to finish the foundation first.

Do not smuggle prerequisite work into a plan under another milestone. The milestone is the scheduling contract, not a filing category.

---

## Partial-Scope Plans

Not every plan touches every phase. Three common shapes:

| Plan shape                           | Phases used                  |
|--------------------------------------|------------------------------|
| Single adapter method addition       | 1, 4, 8                      |
| CLI flag and config wiring           | 2, 7, 8                      |
| New persistence table + CRUD         | 1, 3, 8                      |
| Full end-to-end feature              | 1–8                          |
| Pure reconciliation refactor         | 6, 8                         |
| Orchestrator state-machine extension | 1, 6, 8                      |

Name phases by their purpose, not by their position. A plan with only Domain and Verification writes "Phase 1: Domain Model" and "Phase 2: Verification & Cleanup" — the user does not need to know these correspond to positions 1 and 8 in the canonical order.
