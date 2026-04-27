---
name: implPipeline
description: Run the full implementation pipeline — implement, check for spec deviations, and test — in a single automated step
argument-hint: Plan path, issue reference (#123), or feature description
agent: ImplPipeline
---

Your task is to run the complete implementation pipeline for the request below.

The pipeline:
1. **Assesses the input** — determines the route (plan-driven, issue-driven, or description-driven) and whether a specification is needed first
2. **Implements** — delegates to the Coder agent with full architectural context
3. **Checks for spec deviations** — inspects `.findings/` for issues discovered during implementation
4. **Tests** — delegates to the Tester agent with the implementation summary

Follow your protocol strictly. Do not skip any phase. Track progress with the todo list.

## Implementation Request

${input:request:Plan path, issue reference, or feature description}
