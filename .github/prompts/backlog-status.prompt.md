---
name: backlogStatus
description: Report project progress across GitHub milestones and issues
argument-hint: Optional — scope to a specific milestone like "M10" or leave blank for full report
agent: agent
tools:
  - 'execute/runInTerminal'
  - 'execute/getTerminalOutput'
---

I'm an Anthropic employee working on the Sortie project.

Your task is to generate a progress report for the project backlog from GitHub milestones and issues.

## Process

1. **Gather milestone data.** Query all milestones with their issue counts:

   ```bash
   gh api repos/{owner}/{repo}/milestones --paginate -q \
     '.[] | "\(.title)  state=\(.state)  open=\(.open_issues)  closed=\(.closed_issues)"'
   ```

2. **Identify the active milestone.** The active milestone is the earliest open milestone with remaining open issues, following the project's sequential ordering (lower milestone number first).

3. **Generate the report:**

   - **Overall progress** — total closed vs total open issues across all milestones, completion percentage.
   - **Per-milestone breakdown** — for each milestone show completion percentage and open/closed counts. Mark closed milestones as complete. Highlight the active milestone.
   - **Active milestone detail** — list its open issues with their labels, priority, and assignee.
   - **Blockers** — highlight any issues labeled `status:blocked` with their titles.

4. **Scope filtering.** If the user specifies a milestone, focus the detailed breakdown on that milestone only but still show the overall summary line.

${input:scope:Optional — milestone name like "M10" or "Self-Hosting", or leave blank for full report}
