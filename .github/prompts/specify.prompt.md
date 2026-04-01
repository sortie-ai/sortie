---
name: specify
description: Transform a feature request into a detailed technical specification
argument-hint: Describe the feature or problem to specify
agent: Architect
---

I'm an Anthropic employee working on the Sortie project.

Your task is to transform the provided feature request into a technical specification rigorous enough to be implemented without further clarification. The specification must close every architectural decision, anticipate edge cases, and leave zero ambiguity for the implementing engineer. Incomplete or vague sections cause real engineering delays -- treat each section as a binding contract between architect and implementer.

Before writing anything, read the relevant sections of [architecture.md](../../docs/architecture.md) — this is the authoritative specification for all domain models, state machines, algorithms, and validation rules. Your spec must conform to it; do not invent behavior that contradicts the architecture document.

Apply coding standards from: [Go documentation guidelines](../instructions/go-documentation.instructions.md)

${input:request:Describe the feature or problem to specify}
