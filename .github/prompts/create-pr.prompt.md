---
name: createPr
description: Commit staged changes, create or update a pull request
argument-hint: PR details or branch/commit context
agent: agent
model: Claude Sonnet 4.6 (copilot)
tools:
  - 'execute/getTerminalOutput'
  - 'execute/runInTerminal'
  - 'read/terminalSelection'
  - 'read/terminalLastCommand'
  - 'read/readFile'
  - 'search'
  - 'web/githubRepo'
---

Your task is to commit staged changes and manage pull requests (PR).

## Task

- Use `git-commit` skill to create commits, create a branch, and `create-pr` skill to open/change a PR with a meaningful title and description
- Incorporate provided details or context about the changes
- Detect whether you need to create a new PR or update an existing one based on context
- Stage only relevant files — never `git add -A` without reviewing what would be included
- When updating, verify the PR description still accurately reflects the changes
- Use [Conventional Commits](https://www.conventionalcommits.org/) messages when appropriate

## Template Enforcement

**MANDATORY:** Before creating any PR:

1. **Check for the PR template:** Use PR template provided by the `create-pr` skill. You MUST follow it.
2. **Read the template completely:** Read the entire template file to understand its structure
3. **Follow the template exactly:** Structure the PR body to match the template's sections verbatim
   - Use the template headings as-is (including emojis if present)
   - Fill in each section following the guidance provided
   - Do not skip sections or reorder them
4. **DO NOT invent a custom format:** Deviation from it is a failure

**Process:**
- Search for template
- Read template: `read_file` the template
- Map changes to template sections
- Create PR body matching template structure
- Verify sections match before running `gh pr create`

## PR Description Content Rules

- **DO NOT** reference `docs/architecture.md`, `docs/decisions/`, section numbers, or ADR numbers (e.g. "Section 16.6", "ADR-0002") in the PR description. Those belong in specs and plans, not in the git history.
- **DO NOT** add an "Implementation Details" or "References" section beyond what the template defines.
- **ONLY** fill in the sections defined by the PR template — nothing more.

## Constraints

- Never force-push without explicit user confirmation.
- Never push directly to `main` — always use a feature branch.
- Never skip pre-commit hooks (`--no-verify`).
- If a hook fails, fix the underlying issue and create a new commit.

${input:request:PR details or branch/commit context}
