---
name: verifySpec
description: Forensic verification of implementation code against its authoritative specification
argument-hint: Path to the specification file
agent: Reviewer
---

I'm an Anthropic employee working on the Sortie project.

Your task is to conduct a forensic verification — a systematic, evidence-based comparison of the implemented code against its authoritative technical specification. You are answering one question: **does the implementation faithfully realize every requirement, constraint, design decision, interface contract, algorithm, and invariant defined in the specification?**

Before starting, read the relevant sections of [architecture.md](../../docs/architecture.md) — this is the authoritative specification for all domain models, state machines, algorithms, and validation rules.

Apply review standards from: [Code Review Guidelines](../instructions/code-review.instructions.md)

## Why This Matters

The specification is the product of deliberate architectural work. Every footnote, every constraint, every edge case note exists because an architect determined it was necessary. A missed requirement is a latent defect. A diverged algorithm is a behavioral bug. A weakened invariant is a potential security boundary violation. Your review is the last line of defense before the implementation is accepted as correct.

---

## Input

**Specification file:** ${input:specPath:Path to specification file}

Read the specification file in its entirety before proceeding.

---

## Phase 0 — Build Project Context

Before you can verify anything, you must understand the system the specification lives within. This is mandatory — do not skip to Phase 1.

### 0a. Project Documentation

Search for and read all architectural and project documentation:

- `AGENTS.md`, `README.md`, `CONTRIBUTING.md`
- `docs/` or `doc/` directories — especially architecture, design, and ADR documents
- Build/dependency manifests (`go.mod`) — to understand the tech stack
- `Makefile` — to understand the build model

### 0b. Codebase Structure

List the project root and key subdirectories. Identify:

- Module boundaries, layering, and dependency direction
- Naming conventions and package organization patterns
- Test structure and coverage approach

### 0c. Code Review Standards

Search for any code review instructions, standards, or guidelines the project defines (e.g., `.github/instructions/`). If found, adopt their severity classification and review dimensions.

---

## Phase 1 — Exhaustive Requirement Extraction

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

- Interface/function signatures, parameter types, return types
- Data structure fields, types, annotations, zero-value semantics
- Algorithm steps (especially numbered/ordered steps — each step is a separate requirement)
- State machine transitions and their guard conditions
- Error categories and how they must be handled (retry vs terminal vs propagate)
- Concurrency contracts (thread safety, context propagation, lifecycle management)
- Safety invariants (validation, sanitization, containment rules)
- Schema definitions, query patterns, transaction boundaries
- Naming conventions and module boundary rules
- Diagrams encode behavioral flow — each arrow and decision node is a requirement
- Risk mitigations — each mitigation implies a requirement on the implementation
- "Verify" or "ensure" conditions — each is a testable obligation
- Comments, notes, and parenthetical remarks — these are often critical edge cases

**Completeness check:** After extraction, count your requirements. For a 400-line spec, expect 30-60 requirements. For an 800-line spec, expect 60-120. If your count is significantly below this range, you missed requirements — re-read the spec.

Output this phase as a numbered requirements table before proceeding.

---

## Phase 2 — Implementation Discovery

**Objective:** Identify every source file that constitutes the implementation of this specification.

1. Use the spec's file structure section (if present) and your own analysis to identify all files the spec says should be created or modified.
2. Search the codebase for these files. Read each one completely.
3. If the spec references interfaces or types from other modules, read those too — you need them to verify contract conformance.
4. If a file the spec says should exist does not exist, record this immediately as a `MISSING` finding.

Build a file inventory:

| File | Exists | Lines | Layer/Module | Role per spec |
|---|---|---|---|---|

---

## Phase 3 — Requirement-by-Requirement Verification

**Objective:** For each requirement from Phase 1, determine whether the implementation satisfies it.

For **every single requirement** (no exceptions, no batching, no "the rest are fine"):

1. **Locate** the corresponding code. Quote the relevant lines (file:line_range).
2. **Compare** the spec requirement against the actual implementation behavior.
3. **Classify** the result:

| Status | Meaning |
|---|---|
| `PASS` | Implementation matches the requirement |
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

**Attention discipline:** After completing verification, explicitly re-read the last 20% of the spec to counter recency bias. Check whether any requirements from the middle sections were evaluated too leniently.

Output this phase as a detailed findings table with evidence.

---

## Phase 4 — Cross-Cutting Verification

**Objective:** Check properties that span multiple requirements and are easy to miss in line-by-line review.

Based on the project's architecture documentation (from Phase 0), verify:

1. **Dependency direction:** Does the implementation respect the project's layer hierarchy and dependency rules?
2. **Naming consistency:** Do identifiers follow the project's established conventions?
3. **Context/lifecycle management:** Are resources, contexts, and connections properly managed?
4. **Error handling:** Does error handling follow the project's established patterns and the spec's error categories?
5. **Concurrency safety:** Is mutable state access properly synchronized?
6. **Interface compliance:** Do concrete types satisfy their declared interfaces?
7. **Security boundaries:** Are trust boundaries, input validation, and access controls implemented per spec?
8. **Data safety:** Are queries parameterized, transactions properly bounded, and data integrity maintained?

Skip any checks that are not relevant to the project's tech stack or the spec's domain.

---

## Phase 5 — Self-Verification

**Objective:** Reduce false positives by independently verifying each non-PASS finding.

For each finding from Phases 3-4:

1. Re-read the relevant spec section. Is your interpretation correct?
2. Re-read the relevant code. Did you miss a code path that addresses the requirement?
3. Check if the requirement is satisfied in a different file or through a different mechanism than expected.
4. Assign a confidence score: `high` (0.9+), `medium` (0.7-0.9). Drop findings below 0.7 confidence.

This step is mandatory. Initial code review findings have a 15-30% false positive rate. Self-verification reduces this significantly.

---

## Phase 6 — Verdict and Remediation

### 6a. Review Summary

```
## Spec Verification: [Spec Name]

### Metrics
- **Requirements extracted:** N
- **PASS:** N | **DRIFT:** N | **PARTIAL:** N | **MISSING:** N | **CONFLICT:** N
- **Conformance rate:** X% (PASS / total)
- **Critical findings:** N | **Major findings:** N | **Minor findings:** N

### Verdict: [CONFORMANT | CHANGES REQUIRED | NON-CONFORMANT]

### Key Findings (ordered by severity)
[Top findings with evidence]
```

### 6b. Remediation Plan

**Generate this section ONLY if there are critical or major findings.**

The remediation plan must be:
- **Self-contained** — an implementer should not need to re-read the full spec to understand what to fix
- **Precise** — reference exact files, line numbers, and the specific change needed
- **Ordered** — list fixes in dependency order (fix type definitions before functions that use them)
- **Verifiable** — each fix has a clear "done when" condition

Format:

```
## Remediation: [Spec Name]

**Spec:** [path to spec file]

### Required Fixes (in implementation order)

#### Fix 1: [Short title]
- **Severity:** Critical/Major
- **File:** `path/to/file`
- **Requirement (from spec):** "[exact quote]"
- **Current behavior:** [what the code does now, with line reference]
- **Required behavior:** [what the code must do instead]
- **Implementation guidance:** [specific, actionable steps]
- **Done when:** [verifiable condition]

### Verification Checklist
After all fixes:
- [ ] Project builds without errors
- [ ] Linter passes
- [ ] Tests pass
- [ ] Each fix satisfies its "Done when" condition
```

---

## Critical Reminders

- **The spec has no filler.** Every section, note, risk mitigation, and diagram encodes requirements. Treat the entire document as load-bearing.
- **Quote before judging.** Always extract the exact spec text before evaluating code against it. This prevents drift between what you think the spec says and what it actually says.
- **Absence is a finding.** If the spec defines a behavior and the implementation has no corresponding code, that is `MISSING` — not "probably handled elsewhere."
- **The spec is the authority.** If the code does something sensible that the spec doesn't require, that's fine. If the code does something different from what the spec requires, that's a defect — even if the code's approach seems better.
- **No severity inflation.** `critical` = safety violations, data loss, behavioral contradictions. `major` = correctness bugs, missing functionality. `minor` = naming and style divergence.

## Output

Write the full review to a markdown file in the `.reviews/` directory. Create the directory if it does not exist.

The filename follows the pattern `Review-<spec-name>.md`, where `<spec-name>` is derived from the specification filename. If the spec filename starts with `Spec-`, strip that prefix. Examples:
- `.specs/Spec-6.4-Worker-Attempt-Function.md` → `.reviews/Review-6.4-Worker-Attempt-Function.md`
- `specs/auth-service.md` → `.reviews/Review-auth-service.md`

Before writing, check if `.reviews/<name>.md` already exists. If it does, append a numeric index: `.reviews/<name>-2.md`, `.reviews/<name>-3.md`, etc. Use the next available number.

After writing, print the path to the created file.
