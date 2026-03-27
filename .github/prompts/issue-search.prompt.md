---
name: issueSearch
description: Search GitHub issues by keyword, label, milestone, or status
argument-hint: What to search for — keywords, labels, milestone scope, or status
agent: agent
tools:
  - 'execute/runInTerminal'
  - 'execute/getTerminalOutput'
---

Search GitHub issues and present matching results.

**MANDATORY:** Load and apply the `managing-github-issues` skill to understand label conventions and available search filters.

## Process

1. **Parse the query.** Identify search dimensions: keywords, type labels, area labels, milestone name, open/closed state.
2. **Discover taxonomy** if the query references labels or milestones by shorthand. Run the skill's taxonomy script to resolve shorthand ("M10", "persistence bugs") to exact values.
3. **Build the search command.** Use `gh issue list` with the appropriate combination of `--label`, `--milestone`, `--state`, and `--search` flags. For complex queries, combine multiple filters.
4. **Present results.** Show a concise table with: issue number, title, labels, milestone, assignee, and state. Group by milestone when results span multiple milestones. Include a total count.

If no issues match, suggest broadening the search — e.g., relaxing the state filter or checking for typos in label names.

${input:query:What to search for — keywords, labels, milestone name, or status filter}
