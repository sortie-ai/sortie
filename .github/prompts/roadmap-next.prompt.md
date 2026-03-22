---
name: roadmapNext
description: Determine the next task to work on from the project roadmap
agent: agent
model: Claude Sonnet 4.6 (copilot)
tools:
  - 'read'
  - 'search'
---

Identify the next task to work on from the project roadmap.

**MANDATORY:** Load and apply the `roadmap-management` skill before producing any output. The skill defines the task selection protocol — sequential milestone ordering with dependency-aware task selection.

Read [TODO.md](../../TODO.md), find the first incomplete task in the active milestone, and return the task ID, full description, verify criteria, and any architecture sections to read before starting.

${input:context:Optional — constraints like "only backend tasks" or "skip research tasks"}
