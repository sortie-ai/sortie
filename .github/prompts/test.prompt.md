---
name: test
description: Generate comprehensive Go test coverage for implemented features — table-driven tests, subtests, test helpers, error semantics, fixtures, httptest servers, env-gated integration tests
argument-hint: Path to spec/plan, or description of what to test
agent: Tester
---

Generate test coverage for the implemented feature or changed code.

## Process

1. **Review the specification and implementation plan** (if provided) to understand the intended behavior and contracts.
2. **Analyze the actual implementation** across all layers — read the source files before writing any tests.
3. **Classify each test** by category: unit, unit with fixtures, unit with httptest, or integration (env-gated). Pick the lightest category that validates the behavior.
4. **Identify what requires test coverage:** services, domain logic, state transitions, adapters, edge cases, regression risks, error paths, and concurrency safety.
5. **Generate tests** following project conventions: table-driven patterns, `t.Parallel()`, `t.Helper()` in all helpers, `t.Cleanup()` for teardown, `errors.As()`/`errors.Is()` for error assertions, `t.TempDir()` for filesystem isolation.
6. **Verify** with `make test` — all tests must pass with `-race`.

Apply coding standards from [Go documentation guidelines](../instructions/go-documentation.instructions.md) and follow [Go environment guidelines](../instructions/go-environment.instructions.md) for any necessary environment variable setup.

${input:request:Path to spec/plan or description of what to test}
