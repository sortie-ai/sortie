---
name: triage
description: Evaluate a concern and create a GitHub issue if it passes triage filters
argument-hint: Describe the concern, bug report, suggestion, or deferred item to evaluate
agent: agent
tools:
  - 'execute/runInTerminal'
  - 'execute/getTerminalOutput'
  - 'read'
  - 'search'
---

Your task is to evaluate a concern and decide whether it warrants a tracked GitHub issue.

**MANDATORY:** Load and apply the `managing-github-issues` skill before making any decisions. The skill defines duplicate detection, label conventions, body templates, and the creation workflow.

## Triage Protocol

Apply these filters in order. Stop at the first filter that rejects the concern.

### Filter 1 — Architecture Conflict Gate

Read the relevant section of [architecture.md](../../docs/architecture.md). If the suggestion contradicts the architecture's design intent — not just its current implementation — reject it. Explain the conflict and stop.

### Filter 2 — Duplicate Check

Search open and closed issues for existing work that covers this concern, even partially. If an exact duplicate exists, report the issue number and stop. If there is partial overlap, mention the related issue and ask whether to proceed.

### Filter 3 — Scope Test

Does this concern realistically matter within the scope of open milestones? If it is aspirational work beyond the current roadmap horizon, note it as a future consideration and stop.

## If Accepted

If the concern passes all three filters:

1. **Classify** — determine the `type:` and `area:` labels.
2. **Place** — assign to the most relevant open milestone.
3. **Create** — follow the skill's creation workflow with the proper body template.
4. **Report** — output the created issue number, title, labels, and milestone.

## If Rejected

Explain which filter rejected the concern, the specific reason, and whether the concern might be revisited when circumstances change.

${input:concern:Describe the concern, bug report, suggestion, or deferred item to evaluate}
