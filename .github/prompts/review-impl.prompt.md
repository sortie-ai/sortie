---
name: reviewImpl
description: Review implementation changes against architectural standards
argument-hint: Task description, GitHub issue URL, or issue shorthand (#123)
agent: Reviewer
---

Your task is to review the implementation changes described below. Ground the review in codebase reality and project conventions.

Apply review standards from: [Code Review Guidelines](../instructions/code-review.instructions.md)

## Task

**What was implemented:** ${input:task:Task description or issue reference}

## Process

### Step 0: Resolve Task Reference

If the task description above contains a GitHub issue URL (e.g. `https://github.com/owner/repo/issues/123`) or a shorthand reference (e.g. `owner/repo#123` or `#123`), run `gh issue view <url-or-reference> --json title,body` to fetch the issue title and body. Use the fetched title and body as the canonical task description for all subsequent steps. If the task description is plain text, skip this step.

### Step 1: Understand the Project

Before evaluating any changes, build a mental model of the system:

1. Read project context files: `AGENTS.md`, `README.md`, and anything in `docs/`.
2. List the project root and key subdirectories to understand module structure and layering.
3. Read build/dependency manifests to understand the tech stack.
4. Search for code review standards or guidelines the project defines.

### Step 2: Understand the Task

Analyze the task description to determine:

- What problem is being solved or what capability is being added?
- Which architectural layers and modules should be affected?
- What quality attributes matter most for this change (correctness, performance, security, maintainability)?

### Step 3: Discover What Changed

Identify the implementation changes using all available signals:

1. Run `git diff HEAD~1` (and `git log --oneline -5` for context). If the change spans multiple commits, widen the range to capture the full scope.
2. If the diff is inconclusive, search for files recently modified that relate to the task description.
3. Read every changed file completely — not just the diff hunks. You need surrounding context to evaluate whether the change fits.

Build a change inventory:

| File | Change type | Lines changed | Module/Layer |
|---|---|---|---|

### Step 4: Evaluate the Implementation

For each changed file, assess:

1. **Correctness** — Does the code do what the task requires? Are edge cases handled? Are there logic errors?
2. **Architectural fit** — Does the change respect module boundaries, dependency direction, and the project's established patterns? Does it put logic in the right layer?
3. **Regression risk** — Could this change break existing behavior? Are callers of modified interfaces updated? Are assumptions still valid?
4. **Error handling** — Are errors handled consistently with the project's patterns? Are failure modes explicit?
5. **Naming and contracts** — Do new identifiers communicate intent? Are public API contracts clear?
6. **Completeness** — Is anything missing that the task implies? Migrations, config changes, documentation updates, test coverage?
7. **Simplicity** — Is the solution the simplest that works, or is there unnecessary complexity, indirection, or premature abstraction?

### Step 5: Produce the Review

## Output Format

```
## Implementation Review: [task description, shortened]

### Change Summary
[2-3 sentences: what changed, across which modules, and the overall approach taken]

### Findings

#### [Finding title]
- **Severity:** Critical | High | Medium | Low
- **File:** `path/to/file` (lines N-M)
- **Issue:** [what is wrong, with code evidence]
- **Recommendation:** [concrete fix — not vague advice]

<repeat for each finding, severity-descending>

### What Works Well
[Brief — 1-3 sentences acknowledging sound decisions. Do not pad.]

### Verdict
- **Risk level:** Critical | High | Moderate | Low
- **Decision:** Block | Request changes | Approve with notes | Approve
- **Key actions:**
  1. …
  2. …
  3. …
```

### Severity Criteria

- **Critical** — Data loss, security vulnerability, correctness bug that affects users. Must fix before merge.
- **High** — Behavioral bug, broken contract, architectural violation that will cause pain. Should fix before merge.
- **Medium** — Maintainability concern, missing edge case in non-critical path, suboptimal but working approach. Fix soon.
- **Low** — Minor naming issue, opportunity for simplification, style inconsistency. Fix if convenient.

### Rules

- Every finding must cite specific code. No finding without a file path and line reference.
- Do not comment on formatting, whitespace, or style unless it signals a real problem.
- Do not suggest refactors beyond the scope of the task. Review what was changed, not what was not.
- If the change is clean and correct, say so briefly. A short review of good code is more valuable than a long review searching for problems that are not there.

## Save the Review

Write the review to `.reviews/Review-impl-<slug>.md`, where `<slug>` is a short kebab-case slug derived from the task description (3-5 words max).

Create the `.reviews/` directory if it does not exist. If the file already exists, append a numeric index: `-2`, `-3`, etc.

After writing, print the path to the created file.
