---
name: 'Go Code Style'
description: 'Naming, control flow, comment structure, and idiomatic Go patterns — prohibits labeled/numbered inline comments and other non-idiomatic habits'
applyTo: '**/*.go'
---

# Go Code Style

Companion to [Go Documentation and Comments](go-documentation.instructions.md). This file covers naming, comment structure, control flow, and Go idioms. For godoc and exported symbol comments see that file; for structured logging see [Go Structured Logging](go-logging.instructions.md).

## Inline Comment Structure

Never prefix inline comments with labels, sequence numbers, or hierarchical markers. Write the comment content directly.

```go
// ❌ Labeled — the number is noise; the reason is what matters.
// Rule 1: required fields.
if issue.ID == "" || issue.Identifier == "" {
    return false
}

// ❌ Step / Section / Phase / Check variants.
// Step 1: dispatch preflight validation.
validation := ValidateDispatchConfig(params)

// ❌ Bare numeric prefixes.
// 1. Session started.
// 2. First token usage.
```

```go
// ✅ Reason first — state the invariant or constraint being enforced.
// Issues with missing required fields are not eligible for dispatch.
if issue.ID == "" || issue.Identifier == "" {
    return false
}

// ✅ Describe what validation accomplishes, not its sequence position.
// Preflight triggers a defensive Reload() so the config snapshot below
// reflects the latest disk state.
validation := ValidateDispatchConfig(params)
```

The label (`Rule 4:`, `Step 2:`, `Section 3.5:`) carries no information the code does not already provide. A reader following the code does not need a counter; they need the **reason** for the guard.

**Prohibited patterns:**

| Pattern | Examples |
|---|---|
| Rule labels | `// Rule N:`, `// Rule Nb:` |
| Step labels | `// Step N:`, `// Step N.N:` |
| Section labels | `// Section N.N:` |
| Bare numbers | `// N.`, `// N:`, `// 1.`, `// 2.` |
| Phase / Check | `// Phase N:`, `// Check N:` |

When a sequence genuinely matters (e.g., a three-phase commit), use a prose description of *what* each phase does rather than numbering it.

### No Internal References in Comments

Never cite `docs/architecture.md`, `docs/decisions/`, section numbers, ADR numbers, or internal project tracker tickets (e.g., `SORT-42`) in any comment. Source files must stand alone; internal references create maintenance debt and break when documents are reorganized or the tracker migrates.

Upstream external references — Go issue tracker (`golang/go#NNNNN`), GitHub issues in third-party repos, RFC numbers, CVE IDs — are permitted when they explain a non-obvious workaround or constraint that cannot be fully described in prose alone. The reference must accompany an explanation, not replace it.

```go
// ❌ Section reference — useless if the doc is renamed or restructured.
// Section 3.4: continuation turns must produce shorter output than first turns.
if turn > 1 && len(output) >= len(prev) {

// ❌ ADR reference — meaningless to someone reading the code cold.
// ADR-0003: use modernc.org/sqlite to avoid CGo.
db, err := sql.Open("sqlite", path)

// ❌ Internal ticket — rots when the tracker migrates.
// SORT-42: workaround for upstream pagination off-by-one.
offset := 0

// ❌ Upstream reference with no explanation — the link alone is useless.
// golang/go#22315
n := runtime.NumCPU()

// ✅ State the invariant directly — no internal document required.
// Continuation turns must be shorter than the first turn; longer output
// indicates the model is re-summarizing instead of continuing.
if turn > 1 && len(output) >= len(prev) {

// ✅ Name the constraint in the code itself.
// modernc.org/sqlite is the only permitted SQLite driver — CGo breaks
// the single-binary zero-dependency deployment model.
db, err := sql.Open("sqlite", path)

// ✅ Describe the workaround and cite the upstream root cause.
// Upstream pagination returns one extra item on the last page; discard
// the duplicate on receipt.
offset := 0

// ✅ Upstream reference paired with an explanation.
// strings.Clone forces a heap allocation so the GC can collect the
// original large buffer; without it the substring pins the whole input.
// See golang/go#40200 for the compiler limitation that makes this necessary.
s = strings.Clone(large[:n])
```

## Naming

### Variables

Prefer names proportional to scope. Short names (`i`, `err`, `ok`, `n`, `id`) are appropriate inside short blocks. Use longer names at package scope or when a short name would be ambiguous.

Never use `data`, `info`, `val`, `item`, `obj`, or `result` as variable names. Name the thing.

```go
// ❌ Generic — tells nothing about what is being iterated or processed.
for _, item := range issues {
    data := fetch(item)
}

// ✅ Names match domain vocabulary.
for _, issue := range issues {
    detail, err := fetch(issue.ID)
}
```

### Acronyms

Treat acronyms as words in mixed-case identifiers, consistent with the Go standard library:

```go
// ✅ Correct.
issueID    string
sessionID  string
apiURL     string
httpClient *http.Client

// ❌ Incorrect — breaks Go stdlib convention.
issueId    string
sessionId  string
apiUrl     string
httpClient *http.Client // Http instead of HTTP
```

### Booleans

Name booleans as affirmative predicates. Negative names produce double negations in conditions.

```go
// ✅ Readable in conditions: if isRunning { ... }
var isRunning bool
var hasAvailableSlots bool

// ❌ Produces double negation: if !notRunning { ... }
var notRunning bool
var noSlotsAvailable bool
```

## Control Flow

### Early Return

Return (or `continue`) at guard clauses and error conditions. Do not nest the happy path inside conditionals.

```go
// ❌ Nested happy path — every guard adds one indentation level.
func process(issue domain.Issue) error {
    if issue.ID != "" {
        if !state.IsRunning(issue.ID) {
            return dispatch(issue)
        }
    }
    return nil
}

// ✅ Flat — guards are at the top, happy path at the bottom.
func process(issue domain.Issue) error {
    if issue.ID == "" {
        return nil
    }
    if state.IsRunning(issue.ID) {
        return nil
    }
    return dispatch(issue)
}
```

### No Else After Return

When an `if` block ends with `return`, `continue`, or `break`, the `else` clause is unnecessary.

```go
// ❌ Redundant else.
if err != nil {
    return err
} else {
    doWork()
}

// ✅ Flat.
if err != nil {
    return err
}
doWork()
```

### Switch Over Long If-Else Chains

Use `switch` when comparing a single expression against three or more values.

```go
// ❌ Chain — hard to scan.
if state == "open" {
    ...
} else if state == "in_progress" {
    ...
} else if state == "done" {
    ...
}

// ✅ Switch — each case aligns.
switch state {
case "open":
    ...
case "in_progress":
    ...
case "done":
    ...
}
```

## Error Messages

Error strings are lowercase with no trailing punctuation. Callers wrap them; the chain reads left-to-right.

```go
// ✅ Lowercase, no period.
fmt.Errorf("failed to open workspace: %w", err)
fmt.Errorf("issue %s not found", id)

// ❌ Capital letter or trailing punctuation — breaks wrapping.
fmt.Errorf("Failed to open workspace: %w", err)
fmt.Errorf("issue %s not found.", id)
```

Include the resource identifier being operated on so the wrapped chain is useful:

```go
// ✅ Caller can identify which workspace and why it failed.
fmt.Errorf("prepare workspace for issue %s: %w", issue.ID, err)

// ❌ No context — caller cannot tell which workspace.
fmt.Errorf("prepare workspace: %w", err)
```

## Struct Initialization

Initialize only non-zero fields. Explicitly setting zero values adds noise and implies they were chosen deliberately when they were not.

```go
// ✅ Only meaningful fields — intent is unambiguous.
entry := RunningEntry{
    Identifier: issue.Identifier,
    StartedAt:  time.Now(),
}

// ❌ Redundant zero values — reader must verify these are intentional.
entry := RunningEntry{
    Identifier: issue.Identifier,
    SessionID:  "",
    StartedAt:  time.Now(),
    ExitCode:   0,
}
```

## Type Assertions

Always use the two-result form in production code. The single-result form panics on type mismatch.

```go
// ✅ Safe.
v, ok := x.(SomeInterface)
if !ok {
    return fmt.Errorf("unexpected type %T for x", x)
}

// ❌ Panics if x is not SomeInterface.
v := x.(SomeInterface)
```

Exception: inside a `switch x.(type)` block, single-result assertions on case branches are idiomatic and safe.

## Imports

Group imports into three blocks separated by blank lines: stdlib, external modules, internal packages. `goimports` enforces this automatically via `make fmt`.

```go
import (
    "context"
    "fmt"
    "time"

    "golang.org/x/sync/errgroup"

    "github.com/sortie-ai/sortie/internal/domain"
    "github.com/sortie-ai/sortie/internal/logging"
)
```

Never use dot imports (`. "pkg"`). Never use blank imports (`_ "pkg"`) in non-`main` packages without a comment explaining the side effect being triggered.

## Go 1.22+ Idioms

This project targets Go 1.26. Use the modern forms below; without explicit instruction, Copilot defaults to pre-1.22 patterns.

### Loop variable scoping

Go 1.22 gives each loop iteration its own copy of the loop variables. The `v := v` shadowing workaround is no longer needed and should not be written.

```go
// ❌ Stale — unnecessary and misleading since Go 1.22.
for _, url := range urls {
    url := url
    go func() { fetch(url) }()
}

// ✅ Correct in Go 1.22+.
for _, url := range urls {
    go func() { fetch(url) }()
}
```

### Range over integers

```go
// ❌ C-style three-clause loop.
for i := 0; i < n; i++ { ... }

// ✅ Go 1.22+.
for i := range n { ... }
```

### Built-in `min` and `max`

Never write helper functions for min/max. The builtins accept any ordered type and two or more arguments.

```go
// ❌ Unnecessary helper.
func maxInt(a, b int) int { if a > b { return a }; return b }

// ✅ Built-in since Go 1.21.
hi := max(a, b)
clamped := max(lo, min(hi, v))
```

### `slices` and `maps` packages

Use the standard-library `slices` and `maps` packages for collection operations. Do not write helpers that duplicate their functionality.

```go
import ("maps"; "slices")

// ❌ Hand-rolled contains check and sort.
found := false
for _, v := range allowed { if v == role { found = true; break } }
sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })

// ✅ slices/maps equivalents.
found := slices.Contains(allowed, role)
slices.SortFunc(users, func(a, b User) int { return cmp.Compare(a.Name, b.Name) })
for k, v := range slices.Sorted(maps.All(m)) { ... } // sorted map iteration
```

### `strings.Cut`

Use `strings.Cut` to split a string on a delimiter. Do not use `strings.SplitN(s, sep, 2)`.

```go
// ❌ SplitN — does not signal whether the delimiter was found.
parts := strings.SplitN(line, "=", 2)
key, value := parts[0], parts[1]

// ✅ Cut — returns a found flag and handles the missing-delimiter case cleanly.
key, value, ok := strings.Cut(line, "=")
if !ok { ... }
```

### `cmp.Or` for fallback chains

```go
// ❌ Verbose if-chain.
name := envName
if name == "" { name = configName }
if name == "" { name = "default" }

// ✅ cmp.Or returns the first non-zero value.
name := cmp.Or(envName, configName, "default")
```
