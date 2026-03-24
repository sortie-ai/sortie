---
name: 'Go Structured Logging'
description: 'slog conventions, log levels, context fields, message formatting, error logging, and observability rules for Go code in Sortie'
applyTo: '**/*.go'
---

# Go Structured Logging

Sortie uses `log/slog` exclusively. No external logging libraries (zap, zerolog, logr) — the stdlib handler is sufficient and preserves the zero-dependency deployment model.

## Handler and Setup

- One `slog.TextHandler` configured at startup via `logging.Setup`. All loggers descend from `slog.Default()`.
- TextHandler in development, JSONHandler in production deployments (decided by the operator, not the code). The `logging` package owns handler construction — never create handlers elsewhere.
- Never call `slog.SetDefault` outside `logging.Setup`.

## Attribute Style

Use typed `slog.Attr` constructors exclusively. Never use the alternating `key, value` shorthand — it silently produces `!BADKEY` on arity mismatches.

```go
// ✅ Typed attributes, compile-time safe.
logger.Info("workspace prepared",
    slog.String("workspace", wsResult.Path),
    slog.Int("attempt", attempt))

// ❌ Positional key-value pairs, silent corruption on mismatch.
logger.Info("workspace prepared", "workspace", wsResult.Path, "attempt", attempt)
```

Enforce with `sloglint` (`attr-only: true`) in `.golangci.yml` when added.

## Context Fields (Architecture § 13.1)

Issue-related logs must carry `issue_id` and `issue_identifier`. Session lifecycle logs must also carry `session_id`. Use the composable constructors in `internal/logging`:

```go
logger := logging.WithIssue(base, issueID, issueIdentifier)
logger  = logging.WithSession(logger, sessionID)
```

Never attach these fields manually with `logger.With(...)` — the helper functions guarantee consistent key names and ordering.

### Logger Derivation Patterns

Derive the issue-scoped logger **once at function entry** (after a nil-logger guard if applicable), then use it for every log call in that scope. This eliminates repeated manual attributes and prevents accidental omissions.

**Single-issue functions** (e.g., exit handlers, retry handlers):

```go
func HandleWorkerExit(state *State, result WorkerResult, log *slog.Logger) {
    if log == nil {
        log = slog.Default()
    }
    log = logging.WithIssue(log, result.IssueID, result.Identifier)

    // All subsequent log calls carry issue_id + issue_identifier automatically.
    log.Info("worker exited", slog.Int("exit_code", result.ExitCode))
}
```

**Multi-issue loops** (e.g., reconciliation): derive a scoped logger per iteration. The allocation is negligible at O(10) concurrent sessions.

```go
for issueID, entry := range state.Running {
    entryLog := logging.WithIssue(log, issueID, entry.Identifier)
    entryLog.Warn("stall detected", slog.Int64("elapsed_ms", elapsed))
}
```

**Early returns before identifier is available**: use manual `slog.String("issue_id", issueID)` only when the identifier genuinely cannot be resolved (e.g., entry not found). After the identifier is known, switch to `WithIssue`.

```go
popped, exists := state.RetryAttempts[issueID]
if !exists {
    log.Debug("retry timer for unknown entry", slog.String("issue_id", issueID))
    return
}
// Identifier now available — derive scoped logger for remaining logic.
log = logging.WithIssue(log, issueID, popped.Identifier)
```

### Composing `WithSession`

When the session ID is known, compose `WithSession` onto the issue-scoped logger so all subsequent logs carry `session_id`. Apply conditionally — the session ID may be empty early in a lifecycle or absent entirely.

```go
log := logging.WithIssue(logger, issueID, entry.Identifier)
if entry.SessionID != "" {
    log = logging.WithSession(log, entry.SessionID)
}
```

Order matters: always `WithIssue` first, then `WithSession`. If the session ID changes mid-function (e.g., `EventSessionStarted` updates `entry.SessionID`), the logger chain carries the *previous* session ID. Log the new session ID as an explicit attribute where it differs from the chain value.

### Nil-Logger Guard

Functions accepting `*slog.Logger` must guard against nil at entry:

```go
if log == nil {
    log = slog.Default()
}
```

Place the guard before `WithIssue` derivation. One guard per function entry point is sufficient.

## Log Levels

| Level   | Semantics                                                                                       |
|---------|-------------------------------------------------------------------------------------------------|
| `Debug` | Internal detail useful only during development: timer values, stale-entry detection, raw stderr, agent event processing |
| `Info`  | Operator-visible lifecycle events: startup, workspace ready, session started, turn completed    |
| `Warn`  | Degraded but recoverable: config clamping, stale timers, hook failures, no available slots      |
| `Error` | Failed operations requiring attention: persistence errors, API failures, panics                  |

Rules:

- Every `Error` must include an `"error"` attribute with the original `error` value (not `.Error()` string).
- `Warn` is for things the operator should know but that the system handles automatically. If the system cannot continue, use `Error`.
- `Debug` should be cheap to produce. `slog` handlers skip formatting when the level is below threshold (zero allocation at `Info` default). No explicit `logger.Enabled()` guard is needed unless the attribute construction itself is expensive.
- Never use `slog.Log` with custom numeric levels. Four levels are enough.

### Hot-Path Debug Logging

Event handlers called on every agent event (e.g., `HandleAgentEvent`) use `Debug` level to avoid noise at `Info`. Include event-specific attributes:

- `EventSessionStarted`: `session_id`
- `EventTokenUsage`: `delta_input`, `delta_output`, `delta_total`
- Turn-finalization events: `turn_count`
- All others: `event_type` only

## Message Formatting

Messages are lowercase verb phrases describing the action and its outcome:

```
"workspace prepared"                 — success, Info
"turn exit reason indicates failure" — degraded, Warn
"failed to persist retry entry"      — failure, Error
"agent event processed"              — hot-path detail, Debug
```

Rules:

- Start with a lowercase verb or past participle. No sentence-case, no trailing period.
- Include the **outcome** in the message itself (`completed`, `failed`, `retrying`, `skipped`).
- Keep messages stable across releases — operators build alerts on them. Changing a message string is a behavioral change.
- Never interpolate variable data into the message string. Variables go in attributes:

```go
// ✅ Stable message, variable in attribute.
logger.Error("failed to persist retry entry",
    slog.Any("error", err))

// ❌ Message changes with every call, breaks alerting.
logger.Error(fmt.Sprintf("failed to persist retry entry for %s: %v", issueID, err))
```

## Error and Validation Attributes

### The `"error"` key

Reserve `"error"` exclusively for Go `error` values logged via `slog.Any("error", err)`. This preserves the full error chain for log processors that support `errors.Is`/`errors.As`.

```go
// ✅ error value via slog.Any — preserves chain.
logger.Error("persistence failed", slog.Any("error", err))

// ❌ Stringified error — strips unwrap chain.
logger.Error("persistence failed", slog.String("error", err.Error()))
```

### Non-error diagnostic strings

For diagnostic strings that are not Go `error` values (e.g., validation summaries, preflight results), use a descriptive key — never `"error"`:

```go
// ✅ Distinct key for non-error diagnostic.
logger.Warn("preflight check failed",
    slog.String("validation_error", validation.Error()))

// ❌ Collides with the error convention, confuses log processors.
logger.Warn("preflight check failed",
    slog.String("error", validation.Error()))
```

### Decision-point logging

- Log errors at the **point of decision**, not at every intermediate return. If a function returns an error to its caller, it should not also log it — that produces duplicates.
- The orchestrator layer (`internal/orchestrator`) is the primary decision point. Adapter and domain packages return errors; the orchestrator logs them.
- After a panic recovery, log at `Error` with the panic value and then continue — never `log.Fatal` or `os.Exit` from a worker goroutine.

## What Not to Log

- **Large payloads**: raw API responses, full issue bodies, workspace file trees. Summarize or omit.
- **Sensitive data**: API keys, tokens, credentials. Implement `slog.LogValuer` on types that carry secrets to redact them automatically.
- **Redundant context**: do not re-log fields already present in the logger chain (`issue_id` from `WithIssue`).
- **Control flow narration**: "entering function X", "about to call Y". Logs are for outcomes, not traces.

## Sink Failure (Architecture § 13.2)

If a log sink fails, Sortie must continue running. Never `log.Fatal` or `os.Exit` on a write error. Emit a warning through any remaining sink and carry on.

## Adding New Context Helpers

When a new cross-cutting context field is needed (e.g., `workspace_id`), add a `With*` function to `internal/logging` following the existing `WithIssue`/`WithSession` pattern. Do not create ad-hoc `.With()` calls scattered across packages.

## Testing

Use a `bytes.Buffer`-backed `slog.NewTextHandler` at `Debug` level to capture and assert log output in tests:

```go
var buf bytes.Buffer
h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
logger := slog.New(h)

// ... call function under test with logger ...

output := buf.String()
// Assert presence of structured attributes, not exact formatting.
if !strings.Contains(output, "issue_id=") { t.Error("missing issue_id") }
```

Rules:
- Never assert on exact timestamp values.
- Assert on attribute key presence and expected values, not full line formatting.
- For functions that accept `*slog.Logger`, pass the test logger — never rely on `slog.Default()` in tests.
