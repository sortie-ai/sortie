---
name: roadmapValidate
description: Validate TODO.md structural integrity against roadmap format rules
agent: agent
model: Claude Sonnet 4.6 (copilot)
tools:
  - 'read'
  - 'search'
  - 'execute'
---

Validate the project roadmap for structural integrity.

**MANDATORY:** Load and apply the `roadmap-management` skill before producing any output. The skill defines the validation protocol and format specification.

Run the validation script against [TODO.md](../../TODO.md):

```bash
python3 .github/skills/roadmap-management/scripts/validate_roadmap.py TODO.md
```

If violations are found, report each with its line number, the rule violated, and the suggested fix. If no violations are found, confirm the file is structurally sound with summary statistics.

${input:scope:Optional — "fix" to auto-fix violations, or leave blank for report only}
