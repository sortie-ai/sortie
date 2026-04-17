# Library Evidence Table — Template

Build this table at Step 2c. One row per **[C7-REQUIRED]** comment.
The table is evidence, not interpretation — classification happens in
Step 3.

## Blank template

Copy this into your analysis and fill one row per [C7-REQUIRED]
comment:

| # | Comment (summary) | Library | Context7 query (reformulated) | Context7 finding | Verdict |
|---|---|---|---|---|---|
| 1 |   |   |   |   |   |
| 2 |   |   |   |   |   |

## Filled examples (calibration)

These four examples show the level of specificity expected in each
column. Use them as a reference for column discipline, not as content
to copy. Examples are drawn from Sortie's Go stack.

| # | Comment (summary) | Library | Context7 query | Context7 finding | Verdict |
|---|---|---|---|---|---|
| 1 | "Use `modernc.org/sqlite` `sqlite3_busy_timeout` PRAGMA instead of `SetMaxOpenConns(1)`" | `modernc.org/sqlite` | "Does `modernc.org/sqlite` support `sqlite3_busy_timeout` PRAGMA, and is it an adequate substitute for `db.SetMaxOpenConns(1)` for single-writer serialization?" | `modernc.org/sqlite` exposes `busy_timeout` via PRAGMA, but it handles lock contention; it does not serialize writers, so concurrent writers still risk `SQLITE_BUSY`. The architecture's single-writer invariant requires `SetMaxOpenConns(1)`. | **REVIEWER INCORRECT** |
| 2 | "Pass the `context.Context` to `fsnotify.NewWatcher()` for cancellation" | `fsnotify` | "Does `fsnotify.NewWatcher()` accept a `context.Context` parameter in the current release?" | `fsnotify.NewWatcher()` takes no context. Cancellation is achieved by calling `watcher.Close()` from the shutdown path, typically in a `defer`. | **REVIEWER INCORRECT** |
| 3 | "Use `yaml.Node` for lossless round-trip of workflow front matter" | `gopkg.in/yaml.v3` | "Does `yaml.Node` in `yaml.v3` preserve comments, ordering, and style for round-tripping?" | `yaml.Node` preserves key order, comments, and style for round-trip encoding. Plain `map[string]any` does not. | **REVIEWER CORRECT — optional improvement** |
| 4 | "Switch to GitHub GraphQL `searchIssues` for duplicate detection — REST search is rate-limited harder" | GitHub GraphQL API | "Are GitHub GraphQL `search` nodes subject to a lower secondary-rate-limit than REST `/search/issues`?" | GraphQL and REST share the same secondary-rate-limit budget for search endpoints; GraphQL has no inherent advantage for `search`. | **REVIEWER INCORRECT** |

## Column discipline

- **Comment (summary)** — a short quote or paraphrase, not a full
  paragraph. Enough to identify the claim.
- **Library** — the name from the library table in the
  context7-triage reference (already loaded at Step 2a). Use the
  project's version designation when relevant ("yaml.v3", not just
  "yaml"; "Jira Cloud REST API v3", not just "Jira API").
- **Context7 query (reformulated)** — the specific question you sent
  to `query-docs`, not the reviewer's wording verbatim. Reformulating
  the reviewer's claim as a question forces you to extract the
  falsifiable kernel.
- **Context7 finding** — what the retrieved documentation actually
  says about the claim. Cite concretely. Never "Context7 confirms" or
  "Context7 agrees" — state the specific behavior the docs describe.
- **Verdict** — exactly one of:
  - **REVIEWER CORRECT**
  - **REVIEWER CORRECT — optional improvement** (the claim is valid
    but the suggestion is a non-mandatory refinement)
  - **REVIEWER INCORRECT**
  - **AMBIGUOUS** (or **VERSION-CONFLICTING**)
  - **FALLBACK: web** (Context7 not indexed; authoritative web or
    `pkg.go.dev` source consulted — the finding is still treated as
    authoritative)

The verdict feeds Step 3 via the mapping in the
classification-categories reference (loaded alongside this template
when the agent enters Step 3).
