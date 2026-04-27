---
name: issueCreate
description: Create a properly labeled GitHub issue with milestone and project board assignment
argument-hint: Describe the bug, feature, or task to track
agent: agent
tools:
  - 'execute/runInTerminal'
  - 'execute/getTerminalOutput'
  - 'read'
  - 'search'
---

Your task is to create a GitHub issue for the described work item.

**MANDATORY:** Load and apply the `managing-github-issues` skill before creating any issue. The skill defines label conventions, body templates per issue type, duplicate detection, and the complete creation workflow. Follow the skill — do not improvise formatting or skip steps.

## Process

1. **Discover taxonomy.** Run the skill's taxonomy script to get current labels, milestones, and project board fields. Use this live data for all `--label` and `--milestone` flags.
2. **Classify the issue.** Determine exactly one `type:` label and at least one `area:` label.
3. **Check for duplicates.** Search open and closed issues using relevant keywords. If a duplicate exists, report it instead of creating a new one.
4. **Assign milestone.** Match the work to the most relevant open milestone. If the user omits the milestone, ask — do not guess.
5. **Write the body.** Use the skill's body template for the determined type. Every body must end with a `## Verification` section.
6. **Create the issue.** Use `gh issue create` with the assembled title, body, labels, and milestone.
7. **Add to project board.** Assign the issue to the project board with appropriate Status and Priority fields.

## Constraints

- Do not create issues with empty or placeholder bodies.
- Do not apply `status:` labels at creation time — those are set when work state changes.
- If the description is ambiguous about type classification, ask for clarification.

${input:description:Describe the bug, feature, or task — what needs to be done and why}
