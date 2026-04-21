# Output Style Rules — Do / Don't Reference

## Contents

1. [The Ten Rules at a Glance](#the-ten-rules-at-a-glance)
2. [Rule 1 — No Code Blocks for Implementations](#rule-1--no-code-blocks-for-implementations)
3. [Rule 2 — No Test Implementations](#rule-2--no-test-implementations)
4. [Rule 3 — Signatures Only](#rule-3--signatures-only)
5. [Rule 4 — Logical Steps, Not Code](#rule-4--logical-steps-not-code)
6. [Rule 5 — Explicit File Paths](#rule-5--explicit-file-paths)
7. [Rule 6 — Markdown Checkboxes](#rule-6--markdown-checkboxes)
8. [Rule 7 — Spec Traceability](#rule-7--spec-traceability)
9. [Rule 8 — No Symphony / Elixir / BEAM Patterns](#rule-8--no-symphony--elixir--beam-patterns)
10. [Rule 9 — Generic Naming in Core](#rule-9--generic-naming-in-core)
11. [Rule 10 — No Architecture Doc References in Source Comments](#rule-10--no-architecture-doc-references-in-source-comments)
12. [Anti-Patterns to Reject on Self-Review](#anti-patterns-to-reject-on-self-review)

---

## The Ten Rules at a Glance

| #  | Rule                                                            | Why                                                                                     |
|----|-----------------------------------------------------------------|-----------------------------------------------------------------------------------------|
| 1  | No code blocks for implementations                              | The plan is a contract, not a first draft. Bodies belong to the Developer Agent.        |
| 2  | No test implementations                                         | Tests are written by Tester agent against the plan's verify conditions.                 |
| 3  | Signatures only                                                 | Signatures are part of the design — bodies are not.                                     |
| 4  | Logical steps, not code                                         | Natural-language descriptions survive refactors; `if err != nil` blocks don't.          |
| 5  | Explicit file paths                                             | "Somewhere in `internal/`" forces re-planning during implementation.                    |
| 6  | Markdown checkboxes `- [ ]`                                     | The Developer Agent marks progress against the plan. Other formats break this.          |
| 7  | Spec traceability                                               | Every step names the architecture section it implements, or it's speculative.           |
| 8  | No Symphony / Elixir / BEAM patterns                            | This project diverges intentionally.                                                    |
| 9  | Generic naming in core, specific in adapters                    | Security and layering boundary, not style preference.                                   |
| 10 | No architecture doc refs in source comments                     | Specs live in `docs/`; comments explain code, not provenance.                           |

---

## Rule 1 — No Code Blocks for Implementations

**Do not write:** function bodies, goroutine orchestration, channel wiring, SQL query strings, regex patterns, full struct method implementations, or anything that would compile as-is.

**Why:** The plan's job is to fix the contract (what, where, in what order, verified how). The Developer Agent's job is to write the code against that contract. If the plan already contains the code, there is no contract — there's just code waiting to be pasted, and any drift from the architecture doc is already baked in.

**Allowed exception:** a short inline Go snippet (two to four lines) showing a signature, struct field layout, or a non-obvious type declaration. No function bodies ever.

**Good:**

> - [ ] Implement `Store.LoadRetryEntriesForRecovery` on `internal/persistence/retry.go`.
>   - Signature: `func (s *Store) LoadRetryEntriesForRecovery(ctx context.Context, nowMs int64) ([]PendingRetry, error)`
>   - Logic: load all retry entries, compute `remaining := max(entry.DueAtMs - nowMs, 0)` per entry, return a non-nil slice.
>   - **Verify:** `make test` passes with `-race`.

**Bad (has a function body):**

> ```go
> func (s *Store) LoadRetryEntriesForRecovery(ctx context.Context, nowMs int64) ([]PendingRetry, error) {
>     entries, err := s.LoadRetryEntries(ctx)
>     if err != nil {
>         return nil, fmt.Errorf("load retry entries for recovery: %w", err)
>     }
>     // ...
> }
> ```

---

## Rule 2 — No Test Implementations

The plan mentions tests only in two ways:

1. **Verify conditions.** `**Verify:** go test ./internal/persistence/... -run TestLoadRetry -race` tells the Developer Agent what must pass. It does not say what the test body contains.
2. **Named test additions.** Listing tests to add is fine (e.g. "Add `TestLoadRetryEntriesForRecovery_Mixed` covering past-due, exactly-now, and future entries"). The test body is the Tester agent's work.

Do not write `t.Run`, `assert.Equal`, table-driven test skeletons, or fixture construction in the plan. If the plan contains the test, the Developer Agent will copy it, and the test will mirror the plan's assumptions instead of verifying the code.

---

## Rule 3 — Signatures Only

Signatures are part of the design; bodies are implementation choice. You may write:

- Go interface method signatures (with short doc comments).
- Struct field layouts (fields, types, and one-line doc hints).
- Function signatures when the plan needs to pin a specific contract (parameters, return types).
- Type aliases (`type MilliTimestamp int64`) where the alias is part of the spec.

You may not write:

- Function bodies, including one-liner returns.
- Methods on structs where the body would be more than a single `return` statement.
- SQL query strings (the schema is part of the design, the specific query text is not).
- Regular expressions (name the pattern's intent; the Developer Agent writes the regex).

---

## Rule 4 — Logical Steps, Not Code

When a step requires branching or validation logic, write it as a numbered natural-language list, matching the pseudo-code style of architecture Section 16:

**Good:**

> - [ ] Validate the workspace path before creation.
>   1. Compute the absolute path of the workspace directory.
>   2. Confirm the path is under the workspace root (absolute prefix check).
>   3. If not, return `invalid_workspace_cwd` without attempting to create the directory.
>   4. If a symlink in the parent chain points outside the root, treat it as containment failure.

**Bad:**

> ```go
> absPath, _ := filepath.Abs(workspacePath)
> if !strings.HasPrefix(absPath, workspaceRoot) {
>     return errors.New("invalid workspace cwd")
> }
> ```

The first form survives any refactoring of the containment check; the second form becomes stale the moment the Developer Agent picks a different string-comparison approach.

---

## Rule 5 — Explicit File Paths

Every step that creates, modifies, or deletes a file names the exact path. No "somewhere in `internal/orchestrator/`". If the step touches multiple files, list them.

Use the `internal/` package convention established by the project:

- Domain types: `internal/domain/`
- Configuration: `internal/workflow/`, `internal/config/`
- Persistence: `internal/persistence/`
- Tracker adapters: `internal/tracker/{file,jira,github}/`
- Agent adapters: `internal/agent/{mock,claude,codex,copilot}/`
- Workspace: `internal/workspace/`
- Orchestrator: `internal/orchestrator/`
- CLI entry point: `cmd/sortie/`

When in doubt about whether a file exists, check with `tree -d -L 3 internal/` before writing the step. Inventing directories produces plans that require `mkdir -p` improvisation on first contact with the repo.

---

## Rule 6 — Markdown Checkboxes

Every implementable step uses `- [ ]`. Sub-bullets under a step use regular `-` or indented `- [ ]` if they are themselves independently verifiable.

The Developer Agent marks progress by flipping `- [ ]` to `- [x]`. Any other format (numbered lists, prose paragraphs, tables) breaks this tracking.

**Exceptions** — these use regular bullets or headings, not checkboxes:
- Phase headers (`## Phase N: <name>`)
- Dependency graphs and file-structure summaries
- Risk tables, impact tables, metadata headers
- Rationale or motivation notes inside a step (indented sub-bullets)

---

## Rule 7 — Spec Traceability

Every non-trivial step cites the architecture section it implements, the spec section, or the GitHub issue that motivates it. Speculative steps (no citation) are the most common source of scope creep.

**Citations belong in the plan, not in source comments.** Inline forms:

- "implementing the state transition per architecture Section 7.3"
- "per `.specs/Spec-SORT-42.md` Section 3.2"
- "closes issue #198"

**Do not** copy the architecture section's text into the plan. Cite and summarize the binding part in one sentence. The plan reader can open the architecture doc if they need the full context.

---

## Rule 8 — No Symphony / Elixir / BEAM Patterns

This project is Go. Do not reference OpenAI Symphony, Elixir, BEAM, actor models, OTP supervisors, or any Erlang-lineage vocabulary. The derivation relationship is that Sortie was *inspired* by Symphony — not that it should *mimic* Symphony.

Specifically:
- No GenServer-like patterns ("supervisor tree", "process registry", "mailbox", "let it crash").
- No Elixir idioms ("pipe into", "pattern match on", "case expression").
- No actor metaphors. Goroutines are not actors.

If a spec section you are citing uses one of these terms, translate it into the Go equivalent in the plan's step text.

---

## Rule 9 — Generic Naming in Core

**Core packages** (`internal/orchestrator/`, `internal/workspace/`, `internal/domain/`, `internal/config/`, `internal/persistence/`) use generic names: `agent_*`, `tracker_*`, `session_*`, `workspace_*`.

**Adapter packages** (`internal/tracker/jira/`, `internal/agent/claude/`, `internal/agent/codex/`, `internal/agent/copilot/`, etc.) are the only place where integration-specific identifiers appear: `jira_*`, `claude_*`, `codex_*`, `copilot_*`, `github_*`.

This is a **layering boundary**, not a style preference. Once integration-specific names leak into core, the core depends on the integration and the adapter interface stops being a boundary.

**Good:**

> - [ ] Extend `AgentAdapter.Launch` with an `effortBudget` parameter in `internal/domain/agent.go`.

**Bad:**

> - [ ] Extend `AgentAdapter.Launch` with a `claude_max_turns` parameter in `internal/domain/agent.go`.

---

## Rule 10 — No Architecture Doc References in Source Comments

This is an inversion of Rule 7. Architecture section numbers, ADR numbers, spec file paths, and ticket IDs belong in **plans** and **commit messages**, not in Go source comments — whether godoc or inline.

- In the plan: "per Section 9.6" is correct. Cite freely.
- In the source comment the Developer Agent will write: "Validate path containment under the workspace root." No "Section 9.6", no "ADR-0003", no "fixes #198".

The reason is lifecycle: source comments persist for the lifetime of the code; architecture section numbers renumber; ticket IDs rot. A comment that references a moved section actively misinforms.

**Do not** instruct the Developer Agent to write source comments that cite these references. If a plan step says "add a godoc comment referencing Section 7.3 to `tracker.go`", the plan itself is wrong.

---

## Anti-Patterns to Reject on Self-Review

Scan the draft plan for each of these before Step 6 (writing the file):

1. **Phantom files.** File paths that do not exist and are not created in an earlier step. If `internal/foo/bar.go` is modified in Phase 4, Phase 1 or Phase 4 itself must create it.
2. **Upward imports.** A step in Phase 1 (Domain) that imports from Phase 6 (Orchestrator). This is always a planning bug, never a deliberate design.
3. **Missing verify.** A step without `**Verify:**`. Every checkbox has a verify condition — even if the condition is "the file exists" or "`make build` compiles".
4. **Compound steps.** A single `- [ ]` that contains three independent actions. Split them.
5. **Unscoped steps.** "Add error handling" without naming which function, which error types, and which retry behavior.
6. **Test body smuggling.** A step that lists assertions (`assert.Equal(expected, got)`) inside its bullets. Name the test and its intent; leave the body to Tester.
7. **Adapter leakage into core.** A step in Phase 6 (Orchestrator) that mentions `jira_*` or `claude_*` identifiers.
8. **Oversized steps.** A step that would require more than ~300 lines of code or touches more than three files. Split it — or move the split to Phase 2 (classification).
9. **Empty Constraint Check.** A phase ending in `- [ ] **Constraint Check:** <TBD>` or a one-word assertion. Every constraint check names the specific invariant it enforces.
10. **Plan-as-spec.** The plan restates the architecture section verbatim instead of citing and pointing to concrete work. If a step is 90% text from the architecture doc, it is not a plan step — it is a citation.

A clean plan fails none of these on self-review.
