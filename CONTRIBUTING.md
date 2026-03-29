# Contributing to Sortie

Sortie turns issue tracker tickets into autonomous coding agent sessions — a single Go
binary that orchestrates workspaces, retries, and state reconciliation for AI coding
agents. The full picture is in the [README](README.md).

The project follows a spec-first model: [docs/architecture.md](docs/architecture.md)
defines every entity, state machine, and validation rule. The implementation conforms to
it. This matters because if you are fixing a bug in the orchestrator, the architecture
doc tells you what the correct behavior *is*. You do not need to reverse-engineer intent
from the code.

## Finding something to work on

Browse [open issues](https://github.com/sortie-ai/sortie/issues) and look for the
labels `good first issue` and `help wanted`. These are curated for newcomers and do not
require deep familiarity with the codebase.

If nothing catches your eye, test coverage and documentation fixes are always useful and
require no prior discussion:

```bash
# Find packages with low coverage
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | grep -v '100.0%' | sort -k3 -n
```

For larger work — new features, new adapters, architectural changes — open an issue
first to discuss the approach.

## Setup

**Requirements:** Go 1.26.1 (see [go.mod](go.mod)), golangci-lint.

```bash
git clone https://github.com/sortie-ai/sortie.git
cd sortie
make test    # runs all tests with -race
make build   # compiles to ./sortie
make lint    # golangci-lint
make fmt     # gofmt + goimports
```

All commands go through the Makefile. If `make test` passes, the change is safe to
submit.

The SQLite dependency is [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) —
a pure-Go driver. No C compiler needed.

### Integration tests

Integration tests are gated by environment variables and skip cleanly without them:

```bash
SORTIE_JIRA_TEST=1 SORTIE_JIRA_ENDPOINT="..." SORTIE_JIRA_API_KEY="..." \
  make test PKG=./internal/tracker/jira/...

SORTIE_GITHUB_TEST=1 SORTIE_GITHUB_TOKEN="ghp_..." SORTIE_GITHUB_PROJECT="owner/repo" \
  make test PKG=./internal/tracker/github/...

SORTIE_CLAUDE_TEST=1 ANTHROPIC_API_KEY="sk-..." \
  make test PKG=./internal/agent/claude/...
```

GitHub integration tests also accept an optional `SORTIE_GITHUB_ISSUE_ID` variable
(a valid issue number in the configured repo) to enable `FetchIssueByID` and
`FetchIssueStatesByIDs` test cases.

You do not need access to Jira, GitHub, or Claude to contribute. The unit test suite covers the
vast majority of the codebase.

## Making a change

**Small changes** (docs, typos, test coverage, bug fixes in a single package): fork,
fix, test, submit a PR. No ceremony needed.

**Medium changes** (multi-file bug fixes, new test scenarios, adapter improvements):
read the relevant section of [docs/architecture.md](docs/architecture.md) before
implementing. The spec defines what correct behavior looks like, so reading it first
prevents wasted effort.

**Large changes** (new features, new adapters, orchestrator changes): open an issue to
discuss the design before writing code. The architecture doc is the source of truth —
changes that contradict it will not be merged. If you believe the spec itself should
change, that is a valid conversation to have in an issue.

## Project layout

```
cmd/sortie/            entry point, CLI wiring
internal/
  agent/               agent adapters (claude/, mock/)
  config/              typed config, defaults, env-var resolution
  domain/              pure types, interfaces, error categories (imports nothing)
  logging/             structured slog helpers (imports nothing)
  maputil/             generic map utility helpers (imports nothing)
  orchestrator/        dispatch, retry, reconciliation, state machine
  persistence/         SQLite store, migrations, retry queues
  prompt/              text/template rendering, strict mode
  registry/            adapter registration
  server/              HTTP API, dashboard, metrics
  tool/                client-side tool framework (trackerapi/)
  tracker/             tracker adapters (jira/, github/, file/)
  workflow/            WORKFLOW.md parser, file watcher
  workspace/           filesystem isolation, path safety, hook execution
docs/
  architecture.md      the specification (~2100 lines)
  decisions/           Architecture Decision Records (ADRs)
```

Imports flow downward. `domain/` and `logging/` sit at the bottom with no internal
dependencies. Adapters (`tracker/*`, `agent/*`) implement interfaces defined in
`domain/` and never import each other or the orchestrator.

## Code conventions

The linter config in [.golangci.yml](.golangci.yml) catches most issues. Beyond what the
linter enforces:

- **Naming:** core packages use generic names (`agent_*`, `tracker_*`, `session_*`).
  Adapter-specific names (`jira_*`, `claude_*`) belong only inside their adapter package.
- **Errors:** wrap with context (`fmt.Errorf("operation: %w", err)`), no capitals, no
  trailing punctuation. Use the error categories in `internal/domain/errors.go`.
- **Logging:** `log/slog` with typed `slog.Attr` constructors. Derive loggers via
  `logging.WithIssue` and `logging.WithSession`.
- **Templates:** `Option("missingkey=error")` on every `text/template` — strict mode is
  mandatory.

## Testing conventions

- Table-driven tests with `t.Parallel()` at both test and subtest level.
- `t.Helper()` as the first line of every test helper.
- `t.TempDir()` for filesystem isolation, `t.Setenv()` for environment variables.
- Error assertions via `errors.As()` / `errors.Is()`, never string matching.
- Failure format: `FuncName(input) = got, want expected`.
- Standard library only — no third-party assertion frameworks.
- Fixtures live in `testdata/` within each package.

## Commits and PRs

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(orchestrator): add stall detection for running sessions
fix(workspace): reject symlinks escaping workspace root
test(tracker): cover pagination edge cases in Jira adapter
```

PRs use the [template](.github/pull_request_template.md). One logical change per PR.
CI runs `make lint` and `make test` — both must pass.

## What will not be merged

- CGo or any dependency requiring a C compiler.
- Adapter-specific logic in core packages (`internal/orchestrator/`, `internal/domain/`).
- Weakened workspace path containment or input sanitization — these are security
  boundaries.
- Behavior that contradicts [docs/architecture.md](docs/architecture.md).

If you are unsure whether a change fits, open an issue. A five-minute conversation saves
hours of work.

## AI-assisted contributions

Sortie is primarily developed with AI coding agents. Contributions using AI tools are
welcome under the same quality bar: the code must be correct, spec-conformant, tested,
and reviewed by you before submitting. `make test` and `make lint` must pass — not just
"the agent said it works."

## Security

Workspace path containment and input sanitization are security boundaries, not
convenience features. Changes to `internal/workspace/` receive additional scrutiny. If
you find a vulnerability, report it privately rather than opening a public issue.

## License

Contributions are licensed under [Apache License 2.0](LICENSE).
