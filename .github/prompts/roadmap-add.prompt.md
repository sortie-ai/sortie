---
name: roadmapAdd
description: Add a new task to the project roadmap (TODO.md)
agent: agent
model: Claude Sonnet 4.6 (copilot)
tools:
  - 'read'
  - 'search'
  - 'edit'
---

Add a new task to the project roadmap.

**MANDATORY:** Load and apply the `roadmap-management` skill before making any changes. The skill defines the exact task format, numbering scheme, milestone placement rules, and validation protocol. Read the skill's format specification reference before writing.

Add the described task to [TODO.md](../../TODO.md) following the Add operation workflow. After writing, run the validation script to confirm structural integrity.

${input:task:Describe the task to add — what needs to be done and how to verify it}
