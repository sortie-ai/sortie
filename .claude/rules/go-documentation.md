# Go Documentation and Comments

## Package Comments

- Every package must have a `// Package <name>` comment.
- Place the package doc comment in the file named after the package (e.g. `config.go` for package `config`, `prompt.go` for package `prompt`). When no such file exists, use the primary entry-point file. Never place it in `errors.go` or other secondary files.
- Only one file per package may carry the `// Package` doc comment — duplicates cause godoc to pick one arbitrarily.
- State what the package provides and its role within the architecture.
- Mention key types or functions the caller should start with.
- Do not reference internal architecture doc sections — package comments are public API surface.

## Exported Symbol Comments

- Begin with the symbol name as the grammatical subject: `// Setup initialises…`, `// WithIssue derives…`.
- Describe the **contract** (what holds after the call), not the implementation mechanics.
- Use `[TypeOrFunc]` bracket syntax for cross-references to related symbols (Go 1.19+ doc links).
- Include a short code example in the comment when the composition pattern is non-obvious.
- Mention concurrency safety when relevant ("safe for concurrent use").

## What to Avoid

- Do not restate the function signature in prose ("takes an io.Writer and returns an error").
- Do not reference `docs/architecture.md`, `docs/decisions/`, section numbers, ADR numbers, or ticket IDs in godoc comments. Those belong in specs and plans, not in the source.
- Do not write tutorial-style or conversational comments ("This is a helper that…", "We use this to…").
- Do not add comments to unexported symbols unless the logic is genuinely non-obvious.
- Do not add inline comments that merely repeat what the code already says.

## Adapter Packages (`internal/tracker/*`, `internal/agent/*`)

- The package comment must name the external system and the domain interface it implements (e.g. "Package jira implements [domain.TrackerAdapter] for Atlassian Jira Cloud REST API v3").
- Document which normalization rules the adapter applies (label casing, priority mapping, state mapping) so reviewers can verify conformance without reading the spec.
- Do not expose adapter-specific types or terminology in exported symbol names — all public surface uses domain vocabulary.

## Inline Comments

- Reserve inline comments for **why**, not **what**.
- Acceptable: explaining a non-obvious invariant, a safety constraint, or a workaround.
- Unacceptable: narrating control flow (`// loop over items`, `// return error`).

## Lint Suppression (`//nolint`)

- Always specify the linter name: `//nolint:errcheck`, never bare `//nolint`.
- Always include a justification on the same line: `//nolint:errcheck // best-effort cleanup in defer`.
- Prefer fixing the code over suppressing the diagnostic. Suppression is a last resort when the linter is provably wrong or the fix would harm readability.
- Never suppress `govet` or `staticcheck` — those catch real bugs.

## Style

- **Identifiers** (exported and unexported): American English, matching stdlib conventions (`Initialize`, `Normalize`, `Color`). Never use British spelling in symbol names.
- **Prose** (comments, doc strings, error messages): American English for consistency (`initialize`, `normalize`, `behavior`).
- **Package comments** may span multiple paragraphs when the package is a major architectural component (see `encoding/json`, `net/http` in stdlib). Use judgement — breadth of the package justifies breadth of the doc.
- **Function and type comments** should be a single short paragraph. If more is needed, the function is doing too much — simplify it before expanding the doc.
