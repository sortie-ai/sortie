# Contributing to Sortie

Sortie turns issue tracker tickets into autonomous coding agent sessions - a single Go binary that orchestrates workspaces, retries, and state reconciliation for AI coding agents. The full picture is in the [README](README.md).

The project follows a spec-first model: [docs/architecture.md](docs/architecture.md) defines every entity, state machine, and validation rule. The implementation conforms to it. This matters because if you are fixing a bug in the orchestrator, the architecture doc tells you what the correct behavior *is*. You do not need to reverse-engineer intent from the code.

## Finding something to work on

Browse [open issues](https://github.com/sortie-ai/sortie/issues) and look for the labels `good first issue` and `help wanted`. These are curated for newcomers and do not require deep familiarity with the codebase.

If nothing catches your eye, test coverage and documentation fixes are always useful and require no prior discussion:

```bash
# Find packages with low coverage
make test-coverage | grep -v '100.0%' | sort -k3 -n
```

For larger work - new features, new adapters, architectural changes - open an issue first to discuss the approach. To talk it through before writing anything down, join the [Discord server](https://discord.gg/MCNrc8Wf9M). Decisions still land in the issue, so they stay findable.

## Setup

**Requirements:** Go 1.26.2 (see [go.mod](go.mod)), golangci-lint, gopls, and [lychee](https://github.com/lycheeverse/lychee) for documentation changes.

```bash
git clone https://github.com/sortie-ai/sortie.git
cd sortie
make test    # runs all tests with -race
make build   # compiles to ./sortie
make lint    # golangci-lint and comment style of changed files
make fmt     # gofmt + goimports
make lint-docs  # links and anchors in tracked Markdown files
make test-darwin  # darwin-only tests; macOS only, fails on any other host
```

All commands go through the Makefile. If `make test` passes, the change is safe to submit.

The SQLite dependency is [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) -a pure-Go driver. No C compiler needed.

### Dashboard

The dashboard application lives in `web/`. Go-only changes need none of this: `make build` and `make test` never call Node, and `go install` builds the full binary from committed files.

A change under `web/` needs Node 24.15.0 or later and npm 11.10.0 or later; older versions are refused. CI uses Node 24.22.0. `make ui-install` comes first:

```bash
make ui-install    # npm ci in web/
make ui-build      # writes internal/server/ui/dist
make ui-lint       # Oxlint and the TypeScript check
make ui-fmt        # Oxfmt, rewrites files
make ui-fmt-check  # fails on any formatting difference
make ui-check      # lint, format check, and the build output check
```

The response types of the JSON API reach the application through generated files: `web/src/api/types.gen.ts` and `docs/openapi.json`, the OpenAPI description of the API. A change to a JSON response type fails `go test ./internal/server` until the files are regenerated with `go test ./internal/server -run TestAPIContract -update`. The same test fails when the mux and the route table in `internal/server/api_routes_test.go` (`apiRoutes`) disagree, and CI runs oasdiff on `docs/openapi.json`. The command needs no Node, and no generated file is edited by hand.

A change under `web/` passes `make ui-check`, which `make check` also runs. `internal/server/ui/dist` is the build output the binary embeds. It comes from `make ui-build`, is committed with the sources it was built from, and is never edited by hand. A conflict in it is resolved by rebuilding. The Dashboard CI job runs on every pull request and fails when the committed output differs from a fresh build.

If `make ui-install` or `make ui-build` reports a missing native binding, run `make ui-install` again: it replaces `web/node_modules` with a clean install from the committed `web/package-lock.json`. If the binding is still missing, the lockfile lacks your platform: run `npm install` in `web/`, then review the lockfile change and commit it with the rebuilt output.

### Price table

The embedded price snapshot is generated: the upstream price file, `REVISION` and `REVISION_TIME` under `internal/pricing/snapshot`. The snapshot files are committed together and never edited by hand.

`make price-table` refreshes them to the latest upstream revision that changed the price file. It needs Go and access to `api.github.com`, and an optional `GITHUB_TOKEN` raises the rate limit. The command changes nothing when it fails. `make build` then embeds the new prices.

A weekly workflow opens a pull request with the refresh and posts to the CI Discord channel when the refresh fails. A conflict in the snapshot is resolved by rerunning the refresh.

### Integration tests

Integration tests are gated by environment variables and skip cleanly without them:

```bash
SORTIE_JIRA_TEST=1 SORTIE_JIRA_ENDPOINT="..." SORTIE_JIRA_API_KEY="..." \
  make test PKG=./internal/tracker/jira/...

SORTIE_GITHUB_TEST=1 SORTIE_GITHUB_TOKEN="ghp_..." SORTIE_GITHUB_PROJECT="owner/repo" \
  make test PKG=./internal/scm/github/...

SORTIE_CLAUDE_TEST=1 ANTHROPIC_API_KEY="sk-..." \
  make test PKG=./internal/agent/claude/...
```

GitHub integration tests also accept an optional `SORTIE_GITHUB_ISSUE_ID` variable (a valid issue number in the configured repo) to enable `FetchIssueByID` and `FetchIssueStatesByIDs` test cases.

The agent adapters' scripted-model suites also run an SSH variant against a throwaway sshd container. It needs `docker`, `ssh`, and `ssh-keygen` on `PATH` and skips with a reason naming the missing tool otherwise.

To probe a runtime over SSH by hand, start a lab and keep it running until you interrupt it:

```bash
make ssh-lab INSTALL='npm install -g <package>@<version>'
```

Omit `INSTALL` for a base image with no runtime. The command prints the `ssh` command that connects and the name of the container. Each distinct install command builds and keeps its own image, so every runtime version leaves one behind; `make ssh-lab-clean` removes every lab container and image from the local Docker cache.

You do not need access to Jira, GitHub, or Claude to contribute. The unit test suite covers the vast majority of the codebase.

## Making a change

**Small changes** (docs, typos, test coverage, bug fixes in a single package): fork, fix, test, submit a PR. No ceremony needed.

**Medium changes** (multi-file bug fixes, new test scenarios, adapter improvements): read the relevant section of [docs/architecture.md](docs/architecture.md) before implementing. The spec defines what correct behavior looks like, so reading it first prevents wasted effort.

**Large changes** (new features, new adapters, orchestrator changes): open an issue to discuss the design before writing code. The architecture doc is the source of truth -changes that contradict it will not be merged. If you believe the spec itself should change, that is a valid conversation to have in an issue.

## Code conventions

The linter config in [.golangci.yml](.golangci.yml) catches most issues. Beyond what the linter enforces:

- **Naming:** core packages use generic names (`agent_*`, `tracker_*`, `session_*`). Adapter-specific names (`jira_*`, `claude_*`) belong only inside their adapter package.
- **Errors:** wrap with context (`fmt.Errorf("operation: %w", err)`), no capitals, no trailing punctuation. Use the error categories in `internal/domain/errors.go`.
- **Logging:** `log/slog` with typed `slog.Attr` constructors. Derive loggers via `logging.WithIssue` and `logging.WithSession`.
- **Templates:** `Option("missingkey=error")` on every `text/template` - strict mode is mandatory.
- **Shared adapter helpers:** don't re-implement a helper already extracted to a shared package (`internal/httpkit`, `internal/typeutil`, `internal/issuekit`, or an adapter-family package like `internal/scm/scmcore` or the agent family's `internal/agent/agentcore` and `internal/agent/procutil`). A contract test in `internal/adaptertest` fails `make test` if a tracker, SCM, agent, or notifier package re-declares one of the listed names under a local name. For agent packages it also fails a private per-turn subprocess lifecycle (rule `LIFECYCLE`), a direct read of `ReadTimeoutMS` (rule `READTIMEOUT`), and token counters added by hand (rule `USAGESUM`). A kind that runs one process per turn builds on `agentcore.NewForkPerTurnSession` and adds what it lacks as a hook.

## Testing conventions

- Table-driven tests with `t.Parallel()` at both test and subtest level.
- `t.Helper()` as the first line of every test helper.
- `t.TempDir()` for filesystem isolation, `t.Setenv()` for environment variables.
- Error assertions via `errors.As()` / `errors.Is()`, never string matching.
- Failure format: `FuncName(input) = got, want expected`.
- Standard library only - no third-party assertion frameworks.
- Fixtures live in `testdata/` within each package.
- A test that only a macOS host can run lives in a `_darwin_test.go` file and never skips: the macOS CI job selects tests by that suffix and fails on a skip. An env-gated integration test skips by design, so none lives in such a file.

## Commits and PRs

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(orchestrator): add stall detection for running sessions
fix(workspace): reject symlinks escaping workspace root
test(tracker): cover pagination edge cases in Jira adapter
```

PRs use the [template](.github/pull_request_template.md). One logical change per PR. CI lints, tests (including on Windows, and the darwin-only tests on macOS), and builds the binary - all must pass.

## What will not be merged

- CGo or any dependency requiring a C compiler.
- Adapter-specific logic in core packages (`internal/orchestrator/`, `internal/domain/`).
- Weakened workspace path containment or input sanitization - these are security boundaries.
- Behavior that contradicts [docs/architecture.md](docs/architecture.md).
- Changes the author cannot explain or justify under review ([see below](#ai-assisted-contributions)).

If you are unsure whether a change fits, open an issue. A five-minute conversation saves hours of work.

## AI-assisted contributions

Sortie is primarily developed with AI coding agents. Contributions using AI tools are welcome under the same quality bar: the code must be correct, spec-conformant, tested, and reviewed by you before submitting. `make test` and `make lint` must pass - not just "the agent said it works".

Whoever opens the pull request owns it. You are accountable for every line you submit and for its conformance to the conventions, spec, and tests described here, regardless of how the change was produced: by hand, by a teammate, by a contractor, or by an AI agent. We have no preference among those. What we do not accept is "the agent decided" as the explanation for why a change is the way it is. If you cannot explain a change under review, it is not ready to merge. The verification gate is identical for everyone: `make lint` and `make test` pass because you ran them and read the output, not because a tool reported success.

## Security

Workspace path containment and input sanitization are security boundaries, not convenience features. Changes to `internal/workspace/` receive additional scrutiny. If you find a vulnerability, report it privately rather than opening a public issue.

## License

Contributions are licensed under [Apache License 2.0](LICENSE).
