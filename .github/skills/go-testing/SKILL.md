---
name: go-testing
description: "Write, review, and improve Go test code for this project. Use when creating tests, adding test cases, writing test helpers, reviewing test quality, or when asked to test any Go function — even without explicit mention of table-driven tests, subtests, or fixtures. Covers table-driven tests, subtests, parallel execution, test helpers, error semantics, fixture loading, httptest servers, env-gated integration tests, and adapter conformance. Do NOT use for benchmarks or performance profiling."
---

# Go Testing — Sortie Project

## Decision Framework

Before writing any test, determine which category it belongs to:

| Category               | Characteristics                                    | Run condition                                           |
| ---------------------- | -------------------------------------------------- | ------------------------------------------------------- |
| **Unit**               | Deterministic, no I/O, no network                  | Always (`make test`)                                    |
| **Unit with fixtures** | Reads `testdata/` files, uses `t.TempDir()`        | Always                                                  |
| **Unit with httptest** | Spins up `httptest.NewServer`, tests HTTP adapters | Always                                                  |
| **Integration**        | Talks to real external service                     | Env-gated: `SORTIE_JIRA_TEST=1`, `SORTIE_CLAUDE_TEST=1` |

Pick the lightest category that validates the behavior.

---

## Canonical Test Structure

Every test file in this project follows this skeleton. Internalize it — do not deviate.

```go
package pkg // or pkg_test for black-box

import (
    "testing"
    // stdlib, then project imports, then third-party
)

// --- Test helpers (file-scoped, before test functions) ---

func helperName(t *testing.T, args ...) ReturnType {
    t.Helper()
    // setup or assertion logic
    // use t.Cleanup() for teardown, never defer in helpers
}

// --- Tests ---

func TestFunctionName(t *testing.T) {
    t.Parallel()
    // ...
}
```

**Key rules this project enforces:**

1. `t.Helper()` is the first statement in every helper — no exceptions.
2. `t.Cleanup()` for teardown in helpers; `defer` only in test functions themselves.
3. `t.Parallel()` at both test and subtest level for independent cases.
4. `t.TempDir()` for filesystem isolation — never write to fixed paths.
5. `t.Setenv()` for environment variable isolation in tests.
6. Errors use `errors.As()` / `errors.Is()` — never string comparison.

---

## Table-Driven Tests

Use when multiple cases share identical execution logic. This is the dominant pattern in this project.

```go
func TestSanitizeKey(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name    string
        input   string
        want    string
        wantErr bool
    }{
        {"simple key", "ABC-123", "ABC-123", false},
        {"empty input", "", "", true},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()

            got, err := SanitizeKey(tt.input)

            if tt.wantErr {
                if err == nil {
                    t.Fatalf("SanitizeKey(%q) = %q, want error", tt.input, got)
                }
                return
            }
            if err != nil {
                t.Fatalf("SanitizeKey(%q) unexpected error: %v", tt.input, err)
            }
            if got != tt.want {
                t.Errorf("SanitizeKey(%q) = %q, want %q", tt.input, got, tt.want)
            }
        })
    }
}
```

**When NOT to use tables:** cases needing different setup, conditional mocking, or complex branching. Write separate `t.Run` blocks or separate test functions instead.

**Table struct conventions:**

- Always include `name string` as the first field
- Use `wantErr bool` for error presence; add `wantKind` field for typed error checking
- Use field names (not positional) when structs have more than 3 fields
- Include inputs in failure messages: `FuncName(%v) = %v, want %v`

---

## Error Testing

This project uses custom typed errors extensively. Test error semantics, never strings.

```go
// Domain error types: TrackerError, ConfigError, PathError, TemplateError
// Each has a Kind or Field for categorization

// Pattern: typed error assertion helper
func assertTrackerErrorKind(t *testing.T, err error, want domain.TrackerErrorKind) {
    t.Helper()
    if err == nil {
        t.Fatalf("expected error with kind %q, got nil", want)
    }
    var te *domain.TrackerError
    if !errors.As(err, &te) {
        t.Fatalf("error type = %T, want *domain.TrackerError", err)
    }
    if te.Kind != want {
        t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, want)
    }
}
```

**Error testing rules:**

- `errors.As()` for type assertion — validates the error chain, not just the top
- `errors.Is()` for sentinel comparison
- Test the `Kind`/`Field`/`Op` of typed errors, not `.Error()` strings
- `t.Fatal` when nil-error means subsequent assertions will panic; `t.Error` otherwise

---

## Test Helpers

Helpers belong at the top of the test file, before test functions. Each adapter package defines its own helpers — do not create a shared `testutil` package.

**Common helper patterns in this project:**

```go
// Factory helper — creates a valid test subject or fails
func mustAdapter(t *testing.T, config map[string]any) *JiraAdapter {
    t.Helper()
    a, err := NewJiraAdapter(config)
    if err != nil {
        t.Fatalf("NewJiraAdapter: %v", err)
    }
    return a.(*JiraAdapter)
}

// Fixture loader — reads testdata/ files
func loadFixture(t *testing.T, name string) []byte {
    t.Helper()
    data, err := os.ReadFile("testdata/" + name)
    if err != nil {
        t.Fatalf("reading fixture %s: %v", name, err)
    }
    return data
}

// Config builder — returns valid baseline config for modification
func validConfig(endpoint string) map[string]any {
    return map[string]any{
        "endpoint": endpoint,
        "api_key":  "user@test.com:api_token_123",
        "project":  "PROJ",
    }
}

// Resource cleanup helper
func closeStore(t *testing.T, s *Store) {
    t.Helper()
    if err := s.Close(); err != nil {
        t.Errorf("Close: %v", err)
    }
}
```

**Naming conventions:**

- `mustX` — creates X or fatals (setup that cannot fail gracefully)
- `validX` / `defaultX` — returns baseline config/params for test customization
- `loadFixture` — reads from `testdata/`
- `assertX` / `requireX` — assertion helpers (`require` fatals, `assert` errors)

---

## HTTP Adapter Testing

Adapter tests use `httptest.NewServer` with handler functions that return fixture data. Never mock the `http.Client` itself.

```go
func TestFetchIssues(t *testing.T) {
    t.Parallel()

    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Verify request details
        if got := r.Header.Get("Authorization"); got == "" {
            t.Error("missing Authorization header")
        }
        // Return fixture response
        w.Header().Set("Content-Type", "application/json")
        w.Write(loadFixture(t, "search_single_page.json"))
    }))
    defer srv.Close()

    adapter := mustAdapter(t, validConfig(srv.URL))
    issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"To Do"})
    if err != nil {
        t.Fatalf("FetchIssuesByStates: %v", err)
    }
    // Assert on normalized domain objects, not raw JSON
}
```

**Rules for httptest usage:**

- Verify request headers, query params, and method inside the handler
- Return fixture JSON from `testdata/` — do not inline large JSON strings
- Assert on domain-level objects after adapter normalization, not raw payloads
- Use `atomic` counters when verifying call counts across concurrent requests

---

## Integration Tests (Env-Gated)

Integration tests talk to real external services. They MUST be gated by environment variables and skip cleanly when disabled.

> Read [references/integration-tests.md](references/integration-tests.md) for the full integration testing protocol including skip helpers, required env vars, and CI configuration.

**Quick reference:**

```go
func skipUnlessIntegration(t *testing.T) {
    t.Helper()
    if os.Getenv("SORTIE_JIRA_TEST") != "1" {
        t.Skip("skipping Jira integration test: set SORTIE_JIRA_TEST=1 to enable")
    }
}
```

- File naming: `integration_test.go` (separate from unit tests)
- Skip with `t.Skip`, not silent pass — skipped tests are visible in output
- Use isolated test data; clean up artifacts when practical
- Never fail CI when env var is absent

---

## Adapter Conformance Testing

Every adapter (tracker or agent) must prove it satisfies the domain interface. Use compile-time interface checks and conformance test suites.

```go
// Compile-time interface satisfaction — place in test file
var _ domain.TrackerAdapter = (*JiraAdapter)(nil)
var _ domain.AgentAdapter = (*mockAgentAdapter)(nil)
```

**What conformance tests must cover (per architecture Section 17):**

- Normalized field mapping (issue state, priority, labels → domain types)
- Pagination handling (order preserved across pages)
- Error category mapping (transport, auth, API, payload → typed errors)
- Config validation (required fields, defaults, invalid combinations)

---

## Mock and Test Double Patterns

This project uses three kinds of test doubles — pick the lightest one that works.

| Double   | Purpose                                  | Example                                                        |
| -------- | ---------------------------------------- | -------------------------------------------------------------- |
| **Stub** | Returns fixed data                       | `validConfig()` returning a map                                |
| **Fake** | Simplified working implementation        | `internal/agent/mock` package, `internal/tracker/file` adapter |
| **Spy**  | Records interactions for later assertion | `httptest` handler with `atomic` counters                      |

**The mock agent adapter** (`internal/agent/mock/`) is a first-class adapter registered in the registry. Use it for orchestrator-level tests that need controllable agent behavior.

**Mock struct pattern:**

```go
type mockTrackerAdapter struct{}
var _ domain.TrackerAdapter = (*mockTrackerAdapter)(nil)

func (m *mockTrackerAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
    return nil, nil
}
// ... implement all interface methods
```

---

## Fixture Management

Store test data in `testdata/` within the package directory. Go tooling ignores this directory during builds.

```
internal/tracker/jira/testdata/
    search_single_page.json
    search_multi_page_1.json
    search_multi_page_2.json
    issue_detail.json
    comments.json
```

**Rules:**

- One `testdata/` directory per package that needs fixtures
- Name fixtures descriptively: `search_empty.json`, `malformed.json`, `comments_multi_page_1.json`
- Load via `loadFixture(t, name)` helper — never hardcode paths in test functions
- JSON fixtures should be real (or realistic) API responses, not minimal stubs

---

## Failure Message Format

Every assertion must produce a message diagnosable without reading the test source.

```
Format: FuncName(inputs) = got, want expected
```

```go
// Correct — includes function, input, got, want
t.Errorf("SanitizeKey(%q) = %q, want %q", tt.input, got, tt.want)
t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, want)

// Incorrect — missing context
t.Errorf("got %q, want %q", got, tt.want)
t.Error("wrong result")
```

- Always `got` before `want` in message ordering
- Use `%q` for strings (shows quotes and escapes), `%v` for general values
- Use `%d` for integers, `%f` for floats — match the type

---

## Validation Checklist

After writing or modifying tests, verify:

- [ ] `make test` passes with `-race` (the default)
- [ ] New test functions have `t.Parallel()` where appropriate
- [ ] All helpers call `t.Helper()` as first statement
- [ ] Error assertions use `errors.As()` / `errors.Is()`, not string comparison
- [ ] Failure messages include function name, inputs, got, and want
- [ ] Integration tests skip cleanly without their env var
- [ ] No external assertion libraries introduced (use stdlib + `cmp.Diff` if needed)
- [ ] Fixtures live in `testdata/` and are loaded via helper
- [ ] `t.TempDir()` used for any filesystem operations
