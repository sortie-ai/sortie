# Kiro CLI: adapter research notes

> Kiro CLI 2.4.2 (`kiro-cli`) on Linux x86_64, probed on 2026-05-27. Reference for the Kiro
> `domain.AgentAdapter`, implemented in `internal/agent/kiro`.
>
> Instruments: `--help` surfaces and exit-code probes of the installed binary, `strings` and
> `file` on the resolved binary, the SQLite store at `~/.local/share/kiro-cli/data.sqlite3`,
> and authenticated headless turns run with a `KIRO_API_KEY` credential on a Kiro Pro account.
> The rendered docs at `kiro.dev`, the `aws/amazon-q-developer-cli` source repository, and the
> `kirodotdev/Kiro` tracker corroborate; all are linked under "Sources".
>
> Coverage: every claim about the CLI is anchored to 2.4.2 on that date and spans help
> surfaces, exit codes, authentication gating, the store schema, the success-turn stdout
> shape, the cost-trailer stream, conversation persistence and resume, the model list, and
> MCP config loading. Turns that invoke tools, quota exhaustion, and MCP under a credential
> other than `KIRO_API_KEY` are not observed and appear under "Open questions". Claims about
> Sortie's own code cite Go symbols and are verified against the tree, not against the CLI.

## Overview

Kiro CLI exposes one relevant automation surface for Sortie: the `chat` command run with
`--no-interactive`. Unlike Codex, Kiro CLI does not ship a persistent JSON-RPC app-server, and
unlike OpenCode it has no embedded HTTP server. It is a single binary that, in non-interactive
mode, accepts one prompt, runs an agent loop to completion, prints a human-readable transcript
to stdout, and exits.

| Surface | Transport | What it does | Adapter relevance |
| ------- | --------- | ------------ | ----------------- |
| `kiro-cli chat --no-interactive "<prompt>"` | stdout/stderr, plain text | Non-interactive one-shot execution | Closest match to the Claude Code, Copilot, and OpenCode `run` launch-per-turn adapters |
| `kiro-cli chat` (interactive) | TUI | Rich terminal UI with panels and slash commands | Not suitable for unattended orchestration |
| `kiro-cli acp` | stdin/stdout nd-JSON | Agent Client Protocol server | Exists in the command tree (local `--help-all` probe), but the headless `chat` path is the documented automation surface and the focus of this note |

This places the Kiro adapter in Sortie's synchronous, launch-per-turn category.
`kiro.KiroAdapter` satisfies `domain.AgentAdapter` (declared in `internal/domain/agent.go`)
by driving one subprocess per turn through `agentcore.ForkPerTurnSession`, delivers events
through the `RunTurn` `OnEvent` callback, and returns `nil` from `EventStream`.

The single most important architectural fact for the adapter: **headless Kiro CLI has no
structured output**. There is no JSON event stream, no machine-readable result envelope, and
no token-count reporting on the headless path. stdout carries the assistant's markdown answer
(and tool-progress lines when tools run); the closing cost line is on stderr. The
consequences for `EventTokenUsage` and budget tracking are spelled out in "Token usage and
cost reporting" below.

## Installation and prerequisites

Kiro CLI installs as the `kiro-cli` binary:

```bash
curl -fsSL https://cli.kiro.dev/install | bash
```

| Item | Requirement | Evidence |
| ---- | ----------- | -------- |
| Kiro CLI binary | `kiro-cli` on `PATH` | Local probe, 2.4.2 |
| Credentials | A valid Builder ID, IAM Identity Center, social, external IdP, or `KIRO_API_KEY` token | Published docs, local probe |
| Subscription for headless | `KIRO_API_KEY` requires Kiro Pro, Pro+, or Power | Published docs |
| Working directory | Any project directory; the session runs with cwd set to the workspace | Local probe |
| Headless use | `kiro-cli chat --no-interactive` runs without a TTY once a credential is present | Published docs, hands-on report |

The adapter registers under kind `kiro` with `registry.AgentMeta{RequiresCommand: true}`, so
`orchestrator` preflight rejects a dispatch whose `agent.command` is empty.
`agentcore.ResolveLaunchTarget` resolves that command through `exec.LookPath`, falling back to
`kiro-cli`, and validates the workspace path that becomes the subprocess cwd.

The binary is large (roughly 118 MB for 2.4.2) and dynamically linked against glibc (local
`file` probe). It bundles shell-integration assets (`inline`, autosuggestions, completions)
that are irrelevant to headless orchestration.

## Lineage and source of record

`kiro-cli` 2.4.2 is the Kiro distribution of the open-source Amazon Q Developer CLI, which
is why upstream code and issues are cited from `aws/amazon-q-developer-cli`:

- The binary embeds Rust crate paths such as `crates/q_cli/src/cli/mod.rs`,
  `crates/fig_auth/src/session.rs`, and `crates/fig_api_client/src/endpoints.rs`, and the
  symbol `amzn_codewhisperer_client` (local `strings` probe).
- The 2.4.2 changelog strings link to pull requests in
  `github.com/aws/amazon-q-developer-cli` (for example `/pull/2561` and `/pull/2516`)
  (local `strings` probe).
- The CLI talks to AWS CodeWhisperer endpoints under SSO authentication
  (`codewhisperer.us-east-1.amazonaws.com`, `cps.prod-us-east-1.codewhisperer.ai.aws.dev`)
  and to Kiro runtime endpoints under API-key authentication
  (`https://runtime.us-east-1.kiro.dev`, `https://runtime.eu-central-1.kiro.dev`)
  (local `strings` probe).

The `kirodotdev/Kiro` repository is the Kiro IDE and the public issue tracker (TypeScript,
not the CLI source). CLI feature requests such as machine-readable headless output are filed
there. The CLI source of record remains `aws/amazon-q-developer-cli`.

The two projects number releases differently: the source repository tags its own line (for
example [v1.19.7](https://github.com/aws/amazon-q-developer-cli/releases/tag/v1.19.7)) while
the installed distribution reports `kiro-cli 2.4.2` (local probe). Runtime claims here are
anchored to the installed binary and the `kiro.dev` docs; the source repository is
corroborating evidence, not the version of record.

## Authentication

Kiro CLI supports five credential types. The login subcommand surfaces the interactive
flows; `KIRO_API_KEY` is the unattended path.

| Method | Mechanism | Adapter notes |
| ------ | --------- | ------------- |
| Builder ID (free) | `kiro-cli login --license free` (device or browser OAuth) | Interactive; not suitable for unattended launch |
| IAM Identity Center (pro) | `kiro-cli login --license pro --identity-provider <url> --region <r>` | Interactive; enterprise SSO |
| Social (Google, GitHub) | `kiro-cli login --license free` social option | Interactive |
| External IdP | Refresh-token flow stored under `kirocli:external-idp:token` | Interactive setup |
| API key | `KIRO_API_KEY` environment variable | The unattended path. Requires Kiro Pro, Pro+, or Power. |

`KIRO_API_KEY` is the credential the adapter relies on. Per the first-party blog, "when
the `KIRO_API_KEY` environment variable is set, Kiro CLI skips the browser-based login flow
entirely." The headless docs state plainly that "headless mode requires an API key set as the
`KIRO_API_KEY` environment variable." The binary confirms an `API_KEY` token type and an
`https://runtime.<region>.kiro.dev` endpoint for that path, plus a changelog entry "Fix API
key authentication failing for non us-east-1 users" (local `strings` probe), so the active
region is not fixed at `us-east-1`. The adapter passes no region argument: the subprocess
inherits the orchestrator environment (`cmd.Env = os.Environ()` in
`agentcore.ForkPerTurnSession.RunTurn`), so region selection rests on what the operator
exports. In SSH mode `kiro.buildSSHRemoteCmd` prepends only `KIRO_API_KEY`, shell-quoted,
because OpenSSH drops the local environment.

### Token storage

Authentication tokens persist in a SQLite database at
`~/.local/share/kiro-cli/data.sqlite3`, table `auth_kv`, under keys such as
`kirocli:odic:token`, `kirocli:odic:device-registration`, `kirocli:social:token`, and
`kirocli:external-idp:token` (local SQLite probe). The stored OIDC client registration names
the client `Kiro CLI`, uses the `DeviceCode` OAuth flow, and requests the CodeWhisperer
scopes `codewhisperer:completions`, `codewhisperer:analysis`, and
`codewhisperer:conversations` (local SQLite probe). The adapter never reads this store: it
verifies `KIRO_API_KEY` and leaves token management to the CLI.

### The login-hang hazard

When no valid credential is present, the behavior is not uniform across commands, and this
asymmetry is the single biggest operational hazard for the adapter:

- `kiro-cli mcp list` and `kiro-cli agent list` fail fast with exit code 1 and the message
  `error: You are not logged in, please log in with kiro-cli login` (local probe).
- `kiro-cli chat ...` (including `chat --list-models` and `chat --list-sessions`) instead
  enters an interactive device-authorization flow. It prints a device code and a
  `view.awsapps.com/start` URL to stdout, then spins a `Logging in...` indicator and blocks
  indefinitely waiting for the operator to complete browser authentication (local probe).

The `--no-interactive` flag does not suppress this login flow. A headless `chat` invocation
without a valid credential does not error out; it hangs until killed:
`kiro-cli chat --no-interactive --trust-all-tools "..."` with no credential emits only the
device-login prompt and spinner on stdout, leaves stderr empty, and never exits on its own
(local probe).

The hazard has two distinct shapes. An **invalid** `KIRO_API_KEY` does not hang:
`chat --no-interactive` fails fast with `Authentication failed.` on stderr, empty stdout, and
exit 0. The indefinite device-login hang is specific to **no credential at all** (no
`KIRO_API_KEY` and no cached SSO token). The two shapes need different defenses.

`kiro.checkCredential` defends against both shapes before any turn runs. It rejects an empty
`KIRO_API_KEY` with `domain.ErrResponseError`, then runs a `kiro-cli whoami` canary under a
five-second timeout and rejects the credential unless the output contains
`Authenticated with API key` and does not contain `Authentication failed.`. A canary that
times out or exits non-zero is also rejected, classified as a retryable credential problem
rather than `domain.ErrAgentNotFound`, because `agentcore.ResolveLaunchTarget` has already
proved the binary exists. `kiro.KiroAdapter.StartSession` skips this preflight in SSH mode,
where the key is injected into the remote command instead.

## Relevant CLI commands and flags

### `kiro-cli chat`

The flag surface below is captured verbatim from `kiro-cli chat --help` on 2.4.2 (local
probe), cross-checked against the published CLI command reference. The internal usage string
is `kiro-cli-chat chat [OPTIONS] [INPUT]`, where `[INPUT]` is "the first question to ask".

| Flag | Short | Meaning | Adapter use |
| ---- | ----- | ------- | ----------- |
| `--no-interactive` | | Run without expecting user input; print the response to stdout and exit | Passed on every turn |
| `--trust-all-tools` | `-a` | Allow any tool without confirmation | Passed when `trust_all_tools` is configured |
| `--trust-tools <NAMES>` | | Trust only a comma-separated set, for example `--trust-tools=read,grep`; empty value trusts nothing | Passed with the configured allowlist whenever `trust_all_tools` is off |
| `--model <MODEL>` | | Model to use for this invocation | Passed when `model` is configured |
| `--agent <AGENT>` | | Context profile (custom agent) to use | Passed when `agent` is configured |
| `--resume` | `-r` | Resume the most recent conversation from this directory | Passed on every turn after the first successful one |
| `--resume-id <SESSION_ID>` | | Resume a specific conversation by session ID | Unused; the ID is not enumerable headless (see session continuity) |
| `--resume-picker` | | Interactively select a conversation to resume | Interactive only; unusable headless |
| `--list-sessions` | `-l` | List saved chat sessions for the current directory | Unused |
| `--delete-session <SESSION_ID>` | `-d` | Delete a saved chat session by ID | Unused |
| `--session-source <v1\|v2>` | | Target only the v1 or v2 store for `--delete-session` (default: both) | Unused; two conversation stores exist (see session continuity) |
| `--list-models` | | List available models and exit | Meta command; honors `--format` |
| `--format <FORMAT>` | `-f` | Output format for list commands: `plain`, `json`, `json-pretty` (default `plain`) | Applies to `--list-models` only, not to chat turns |
| `--require-mcp-startup` | | Exit with code 3 if any enabled MCP server fails to start | Unused; MCP is disabled under API-key auth (see MCP integration) |
| `--wrap <WRAP>` | `-w` | Line wrapping: `always`, `never`, `auto` (default auto-detect) | Passed as `--wrap never` on every turn so captured lines are unwrapped |
| `--agent-engine <ENGINE>` | | Agent engine: `v2` (default), `v1`, or `kas` | Unused; the default engine applies |
| `--mode <MODE>` | | KAS agent mode: `vibe` (default) or `spec` | Only meaningful with the `kas` engine |
| `--tui` | | Use the new terminal UI | Interactive only |
| `--legacy-ui` | | Use the legacy harness (alias `--classic`) | Interactive only |
| `--verbose` | `-v` | Increase logging verbosity (repeatable) | Unused |

`kiro.buildArgs` assembles exactly this shape, with the optional segments included per the
table above:

```bash
kiro-cli chat --no-interactive --wrap never \
  [--model <MODEL>] (--trust-all-tools | --trust-tools=<names>) [--agent <AGENT>] [--resume] \
  -- "<rendered prompt>"
```

The `--` separator passes the full prompt as one positional argument, so a prompt with
leading hyphens needs no escaping. Arguments go to `exec.Command` directly, never through a
shell.

### Other commands

| Command | Subcommands or use | Evidence |
| ------- | ------------------ | -------- |
| `kiro-cli mcp` | `add`, `remove`, `list`, `import`, `status` | Local `mcp --help`, published docs |
| `kiro-cli agent` | `list`, `create`, `edit`, `validate`, `migrate`, `set-default` | Local `agent --help` |
| `kiro-cli settings` | Read or write CLI settings, for example `kiro-cli settings chat.defaultModel <model>` | Hands-on report, local probe |
| `kiro-cli login` / `logout` / `whoami` | Authentication management; `whoami` prints `Not logged in` or the active method | Local probe |

## Subprocess behavior

### Headless execution model

`kiro-cli chat --no-interactive "<prompt>"` runs the agent loop to completion and exits. The
flag "tells Kiro to print its response to stdout and exit, rather than starting an interactive
chat session," which is the intended CI behavior per the first-party blog. There is no
persistent process to manage between turns. Each turn is one subprocess launch, matching the
Claude Code and Copilot adapters rather than the Codex app-server.

### Working directory and session scoping

Saved sessions are scoped to the current directory: `--list-sessions` lists "all saved chat
sessions for the current directory" and `--resume` resumes "the most recent conversation from
this directory" (local `chat --help`). `agentcore.ForkPerTurnSession.RunTurn` sets `cmd.Dir`
to the workspace path validated by `agentcore.ResolveLaunchTarget`, so resume operates on the
per-issue conversation set.

### Process lifetime and cancellation

Kiro CLI exposes no native per-turn timeout flag on `chat` (local `chat --help`), so
`agent.turn_timeout_ms` supplies the external wall-clock bound on a turn. The login-hang
hazard above is caught by stall detection, described below, not by that bound. Cancelling
the context passed to `RunTurn` is what stops a Kiro subprocess:
`agentcore.ForkPerTurnSession` puts the process in its own group, sends
SIGTERM to that group through `procutil.SignalGraceful`, waits the five-second
`stopGracePeriod`, and then sends SIGKILL. `KiroAdapter.StopSession` reaches the same path
through `ForkPerTurnSession.Stop`. On SIGTERM the process terminates and the shell reports
exit status 143 (128 + 15); on SIGKILL, 137 (128 + 9). The skeleton detects the signal with
`procutil.WasSignaled` and returns `domain.EventTurnCancelled` with `domain.ErrTurnCancelled`
before the adapter's own finalizer runs.

The orchestrator now derives a per-turn deadline from `agent.turn_timeout_ms` and passes it on
the context given to `RunTurn`, the same inheritance mechanism every other adapter relies on: a
Go context deadline propagates to `agentcore.ForkPerTurnSession`'s own `context.WithCancel`
re-derivation, so an expiry cancels the Kiro subprocess through the same signal-then-force-kill
path described above.

Stall detection still reaches Kiro turns despite the absent event stream: every non-empty
stdout line becomes a `domain.EventNotification` carrying a fresh
timestamp, and `orchestrator.HandleAgentEvent` advances `LastAgentTimestamp` from it, which
is the reference time `orchestrator.reconcileStalled` compares against `stall_timeout_ms`
before cancelling the worker. A turn that prints nothing for longer than that threshold is
cancelled; a turn that keeps printing transcript lines keeps resetting the timer.

## Output format

### There is no structured output on the headless path

This is the defining limitation. In 2.4.2 there is no JSON or otherwise machine-readable
output for a chat turn. Evidence, triangulated:

- `kiro-cli chat --help` lists only `--format` (values `plain`, `json`, `json-pretty`), and
  documents it as "Output format for list commands (used with `--list-models`)" (local
  probe). It does not apply to chat turns.
- The 2.4.2 binary contains no `output-format`, `output_format`, `stop_reason`, or
  `tool_calls` strings (local `strings` probe).
- A structured headless output mode is an open feature request, not a shipped feature:
  [kirodotdev/Kiro#5423](https://github.com/kirodotdev/Kiro/issues/5423) (filed 2026-02-05,
  state OPEN) proposes adding `--output-format json` returning
  `{response, tool_calls, stop_reason}` and a `--progress-file <path>` NDJSON stream. The
  request explicitly motivates the need to "parse the final response without stripping ANSI
  codes or text parsing," which confirms that no such clean output exists today.

Some web summaries describe `--output-format json` as if it ships in Kiro CLI. It does not in
2.4.2, and those summaries paraphrase the wording of feature request #5423. The binary
`--help` and `strings` output are authoritative for this version.

### What stdout actually contains

In `--no-interactive` mode the stdout stream is a human transcript, not a parseable protocol.
The streams divide as follows:

- **stdout carries the assistant answer.** For a turn that invokes no tools, stdout holds
  only the answer, prefixed with a colorized `> ` marker and ANSI styling, with no trailing
  newline (observed: `\x1b[38;5;141m> \x1b[0mPONG`). Per the independent hands-on report, a
  turn that invokes tools also prints tool-progress lines such as
  `Reading directory: ... (using tool: read ...)` and markdown answers including tables.
- **stderr carries the cost trailer and warnings.** The closing cost line of the form
  `▸ Credits: 0.01 • Time: 1s` is emitted on **stderr**, not stdout, even though the hands-on
  report shows it inside the printed transcript; direct observation places it on stderr. A
  `Failed to retrieve MCP settings` warning and any `--trust-tools` warnings also land there.

How the adapter reads these streams:

- The cost trailer on stderr is the one positive success signal on the headless path: a turn
  that actually ran prints `▸ Credits: …`, whereas an auth failure prints
  `Authentication failed.` and no trailer. `kiro.classifyStderr` scans the collected stderr
  lines for both markers by substring containment, never by the numeric values that follow
  (see "Exit codes and error detection").
- Output is formatted for a terminal and carries markdown and ANSI styling. `--wrap never`
  suppresses width-based wrapping, and `kiro.stripANSI` removes the remaining color and style
  escapes from each stdout line before it is emitted or accumulated.
- Each non-empty stripped line becomes a `domain.EventNotification` with the text truncated
  to 500 runes by `typeutil.TruncateRunes`, and the untruncated text accumulates in
  `sessionState.turnStdout` for the turn.
- There is no per-event timestamped stream, so tool calls cannot be reconstructed from stdout
  text. The adapter emits no `domain.EventToolResult` and no `ToolDurationMS`.

## Session continuity

Kiro CLI persists conversations per directory and supports several resume modes (local
`chat --help`, published chat docs):

| Mode | Mechanism | Adapter notes |
| ---- | --------- | ------------- |
| Fresh | No resume flag | The first turn of a session starts a new conversation in the cwd |
| `--resume` / `-r` | Resume the most recent conversation from this directory | The continuation path the adapter uses; "most recent" is positional, not an explicit identity |
| `--resume-id <SESSION_ID>` | Resume a specific session by ID | Retains context headless, but the ID is not obtainable at run time |
| `--resume-picker` | Interactive selection | Unusable headless |
| `--list-sessions` / `-l` | List saved sessions for the cwd | Returns empty for headless conversations |
| `--delete-session <ID>` | Delete a saved session | Cleanup; `--session-source <v1\|v2>` selects which store |

The store schema is observable directly. The `~/.local/share/kiro-cli/data.sqlite3` database
holds tables `migrations`, `history`, `auth_kv`, `state`, `conversations`, and
`conversations_v2`, and `--session-source` selects between the last two. Chat persistence does
not live in the `state` table, which holds only telemetry and migration keys
(`telemetryClientId`, `migration.kiro.completed`, and similar). `conversations_v2` has columns
`key, conversation_id, value, created_at, updated_at`, keyed by the **workspace directory
path** with a UUID `conversation_id`. The `history` table is the unrelated shell
command-history feature. A headless `--no-interactive` turn persists a resumable conversation
scoped to cwd, even though `--list-sessions` returns empty for it, so the conversation ID is
reachable only by reading `conversations_v2`, never through the CLI or the turn output.

The adapter therefore continues by directory rather than by ID. `kiro.sessionState` sets
`resumeRequested` in `OnFinalize` once a turn succeeds, and `kiro.buildArgs` appends `--resume`
on every later turn in the same workspace. The session identity the adapter carries is
`params.ResumeSessionID`, stored in `sessionState.sessionID` and reported back through
`domain.TurnResult.SessionID` and the session logger; it is never passed to `kiro-cli`, and
the adapter reads no SQLite store.

## Tool permissions

Kiro distinguishes tools the agent may attempt from tools pre-approved for unattended
execution. Two flags control approval without a human:

- `--trust-all-tools` (`-a`): auto-approve every tool call. Use only inside a hardened sandbox.
- `--trust-tools=<names>`: auto-approve only the named tools, for example
  `--trust-tools=read,grep`; an empty value (`--trust-tools=`) trusts nothing (local
  `chat --help`).

### Built-in tool catalog

Tool names and default permissions, from the published reference and cross-checked against
symbol counts in the 2.4.2 binary (local `strings` probe). Each name in the aliases column is
accepted by `--trust-tools` alongside the primary name.

| Tool | Aliases | Default permission |
| ---- | ------- | ------------------ |
| `read` | `fs_read`, `fsRead` | Trusted in current directory |
| `write` | `fs_write`, `fsWrite` | Requires approval |
| `glob` | | Trusted in current directory |
| `grep` | | Trusted in current directory |
| `shell` | `execute_bash`, `execute_cmd` | Requires approval |
| `aws` | `use_aws` | Requires approval |
| `web_search` | | Requires approval |
| `web_fetch` | | Requires approval |
| `code` | | Trusted within workspace |
| `introspect`, `delegate`, `subagent`, `tool_search`, `knowledge`, `thinking`, `todo`, `session` | | Auto-activated or configurable |
| `report` | `report_issue` | Trusted by default |

`kiro.parsePassthroughConfig` reads `trust_all_tools` and `trust_tools` from the `kiro`
sub-object in WORKFLOW.md and rejects a config that sets both, because the two trust modes are
mutually exclusive. When `trust_all_tools` is off, `kiro.buildArgs` always emits
`--trust-tools=<joined>`, so an unset `trust_tools` list produces `--trust-tools=` and trusts
nothing. A read-only profile such as `trust_tools: [read, grep, glob]` is the least-privilege
starting point; `write` and `shell` belong there only when the workflow requires file edits or
command execution, and only inside a sandbox.

## Model selection

| Mechanism | Behavior | Evidence |
| --------- | -------- | -------- |
| `--model <MODEL>` | Select the model for this invocation | Local `chat --help` |
| `kiro-cli settings chat.defaultModel <model>` | Set the default model for new sessions | Hands-on report |
| `--list-models [--format json]` | List available models and exit | Local `chat --help`, published docs |
| `/model` slash command | Switch model inside a session | Interactive only; cannot be used in headless mode |

The available model set is fetched from the backend and depends on the account and
subscription, so it is not embedded in the binary (a `strings` search for `claude-` model IDs
returned nothing, local probe). `kiro-cli chat --list-models --format json` returns a JSON
object `{"models": [...], "default_model": "..."}`, where each model carries `model_name`,
`model_id`, `description`, `context_window_tokens`, `rate_multiplier`, and `rate_unit`. On the
probed Kiro Pro account the default model is `auto` (`rate_multiplier` 1.0), and the list
holds `claude-opus-4.7` and `claude-opus-4.6` (2.2), `claude-sonnet-4.6`/`4.5`/`4` (1.3),
`claude-opus-4.5` (2.2), `claude-haiku-4.5` (0.4), `deepseek-3.2` and `minimax-m2.5` (0.25),
`minimax-m2.1` (0.15), `glm-5` (0.5), and `qwen3-coder-next` (0.05). The `rate_multiplier`
scales credit cost per turn, so the cheapest model is far cheaper than the Claude options.
That set is account-specific and is not a stable contract: the authoritative list for an
account comes from `--list-models --format json`. The adapter hardcodes no model IDs and
validates none; whatever `model` the config carries is passed through.

Because `/model` is unavailable headless, the model can be pinned only per invocation or as a
CLI default. `kiro.buildArgs` passes `--model <MODEL>` whenever the `kiro` config sub-object
sets `model`; with no such value the CLI's own `chat.defaultModel` setting decides.

Sortie's `domain.AgentEvent.Model` field expects a model identifier such as
`claude-sonnet-4-20250514`. The headless path does not print the resolved model identifier in
machine-readable form, and the adapter leaves `Model` empty on the events it emits.

## MCP integration

Kiro CLI is an MCP client. MCP servers are managed with the `mcp` subcommand
(`add`, `remove`, `list`, `import`, `status`) and configured via JSON files. The global
configuration lives at `~/.kiro/settings/mcp.json`, with a workspace-level `mcp.json` also
supported. The config uses the standard `mcpServers` object (the binary references the
`mcpServers` key, local `strings` probe).

There is no per-launch `--mcp-config <path>` flag on `chat` (the `chat --help` flag table
above has none). Servers are configured out of band with `kiro-cli mcp import --file <FILE>
[SCOPE]` (`SCOPE` is `default`, `workspace`, or `global`; `--force` overwrites) or
`kiro-cli mcp add --name … --command … [--scope …] [--agent <AGENT>]` for one server at a time
(`mcp add --help`, `mcp import --help`).

`--require-mcp-startup` is documented to exit with code 3 when an enabled MCP server fails to
start; without it, startup failures are non-fatal and the turn proceeds.

MCP is gated behind a server-side profile, and that gate fails under `KIRO_API_KEY`
authentication. With an API key the CLI's `GetProfile` call returns HTTP 403
(`AccessDeniedException`), the CLI logs `Failed to check MCP configuration, defaulting to
disabled`, and prints `Failed to retrieve MCP settings; MCP functionality disabled` to stderr
on every invocation (authenticated probe). With MCP disabled the workspace `mcp.json` is not
loaded: a deliberately broken server placed at `<cwd>/.kiro/settings/mcp.json` does not appear
in `mcp list`, and `kiro-cli chat --no-interactive --require-mcp-startup` runs the turn to
completion and exits 0 rather than 3 (authenticated probe). The same symptom is reported in
[amazon-q-developer-cli#3603](https://github.com/aws/amazon-q-developer-cli/issues/3603) and
[#3650](https://github.com/aws/amazon-q-developer-cli/issues/3650), where it occurs under IAM
Identity Center while Builder ID is reported to work, which points to a profile-entitlement
gate rather than a missing config.

The adapter reflects that gate: it passes no MCP flag and ignores
`domain.StartSessionParams.MCPConfigPath`, since MCP injection does not function on the
unattended `KIRO_API_KEY` path and exit 3 is therefore unreachable. The `mcp import` mechanism
and the exit-3 behavior apply only when MCP is enabled, which on this evidence needs an auth
path whose profile the backend authorizes.

## Exit codes and error detection

### Documented exit codes

From the published reference:

| Code | Meaning |
| ---- | ------- |
| 0 | Success: command completed successfully |
| 1 | General failure: authentication error, invalid arguments, or operation failed |
| 3 | MCP startup failure (only when `--require-mcp-startup` is set) |

Hook exit codes are a separate system: 0 succeeds, 2 (PreToolUse only) blocks the tool and
returns stderr to the model, any other code is treated as a hook failure with stderr shown as
a warning.

### Observed exit codes and a documented conflict

Local probes on 2.4.2:

| Invocation | Exit code | Notes |
| ---------- | --------- | ----- |
| `kiro-cli --version` | 0 | |
| `kiro-cli chat --help` | 0 | |
| `kiro-cli --no-such-flag` | 2 | Top-level argument-parse error (the Rust `clap` usage-error code) |
| `kiro-cli no-such-subcommand` | 2 | `error: unrecognized subcommand 'no-such-subcommand'` |
| `kiro-cli chat --no-such-flag` (pristine auth state) | 1 | Subcommand-level error; reachable only while no device registration is cached |
| `kiro-cli chat --no-interactive ...` (no credential at all) | hangs | Enters interactive device login; killed by external timeout |
| `kiro-cli whoami` (valid `KIRO_API_KEY`) | 0 | Prints `Authenticated with API key` and the account email (authenticated probe) |
| `kiro-cli chat --list-models --format json` (valid key) | 0 | Prints the model JSON and exits (authenticated probe) |
| `kiro-cli chat --no-interactive ...` (valid key, success) | 0 | Answer on stdout, `▸ Credits: … • Time: …` trailer on stderr (authenticated probe) |
| `kiro-cli chat --no-interactive ...` (invalid `KIRO_API_KEY`) | **0** | Empty stdout, `Authentication failed.` on stderr, no credits trailer. Fails fast, does not hang (authenticated probe) |

Conflict, then reconciliation: the docs fold "invalid arguments" into exit code 1, but the
binary returns the `clap` usage-error code 2 for an unrecognized top-level flag or subcommand
(local probe). The reconciliation that fits the observations is that the top-level `clap`
parser uses code 2 for malformed invocations, while a subcommand's own argument and semantic
errors exit 1 (a pristine `kiro-cli chat --no-such-flag` exits 1, local probe), which is the
case the docs describe. The docs do not mention code 2 at all. The adapter reads no meaning
into the specific value: any non-zero exit reaches the shared decision's non-zero row and
fails the turn.

**Exit code 0 does not mean the turn succeeded.** A successful turn and an invalid-credential
turn both exit 0. With an invalid `KIRO_API_KEY`, `chat --no-interactive` produces empty
stdout, prints `Authentication failed. Your API key may be invalid or expired.` to stderr,
emits no `▸ Credits:` trailer, and exits 0 (authenticated probe). This conflicts with the
documented "exit 1 = authentication error". The invalid-credential case is distinct from the
no-credential case: an invalid key fails fast as just described, whereas no credential at all
triggers the interactive device-login hang. So an exit-0 turn with empty stdout and no credits
trailer is a silent failure, not an empty success. `kiro.KiroAdapter` never maps a bare exit 0
to `turn_completed`: its `OnFinalize` hook reports `agentcore.TerminalSuccess` only when the
credits trailer is on stderr, and reports `agentcore.TerminalFailure` with
`domain.ErrResponseError` and the message `kiro authentication failed` when exit 0 arrives with
`Authentication failed.` on stderr and empty stdout. The `whoami` canary in
`kiro.checkCredential` catches an invalid key before any turn runs.

A cached device registration makes flag validation nondeterministic: once the `auth_kv` table
holds a registration written by an earlier login attempt, an unauthenticated `chat` invocation
resumes that pending device authorization and blocks on the login flow before surfacing a flag
error. In that state, `kiro-cli chat --no-such-flag`, `kiro-cli chat --model` (missing value),
and `kiro-cli chat --agent-engine bogus` all print the device code and hang rather than
returning a parse-error code (repeated local probes); in a pristine state the same unknown-flag
invocation exits 1. Kiro is therefore no guard against a malformed invocation, which is why the
credential preflight and the external cancellation bound carry that weight instead.

### How a turn is classified

Because there is no structured output, classification rests on three signals:

1. The `▸ Credits: … • Time: …` trailer on stderr, the one positive proof that a turn actually
   executed (authenticated probe). Its presence with exit 0 means success; its absence with
   exit 0 means the turn did not run, most commonly an authentication failure.
2. The `Authentication failed.` line on stderr, which with exit 0 and empty stdout marks an
   invalid-credential turn rather than an empty success.
3. The process exit and signal status. A non-zero code means failure and the specific value
   carries no category (see the conflict above). Exit 3 would mean an MCP startup failure under
   `--require-mcp-startup`, which the adapter never passes and which is unreachable under
   `KIRO_API_KEY` auth anyway (see MCP integration).

Error prose in the transcript is not a fourth signal. The binary defines quota error types
`DailyRequestCount`, `MonthlyRequestCount`, and `InsufficientModelCapacity` (local `strings`
probe) that surface as upstream failures, but `kiro.classifyStderr` matches only the credits
and authentication markers and the adapter classifies nothing else from text.

Resulting `domain.TurnResult.ExitReason`, as `kiro.KiroAdapter` and the
`agentcore.ForkPerTurnSession` skeleton produce it:

| Kiro evidence | `ExitReason` and error kind |
| ------------- | --------------------------- |
| Exit 0 with a `▸ Credits:` trailer on stderr | `turn_completed`; the session also arms `--resume` for later turns |
| Exit 0 with empty stdout, no credits trailer, `Authentication failed.` on stderr | `turn_failed` with `domain.ErrResponseError` and the message `kiro authentication failed` |
| Exit 0 with no credits trailer and no authentication marker | `turn_failed` with `domain.ErrTurnFailed`, reported as `agent exited without producing output: no credits trailer on stderr` |
| Any other non-zero exit that is not a signal or 127 | `turn_failed` with `domain.ErrPortExit` and the message `exit code N` |
| Exit 127 | `turn_failed` with `domain.ErrAgentNotFound`; the skeleton handles this arm before the adapter's finalizer, so it is not `startup_failed` |
| SIGTERM or SIGKILL, or a cancelled context | `turn_cancelled` with `domain.ErrTurnCancelled` |
| No-credential launch that hangs | Prevented by the credential preflight in local mode; a hang that reaches a cancelled context yields `turn_cancelled` |

The shared disposition rule in `agentcore.DecideTurn` treats a zero exit code with no positive
work signal as a failed turn, and that signal is ordinarily a per-turn token count. Headless
Kiro reports no token counts, so the adapter reports `agentcore.WorkUnobservable` with the
detail `no credits trailer on stderr` and expresses the same guard in the credits currency: the
trailer is its only success signal, and the trailer's absence with exit 0 is its only evidence
that nothing was produced. Non-empty answer stdout is deliberately not read as a second, looser
signal, because Kiro's stdout is an unstructured transcript with no field distinguishing an
answer from an error message.

## Token usage and cost reporting

**The headless path does not report token counts.** It reports an abstract "credits" cost and
elapsed time as human-readable text.

- The first-party headless announcement does not mention token usage or cost reporting.
- The cost line, `▸ Credits: 0.01 • Time: 1s` (see "What stdout actually contains" for the
  stream it lands on), carries an abstract credits figure and elapsed time, and no input or
  output token counts. The hands-on report shows the same shape with other values
  (`▸ Credits: 0.05 • Time: 6s`), so the shape is stable and the numbers are not a contract.
- Token and context usage are available only through interactive slash commands. The binary
  defines `/usage`, `/context`, `/model`, `/tools`, and `/compact` (local `strings` probe),
  and the chat docs describe `/context show` displaying "per-file token usage". None of these
  are available in `--no-interactive` mode.
- Token counts exist internally as telemetry sent to AWS (the binary defines telemetry fields
  including `output_token_size`, `conversation_id`, and `message_id`, local `strings` probe),
  but telemetry is not exposed on stdout.

### Consequences for `EventTokenUsage` and budget tracking

Sortie's `domain.TokenUsage` carries `InputTokens`, `OutputTokens`, `TotalTokens`, and
`CacheReadTokens`, and `domain.EventTokenUsage` exists to carry these normalized counters. The
orchestrator computes deltas across turns and accumulates session totals.

None of those fields has a source on the headless path, and the credits figure is an abstract
billing unit that `domain.TokenUsage` has no field for. Kiro is therefore a
no-token-accounting adapter: its `GetUsage` hook returns a zero `domain.TokenUsage`, it emits
no `domain.EventTokenUsage`, and `domain.TurnResult` leaves both `Usage` and `UsageMeasured`
at their zero values on every turn. Every run is reported unmeasured, and token-based budget
enforcement is inert for this adapter; only cancellation bounds a turn.

## Concurrency and session isolation

Each headless turn is an independent `kiro-cli` subprocess. Sessions are scoped to the working
directory (local `chat --help`), and Sortie runs one agent session per workspace per issue, so
two turns for the same issue never share a cwd concurrently. The shared local state across
processes is the SQLite store at `~/.local/share/kiro-cli/data.sqlite3` and the configuration
under `~/.kiro`. Two processes in different workspace directories keep separate conversation
sets because `conversations_v2` is keyed by the workspace path. Sortie runs one session per
workspace, so concurrent writes to a single workspace's conversation do not arise.
`kiro.KiroAdapter` is safe for concurrent use across sessions: one adapter instance serves all
sessions and per-session state lives in `kiro.sessionState` behind `domain.Session.Internal`,
while turns within one session are serialized by the orchestrator.

## Adapter lifecycle

`kiro.KiroAdapter` is a synchronous, launch-per-turn adapter built on
`agentcore.ForkPerTurnSession`:

- `NewKiroAdapter` parses the `kiro` config sub-object into `kiro.passthroughConfig` and fails
  when `trust_all_tools` and `trust_tools` are both set.
- `StartSession` resolves the launch target, runs the credential preflight in local mode,
  builds `kiro.sessionState`, and constructs the fork-per-turn session. It starts no
  long-lived process.
- `RunTurn` resets the per-turn stdout accumulator and delegates to
  `agentcore.ForkPerTurnSession.RunTurn`, which forks `kiro-cli chat --no-interactive` with the
  arguments from `kiro.buildArgs` in the workspace directory. It panics when
  `domain.RunTurnParams.OnEvent` is nil, and returns `domain.ErrResponseError` when the
  session's `Internal` value is not a `kiro.sessionState`.
- Events: each non-empty stdout line becomes a `domain.EventNotification`, and the terminal
  event is `turn_completed`, `turn_failed`, or `turn_cancelled`. The adapter sets no
  `EmitSessionStartID` hook, so it emits no `domain.EventSessionStarted`; it emits no
  `domain.EventTokenUsage` and no `domain.EventToolResult`.
- `StopSession` returns nil when no subprocess is active and otherwise delegates to
  `agentcore.ForkPerTurnSession.Stop`. `EventStream` returns nil.

## Comparison with Sortie's other agent adapters

| Aspect | Claude Code | Codex CLI | OpenCode `run` | Kiro CLI |
| ------ | ----------- | --------- | -------------- | -------- |
| Binary | `claude` | `codex` | `opencode` | `kiro-cli` |
| Lineage | Anthropic | OpenAI | OpenCode | Amazon Q Developer CLI, Kiro distribution |
| Integration | Subprocess per turn | Persistent JSON-RPC app-server | Subprocess per turn (in-process server) | Subprocess per turn |
| Headless output | `--output-format stream-json` (JSONL) | JSON-RPC notifications (JSONL) | `--format json` (CLI projection) | Plain text transcript, no structured output |
| Token usage | `usage` in result event | `usage` in `turn/completed` | `step_finish.tokens` (step-scoped) | None on headless path (credits only) |
| Auth | `ANTHROPIC_API_KEY`, Bedrock, Vertex | `CODEX_API_KEY`, ChatGPT session | Provider env vars and config | `KIRO_API_KEY` (Pro+), Builder ID, IAM Identity Center |
| Permission bypass | `--dangerously-skip-permissions` | `approvalPolicy: "never"` | `--dangerously-skip-permissions` | `--trust-all-tools` or `--trust-tools` |
| Session resume | `--resume <id>` | `thread/resume` | `--session <id>` | `--resume` (directory-scoped, no ID) |
| Models | Claude family | OpenAI family | provider/model | Backend-served set (Claude, DeepSeek observed) |
| MCP config | `--mcp-config <path>` | `.codex/mcp.json`, `config.toml` | `opencode.json` | `~/.kiro/settings/mcp.json`, workspace `mcp.json` |
| Cancellation | Signal | `turn/interrupt` then signal | Signal | Signal (no native interrupt) |

The fifth adapter, `internal/agent/copilot`, is also launch-per-turn and parses a structured
stream (`--output-format json`). Kiro is closest to the OpenCode `run` adapter in shape but has
strictly less information on stdout: OpenCode `run --format json` at least emits a CLI JSON
projection, while Kiro emits only a human transcript.

## Open questions

- The shape of a tool-using turn. Coverage extends to no-tool turns only, so where
  tool-progress lines land relative to stdout and stderr rests on the hands-on report alone.
  Settled by an
  authenticated headless turn with `--trust-tools=read,grep` over a small workspace, capturing
  stdout and stderr to separate files.
- Behavior on quota exhaustion and on a mid-turn upstream error. The `DailyRequestCount`,
  `MonthlyRequestCount`, and `InsufficientModelCapacity` error types exist in the binary, but
  the exit code and stderr shape they produce are unobserved, so the adapter's classification
  of them is unknown. Settled by driving an account to its request limit, or by inducing an
  upstream error mid-turn, under a captured-streams probe.
- Whether MCP loads under an auth path other than `KIRO_API_KEY`. The upstream issues report
  Builder ID working while IAM Identity Center fails. Settled by repeating the workspace
  `mcp.json` probe (`mcp list` plus `chat --no-interactive --require-mcp-startup`) under a
  Builder ID login.

## Sources

Rendered docs (`kiro.dev`):

- [CLI overview](https://kiro.dev/docs/cli/)
- [Headless mode](https://kiro.dev/docs/cli/headless/)
- [Migrating from Amazon Q](https://kiro.dev/docs/cli/migrating-from-q/)
- [Exit codes](https://kiro.dev/docs/cli/reference/exit-codes/)
- [Built-in tools](https://kiro.dev/docs/cli/reference/built-in-tools/)
- [CLI commands](https://kiro.dev/docs/cli/reference/cli-commands/)
- [Chat command](https://kiro.dev/docs/cli/chat/)

First-party blog:

- [Introducing headless mode](https://kiro.dev/blog/introducing-headless-mode/)

Source code and tracker:

- [`aws/amazon-q-developer-cli`](https://github.com/aws/amazon-q-developer-cli) (CLI source of record)
- [`kirodotdev/Kiro`](https://github.com/kirodotdev/Kiro) (IDE and public issue tracker)
- [Kiro#5423: structured headless output request](https://github.com/kirodotdev/Kiro/issues/5423)
- [amazon-q-developer-cli#3603: MCP functionality disabled](https://github.com/aws/amazon-q-developer-cli/issues/3603)
- [amazon-q-developer-cli#3650: MCP disabled under Identity Center](https://github.com/aws/amazon-q-developer-cli/issues/3650)
- [Tag v1.19.7](https://github.com/aws/amazon-q-developer-cli/releases/tag/v1.19.7)

Independent hands-on report:

- [DevelopersIO (Classmethod): Kiro CLI 2.0 headless mode and API-key auth](https://dev.classmethod.jp/en/articles/kiro-cli-2-0-headless-mode-api-key-auth/)
