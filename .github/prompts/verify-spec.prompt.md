---
name: verify-spec
description: >
  Verify implementation correctness against its technical specification.
  Performs rigorous, evidence-based architectural review comparing written
  code to every requirement, constraint, and design decision in the spec.
  Generates actionable remediation prompt for the Coder agent when issues found.
argument-hint: Path to the spec file (e.g., .specs/Spec-6.4-Worker-Attempt-Function.md)
agent: Architect
---

# Spec-vs-Implementation Verification Review

You are conducting a **forensic verification** — a systematic, evidence-based comparison of the implemented code against its authoritative technical specification. This is not a general code review. You are answering one question: **does the implementation faithfully realize every requirement, constraint, design decision, interface contract, algorithm, and invariant defined in the specification?**

## Why This Matters

The specification is the product of deliberate architectural work. Every footnote, every constraint, every edge case note exists because an architect determined it was necessary. A missed requirement is a latent defect. A diverged algorithm is a behavioral bug. A weakened invariant is a potential security boundary violation. Your review is the last line of defense before the implementation is accepted as correct.

---

## Inputs

- **Specification:** ${input:spec_path:Path to the spec file (e.g., .specs/Spec-6.4-Worker-Attempt-Function.md)}
- **Scope hint (optional):** ${input:scope:Optional — specific files or sections to focus on, or leave blank for full review}

Before you begin, also read:
- [architecture.md](../../docs/architecture.md) — the authoritative system specification
- [Code Review Standards](../instructions/code-review.instructions.md) — severity classification, confidence calibration, and review dimensions

---

## Review Protocol

You MUST execute the following phases **in strict sequence**. Do not skip, merge, or reorder phases. Each phase builds on the outputs of the previous one. This separation is not bureaucratic — research demonstrates that LLMs produce significantly more accurate reviews when requirement extraction is decoupled from code evaluation (arXiv:2508.12358).

---

### Phase 1 — Exhaustive Requirement Extraction

**Objective:** Build a complete, numbered inventory of every verifiable obligation the specification imposes on the implementation.

Read the specification **in its entirety**, from the first line to the last. Do not skim. Do not summarize sections as "standard boilerplate." The specification has no filler — every sentence potentially encodes a requirement.

For each requirement you extract, record:

| Field | Content |
|---|---|
| **ID** | `R-{section}-{seq}` (e.g., `R-3.2-04`) |
| **Spec quote** | The exact text from the specification (verbatim, in quotes) |
| **Spec location** | Section number and/or line range in the spec file |
| **Category** | One of: `interface-contract`, `struct-layout`, `algorithm`, `state-transition`, `error-handling`, `safety-invariant`, `concurrency`, `persistence`, `configuration`, `naming`, `boundary` |
| **Criticality** | `must` (violation = defect), `should` (violation = concern), `note` (informational intent) |

**Extraction guidance — what counts as a requirement:**
- Interface method signatures, parameter types, return types
- Struct field names, types, tags, zero-value semantics
- Algorithm steps (especially numbered/ordered steps — each step is a separate requirement)
- State machine transitions and their guard conditions
- Error categories and how they must be handled (retry vs terminal vs propagate)
- Concurrency contracts (single-writer, context propagation, goroutine lifecycle)
- Safety invariants (path containment, key sanitization, validation before use)
- SQLite schema, query patterns, transaction boundaries
- Naming conventions and package boundary rules
- Mermaid diagrams encode behavioral flow — each arrow and decision node is a requirement
- Risk mitigations in the Risk Assessment table — each mitigation implies a requirement
- "Verify" conditions in Implementation Steps — each is a testable obligation
- Comments, notes, and parenthetical remarks in the spec — these are often critical edge cases

**Completeness check:** After extraction, count your requirements. For a 400-line spec, expect 30-60 requirements. For an 800-line spec, expect 60-120. If your count is significantly below this range, you missed requirements — re-read the spec.

Output this phase as a numbered requirements table before proceeding.

---

### Phase 2 — Implementation Discovery

**Objective:** Identify every source file that constitutes the implementation of this specification.

Using the spec's "File Structure Summary" section (if present) and your own analysis:

1. Identify all files the spec says should be created or modified.
2. Search the codebase for these files. Read each one completely.
3. If the spec references interfaces or types from other packages, read those too — you need them to verify contract conformance.
4. If a file the spec says should exist does not exist, record this immediately as a `NOT_IMPLEMENTED` finding.

Build a file inventory:

| File | Exists | Lines | Layer | Role per spec |
|---|---|---|---|---|
| `internal/orchestrator/dispatch.go` | ✅ | 245 | Coordination | Dispatch function |
| ... | ... | ... | ... | ... |

---

### Phase 3 — Requirement-by-Requirement Verification

**Objective:** For each requirement from Phase 1, determine whether the implementation satisfies it.

This is the core of the review. For **every single requirement** (no exceptions, no batching, no "the rest are fine"):

1. **Locate** the corresponding code. Quote the relevant lines (file:line_range).
2. **Compare** the spec requirement against the actual implementation behavior.
3. **Classify** the result:

| Status | Meaning |
|---|---|
| `PASS` | Implementation matches the requirement exactly |
| `DRIFT` | Implementation works but deviates from the spec in a way that changes behavior |
| `PARTIAL` | Requirement is partially implemented — some aspects present, others missing |
| `MISSING` | No corresponding implementation found |
| `CONFLICT` | Implementation contradicts the requirement |

4. **For non-PASS findings**, provide:
   - The exact spec quote (what was required)
   - The exact code quote (what was implemented, or "not found")
   - A precise explanation of the discrepancy
   - Severity: `critical` (safety/correctness), `major` (behavioral), `minor` (naming/style)

**Anti-bias protocol:** Do not rationalize discrepancies. If the spec says X and the code does Y, that is a finding — even if Y seems reasonable. The spec is the authority. If the spec is wrong, that is a separate concern to flag, but it does not excuse the implementation divergence.

**Attention discipline for long specs:** After completing verification of requirements R-1 through R-N, explicitly re-read the last 20% of the spec to counter recency bias. Check whether any requirements from the middle sections were evaluated too leniently.

Output this phase as a detailed findings table with evidence.

---

### Phase 4 — Cross-Cutting Verification

**Objective:** Check properties that span multiple requirements and are easy to miss in line-by-line review.

Verify these cross-cutting concerns against both the spec and [architecture.md](../../docs/architecture.md):

1. **Import graph:** Does the implementation respect the architectural layer hierarchy? (domain ← config ← persistence ← workspace ← orchestrator)
2. **Naming consistency:** Are all identifiers in core packages generic (`agent_*`, `tracker_*`)? No adapter-specific names leaked?
3. **Context propagation:** Does every goroutine and subprocess accept and respect `context.Context`?
4. **Error wrapping:** Are all errors wrapped with `fmt.Errorf("context: %w", err)` and using the spec's error categories?
5. **Single-writer invariant:** Is all mutable state access serialized through the designated authority?
6. **Interface compliance:** Do concrete types satisfy their interfaces? (Method signatures, return types, nil behavior)
7. **SQLite safety:** `modernc.org/sqlite` only, parameterized queries, single-writer transactions, `defer tx.Rollback()`?
8. **Workspace safety:** Path containment, key sanitization, symlink rejection, cwd validation?

---

### Phase 5 — Self-Verification of Findings

**Objective:** Reduce false positives by independently verifying each non-PASS finding.

For each finding from Phases 3-4:

1. Re-read the relevant spec section. Is your interpretation correct?
2. Re-read the relevant code. Did you miss a code path that addresses the requirement?
3. Check if the requirement is satisfied in a different file or through a different mechanism than you expected.
4. Assign a confidence score: `high` (0.9+), `medium` (0.7-0.9). Drop findings below 0.7 confidence.

This step is mandatory. Research shows that initial code review findings have a 15-30% false positive rate. Self-verification reduces this significantly.

---

### Phase 6 — Verdict and Remediation

**Objective:** Synthesize findings into a verdict and, if needed, generate a precise remediation prompt.

#### 6a. Review Summary

```markdown
## Spec Verification: [Spec Name]

### Metrics
- **Requirements extracted:** N
- **PASS:** N | **DRIFT:** N | **PARTIAL:** N | **MISSING:** N | **CONFLICT:** N
- **Conformance rate:** X% (PASS / total)
- **Critical findings:** N | **Major findings:** N | **Minor findings:** N

### Verdict: [APPROVED | CHANGES REQUIRED | BLOCKED]

### Key Findings (ordered by severity)
[Top 5 findings with evidence]
```

#### 6b. Coder Remediation Prompt

**Generate this section ONLY if there are critical or major findings.** This prompt will be handed to the Coder agent, who implements fixes in production `.go` files only (never test files).

The prompt must be:
- **Self-contained** — the Coder should not need to re-read the full spec to understand what to fix
- **Precise** — reference exact files, line numbers, and the specific change needed
- **Ordered** — list fixes in dependency order (fix type definitions before functions that use them)
- **Verifiable** — each fix should have a clear "done when" condition

Format the remediation prompt inside a fenced markdown block so it can be copied directly:

````markdown
```markdown
## Remediation: [Spec Name]

**Spec:** [path to spec file]
**Review date:** [today's date]

### Context

[2-3 sentences explaining what the spec defines and what the implementation gets wrong at a high level]

### Required Fixes (in implementation order)

#### Fix 1: [Short title]
- **Severity:** Critical/Major
- **File:** `path/to/file.go`
- **Requirement (from spec):** "[exact quote from spec]"
- **Current behavior:** [what the code does now, with line reference]
- **Required behavior:** [what the code must do instead]
- **Implementation guidance:** [specific, actionable steps — not vague suggestions]
- **Done when:** [verifiable condition]

#### Fix 2: [Short title]
...

### Verification Checklist
After all fixes are applied:
- [ ] `make build` compiles without errors
- [ ] `make lint` passes without warnings
- [ ] `make test` passes (if existing tests cover modified code)
- [ ] Each fix addresses its "Done when" condition

### Files Modified
[Expected list of files that will need changes]
```
````

**Prompt quality criteria:**
- A Coder reading this prompt for the first time, with no prior context, must be able to implement every fix correctly
- No fix should require the Coder to make architectural decisions — all decisions are made here
- Error descriptions must distinguish between "code is wrong" and "code is missing"
- Implementation guidance must respect the Coder agent's layer constraints (domain is pure types, no I/O; orchestrator is single-writer; adapters normalize to domain types)

---

## Critical Reminders

- **The spec has no filler.** Every section, note, risk mitigation, and diagram encodes requirements. Treat the entire document as load-bearing.
- **Quote before judging.** Always extract the exact spec text before evaluating code against it. This prevents drift between what you think the spec says and what it actually says.
- **Absence is a finding.** If the spec defines a behavior and the implementation has no corresponding code, that is `MISSING` — not "probably handled elsewhere."
- **The spec is the authority.** If the code does something sensible that the spec doesn't require, that's fine. If the code does something different from what the spec requires, that's a defect — even if the code's approach seems better.
- **No severity inflation.** Use `critical` only for safety violations, data loss risks, and behavioral spec contradictions. Use `major` for correctness bugs and missing functionality. Use `minor` for naming and style.
