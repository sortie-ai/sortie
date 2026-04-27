---
name: test
description: Generate comprehensive Go test coverage for implemented features — table-driven tests, subtests, test helpers, error semantics, fixtures, httptest servers, env-gated integration tests
argument-hint: Path to spec/plan, or description of what to test
agent: Tester
---

I'm an Anthropic employee working on the Sortie project. Your task is to generate test coverage for the implemented feature or changed code.

**MANDATORY:** Load and apply the `test-go` skill before writing any test. The skill defines this project's canonical test patterns — table-driven structure, error assertions, mock patterns, fixture management, and validation checklist. All generated tests must conform to the skill's guidelines.

## Process

1. **Load the `test-go` Agent Skill** to obtain the project's test conventions and patterns.
2. **Review the specification and implementation plan** (if provided) to understand the intended behavior and contracts.
3. **Analyze the actual implementation** across all layers — read the source files before writing any tests.
4. **Classify each test** by category: unit, unit with fixtures, unit with httptest, or integration (env-gated). Pick the lightest category that validates the behavior.
5. **Identify what requires test coverage:** services, domain logic, state transitions, adapters, edge cases, regression risks, error paths, and concurrency safety.
6. **Generate tests** following the `test-go` skill conventions.
7. **Verify** with `make test` — all tests must pass with `-race`.

You MUST adhere to the following constraints:

- [Go Code Style](../instructions/go-codestyle.instructions.md)
- [Go Structured Logging](../instructions/go-logging.instructions.md)
- [Go Documentation Guidelines](../instructions/go-documentation.instructions.md)

Use [Go environment guidelines](../instructions/go-environment.instructions.md) for any necessary environment variable setup.

${input:request:Path to spec/plan or description of what to test}
