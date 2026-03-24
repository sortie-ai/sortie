# Contributing to Sortie

Sortie is built spec-first: the architecture document defines every behavior, and the
implementation conforms to it. Contributions that improve conformance, coverage, and
reliability are welcome. This guide explains how to work within that model.

## What we accept

**Always welcome:**

- Bug fixes with a regression test
- Test coverage for uncovered paths
- Implementations of tasks listed in [TODO.md](TODO.md), in milestone order
- New adapter packages behind existing interfaces (additive only)
- Documentation fixes

**Discuss first** (open an issue before writing code):

- Changes to [docs/architecture.md](docs/architecture.md) or accepted ADRs
- Features not yet in `TODO.md`
- New dependencies beyond what the architecture specifies
- Reordering or skipping `TODO.md` milestones

**Will not be accepted:**

- CGo or any dependency requiring a C toolchain
- Changes that break the single-binary, zero-dependency deployment model
- Adapter-specific logic in core packages (`internal/orchestrator/`, `internal/domain/`)
- Weakened workspace path containment or input sanitization
- Implementations that contradict `docs/architecture.md` -- drift from the spec is a bug

## Prerequisites

- **Go** -- version matching [go.mod](go.mod) (currently 1.26.1), managed via
  [asdf](https://asdf-vm.com/) or installed directly. Do not override `GOPATH` or `GOMODCACHE`.
- **golangci-lint** -- installed separately; `make lint` invokes it.
- **SQLite library** -- `modernc.org/sqlite` only. Never `mattn/go-sqlite3`.
- No C compiler needed. The entire dependency tree is pure Go.

## Build and test

All operations go through the Makefile. Do not invoke `go` subcommands directly.

```bash
make build                                 # compile to ./sortie
make test                                  # all tests, -race enabled
make test PKG=./internal/persistence       # single package
make test RUN=TestOpenStore                # single test
make lint                                  # golangci-lint
make fmt                                   # gofmt + goimports
```

Tests run with the race detector on every invocation. If `make test` passes, the change
is safe to submit.

### Integration tests

Integration tests talk to real services and are gated by environment variables. Without
the variables, they skip cleanly and never fail CI.

```bash
SORTIE_JIRA_TEST=1 \
  SORTIE_JIRA_ENDPOINT="..." \
  SORTIE_JIRA_API_KEY="..." \
  SORTIE_JIRA_PROJECT="..." \
  make test PKG=./internal/tracker/jira/...

SORTIE_CLAUDE_TEST=1 ANTHROPIC_API_KEY="sk-..." \
    make test PKG=./internal/agent/claude/...
```

## Project layout

```plaintext
cmd/sortie/            -- entry point, CLI wiring
internal/
  agent/               -- agent adapters (claude-code, mock)
  config/              -- typed config, defaults, env var resolution, validation
  domain/              -- pure types, interfaces, constants (imports nothing internal)
  logging/             -- structured slog helpers (imports nothing internal)
  orchestrator/        -- dispatch, retry, reconciliation, state machine
  persistence/         -- SQLite store: migrations, retry queues, run history
  prompt/              -- template rendering (text/template, strict mode)
  registry/            -- adapter registration
  server/              -- HTTP API and dashboard
  tracker/             -- tracker adapters (jira, file)
  workflow/            -- WORKFLOW.md loader, file watcher
  workspace/           -- workspace creation, path safety, hook execution
docs/
  architecture.md      -- the specification
  decisions/           -- accepted ADRs
```

The `internal/` directory enforces encapsulation at the compiler level. Each sub-package
maps to one architectural component. Imports flow strictly downward; `domain/` and
`logging/` sit at the bottom with no internal dependencies.

## The spec-first contribution loop

Every change follows the same sequence:

1. **Read the spec.** Open the relevant section of `docs/architecture.md`. If touching
   the orchestrator, read Sections 7-8 and 16. For adapters, Sections 10-13. For
   workspace safety, Section 9.5. For persistence, Section 14.

2. **Check TODO.md.** Milestones are ordered by dependency. Verify the task you are
   implementing does not depend on incomplete earlier work.

3. **Implement to match the spec.** Do not invent behavior. If the spec is ambiguous,
   clarify it before writing code.

4. **Write tests that verify spec behavior.** Cover both happy paths and the error
   conditions the spec defines. Use `make test` to confirm.

5. **Submit a PR.** Reference the TODO.md task and the architecture section your change
   implements.

## Code style

The linter configuration in [.golangci.yml](.golangci.yml) is the style authority.
Beyond what the linter enforces:

- **Generic naming in core.** Use `agent_*`, `tracker_*`, `session_*` in orchestrator
  and domain packages. Adapter-specific terms (`jira_*`, `claude_*`) belong only in
  their adapter package.
- **American English** in identifiers, comments, and error messages: `initialize`,
  `normalize`, `behavior`.
- **Structured logging** with `log/slog` only. Use typed `slog.Attr` constructors, not
  alternating key-value pairs. Derive loggers through `logging.WithIssue` and
  `logging.WithSession`.
- **Error wrapping** with context: `fmt.Errorf("operation context: %w", err)`. No
  capital letters or trailing punctuation in error messages.
- **Template strict mode** is mandatory: `Option("missingkey=error")` on every
  `text/template`.

## Testing standards

- Table-driven tests with `t.Parallel()` at both test and subtest level.
- `t.Helper()` as the first statement in every test helper.
- `t.TempDir()` for filesystem isolation, `t.Setenv()` for environment variables.
- Error semantics via `errors.As()` / `errors.Is()`, never string matching.
- Failure messages in the format: `FuncName(input) = got, want expected`.
- No external assertion libraries. Use only the Go standard library for comparisons and diffs.
- Fixtures in `testdata/` within the package directory, loaded through helpers.
- Integration tests in `integration_test.go`, gated by `SORTIE_*_TEST=1` skip helpers.

## Commit messages

Follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/). Subject
line under 72 characters, imperative mood, no trailing period.

```
feat(orchestrator): add stall detection for running sessions
fix(workspace): reject symlinks escaping workspace root
test(tracker): cover pagination edge cases in Jira adapter
refactor(config): split validation from env var resolution
chore(deps): bump modernc.org/sqlite to v1.47.0
```

The body explains **why**, not what. Reference GitHub issues or TODO.md tasks when
relevant. Wrap body lines at 72 characters.

## Pull requests

- One logical change per PR. Split unrelated work into separate PRs.
- CI runs `make lint` and `make test` on every PR. Both must pass.
- The PR description states the problem, the approach, what was tested, and what could
  break. Do not narrate the diff.
- Use the [PR template](.github/pull_request_template.md) -- it asks for scope, reviewer
  entry point, and risk assessment.

## Architectural boundaries

These import rules are enforced by convention and code review. A violation is a blocking
review finding.

```
cmd/sortie/            -> internal/*              (wiring only)
internal/orchestrator/ -> domain, config, persistence, workspace, registry, prompt, workflow
internal/workspace/    -> domain, config, persistence
internal/persistence/  -> domain, config
internal/tracker/*/    -> domain, registry        (no cross-adapter imports)
internal/agent/*/      -> domain, registry        (no cross-adapter imports)
internal/config/       -> domain
internal/prompt/       -> domain
internal/domain/       -> (nothing internal)
internal/logging/      -> (nothing internal)
```

## AI-assisted contributions

Sortie is developed with AI coding agents. If your contribution involves AI assistance:

- The agent must read the relevant architecture section before implementing.
- Do not accept generated code that invents behavior beyond the spec.
- Include the actual `make test` and `make lint` output demonstrating the change works.
- Review generated code for security, correctness, and spec conformance before submitting.

AI-generated code is held to the same standard as human-written code. The spec is the
source of truth regardless of who -- or what -- wrote the implementation.

## Security

Workspace path containment, input sanitization, and key validation are security
boundaries. Changes touching `internal/workspace/` receive additional scrutiny. If you
find a vulnerability, report it privately rather than opening a public issue.

## License

By contributing, you agree that your contributions will be licensed under the
[Apache License 2.0](LICENSE).
