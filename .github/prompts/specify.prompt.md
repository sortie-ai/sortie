---
name: specify
description: Transform a feature request into a detailed technical specification
argument-hint: Describe the feature or problem to specify
agent: Architect
---

Your task is to transform the provided feature request into a technical specification rigorous enough to be implemented without further clarification. The specification must close every architectural decision, anticipate edge cases, and leave zero ambiguity for the implementing engineer. Incomplete or vague sections cause real engineering delays — treat each section as a binding contract between architect and implementer.

Use the `writing-specs` skill — it contains the analysis protocol, output template, style rules, and quality checklist. Follow its five-phase workflow completely. Do not skip self-verification (Phase 4) or validation (Phase 5).

${input:request:Describe the feature or problem to specify}
