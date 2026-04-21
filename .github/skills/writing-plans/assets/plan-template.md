# Plan: {Short Title}

<!--
File name: if .specs/Spec-{TASK_NAME}.md is existent, use .plans/Plan-{TASK_NAME}.md; 
otherwise, use the following strategy to derive the name: .plans/Plan-{ID}-{kebab-case-slug}.md
  - Jira ID present:       Plan-SORT-42.md
  - GitHub issue present:  Plan-238-codex-agent-adapter.md
  - Neither present:       Plan-agent-max-turns-passthrough-leak.md
Keep the title under ~60 characters. Title in H1 matches the filename slug
in human-readable form.
-->

**Created at:** {ISO timestamp} \
**Issue ID:** {#NNN or SORT-NNN or N/A} \
**Feature:** {One-sentence summary of the feature or change.} \
**Source spec:** `.specs/Spec-{TASK_NAME}.md`

---

## Summary

<!--
Two to four sentences. State the goal, the shape of the change (one-layer
modification? full end-to-end feature? evaluation only?), and the high-level
dependency graph (linear? parallel phases? bottleneck?).

A good summary lets a reader decide in fifteen seconds whether this plan is
the right file to open. Omit flavor — this is an index entry, not a preamble.
-->

---

## Phase 1: {Phase Name}

*{One-sentence contract for this phase — what it produces.}*

- [ ] **1.1** {Step title — imperative verb + object}
  - **File:** `internal/domain/{filename}.go`
  - **Location:** {where in the file — "after the FooBar struct", "at end of file", "new file"}
  - **Signature:**
    ```go
    type FooBar interface {
        Operation(ctx context.Context, id string) (*Result, error)
    }
    ```
    <!--
    Signatures only. No bodies. Include a short doc-style line under the
    signature if the contract (preconditions, postconditions, error modes)
    is not obvious.
    -->
  - **Logic:** {short numbered list if the step has branching; omit otherwise}
  - **Constraint Check:** {What must not be imported; what must not be mutated; what invariant must hold.}
  - **Verify:** `make build` compiles cleanly in `internal/domain/...`.

- [ ] **1.2** {Step title}
  - **File:** `internal/domain/{filename}.go`
  - **Logic:**
    1. {First validation or transition}
    2. {Second validation or transition}
    3. If {condition}, return `{error_category}` without attempting {side effect}.
  - **Verify:** `make test ./internal/domain/...` passes.

- [ ] **Constraint Check (Phase 1):** No import of any package outside `internal/domain/`. No methods with side effects, no database handles, no adapter imports.

---

## Phase 2: {Phase Name}

*{One-sentence contract.}*

- [ ] **2.1** {Step title}
  - **File:** `internal/{package}/{filename}.go`
  - **Logic:** {...}
  - **Verify:** {...}

- [ ] **Constraint Check (Phase 2):** {Layer-boundary assertion specific to this phase.}

<!--
Repeat for additional phases as needed. Use the architectural order:

  Phase 1  — Domain Model            (internal/domain/)
  Phase 2  — Configuration           (internal/workflow/, internal/config/)
  Phase 3  — Persistence             (internal/persistence/)
  Phase 4  — Integration Adapters    (internal/tracker/*, internal/agent/*)
  Phase 5  — Workspace Manager       (internal/workspace/)
  Phase 6  — Orchestrator            (internal/orchestrator/)
  Phase 7  — CLI & Observability     (cmd/sortie/, internal/logging/, internal/httpapi/, internal/metrics/)
  Phase 8  — Verification & Cleanup

Include ONLY the phases this plan touches. Do not pad with empty phases.
Number phases in execution order starting from 1, even if you skip
architectural layers — the reader cares about the sequence, not the
mapping to layer positions.
-->

---

## Phase N: Verification and Cleanup

*Confirm the cumulative output compiles, lints, and tests green.*

- [ ] **N.1** Run `make lint` — zero warnings across affected packages.
- [ ] **N.2** Run `make test` — all tests pass with `-race`. Existing test count preserved; new tests from the Tester agent pass.
- [ ] **N.3** Run `make build` — produces a single static binary. Confirm the binary size has not unexpectedly ballooned (> ~10% change is a surprise worth investigating).
- [ ] **N.4** Manual smoke check: {describe the minimal manual invocation that exercises the new behavior end-to-end, if applicable}.
- [ ] **N.5** Confirm env-gated integration tests skip cleanly without their guard variable (`SORTIE_JIRA_TEST`, `SORTIE_GITHUB_TEST`, `SORTIE_CLAUDE_TEST`, `SORTIE_COPILOT_TEST`, `SORTIE_GITHUB_E2E`).

---

## Files Affected

<!--
Required. One table row per file the plan creates or modifies. Use the
format: path, change type (NEW / MOD / DEL), one-line purpose.

Group by architecture layer if the plan spans multiple layers:

| File                                       | Change | Purpose                                         |
|--------------------------------------------|--------|-------------------------------------------------|
| `internal/domain/foo.go`                   | NEW    | Foo interface + normalized types                |
| `internal/persistence/migrations/0005.go`  | NEW    | foo_table schema                                |
| `internal/orchestrator/dispatch.go`        | MOD    | Route Foo events through the dispatch path      |

List only files the plan directly changes. Do not list files the Developer
Agent might touch incidentally during implementation.
-->

| File | Change | Purpose |
|------|--------|---------|
| `internal/{package}/{filename}.go` | NEW/MOD | {short purpose} |

---

## Philosophy Checklist

<!--
This block is a SELF-CHECK the planner performs before delivering the plan.
Convert `- [ ]` to `- [x]` as each item is verified during Step 5 of the
writing-plans workflow. The checklist stays in the delivered plan as
evidence of the verification pass.

If an item cannot be checked, either the plan is incomplete or the item
does not apply. If it does not apply, strike it out (`~~item~~`) and
briefly say why. Do not delete items.
-->

- [ ] Every step traces to an architecture section, spec section, or issue.
- [ ] Adapter-specific identifiers (`jira_*`, `claude_*`, `codex_*`, `copilot_*`) appear only inside adapter-package steps.
- [ ] The plan respects milestone sequencing — no dependency on unbuilt foundations.
- [ ] Every step has an executable `**Verify:**` condition.
- [ ] Workspace safety invariants are explicit where applicable (path containment, key sanitization, cwd validation).
- [ ] The dependency graph flows downward only (Domain ← Config ← Persistence ← Adapters ← Workspace ← Orchestrator ← CLI).
- [ ] No function bodies, no test implementations, no SQL query strings — signatures and logic descriptions only.
- [ ] The plan is the minimum viable work that meets the spec.
