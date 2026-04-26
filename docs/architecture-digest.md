# Sortie Architecture — Digest

**Purpose.** A 2-page map of the system for AI agents (Architect, Planner, Reviewer, Coder, Tester). Read this document as your first reference during specification, planning, and review. Open the full [`docs/architecture.md`](architecture.md) only when the feature you are working on touches one of the areas flagged in the "deep-read" section at the bottom.

**Authority.** When the digest and the full spec disagree, `docs/architecture.md` wins. The digest is a lossy summary. Report any drift you notice.

**ADR index:** [`docs/decisions/`](decisions/) — accepted ADRs are architectural law.

## 1. Primary components

- **Workflow Loader.** Parses `WORKFLOW.md` (YAML front matter + prompt body). Returns `{config, prompt_template}`. Live-reload via `fsnotify`.
- **Config Layer.** Typed getters over front matter; defaults, `$VAR` resolution, `~` expansion, validation pre-dispatch.
- **Issue Tracker Client.** Adapter interface (Jira, GitHub today). Fetches candidates, current states, terminal-state cleanup; normalizes to a stable `domain.Issue`.
- **Orchestrator.** Owns the poll tick and the authoritative runtime state, backed by SQLite for durability. Single-writer for `running` / `claimed` / `retry_attempts`. Dispatch, retry, stop, release.
- **Workspace Manager.** Maps issue identifier → sanitized workspace key → workspace path under workspace root. Hooks: `after_create`, `before_run`, `after_run`, `before_remove`.
- **Agent Runner.** Builds prompt from `(issue, workflow_template)`, launches the agent subprocess via the configured agent adapter, relays updates back to the orchestrator. Optional bounded self-review loop after the coding turn loop.
- **Persistence Layer.** Embedded SQLite (`modernc.org/sqlite`, WAL mode, single writer). Retry queues, session metadata, workspace registry, token accounting, run history. Survives process restarts.
- **Status Surface.** Optional HTTP server exposing operator-readable runtime state. Not required for orchestrator correctness.
- **Logging.** Structured logs (`log/slog`) routed to one or more sinks.
- **CI Status Provider.** Read-only single-method (`FetchCIStatus`) adapter. Activated only when workflow front matter requests CI feedback.
- **SCM Adapter.** Read-only multi-method (`FetchPendingReviews`) adapter. Activated only when `reactions.review_comments.provider` is configured.

## 2. Abstraction layers (strict downward dependency)

1. **Policy Layer** — repo-defined `WORKFLOW.md` prompt body and team rules.
2. **Configuration Layer** — typed front-matter getters, defaults, env resolution.
3. **Coordination Layer** — orchestrator: poll loop, eligibility, concurrency, retries, reconciliation.
4. **Execution Layer** — workspace lifecycle and agent subprocess management.
5. **Integration Layer** — tracker adapters, agent adapters, CI status providers, SCM adapters.
6. **Observability Layer** — logs and the status surface.

A layer MUST NOT import from a layer above it. Integration-specific identifiers (`jira_*`, `claude_*`, `codex_*`, `copilot_*`, `github_*`) appear only inside their adapter packages — core code uses generic vocabulary (`agent_*`, `tracker_*`, `session_*`, `workspace_*`).

## 3. Adapter model

New trackers and agents are **new packages behind existing Go interfaces** — additive only, zero changes to core orchestration. Registered via `init()` into `internal/registry`. Core packages import `internal/domain` types, never adapter packages.

Existing adapter dimensions:

- **Tracker adapters** — Jira, GitHub.
- **Agent adapters** — Claude Code, Codex, Copilot.
- **CI status providers** — GitHub Checks (only when `ci_feedback.kind: github` or `reactions.ci_failure.provider: github`).
- **SCM adapters** — GitHub (only when `reactions.review_comments.provider: github`).

## 4. Hard constraints (memory refresh)

These are reproduced here from `AGENTS.md` for quick reference. When in doubt, `AGENTS.md` is authoritative.

- **Single statically-linked binary.** Zero runtime dependencies on the host. No CGo, no external database server, no shared libraries beyond the Go standard library and approved pure-Go modules.
- **SQLite library is `modernc.org/sqlite` only.** Never `mattn/go-sqlite3` (CGo).
- **Workspace path containment.** Workspace path under workspace root, verified by absolute-path prefix check. Symlink escapes rejected.
- **Workspace key sanitization.** Only `[A-Za-z0-9._-]` in directory names. No exceptions.
- **Agent cwd validation.** `cwd == workspace_path` MUST be verified *before* `exec`, not after.
- **Single-writer persistence.** SQLite WAL mode; orchestrator state mutations serialized through one authority.
- **Generic naming in core.** `agent_*`, `tracker_*`, `session_*`, `workspace_*`. Never `jira_*`, `claude_*`, `codex_*`, `copilot_*`, `github_*` outside their adapter packages.
- **Symphony is prior art, not a template.** No Symphony / Elixir / BEAM patterns or vocabulary anywhere.
- **Integration tests are env-gated.** `SORTIE_JIRA_TEST`, `SORTIE_GITHUB_TEST`, `SORTIE_GITHUB_E2E`, `SORTIE_CLAUDE_TEST`, `SORTIE_COPILOT_TEST`. Without the guard variable, the test MUST skip cleanly — never fail.
- **No architecture-doc references in source comments.** `docs/architecture.md`, `docs/decisions/`, section numbers, ADR numbers, and ticket IDs belong in specs, plans, and ADRs — not in `*.go` godoc or inline comments.

## 5. When to deep-read the full spec

Open [`docs/architecture.md`](architecture.md) when your feature touches one of the areas below. Otherwise, this digest plus `AGENTS.md` plus relevant ADRs from `docs/decisions/` is sufficient.

| Section in full spec                                  | Deep-read trigger                                                                                  |
|-------------------------------------------------------|----------------------------------------------------------------------------------------------------|
| §3 System Overview                                    | Adding new top-level components or changing component boundaries                                   |
| §4 Core Domain Model                                  | Adding new domain entities or materially changing field shapes                                     |
| §5 Workflow Specification                             | Changing `WORKFLOW.md` schema, front-matter keys, or prompt-template behavior                      |
| §6 Configuration Specification                        | Adding config fields, defaults, env-var resolution, or validation rules                            |
| §7 Orchestration State Machine                        | Touching `running` / `claimed` / `retry_attempts`, dispatch transitions, or release semantics      |
| §8 Polling, Scheduling, and Reconciliation            | Changing poll cadence, eligibility logic, concurrency caps, or reconciliation rules                |
| §9 Workspace Management and Safety                    | Touching workspace paths, key sanitization, hook execution, or cwd validation (security boundary)  |
| §10 Agent Adapter Contract                            | Adding/modifying agent adapters, session lifecycle, event parsing, or token extraction             |
| §11 Issue Tracker Integration Contract                | Adding/modifying tracker adapters, normalization, pagination, or error categories                  |
| §11A CI Feedback Contract                             | Wiring CI feedback (provider activation, log truncation, normalized result shape)                  |
| §11B PR Review Comment Feedback Contract              | Wiring review-comment feedback (SCM provider activation, pending-review semantics)                 |
| §12 Prompt Construction and Context Assembly          | Changing prompt rendering, FuncMap, or context fields supplied to the template                     |
| §13 Logging, Status, and Observability                | Adding log fields, metrics, status endpoints, or dashboard surfaces                                |
| §14 Failure Model and Recovery Strategy               | Changing retry/backoff, restart-recovery, or failure categorization                                |
| §15 Security and Operational Safety                   | Any trust boundary, secret handling, or operator-confirmation policy change                        |
| §16 Reference Algorithms                              | Implementing one of the algorithms verbatim or proposing a deviation                               |
| §17 Test and Validation Matrix                        | Adding test layers, env-gates, or fixture infrastructure                                           |
| §19 Persistence Schema                                | Adding/altering tables, columns, indexes, or migrations                                            |
| §20 Webhook Support                                   | Wiring webhook ingress (currently future-extension)                                                |
| §21 Agent-Authored Workspace Files                    | Changing how the agent leaves artifacts inside the workspace                                       |
| Appendix A SSH Worker Extension                       | Wiring or modifying the optional SSH-worker remote-execution path                                  |
