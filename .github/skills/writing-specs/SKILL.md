---
name: writing-specs
description: >
  Write technical specifications for features, components, and architectural
  changes. Use when asked to specify, architect, design, write a spec, create
  requirements, produce an SRS, define technical requirements, or transform a
  feature request into a specification. Also use when creating any .specs/
  artifact. Handles analysis protocol, compliance checks, Mermaid diagrams,
  Go interface design, SQLite schema, risk assessment, and implementation
  step sizing. Do NOT use for implementation plans (use Planner), code reviews
  (use Reviewer), or standalone architecture analysis without spec output.
metadata:
  author: Sortie LLC
  version: "1.0"
  category: architecture
---

# Writing Technical Specifications

Produce a Markdown specification rigorous enough to be implemented without further clarification. Every section is a binding contract between architect and implementer. Incomplete or vague sections cause real engineering delays.

## Workflow

Five phases, executed sequentially. Do not skip or reorder phases.

### Phase 1: Context Gathering

Before writing a single line of spec, collect all inputs:

1. **Read the feature request** in full. Identify the core problem being solved.
2. **Read the relevant sections of `docs/architecture.md`** — this is the authoritative specification for all domain models, state machines, algorithms, and validation rules. The spec must conform to it; do not invent behavior that contradicts the architecture document.
3. **Read `docs/decisions/`** — check accepted ADRs for binding constraints on technology choices, interfaces, and deployment model.
4. **Scan codebase structure** — run `tree -d -L 3 internal/` to understand the current package layout and identify where new code belongs.
5. **Fetch external context** (when Atlassian MCP is available):
   - If a Jira issue ID is provided, fetch the issue details, acceptance criteria, and linked issues.
   - Check linked Confluence pages for domain documentation.
   - Extract constraints from Jira issue fields.

If any input is missing or ambiguous, stop and ask before proceeding. Do not guess requirements.

### Phase 2: Analysis Protocol

Execute the seven-check analysis protocol before designing anything. Each check gates the design — a failure must be resolved before proceeding.

> Read [references/analysis-protocol.md](references/analysis-protocol.md) for the full protocol with check descriptions and failure actions.

Record findings from each check.

### Phase 3: Specification Drafting

Use the template from [assets/spec-template.md](assets/spec-template.md) as the output skeleton. Fill each section sequentially:

1. **Business Goal & Value** — state the problem and why it matters. Map to the architecture doc's problem statement or a specific GitHub issue.
2. **System Diagram** — create a Mermaid sequence or component diagram showing data flow through the system's layers.
3. **Technical Architecture** — the core design section. Define Go interfaces, struct layouts, SQLite schema, state machine transitions, error categories, and adapter boundaries. See Output Style Rules below.
4. **Risk Assessment** — identify risks with severity and mitigation. Security boundaries (workspace path containment, key sanitization, cwd validation) are always relevant when the feature touches workspace or filesystem operations.
5. **File Structure Summary** — tree view of new/modified files with architecture layer annotations.

Write the spec to `.specs/Spec-{TASK_NAME}.md`.

- Derive `TASK_NAME` as concise kebab-case from the feature request.
- If a Jira ID is provided (e.g., `SORT-42`), use it: `Spec-SORT-42.md`.
- If a GitHub issue number is provided (e.g., `#198`), use it: `Spec-198-short-name.md`.

### Phase 4: Self-Verification

After drafting, verify the spec against the quality checklist before delivering.

> Read [references/quality-checklist.md](references/quality-checklist.md) for the
> full verification criteria.

At minimum, confirm:

- [ ] Every functional requirement is testable and unambiguous
- [ ] Every design decision traces to a specific architecture doc section
- [ ] Go interfaces define contracts, not implementations
- [ ] No implementation code — pseudo-code and signatures only
- [ ] Mermaid diagram renders and matches the described data flow
- [ ] Risk assessment covers security boundaries when applicable
- [ ] No Symphony/Elixir/BEAM patterns referenced
- [ ] No generic naming violations (`jira_*`, `claude_*` outside adapter scope)

If any check fails, revise the spec before delivering.

### Phase 5: Validation

When `python3` is available, run structural validation:

```bash
python3 .github/skills/writing-specs/scripts/validate_spec.py .specs/Spec-{TASK_NAME}.md
```

If the script is unavailable, manually verify:
- All five sections are present and non-empty
- At least one Mermaid code block exists
- The risk assessment table has at least one row

Report the spec file path after completion.

---

## Output Style Rules

These rules are non-negotiable. Violations produce specs that mislead implementers.

1. **NO IMPLEMENTATION.** Do not write function bodies, goroutine logic, or full package implementations. The task is to translate the request into a technical specification, not to implement it.
2. **GO INTERFACES ARE DESIGN.** Define Go interface signatures, struct field layouts, and method contracts. In this stack, the interface IS the specification.
3. **SQLITE SCHEMA IS DESIGN.** When the feature touches persistence, define the SQLite table schema, migration SQL, and query patterns. The schema IS the specification.
4. **PSEUDO-CODE FOR ALGORITHMS.** For orchestration logic, use pseudo-code or step-by-step descriptions matching the style in architecture doc Section 16 (Reference Algorithms).
5. **NO SYMPHONY PATTERNS.** Do not reference or replicate OpenAI Symphony, Elixir, or BEAM patterns. This project diverges intentionally.
6. **NO GENERIC NAMING VIOLATIONS.** Core specs use `agent_*`, `tracker_*`, `session_*`. Never `jira_*`, `claude_*`, `codex_*` outside adapter package specs.
7. **CITE THE SPEC.** Reference specific `docs/architecture.md` sections using Markdown anchor-links (e.g., `[Section 9.6](../docs/architecture.md#96-workspace-safety)`) for every design decision that traces to the spec.

---

## Coding Standards

The specification must respect these project standards. Read the relevant instruction files when designing interfaces, struct layouts, and error types:

- **Go Code Style** (`.github/instructions/go-codestyle.instructions.md`) — naming, control flow, comment structure, Go 1.22+ idioms
- **Go Documentation** (`.github/instructions/go-documentation.instructions.md`) — package comments, exported symbol docs, godoc conventions
- **Go Structured Logging** (`.github/instructions/go-logging.instructions.md`) — slog patterns, context field propagation
