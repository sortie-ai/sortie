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
// Correct — typed attributes, compile-time safe.
logger.Info("workspace prepared",
    slog.String("workspace", wsResult.Path),
    slog.Int("attempt", attempt))

// Wrong — positional key-value pairs, silent corruption on mismatch.
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

## Log Levels

| Level   | Semantics                                                                                       |
|---------|-------------------------------------------------------------------------------------------------|
| `Debug` | Internal detail useful only during development: timer values, stale-entry detection, raw stderr |
| `Info`  | Operator-visible lifecycle events: startup, workspace ready, session started, turn completed    |
| `Warn`  | Degraded but recoverable: config clamping, stale timers, hook failures, no available slots      |
| `Error` | Failed operations requiring attention: persistence errors, API failures, panics                  |

Rules:

- Every `Error` must include an `"error"` attribute with the original `error` value (not `.Error()` string).
- `Warn` is for things the operator should know but that the system handles automatically. If the system cannot continue, use `Error`.
- `Debug` should be cheap to produce. Guard expensive serialization behind `logger.Enabled(ctx, slog.LevelDebug)`.
- Never use `slog.Log` with custom numeric levels. Four levels are enough.

## Message Formatting

Messages are lowercase verb phrases describing the action and its outcome:

```
"workspace prepared"          — success, Info
"turn exit reason indicates failure" — degraded, Warn
"failed to persist retry entry"      — failure, Error
```

Rules:

- Start with a lowercase verb or past participle. No sentence-case, no trailing period.
- Include the **outcome** in the message itself (`completed`, `failed`, `retrying`, `skipped`).
- Keep messages stable across releases — operators build alerts on them. Changing a message string is a behavioral change.
- Never interpolate variable data into the message string. Variables go in attributes:

```go
// Correct — stable message, variable in attribute.
logger.Error("failed to persist retry entry",
    slog.String("issue_id", issueID),
    slog.Any("error", err))

// Wrong — message changes with every call, breaks alerting.
logger.Error(fmt.Sprintf("failed to persist retry entry for %s: %v", issueID, err))
```

## Error Logging

- Log errors at the **point of decision**, not at every intermediate return. If a function returns an error to its caller, it should not also log it — that produces duplicates.
- The orchestrator layer (`internal/orchestrator`) is the primary decision point. Adapter and domain packages return errors; the orchestrator logs them.
- Use `slog.Any("error", err)` to preserve the full error chain. Never `slog.String("error", err.Error())` — it strips `errors.Is`/`errors.As` structure from log processors that support structured errors.
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

Use `slogtest.Handler` or a `bytes.Buffer`-backed `TextHandler` to assert log output in tests. Never assert on exact timestamp values. Assert on the presence and correctness of structured attributes.
