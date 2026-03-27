---
name: reviewSpec
description: Conduct a deep architectural review of a specification or design document
argument-hint: "<task-name> <path/to/spec>"
agent: Reviewer
---

Conduct an in-depth architectural review of the specification described below. Ground the review in codebase reality by studying project docs, existing patterns, and module structure before evaluating.

Before writing anything, read the relevant sections of [architecture.md](../../docs/architecture.md) — this is the authoritative specification for all domain models, state machines, algorithms, and validation rules.

Apply review standards from: [Code Review Guidelines](../instructions/code-review.instructions.md)

## Task

**Input from the user:** ${input:task:Task name or issue reference} ${input:specPath:Path to specification file}

The first argument is the task or feature name (or a GitHub issue reference). The second argument is the path to the specification file. Read that file carefully — it is the primary subject of this review.

### Resolve Task Reference

If the first argument is a GitHub issue URL (e.g. `https://github.com/owner/repo/issues/123`) or a shorthand reference (e.g. `owner/repo#123` or `#123`), run `gh issue view <url-or-reference> --json title,body` to fetch the issue title and body. Use the fetched title as the task name and the body as additional context for the review. If the first argument is plain text, skip this step.

## Preparation: Build Context First

Before you begin the review, you MUST thoroughly study the project to ground your evaluation in reality. Follow these steps in order:

### Step 1: Project Documentation

Search for and read all architectural and project documentation. Look for files such as:

- `AGENTS.md`, `README.md`, `CONTRIBUTING.md`
- `docs/` or `doc/` directories — read everything relevant inside
- `docs/decisions/` — Architecture Decision Records
- `go.mod` — for understanding the tech stack and dependencies
- `Makefile` — for understanding the build model

### Step 2: Codebase Structure

Run a directory listing of the project root and key subdirectories to understand the module structure, layering, and boundaries. Identify:

- Entry points and public interfaces
- Domain boundaries and module organization
- Shared infrastructure and cross-cutting concerns
- Test structure and coverage patterns

### Step 3: Existing Patterns

Search the codebase for patterns relevant to the specification:

- How similar features were implemented before
- Existing abstractions the spec should reuse or align with
- Integration points the spec will touch
- Data models and storage patterns already in use

## Review Execution

Now conduct the architectural review of the specification. Your review must be grounded in the project context you just gathered. Specifically:

1. **Evaluate alignment** — Does the spec align with the existing architecture, patterns, and conventions? Where does it deviate, and is the deviation justified?

2. **Assess feasibility** — Given the current codebase structure, tech stack, and existing abstractions, is the spec implementable as written? Are there hidden complexities or dependencies the spec does not account for?

3. **Identify risks** — What architectural risks does this spec introduce? Consider coupling, data consistency, failure modes, scalability, security boundaries, and operational impact.

4. **Check completeness** — What does the spec leave unspecified that an implementer would need to decide? Are there ambiguities that could lead to divergent implementations?

5. **Evaluate tradeoffs** — What quality attributes does this spec prioritize? What does it sacrifice? Are those tradeoffs appropriate for the project context?

6. **Recommend improvements** — Concrete, actionable recommendations. Reference existing project patterns where the spec should align. Do not suggest abstractions or technologies the project does not already use unless solving a critical gap.

Be thorough. Be direct. Ground every finding in evidence from the spec and the codebase.

## Output

Write the full review to a markdown file in the `.reviews/` directory. Create the directory if it does not exist.

The filename follows the pattern `Review-<spec-name>.md`, where `<spec-name>` is derived from the specification filename. If the spec filename starts with `Spec-`, strip that prefix. Examples:
- `specs/Spec-6.4-Worker-Attempt-Function.md` → `.reviews/Review-6.4-Worker-Attempt-Function.md`
- `specs/auth-service.md` → `.reviews/Review-auth-service.md`

Before writing, check if `.reviews/<name>.md` already exists. If it does, append a numeric index: `.reviews/<name>-2.md`, `.reviews/<name>-3.md`, etc. Use the next available number.

After writing, print the path to the created file.
