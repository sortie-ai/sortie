---
name: writing-plans
description: >
  Produce atomic, layer-aware implementation plans for engineering tasks in
  this Go project. Use when asked to plan, break down, outline steps, create
  an actionable checklist, convert a specification into tasks, sequence the
  work, turn a Spec-*.md / Jira issue / GitHub issue / feature request into
  a Plan-*.md artifact, or answer "what's the plan for X", "how should we
  approach this", "decompose this feature", "write a roadmap for",
  "implementation plan for", or "break this down into phases". Also use
  whenever the output is meant to live under .plans/. The skill defines the
  phase structure, output style rules, constraint checks, and verification
  checklist — it does NOT write implementation code, tests, function
  bodies, goroutine logic, or SQL queries. Do NOT use for specifications,
  ADR, code reviews, implementing code, or triaging review feedback.
metadata:
  version: 2026-04-21
  category: planning
---

# Writing Implementation Plans

Translate a technical specification into a rigorous, atomic, linearly-executable implementation plan. The plan tells the Developer Agent **WHAT** needs to be done, at which file path, in which order, and how to verify each step. It never tells **HOW** to write the code — that is the Developer Agent's job.

A good plan is the compiled form of a spec: it removes ambiguity, sequences dependencies, and names every file and signature that will change. A bad plan forces the Developer Agent to re-interpret the spec on every step, which is how drift from the architecture doc starts.

## When to use

| User intent                                                          | Apply? |
|----------------------------------------------------------------------|--------|
| "Make a plan for Spec-*.md" / "plan this feature"                    | Yes    |
| "Break down this spec into steps"                                    | Yes    |
| "Convert this Jira issue / GitHub issue into a Plan-*.md"            | Yes    |
| "What's the plan for adding X"                                       | Yes    |
| "Sequence the work for this refactor"                                | Yes    |
| "Decompose this into phases"                                         | Yes    |
| "Write the spec for X" (no plan)                                     | No — use writing-specs |
| "Implement this plan" / "write the code"                             | No — Coder or Tester   |
| "Review this PR"                                                     | No — Reviewer          |
| "Document this ADR"                                                  | No — manage-adrs     |
| "Resolve review feedback"                                            | No — babysit-pr        |

## Inputs

Before drafting, confirm you have:

1. **A specification source.** Typically `.specs/Spec-{TASK_NAME}.md`, but may be a Jira issue, GitHub issue, or feature description pasted inline. If only a one-line request is given, stop and ask for the spec. Do not plan from assumptions.
2. **[`docs/architecture-digest.md`](../../../docs/architecture-digest.md).** Read this by default — it summarizes components, layers, the adapter model, hard constraints, and a deep-read trigger table. Open the full [`docs/architecture.md`](../../../docs/architecture.md) only when the plan touches an area flagged in the digest's deep-read table. Anchor-link citations in the plan still point at the full document. When the digest and the full document disagree, the full document wins; flag the drift.
3. **The current file tree.** Run `tree -d -L 3 internal/` (or equivalent) to know where new code belongs before assigning file paths.

If any input is missing, stop and ask. Planning from guesses produces plans that diverge from the spec on first contact with the code.

## Workflow

Copy this checklist into your response and mark items as you go. Each gate exists to catch a specific failure mode — do not skip gates.

- [ ] Step 1 — Read spec, architecture refs, and file tree
- [ ] Step 2 — Classify work into architectural phases
- [ ] Step 3 — Draft atomic steps using the template
- [ ] Step 4 — Attach constraint checks per phase
- [ ] Step 5 — Verify against the philosophy checklist
- [ ] Step 6 — Write to `.plans/Plan-{TASK_NAME}.md` and report the path

### Step 1 — Read spec, architecture refs, and file tree

Read the spec in full. Extract: the goal, the layers touched, the milestone, prerequisites, and any explicit "do not" constraints.

Read [`docs/architecture-digest.md`](../../../docs/architecture-digest.md) for the system map. Open the matching section of the full [`docs/architecture.md`](../../../docs/architecture.md) only when the spec cites it explicitly or when the digest's deep-read table flags an area the plan touches. If the spec references Section 9.6 (workspace safety) and your plan does not address path containment, the plan is incomplete.

Run a file-tree listing of the affected packages. This grounds your file-path decisions in reality — no inventing `internal/foo/bar.go` if `internal/foo/` does not exist.

**Stop condition:** If any step in the plan would contradict the architecture doc, surface the contradiction to the user. Do not "fix" it silently.

### Step 2 — Classify work into architectural phases

Group work into the project's six-layer architecture, plus CLI/Observability and Verification. Each layer is its own phase. Phases execute in dependency order — no upward imports, no forward references.

> Read [references/phase-structure.md](references/phase-structure.md) for the eight phases, their dependency graph, constraint checks, and the file packages each owns.

Only include phases that are actually touched by this spec. A two-layer change produces a two-phase plan. Do not pad plans with empty phases.

### Step 3 — Draft atomic steps using the template

Use [assets/plan-template.md](assets/plan-template.md) as the output skeleton. Fill it section by section. Every step must be:

- **Atomic** — sized for a single agent session (roughly one file, one signature, one test batch).
- **Independently verifiable** — states a concrete `Verify:` condition the Developer Agent can execute (`make build`, `make test`, `grep -c ...`).
- **Path-explicit** — names the exact file path, not "somewhere in internal/".
- **Signature-precise** — shows Go interface or function signatures where relevant, but never the body.

Steps must follow the Markdown checkbox format `- [ ] Step description` so the Developer Agent can mark progress.

### Step 4 — Attach constraint checks per phase

At the end of each phase, add a **Constraint Check** bullet. This is the layer-boundary assertion: "no upward imports", "single-writer state mutation", "path containment preserved", etc. Constraint checks are not optional padding — they are the seam that keeps the architecture from drifting over time.

> Read [references/output-style-rules.md](references/output-style-rules.md) for the full Do / Don't table covering code blocks, test integration, naming conventions, spec references, and Symphony pattern prohibitions.

### Step 5 — Verify against the philosophy checklist

Before writing the file, run every item in [references/philosophy-checklist.md](references/philosophy-checklist.md). A failing item is a rewrite, not a note-to-self.

At minimum, confirm:

- [ ] Every step traces to a specific architecture doc section or GitHub issue.
- [ ] The plan respects milestone sequencing — no steps depend on unbuilt foundations.
- [ ] Workspace safety invariants (path containment, key sanitization, cwd validation) are explicit where applicable.
- [ ] The plan is the minimum viable work that meets the spec.

### Step 6 — Write the file and report the path

Write the plan to `.plans/Plan-{TASK_NAME}.md`. Derive `{TASK_NAME}`:

- If a Jira ID is present (e.g., `SORT-42`), use `Plan-SORT-42.md`.
- If a GitHub issue number is present (e.g., `#238`), use `Plan-238-short-description.md`.
- Otherwise, use kebab-case derived from the spec title: `Plan-agent-max-turns-passthrough-leak.md`.

Report the absolute file path after writing. Do not produce the plan inline in chat unless the user explicitly asks — the artifact is the plan file.

## Output style: the three rules that catch 90% of defects

Violations produce plans that silently become implementation spec, which is the wrong contract. The detailed table lives in [references/output-style-rules.md](references/output-style-rules.md); the three that matter most:

1. **Signatures, not bodies.** Go function and interface signatures are allowed because signatures are part of the design. Function bodies, goroutine orchestration, channel wiring, and SQL query strings are not — those are the Developer Agent's to write.
2. **Logic descriptions, not code.** When a step needs to describe branching behavior, write it as a numbered list of validations and state transitions, matching the pseudo-code style from architecture Section 16. Never write `if err != nil { return fmt.Errorf(...) }` — write "If the workspace path is not under the workspace root, return `invalid_workspace_cwd`".
3. **Phase naming is generic in core, specific in adapters.** Orchestrator-core phases use `agent_*`, `tracker_*`, `session_*`. Integration-specific identifiers (`jira_*`, `claude_*`, `copilot_*`, `codex_*`) appear only inside steps for their adapter package.

## Why this skill exists

Planner agents drift in two predictable ways: they either bleed implementation details into the plan (turning the plan into a first draft of code), or they under-specify (producing a bulleted list the Developer Agent has to re-interpret on every step). This skill encodes the phase structure, output style, and verification checklist that keeps plans in the narrow band between over-specification and under-specification.

The architecture doc is the spec; the spec file is the contract; this skill produces the bridge between them.
