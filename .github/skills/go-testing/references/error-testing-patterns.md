# Error Testing Patterns

Sortie defines custom error types throughout its domain layer. Each carries structured context beyond a message string. Tests must validate error semantics through these types.

## Domain Error Types

| Type            | Key Field               | Package     | Use                        |
| --------------- | ----------------------- | ----------- | -------------------------- |
| `TrackerError`  | `Kind TrackerErrorKind` | `domain`    | Tracker adapter failures   |
| `ConfigError`   | `Field string`          | `config`    | Configuration validation   |
| `PathError`     | `Op string`             | `workspace` | Workspace path operations  |
| `TemplateError` | `Kind, Line, Source`    | `prompt`    | Template parsing/rendering |

## Assertion Helper Template

Each package that tests typed errors defines its own assertion helper. Do not create a shared package.

```go
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

Adapt this pattern for each error type:

- `assertConfigErrorField(t, err, wantField string)`
- `assertPathErrorOp(t, err, wantOp string)`
- `requireTrackerError(t, err) *domain.TrackerError` (returns the unwrapped error for further inspection)

## Table-Driven Error Testing

When testing config validation or input parsing with many error cases:

```go
tests := []struct {
    name     string
    config   map[string]any
    wantErr  bool
    wantKind domain.TrackerErrorKind
}{
    {
        name:   "valid config",
        config: validConfig("https://test.atlassian.net"),
    },
    {
        name:     "missing endpoint",
        config:   map[string]any{"api_key": "u@t.com:tok", "project": "P"},
        wantErr:  true,
        wantKind: domain.ErrTrackerPayload,
    },
}
```

## Error Chain Traversal

Use `errors.As` for type unwrapping and `errors.Is` for sentinel matching. Never inspect `.Error()` strings.

```go
// Type check through wrapped chain
var pe *PathError
if errors.As(err, &pe) {
    // pe is the first PathError in the chain
}

// Sentinel check
if errors.Is(err, context.DeadlineExceeded) {
    // timeout somewhere in the chain
}
```

## When to Fatal vs Error on nil

- `t.Fatal` when a nil error means the next assertion will panic (e.g., accessing fields on a nil pointer)
- `t.Error` when subsequent assertions remain meaningful even without the error
