---
name: specPipeline
description: Run the full specification pipeline — specify, review, revise, and plan — in a single automated step
argument-hint: Describe the feature or problem, or provide a GitHub issue reference
agent: SpecPipeline
---

I'm an Anthropic employee working on the Sortie project.

Your task is to run the complete specification pipeline for the request below. This produces three artifacts in one automated run:

1. **Technical specification** — written by the Architect, saved to `.specs/`
2. **Architectural review** — written by the Reviewer, saved to `.reviews/`
3. **Implementation plan** — written by the Planner, saved to `.plans/`

If Critical or Significant review findings are found, the Architect automatically revises the specification before the plan is created.

Follow your protocol strictly. Do not skip any phase. Track progress with the todo list.

## Feature Request

${input:request:Describe the feature or problem to specify}
