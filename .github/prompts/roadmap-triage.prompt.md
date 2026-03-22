---
name: roadmapTriage
description: Evaluate a concern and add it to the roadmap if it passes triage filters
agent: agent
model: Claude Sonnet 4.6 (copilot)
tools:
  - 'read'
  - 'search'
  - 'edit'
---

Triage a concern or suggestion into the project roadmap.

**MANDATORY:** Load and apply the `roadmap-management` skill before making any decisions. The skill defines the three-filter triage protocol: architecture conflict gate, redundancy check, and roadmap horizon test.

Evaluate the described concern against [TODO.md](../../TODO.md) and [architecture.md](../../docs/architecture.md). If it passes all three filters, add it as a new task in the appropriate milestone. If it fails any filter, explain why and do not modify TODO.md.

${input:concern:Describe the concern, suggestion, or deferred item to triage}
