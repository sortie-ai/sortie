---
name: makePlan
description: Generate a detailed implementation plan from a specification
argument-hint: Path to spec file or feature description
agent: Planner
---

Your task is to analyze the provided specification section-by-section and create an atomic, layer-aware implementation plan.

Before planning, read the relevant sections of [Architecture](../../docs/architecture.md) to ensure the plan respects milestone ordering and dependencies.

Follow your planning process and output format rules strictly.

You MUST adhere to the following constraints:

- [Go Code Style](../instructions/go-codestyle.instructions.md)
- [Go Structured Logging](../instructions/go-logging.instructions.md)
- [Go Documentation Guidelines](../instructions/go-documentation.instructions.md)

${input:request:Path to spec file or feature description}
