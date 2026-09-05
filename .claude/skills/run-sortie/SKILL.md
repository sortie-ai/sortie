---
name: run-sortie
description: Build, validate, and run the sortie orchestrator end-to-end using its built-in offline fixture (no external tracker/agent credentials needed). Use when asked to run sortie, start the orchestrator, verify a change works, or check that the binary builds.
---

Sortie is a single Go binary (`./cmd/sortie`) with no GUI: it's a CLI that
either runs one-shot subcommands (`validate`, `stats`) or a long-running
polling daemon that dispatches coding-agent sessions. Drive it via
`.claude/skills/run-sortie/driver.sh`, which builds it and runs it against
the repo's own offline fixture — `examples/WORKFLOW.test.md` (file-tracker +
mock agent) paired with `examples/issues.json` — so no GitHub/Jira token or
real coding agent is required to see it actually orchestrate.

All paths below are relative to the repo root.

## Prerequisites

- Go 1.26.1+ (`go.mod` pins `go 1.26.1`). This machine had no Go at all;
  it was installed with:

  ```bash
  winget install --id GoLang.Go --accept-package-agreements --accept-source-agreements
  ```

  This installs to `C:\Program Files\Go\bin`, which is usually not yet on a
  freshly-opened shell's `PATH` — the driver adds it automatically if `go`
  isn't already resolvable.
- Git Bash (ships with `timeout`, used to bound the long-running poll loop).

## Build

```bash
bash .claude/skills/run-sortie/driver.sh build
```

```
== build ==
sortie dev (commit: unknown, built: unknown, go1.27.0, windows/amd64)
```

Produces `./sortie.exe` at repo root (gitignored).

## Run (agent path)

The driver has five subcommands; run them individually or all at once.

| command | what it does |
|---|---|
| `build` | `go build -trimpath -o sortie.exe ./cmd/sortie`, prints `--version` |
| `validate` | `sortie validate examples/WORKFLOW.test.md` — checks config/template syntax |
| `dry-run` | `sortie --dry-run --port 0 examples/WORKFLOW.test.md` — one poll cycle, computes dispatch decisions, spawns nothing |
| `run` | Runs for real, bounded to 15s with `timeout` (the loop otherwise polls forever by design) |
| `clean` | Removes the sqlite DB and workspace dir the run leaves behind |
| `all` (default) | build → validate → dry-run → run → clean |

```bash
bash .claude/skills/run-sortie/driver.sh all
```

`validate` output (one benign warning, exit 0 — the mock agent has no tool
channel, which is expected):

```
== validate ==
warning: agent.kind.no_tool_channel: agent kind "mock" has no tool execution channel: Sortie's tools are neither advertised nor callable for it
```

`dry-run` output — it actually reads `examples/issues.json` and computes
which of the 3 candidate issues would be dispatched:

```
== dry-run (one poll cycle, no dispatch) ==
level=INFO msg="dry-run: candidate" issue_identifier=PROJ-2 ... would_dispatch=true
level=INFO msg="dry-run: candidate" issue_identifier=PROJ-1 ... would_dispatch=false skip_reason=blocked_by
level=INFO msg="dry-run: candidate" issue_identifier=PROJ-5 ... would_dispatch=true
level=INFO msg="dry-run: complete" candidates_fetched=3 would_dispatch=2 ineligible=1 max_concurrent_agents=2
```

`run` actually dispatches: it creates `examples/.sortie.db`, prepares an
isolated workspace per issue under `C:\tmp\sortie-test-workspaces\<ISSUE>`,
writes each workspace's `.sortie/mcp.json`, starts a `mock-session-001`
agent session per issue, and runs it through `max_turns=3` (`"turn started"`
→ `"turn completed"` × 3 → `"worker exiting" exit_kind=normal`). Because the
file tracker never mutates issue state, sortie then retries the same issues
forever — this is the daemon behaving correctly, not a bug; `timeout` cuts
it off with exit 124, which the driver already accounts for.

## Run (human path)

To poke it interactively rather than through the bounded driver:

```bash
export PATH="/c/Program Files/Go/bin:$PATH"
./sortie.exe --port 0 examples/WORKFLOW.test.md
# Ctrl-C to stop, then: rm -f examples/.sortie.db* && rm -rf /c/tmp/sortie-test-workspaces
```

`sortie --help` documents flags for pointing at a real `WORKFLOW.md` with a
real tracker/agent (GitHub, Jira, Claude Code, etc.) — see [README.md](../../../README.md)
and the other `examples/WORKFLOW.*.md` files for those configs; none of
that path was exercised here since it needs live credentials.

## Test

```bash
make test
```

(Not run by this driver — it's the project's own unit/integration suite,
separate from actually launching the binary.)

## Gotchas

- **Go wasn't installed at all** on this machine — not via any package
  manager, not at any common path. `winget search --id GoLang.Go` is what
  found the installable package; `GoLang.Go.1.26` (matching the exact
  `go.mod` version) doesn't exist as a winget id, only the unversioned
  `GoLang.Go` (installs latest, 1.27.0 here). That's fine: `go.mod` declares
  `go 1.26.1` as a minimum with no `toolchain` directive, so a newer
  toolchain builds it without modification. Per this repo's `CLAUDE.md`,
  never add/downgrade a `toolchain` directive to force an exact match.
- **`workspace.root: /tmp/sortie-test-workspaces` in `WORKFLOW.test.md`
  resolves to `C:\tmp\sortie-test-workspaces`**, not under Git Bash's
  `/tmp`. Go's own path resolution (not MSYS) treats a leading `/` as
  drive-root-relative on Windows, so it lands on the current drive's root
  `\tmp\...` regardless of cwd. `driver.sh` cleans up `/c/tmp/...`
  specifically for this reason.
- **The real (non-dry-run) run never terminates on its own** against this
  fixture — the mock agent finishes its turns but the file tracker's issue
  states never reach `terminal_states: [Done]`, so sortie retries
  indefinitely (by design, for stall/backoff testing). Always bound it with
  `timeout` when scripting; don't wait for it to exit cleanly.
- All build/run artifacts (`sortie.exe`, `.sortie.db*`, `/dist`) are already
  covered by [.gitignore](../../../.gitignore); the workspace dir under
  `C:\tmp\` is outside the repo entirely, so nothing here dirties `git
  status` except this skill directory itself.
