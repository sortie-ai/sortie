# Context7 Triage — Sortie Libraries

Use this reference at Step 2a (triage decisions) and Step 2b (executing
the workflow). It defines which libraries require Context7 validation
for this project, the exact `resolve-library-id` argument for each, the
categories of comments that do NOT require Context7, and the fallback
protocol when Context7 lacks coverage.

## Contents

- Libraries requiring Context7 validation
- External APIs requiring Context7 validation
- Comments that do NOT require Context7
- The "when in doubt, run it" rule
- Examples that look safe but are not
- Handling Context7 failures

## Libraries requiring Context7 validation

Sortie is a small-dependency Go service. The libraries below require
Context7 validation for any reviewer claim about their behavior, API
surface, correct usage, or limitations. The **Resolve as** column is
the exact first argument to pass to `resolve-library-id`.

### Direct dependencies (from `go.mod`)

| Library                           | Resolve as                                       |
|-----------------------------------|--------------------------------------------------|
| `modernc.org/sqlite`              | `modernc.org/sqlite` or `modernc sqlite`         |
| `github.com/fsnotify/fsnotify`    | `fsnotify`                                       |
| `github.com/prometheus/client_golang` | `prometheus/client_golang` or `prometheus-go` |
| `github.com/prometheus/client_model` | `prometheus/client_model`                     |
| `gopkg.in/yaml.v3`                | `gopkg.in/yaml.v3` or `go-yaml-v3`              |
| `golang.org/x/sys`                | `golang.org/x/sys`                               |

Pin version expectations to what `go.mod` actually declares — not to
`AGENTS.md`. If `AGENTS.md` disagrees with `go.mod`, treat `go.mod` as
ground truth for Context7 triage and raise the inconsistency in the
Step 6 summary.

If a reviewer comment concerns a library not on this list but still
external — for example, a new dependency introduced in the PR under
review — apply the same two-step workflow: try `resolve-library-id`
with the package path, then fall back to `pkg.go.dev` if not indexed.

### Project-forbidden libraries (never import)

The reviewer may suggest using one of these. The correct answer is
always "no" — any such suggestion is **Incorrect or Counterproductive**
and must be rejected, citing the architectural constraint. No Context7
query is needed.

| Library / toolchain                    | Reason for rejection                   |
|----------------------------------------|----------------------------------------|
| `github.com/mattn/go-sqlite3`          | CGo — breaks single-binary deployment |
| Any other CGo-based library            | Same — see AGENTS.md Never section     |
| C toolchain dependency                 | Violates zero-runtime-dependency goal  |

## External APIs requiring Context7 validation

Sortie integrates with external platforms through adapters. Reviewer
claims about these APIs are also `[C7-REQUIRED]`.

| API                                     | Resolve as                                |
|-----------------------------------------|-------------------------------------------|
| Jira Cloud REST API (tracker adapter)   | `atlassian/jira` or `jira cloud rest`     |
| GitHub REST API (CI, SCM, issues)       | `github/rest-api` or `github rest`        |
| GitHub GraphQL API (issue types, Projects) | `github/graphql` or `github graphql`   |
| GitHub Checks API                        | `github/checks` or `github checks`       |
| Claude Code CLI (agent adapter)          | `anthropic/claude-code` or `claude cli`  |
| OpenAI Codex CLI (agent adapter)         | `openai/codex`                           |
| GitHub Copilot CLI (agent adapter)       | `github/copilot-cli`                     |

If the reviewer's claim concerns a field name, endpoint path, auth
header, or pagination semantic, run Context7 before classifying.

## Comments that do NOT require Context7

The following categories of comments do not make external-library
claims and therefore do not require Context7:

- **Go standard library.** Go stdlib is backward-compatible and the
  training-data coverage is reliable. Do not call Context7 for `net/http`,
  `context`, `sync`, `database/sql`, `encoding/json`, `os/exec`, `time`,
  `log/slog`, or any other `pkg path starting with a bare identifier`.
- **Logic errors.** Off-by-one, wrong predicate, missing guard, unused
  variable. No library API is involved; the reviewer is reasoning
  about the code's own logic.
- **Architectural boundary violations.** `docs/architecture.md` and
  the accepted ADRs in `docs/decisions/` are authoritative. Context7
  cannot override project-internal design decisions. In particular,
  the layer import hierarchy, single-writer orchestrator invariant,
  workspace path safety, and adapter boundary rules are architecture
  questions, not library questions.
- **Naming, godoc, or formatting.** Not API behavior. Follow the
  project's Go documentation and code-style instructions.
- **Project-internal patterns already specified.** If `AGENTS.md`,
  `docs/architecture.md`, or `docs/decisions/` specifies a pattern,
  those files win — no external verification needed.
- **Makefile, shell, git, or OS semantics.** Not library behavior in
  the sense this protocol defines. If specific tooling behavior is
  disputed, verify it against the tool's own documentation, not
  Context7. Specifically, Go toolchain commands must go through the
  Makefile — never invoke `go` directly.
- **Concurrency correctness of Go code.** The race detector via
  `make test` (which runs with `-race`) is the authoritative check
  for data races. For synchronization patterns, reason from Go
  language semantics, not Context7.

## The "when in doubt, run it" rule

The default posture is cautious. A false positive (running Context7
when not strictly necessary) costs one tool call. A false negative
(skipping it when needed) costs a wrong classification plus any
downstream work built on that classification.

The asymmetry favors running it.

## Examples that look safe but are not

These look like they do not involve a library API but they do. Treat
each as **[C7-REQUIRED]**:

- "Use `db.Conn().Raw(...)` for this driver-specific call." The
  `modernc.org/sqlite` driver has distinct semantics from the CGo
  driver for `Raw` access. → [C7-REQUIRED].
- "Pass `context.Context` into `fsnotify.NewWatcher()`." `fsnotify`
  has specific cancellation patterns that differ between versions.
  → [C7-REQUIRED].
- "Use `prometheus.WrapRegistererWith(...)` instead of a direct
  registry." The recommended registerer composition has changed
  across client_golang versions. → [C7-REQUIRED].
- "Decode with `yaml.Node` for round-trip preservation." `yaml.v3`
  has specific `Node` semantics that differ from `v2`. → [C7-REQUIRED].
- "Use the `expand` field when creating a Jira issue." Jira Cloud
  REST API v3 changed `expand` behavior across versions.
  → [C7-REQUIRED].
- "Page GitHub API results with `per_page` and `Link` headers."
  GitHub pagination conventions vary across REST vs GraphQL and
  across endpoint families. → [C7-REQUIRED].

If you catch yourself thinking "I already know how this library works"
about any claim in the Libraries-requiring-Context7 table, treat that
as a signal to run Context7, not a license to skip it. Confidence is
the proximate cause of hallucination.

## Handling Context7 failures

### `resolve-library-id` returns no match

1. Try alternative names. Full import paths and short names are both
   worth attempting: `modernc.org/sqlite` ↔ `modernc sqlite`;
   `github.com/fsnotify/fsnotify` ↔ `fsnotify`;
   `github.com/prometheus/client_golang` ↔ `prometheus-go`;
   `gopkg.in/yaml.v3` ↔ `go-yaml-v3`.
2. Fall back to authoritative documentation. Priority order:
   - `pkg.go.dev/<import-path>` for Go packages.
   - The library's official docs site.
   - The library's GitHub README at the version pinned in `go.mod`.
   - For external APIs: the vendor's official REST/GraphQL reference.
   Do not use random blog posts or community Q&A as authoritative.
3. Record `[FALLBACK: web]` in the Library Evidence Table. Include
   the URL of the authoritative source in your internal reasoning so
   the evidence is auditable.
4. Proceed with classification as if Context7 had returned the same
   finding, per the Step 2d Binding Rules.

### `query-docs` returns irrelevant content

1. Narrow the `topic` parameter — use a single specific word (e.g.
   `transitions`, `pagination`, `webhooks`, `migrations`,
   `transactions`, `watcher`).
2. Rephrase the query to be more specific — name the exact symbol, the
   exact version, the exact scenario.
3. Reduce the `tokens` budget to force higher-relevance filtering.
4. If the best result still does not address the claim, classify the
   comment as **Needs Discussion** per Binding Rule 4. Do not guess.
