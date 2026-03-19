---
name: createPr
description: Commit staged changes, create or update a pull request
argument-hint: PR details or branch/commit context
agent: agent
model: Claude Sonnet 4.5 (copilot)
tools:
  - 'execute/getTerminalOutput'
  - 'execute/runInTerminal'
  - 'read/terminalSelection'
  - 'read/terminalLastCommand'
  - 'read/readFile'
  - 'search'
  - 'web/githubRepo'
---

Commit staged changes and manage pull requests (PR).

## Task

- Use `git-commit` skill to create commits, create a branch, and `creating-pr` skill to open/change a PR with a meaningful title and description
- Incorporate provided details or context about the changes
- Detect whether you need to create a new PR or update an existing one based on context
- Stage only relevant files — never `git add -A` without reviewing what would be included
- When updating, verify the PR description still accurately reflects the changes
- Use [Conventional Commits](https://www.conventionalcommits.org/) messages when appropriate

## Constraints

- Never force-push without explicit user confirmation.
- Never push directly to `main` — always use a feature branch.
- Never skip pre-commit hooks (`--no-verify`).
- If a hook fails, fix the underlying issue and create a new commit.

${input:request:PR details or branch/commit context}
