# Integration Testing Protocol

Integration tests validate adapter behavior against real external services. They are separated from unit tests by file naming and environment gating.

## File Naming

Integration tests live in `integration_test.go` within the adapter package:

```
internal/tracker/jira/integration_test.go
internal/agent/claude/integration_test.go
```

## Environment Gates

Each adapter has its own gate variable. The test must skip — not fail — when the variable is absent.

| Adapter     | Gate Variable          | Required Env Vars                                                    |
| ----------- | ---------------------- | -------------------------------------------------------------------- |
| Jira        | `SORTIE_JIRA_TEST=1`   | `SORTIE_JIRA_ENDPOINT`, `SORTIE_JIRA_API_KEY`, `SORTIE_JIRA_PROJECT` |
| Claude Code | `SORTIE_CLAUDE_TEST=1` | (adapter-specific)                                                   |

Optional vars (e.g. `SORTIE_JIRA_ACTIVE_STATES`) enhance coverage but must not cause failure when absent.

## Skip Helper Pattern

Every integration test file must define and use a skip helper:

```go
func skipUnlessIntegration(t *testing.T) {
    t.Helper()
    if os.Getenv("SORTIE_JIRA_TEST") != "1" {
        t.Skip("skipping Jira integration test: set SORTIE_JIRA_TEST=1 to enable")
    }
}

func requireEnv(t *testing.T, key string) string {
    t.Helper()
    v := os.Getenv(key)
    if v == "" {
        t.Fatalf("required environment variable %s is not set", key)
    }
    return v
}
```

Call `skipUnlessIntegration(t)` as the first line of every integration test function. Call `requireEnv` only after the skip check passes.

## Config Builder

Build adapter config from env vars in a dedicated helper:

```go
func integrationConfig(t *testing.T) map[string]any {
    t.Helper()
    endpoint := requireEnv(t, "SORTIE_JIRA_ENDPOINT")
    apiKey := requireEnv(t, "SORTIE_JIRA_API_KEY")
    project := requireEnv(t, "SORTIE_JIRA_PROJECT")

    cfg := map[string]any{
        "endpoint": endpoint,
        "api_key":  apiKey,
        "project":  project,
    }
    // Add optional vars without fataling when absent
    if states := os.Getenv("SORTIE_JIRA_ACTIVE_STATES"); states != "" {
        // parse and add
    }
    return cfg
}
```

## Running Integration Tests

```bash
# Run Jira integration tests only
SORTIE_JIRA_TEST=1 make test PKG=./internal/tracker/jira/... RUN=Integration

# Run Claude Code integration tests only
SORTIE_CLAUDE_TEST=1 make test PKG=./internal/agent/claude/... RUN=Integration
```

## CI Behavior

- Without env vars: integration tests report as **skipped** (visible in output)
- With env vars: integration tests run and failures fail the job
- Integration tests never block the default `make test` pipeline

## Test Isolation

- Use isolated test identifiers and workspaces
- Clean up tracker artifacts when practical
- Do not rely on pre-existing external state — create what you need, verify, clean up

## Adding a New Integration Test Suite

When implementing a new adapter:

1. Create `integration_test.go` in the adapter package
2. Define `SORTIE_<ADAPTER>_TEST` gate variable
3. Implement `skipUnlessIntegration`, `requireEnv`, and config builder helpers
4. Document required env vars in the test file header comment
5. Add the run command to this reference doc
