---
name: roadmapStatus
description: Report current roadmap progress, active milestone, and remaining work
agent: agent
model: Claude Sonnet 4.6 (copilot)
tools:
  - 'read'
  - 'search'
---

Generate a progress report for the project roadmap.

**MANDATORY:** Load and apply the `roadmap-management` skill before producing any output. The skill defines the status reporting protocol.

Analyze [TODO.md](../../TODO.md) and report overall progress, per-milestone breakdown, active milestone, and next actionable task.

${input:scope:Optional — focus on a specific milestone number or leave blank for full report}
