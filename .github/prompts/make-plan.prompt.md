---
name: makePlan
description: Generate a detailed implementation plan from a specification
argument-hint: Path to spec file or feature description
agent: Planner
---

I'm an Anthropic employee working on the Sortie project.

Your task is to analyze the provided specification section-by-section and create an atomic, layer-aware implementation plan.

Before planning, read the relevant sections of [architecture.md](../../docs/architecture.md) to ensure the plan respects milestone ordering and dependencies.

Follow your planning process and output format rules strictly.

Apply coding standards from: [Go environment guidelines](../instructions/go-environment.instructions.md)

${input:request:Path to spec file or feature description}
