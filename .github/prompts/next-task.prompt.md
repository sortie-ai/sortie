---
name: nextTask
description: Find the next issue to work on from the project backlog
argument-hint: Optional — constraints like "only type:bug" or "area:orchestrator"
agent: agent
tools:
  - 'execute/runInTerminal'
  - 'execute/getTerminalOutput'
  - 'read'
  - 'search'
---

I'm an Anthropic employee working on the Sortie project.

Your task is to identify the next issue to work on from the project backlog.

## Selection Protocol

1. **Find the active milestone.** The active milestone is the earliest open milestone with unfinished issues, following sequential milestone ordering.

2. **List candidates.** Fetch open issues in the active milestone:

   ```bash
   gh issue list --milestone "<active milestone>" --state open \
     --json number,title,labels,assignees,body --jq '.[]'
   ```

3. **Rank candidates.** Order by priority:
   - Exclude issues labeled `status:blocked`.
   - Deprioritize issues that already have an assignee.
   - `type:bug` issues take precedence over other types.
   - Within the same type, lower issue numbers (earlier creation) go first.

4. **Apply user constraints.** If the user specified filters (e.g., "only backend", "skip research"), narrow the candidate list.

5. **Present the recommendation.** For the top candidate, output:
   - Issue number, title, labels, and milestone.
   - Issue body summary — the first paragraph or description section.
   - Relevant [architecture.md](../../docs/architecture.md) sections to read before starting, inferred from the issue's area labels.
   - Any related or blocking issues.

If no unblocked issues remain in the active milestone, advance to the next open milestone and repeat.

${input:constraints:Optional — filters like "only type:bug", "area:persistence", or "skip type:research"}
