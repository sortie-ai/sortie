# Specification Quality Checklist

## Contents

1. [Requirement Quality (IEEE 830)](#requirement-quality-ieee-830)
2. [Structural Completeness](#structural-completeness)
3. [Output Style Compliance](#output-style-compliance)
4. [Project Constraints](#project-constraints)
5. [Common Defects to Catch](#common-defects-to-catch)

---

Post-draft verification. Run through every item after completing Phase 3 (Specification Drafting). A failing check must be fixed before delivering the spec.

This checklist combines IEEE 830 / ISO 29148 requirement quality attributes with project-specific constraints from `AGENTS.md` and `docs/architecture.md`.

---

## Requirement Quality (IEEE 830)

Each functional requirement in the spec must satisfy all eight attributes:

| Attribute       | Test question                                                    |
|-----------------|------------------------------------------------------------------|
| **Correct**     | Does this match the architecture doc and accepted ADRs?          |
| **Unambiguous** | Can two engineers read this and reach the same implementation?   |
| **Complete**    | Are all inputs, outputs, error conditions, and edge cases defined? |
| **Consistent**  | Does this contradict any other requirement in this spec?         |
| **Ranked**      | Is the priority clear (must-have vs. nice-to-have)?              |
| **Verifiable**  | Can you write a test that proves this requirement is met?        |
| **Modifiable**  | Can this requirement change without rewriting the whole spec?    |
| **Traceable**   | Does this trace to a specific architecture doc section or issue? |

If any requirement fails the "two engineers" test for unambiguity, rewrite it with concrete values, explicit types, or a worked example.

---

## Structural Completeness

All five sections must be present and substantive:

- [ ] **Section 1 (Business Goal):** States the problem and its impact.
- [ ] **Section 2 (System Diagram):** Contains at least one Mermaid code block that renders. Diagram matches the data flow described in the text. Uses domain operation names, not implementation details.
- [ ] **Section 3 (Technical Architecture):** Contains at least one of: Go interface definition, struct definition, or SQLite schema. Unused subsections are deleted, not left empty.
- [ ] **Section 4 (Risk Assessment):** Table has at least one row. Severity uses the scale: Critical / High / Medium / Low. Every Critical risk has a concrete mitigation, not "TBD".
- [ ] **Section 5 (File Structure Summary):** Lists all new and modified files. Each file is annotated with its architecture layer.

---

## Output Style Compliance

- [ ] No function bodies, goroutine logic, or full package implementations
- [ ] Go interfaces have method signatures with doc comments
- [ ] SQLite schema uses `CREATE TABLE` DDL (when persistence is involved)
- [ ] Algorithms use pseudo-code or step descriptions, not Go code
- [ ] No OpenAI Symphony, Elixir, or BEAM patterns referenced
- [ ] Core-scope names use `agent_*`, `tracker_*`, `session_*` only
- [ ] Integration-specific names (`jira_*`, `claude_*`) appear only in adapter package scope
- [ ] Architecture doc sections cited for every design decision using Markdown anchor-links (e.g., `[Section 9.6](../docs/architecture.md#96-workspace-safety)`)

---

## Project Constraints

- [ ] **Single binary:** No new CGo dependencies. No `mattn/go-sqlite3`. Only `modernc.org/sqlite` for SQLite. No external runtime dependencies.
- [ ] **Adapter boundary:** Integration-specific logic lives in adapter packages only. Core packages import `internal/domain` types, never adapter packages.
- [ ] **Workspace safety:** If the feature touches paths or directories, the spec addresses path containment, key sanitization, and cwd validation explicitly.
- [ ] **Concurrency safety:** If the feature touches runtime state, the spec confirms single-writer semantics and `context.Context` propagation.
- [ ] **Milestone ordering:** The spec does not depend on work from incomplete milestones (or explicitly flags the dependency).

---

## Common Defects to Catch

These are the most frequent spec quality issues. Scan for them explicitly:

1. **Vague verbs.** "Handle errors appropriately" is not a spec. Replace with: "Return a `*domain.TrackerError` with Kind `transport_error` and the original error wrapped."
2. **Missing error paths.** Every operation that can fail needs an explicit error category and behavior (retry, abort, skip, log-and-continue).
3. **Implicit ordering.** If steps must execute in order, state it. If they can run concurrently, state that too.
4. **Unspecified defaults.** Config fields without defaults are ambiguous. State the default value and the rationale.
5. **Orphaned references.** If the spec cites an architecture section (e.g., `[Section X.Y](../docs/architecture.md#xy-...)`) but that section does not exist in the architecture doc, the reference is wrong. Verify before delivering.
6. **Oversized steps.** If an implementation step requires more than ~300 lines of code (exclude test code and documentation) or touches more than 3 files, decompose it further.
