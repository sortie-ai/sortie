# Philosophy Checklist — Pre-Delivery Verification

## Contents

1. [How to Use This Checklist](#how-to-use-this-checklist)
2. [Traceability](#traceability)
3. [Layering and Boundaries](#layering-and-boundaries)
4. [Atomicity and Verification](#atomicity-and-verification)
5. [Sequencing and Dependencies](#sequencing-and-dependencies)
6. [Security Invariants](#security-invariants)
7. [Style Compliance](#style-compliance)
8. [Simplicity](#simplicity)
9. [Final Delivery Check](#final-delivery-check)

---

## How to Use This Checklist

Run this checklist after drafting the plan (Workflow Step 4), before writing it to disk (Workflow Step 6). Each item is a gate — a failing gate is a rewrite, not a note-to-self.

Items are grouped by concern. If a group has no failures, move on. If any item fails, fix the plan and re-run the full checklist — a fix in one place often causes a regression elsewhere.

Check items by converting `- [ ]` to `- [x]` as you verify them. Do not include the checklist itself in the final plan artifact — this is a self-check, not part of the deliverable.

---

## Traceability

- [ ] Every non-trivial step names either an architecture section (e.g. `[Section 9.6](../docs/architecture.md#96-workspace-safety)`), a spec section (e.g. `[Section 3.2](../.specs/Spec-SORT-42.md#32-...)`), or an issue (e.g. "closes #198").
- [ ] Every cited architecture section actually exists in `docs/architecture.md`. Orphaned citations misinform the Developer Agent.
- [ ] Every cited spec section actually exists in the named spec file.
- [ ] Citations are inline in the plan text, not copied into Go source comments.

---

## Layering and Boundaries

- [ ] Phases appear in dependency order: Domain → Config → Persistence → Adapters → Workspace → Orchestrator → CLI → Verification.
- [ ] No step in Phase N imports or extends work from Phase N+1 or later.
- [ ] Integration-specific identifiers (`jira_*`, `claude_*`, `codex_*`, `copilot_*`, `github_*`) appear only inside their adapter-package steps.
- [ ] Core-layer steps (Domain, Config, Persistence, Orchestrator) use generic vocabulary (`agent_*`, `tracker_*`, `session_*`, `workspace_*`).
- [ ] The orchestrator is named as the single authority for state mutations to `running`, `claimed`, `retry_attempts`. No other phase steals that authority.
- [ ] Adapters are additive — no step in Phase 4 forces changes into core packages unless an adapter-interface-change rationale is explicit and the migration path for every existing adapter is listed.

---

## Atomicity and Verification

- [ ] Every step is atomic — sized for a single agent session (~one file, ~one signature, ~300 lines of implementation code max).
- [ ] Every step has an explicit `**Verify:**` condition a Developer Agent can execute (`make build`, `make test`, `grep -c`, etc.).
- [ ] Compound steps (three independent actions under one checkbox) are split.
- [ ] Test additions are named, not written out — the Tester agent produces the bodies.

---

## Sequencing and Dependencies

- [ ] The plan respects milestone sequencing. Steps that depend on unbuilt milestones are flagged explicitly (or removed and surfaced to the user).
- [ ] File paths referenced in any step either exist in the current tree or are created by an earlier step in the same plan. No phantom files.
- [ ] Steps within a phase flow in execution order (the Developer Agent works top-to-bottom).
- [ ] If Phase N+1 depends on a specific artifact from Phase N (e.g. a struct definition, a migration file), the Phase N step produces exactly that artifact.

---

## Security Invariants

If the feature touches filesystem, path handling, or agent subprocess launch, verify the plan preserves every workspace safety invariant. These are security boundaries, not suggestions.

- [ ] **Path containment.** Workspace paths are under the workspace root, verified by absolute-path prefix check. Symlink escapes are rejected.
- [ ] **Key sanitization.** Workspace directory keys use only `[A-Za-z0-9._-]`. No exceptions.
- [ ] **cwd validation.** Agent launch plans confirm `cwd == workspace_path` *before* `exec`, not after.
- [ ] **Single-writer persistence.** SQLite plans preserve WAL mode and single-writer semantics. No concurrent writes.
- [ ] **Secret handling.** Plans that touch credentials or API keys route them through existing config patterns — no logging, no persistence in `run_history`, no exposure in runtime snapshots.

If any of these invariants are weakened by the plan, reject the plan and redesign. These are not optional.

---

## Style Compliance

- [ ] Zero function bodies, goroutine orchestration, channel wiring, or SQL query strings.
- [ ] Zero test implementations. Tests are named, not written out.
- [ ] Go interface signatures and struct field layouts are used where the design requires pinning a contract.
- [ ] Branching logic is written as numbered natural-language steps (architecture Section 16 pseudo-code style), never as `if/else` Go snippets.
- [ ] Every implementable step uses the `- [ ]` checkbox format. Phase headers, tables, and rationale blocks use regular Markdown.
- [ ] No OpenAI Symphony, Elixir, or BEAM vocabulary.
- [ ] No architecture-doc references inside instructions to write godoc or inline comments.

---

## Simplicity

- [ ] The plan contains the minimum phases, steps, and artifacts needed to meet the spec. No speculative flexibility, no forward-looking abstractions.
- [ ] No "improvements" to adjacent code are smuggled in. Every step traces to the spec — not to the plan author's sense of aesthetics.
- [ ] No step adds configuration knobs, feature flags, or compatibility shims that the spec did not request.
- [ ] No step adds error handling for scenarios that cannot occur (framework-guaranteed invariants, internal-only call sites).
- [ ] If the plan has 20+ steps, reconsider whether it is one plan or several. Large plans become implementation-time guesswork when phases are skipped or reordered in practice.

The test: a senior engineer reading the plan says "this is the minimum to meet the spec". Not "this is clever", not "this is comprehensive" — the minimum.

---

## Final Delivery Check

- [ ] The plan file name follows the convention: `Plan-<ID>-<kebab-case-slug>.md` under `.plans/`, with `<ID>` being the Jira ID, GitHub issue number, or a concise task identifier.
- [ ] The plan header lists the spec path, milestone, dependencies, and architecture refs (top-of-file metadata).
- [ ] The final phase (Phase 8 / Verification) covers end-to-end behavior and environment hygiene only. Do not duplicate `make lint`, `make test`, or `make build` — per-phase `Verify:` conditions already prove those pass.
- [ ] The plan, in isolation, tells the Developer Agent everything needed to implement the feature without re-reading the spec. (The Developer Agent will still read the spec — but the plan should not require it for any individual step.)
- [ ] Re-read the first and last phase. If either is under-specified or repetitive, fix it. First-and-last are where drift starts.

When every item passes, write the plan to disk. Report the absolute file path to the user. Do not produce the plan inline in chat unless the user explicitly asks.
