# Kiro CLI: adapter research notes

> Kiro CLI 2.4.2 (`kiro-cli`), researched and probed on 2026-05-27 on Linux x86_64.
> Reference for implementing the Kiro `AgentAdapter`.
>
> Provenance: Kiro CLI is the rebranded Amazon Q Developer CLI. The November 2025
> Kiro general-availability release renamed the `q` binary to `kiro-cli` and moved
> configuration from `~/.aws/amazonq` to `~/.kiro`. The installed 2.4.2 binary is built from
> the open-source [`aws/amazon-q-developer-cli`](https://github.com/aws/amazon-q-developer-cli)
> codebase: it embeds Rust source paths under `crates/q_cli/` and changelog entries that link
> to pull requests in that repository (local `strings` probe of the 2.4.2 binary).
>
> Local validation: direct probes of `kiro-cli 2.4.2`, including authenticated headless turns
> run with a `KIRO_API_KEY` credential on a Kiro Pro account. These cover help surfaces, exit
> codes, authentication gating, the SQLite store schema, the success-turn stdout shape, the
> cost-trailer location, conversation persistence and resume, the live model list, and the MCP
> config-load mechanism. Claims that rest on secondary sources are attributed in the prose;
> behaviors not exercised here are listed under "Known limitations". All sources are linked
> under "Sources" at the end.
>
> Version note: the docs at `kiro.dev` track the current product. The source repository
> tags its own releases on a separate `1.x` line. Runtime claims here are anchored to the
> installed 2.4.2 binary and the `kiro.dev` docs; the source repository is corroborating
> evidence, not the version of record.

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

This places the Kiro adapter in Sortie's synchronous, launch-per-turn category. It satisfies
`domain.AgentAdapter` (defined in `internal/domain/agent.go`) with one subprocess per turn,
delivers events through the `RunTurn` `OnEvent` callback, and returns `nil` from
`EventStream()`.

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

For backward compatibility the rebrand keeps the legacy `q` and `q chat` entry points
working, and existing Amazon Q users migrate in place via `q update`.

| Item | Requirement | Evidence |
| ---- | ----------- | -------- |
| Kiro CLI binary | `kiro-cli` on `PATH` | Local probe, 2.4.2 |
| Credentials | A valid Builder ID, IAM Identity Center, social, external IdP, or `KIRO_API_KEY` token | Published docs, local probe |
| Subscription for headless | `KIRO_API_KEY` requires Kiro Pro, Pro+, or Power | Published docs |
| Working directory | Any project directory; the adapter must launch with cwd set to the workspace | Local probe |
| Headless use | `kiro-cli chat --no-interactive` runs without a TTY once a credential is present | Published docs, hands-on report |

The binary is large (roughly 118 MB for 2.4.2) and dynamically linked against glibc (local
`file` probe). It bundles shell-integration assets (`inline`, autosuggestions, completions)
that are irrelevant to headless orchestration.

## Provenance and the Amazon Q lineage

The rebrand is not cosmetic-only, and the lineage matters because it lets the adapter reuse
verified facts from the open-source Amazon Q Developer CLI while staying anchored to the
Kiro distribution.

Evidence that `kiro-cli` 2.4.2 is the rebranded Amazon Q Developer CLI:

- The binary embeds Rust crate paths such as `crates/q_cli/src/cli/mod.rs`,
  `crates/fig_auth/src/session.rs`, and `crates/fig_api_client/src/endpoints.rs`, and the
  symbol `amzn_codewhisperer_client` (local `strings` probe).
- The 2.4.2 changelog strings link to pull requests in
  `github.com/aws/amazon-q-developer-cli` (for example `/pull/2561` and `/pull/2516`)
  (local `strings` probe).
- The official migration guide documents the `q` to `kiro-cli` rename and the configuration
  move from `~/.aws/amazonq` to `~/.kiro`.
- The CLI talks to AWS CodeWhisperer endpoints under SSO authentication
  (`codewhisperer.us-east-1.amazonaws.com`, `cps.prod-us-east-1.codewhisperer.ai.aws.dev`)
  and to Kiro runtime endpoints under API-key authentication
  (`https://runtime.us-east-1.kiro.dev`, `https://runtime.eu-central-1.kiro.dev`)
  (local `strings` probe).

The `kirodotdev/Kiro` repository is the Kiro IDE and the public issue tracker (TypeScript,
not the CLI source). CLI feature requests such as machine-readable headless output are filed
there. The CLI source of record remains `aws/amazon-q-developer-cli`.

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

`KIRO_API_KEY` is the credential the adapter should rely on. Per the first-party blog, "when
the `KIRO_API_KEY` environment variable is set, Kiro CLI skips the browser-based login flow
entirely." The headless docs state plainly that "headless mode requires an API key set as the
`KIRO_API_KEY` environment variable." The binary confirms an `API_KEY` token type and an
`https://runtime.<region>.kiro.dev` endpoint for that path, plus a changelog entry "Fix API
key authentication failing for non us-east-1 users" (local `strings` probe), so the adapter
MUST treat the active region as a configurable input rather than assuming `us-east-1`.

### Token storage

Authentication tokens persist in a SQLite database at
`~/.local/share/kiro-cli/data.sqlite3`, table `auth_kv`, under keys such as
`kirocli:odic:token`, `kirocli:odic:device-registration`, `kirocli:social:token`, and
`kirocli:external-idp:token` (local SQLite probe). The stored OIDC client registration names
the client `Kiro CLI`, uses the `DeviceCode` OAuth flow, and requests the CodeWhisperer
scopes `codewhisperer:completions`, `codewhisperer:analysis`, and
`codewhisperer:conversations` (local SQLite probe). The adapter does not need to read this
store; it should provide `KIRO_API_KEY` and let the CLI manage tokens.

### The login-hang hazard (critical)

When no valid credential is present, the behavior is not uniform across commands, and this
asymmetry is the single biggest operational hazard for the adapter:

- `kiro-cli mcp list` and `kiro-cli agent list` fail fast with exit code 1 and the message
  `error: You are not logged in, please log in with kiro-cli login` (local probe).
- `kiro-cli chat ...` (including `chat --list-models` and `chat --list-sessions`) instead
  enters an interactive device-authorization flow. It prints a device code and a
  `view.awsapps.com/start` URL to stdout, then spins a `Logging in...` indicator and blocks
  indefinitely waiting for the operator to complete browser authentication (local probe).

The `--no-interactive` flag does not suppress this login flow. A headless `chat` invocation
without a valid credential does not error out; it hangs until killed. Confirmed locally:
`kiro-cli chat --no-interactive --trust-all-tools "..."` with no credential produced only the
device-login prompt and spinner on stdout, an empty stderr, and never exited on its own.

The authenticated probe sharpened this hazard into two distinct cases. An **invalid**
`KIRO_API_KEY` does not hang: `chat --no-interactive` fails fast with `Authentication failed.`
on stderr, empty stdout, and exit 0. The indefinite device-login hang is specific to **no
credential at all** (no `KIRO_API_KEY` and no cached SSO token). The two failure shapes need
different defenses.

Adapter requirement: the adapter MUST verify that `KIRO_API_KEY` (or a pre-completed SSO
token) is present before launching `chat` (defends against the hang), SHOULD validate the
credential is usable with a free `kiro-cli whoami` or `kiro-cli chat --list-models` call
(defends against the silent exit-0 empty turn an invalid key produces), and MUST enforce its
own turn timeout as a backstop, because the CLI provides no self-timeout on the login wait.

## Relevant CLI commands and flags

### `kiro-cli chat`

The flag surface below is captured verbatim from `kiro-cli chat --help` on 2.4.2 (local
probe), cross-checked against the published CLI command reference. The internal usage string
is `kiro-cli-chat chat [OPTIONS] [INPUT]`, where `[INPUT]` is "the first question to ask".

| Flag | Short | Meaning | Adapter use |
| ---- | ----- | ------- | ----------- |
| `--no-interactive` | | Run without expecting user input; print the response to stdout and exit | Required for headless turns |
| `--trust-all-tools` | `-a` | Allow any tool without confirmation | One option for unattended runs (see permissions) |
| `--trust-tools <NAMES>` | | Trust only a comma-separated set, for example `--trust-tools=read,grep`; empty value trusts nothing | Preferred least-privilege option |
| `--model <MODEL>` | | Model to use for this invocation | Primary per-turn model selector |
| `--agent <AGENT>` | | Context profile (custom agent) to use | Optional; selects a named agent config |
| `--resume` | `-r` | Resume the most recent conversation from this directory | Continuation, but see resume caveat |
| `--resume-id <SESSION_ID>` | | Resume a specific conversation by session ID | Preferred deterministic continuation |
| `--resume-picker` | | Interactively select a conversation to resume | Interactive only; unusable headless |
| `--list-sessions` | `-l` | List saved chat sessions for the current directory | Enumeration |
| `--delete-session <SESSION_ID>` | `-d` | Delete a saved chat session by ID | Cleanup |
| `--session-source <v1\|v2>` | | Target only the v1 or v2 store for `--delete-session` (default: both) | Two conversation stores exist (see session continuity) |
| `--list-models` | | List available models and exit | Meta command; honors `--format` |
| `--format <FORMAT>` | `-f` | Output format for list commands: `plain`, `json`, `json-pretty` (default `plain`) | Applies to `--list-models` only, not to chat turns |
| `--require-mcp-startup` | | Exit with code 3 if any enabled MCP server fails to start | Fail-fast for MCP-dependent pipelines |
| `--wrap <WRAP>` | `-w` | Line wrapping: `always`, `never`, `auto` (default auto-detect) | Set `never` for raw output when parsing |
| `--agent-engine <ENGINE>` | | Agent engine: `v2` (default), `v1`, or `kas` | Leave default unless a reason exists |
| `--mode <MODE>` | | KAS agent mode: `vibe` (default) or `spec` | Only meaningful with the `kas` engine |
| `--tui` | | Use the new terminal UI | Interactive only |
| `--legacy-ui` | | Use the legacy harness (alias `--classic`) | Interactive only |
| `--verbose` | `-v` | Increase logging verbosity (repeatable) | Diagnostic |

Example headless invocation:

```bash
KIRO_API_KEY="$KIRO_API_KEY" \
kiro-cli chat \
  --no-interactive \
  --trust-tools=read,grep \
  --model "$MODEL" \
  --wrap never \
  -- "Implement the requested fix"
```

The `--` separator passes the full prompt as one positional argument so a wrapper does not
have to escape leading hyphens.

### Other commands

| Command | Subcommands or use | Evidence |
| ------- | ------------------ | -------- |
| `kiro-cli mcp` | `add`, `remove`, `list`, `import`, `status` | Local `mcp --help`, published docs |
| `kiro-cli agent` | `list`, `create`, `edit`, `validate`, `migrate`, `set-default` | Local `agent --help` |
| `kiro-cli settings` | Read or write CLI settings, for example `kiro-cli settings chat.defaultModel <model>` | Hands-on report, local probe |
| `kiro-cli login` / `logout` / `whoami` | Authentication management; `whoami` prints `Not logged in` or the active method | Local probe |
| `kiro-cli mcp` / `agent` when unauthenticated | Fail fast with exit 1 (`You are not logged in`) | Local probe |

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
this directory" (local `chat --help`). The adapter MUST launch with cwd set to the per-issue
workspace so that resume and session listing operate on the correct conversation set.

### Process lifetime and cancellation

Kiro CLI exposes no native per-turn timeout flag on `chat` (local `chat --help`). Turn
duration is therefore the orchestrator's responsibility, enforced by killing the subprocess.
Combined with the login-hang hazard above, this means the orchestrator's turn timeout is the
only reliable upper bound on a Kiro turn. On SIGTERM the process terminates and the shell
reports exit status 143 (128 + 15); on SIGKILL, 137 (128 + 9). The adapter maps signal-caused
termination to `turn_cancelled`.

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

Caution on secondary sources: some web summaries describe `--output-format json` as if it
ships in Kiro CLI. It does not in 2.4.2. Those summaries appear to paraphrase the wording of
feature request #5423. Treat the binary `--help` and `strings` output as authoritative for
this version.

### What stdout actually contains

In `--no-interactive` mode the stdout stream is a human transcript, not a parseable protocol.
The authenticated probe refined the picture of how the streams divide:

- **stdout carries the assistant answer.** For a turn that invokes no tools, stdout held only
  the answer, prefixed with a colorized `> ` marker and ANSI styling, with no trailing
  newline (observed: `\x1b[38;5;141m> \x1b[0mPONG`). Per the independent hands-on report, a
  turn that invokes tools also prints tool-progress lines such as
  `Reading directory: ... (using tool: read ...)` and markdown answers including tables. The
  tool-progress case was not re-exercised here, so where exactly tool logs land relative to
  stdout and stderr is still secondary-sourced.
- **stderr carries the cost trailer and warnings.** The closing cost line of the form
  `▸ Credits: 0.01 • Time: 1s` is emitted on **stderr**, not stdout. The format matches the
  `▸ Credits: 0.05 • Time: 6s` shape from the hands-on report, so the trailer format is
  corroborated by two independent observations. A `Failed to retrieve MCP settings` warning
  and any `--trust-tools` warnings also land on stderr.

Consequences for parsing:

- The cost trailer on stderr is the most reliable success signal available on the headless
  path: a turn that actually ran prints `▸ Credits: …`, whereas an auth failure prints
  `Authentication failed.` and no trailer (see "Exit codes"). Exit code alone is not
  sufficient because both success and auth failure exit 0.
- Output is formatted for a terminal and may carry markdown and ANSI styling. Set
  `--wrap never` to avoid width-based line wrapping, and strip ANSI when capturing to a log.
- There is no per-event timestamped stream. Tool calls cannot be reconstructed reliably from
  stdout text, so the adapter cannot emit precise `EventToolResult` events with
  `ToolDurationMS` from the headless path.

## Session continuity

Kiro CLI persists conversations per directory and supports several resume modes (local
`chat --help`, published chat docs):

| Mode | Mechanism | Adapter notes |
| ---- | --------- | ------------- |
| Fresh | No resume flag | Default; starts a new conversation in the cwd |
| `--resume` / `-r` | Resume the most recent conversation from this directory | Convenient, but "most recent" is positional, not an explicit identity |
| `--resume-id <SESSION_ID>` | Resume a specific session by ID | Preferred deterministic continuation; map to `StartSessionParams.ResumeSessionID` |
| `--resume-picker` | Interactive selection | Unusable headless |
| `--list-sessions` / `-l` | List saved sessions for the cwd | Enumeration before resume |
| `--delete-session <ID>` | Delete a saved session | Cleanup; `--session-source <v1\|v2>` selects which store |

Two stores exist. The `--session-source` flag accepts `v1` or `v2` and defaults to both for
deletion (local `chat --help`), indicating a legacy (v1) and current (v2) conversation store.
The store schema is observable directly. The `~/.local/share/kiro-cli/data.sqlite3` database
holds tables `migrations`, `history`, `auth_kv`, `state`, `conversations`, and
`conversations_v2`. Chat persistence does not live in the `state` table, which holds only
telemetry and migration keys (`telemetryClientId`, `migration.kiro.completed`, and similar);
conversations persist in the dedicated `conversations` (legacy) and `conversations_v2`
(current) tables. `conversations_v2` has columns
`key, conversation_id, value, created_at, updated_at`, keyed by the **workspace directory
path** with a UUID `conversation_id`. The `history` table is the unrelated shell
command-history feature. A headless `--no-interactive` turn persists a resumable conversation
scoped to cwd, even though `--list-sessions` does not surface it (see below).

Adapter mapping. Both resume paths work headless and retain conversation context:

- `--resume-id <conversation_id>`: resumes a specific conversation by the UUID recorded in
  `conversations_v2`.
- `--resume` (no id): resumes the most recent conversation in the cwd.

`--list-sessions` returns **empty** for a workspace whose only conversation was created
headless, so the adapter cannot obtain the session ID through the CLI, and the session ID is
absent from the headless turn output (no structured envelope exists). The two viable
continuation strategies are therefore: rely on cwd-scoped `--resume`, which needs no ID and
suits Sortie's one-conversation-per-workspace model, or read `conversation_id` from
`conversations_v2` keyed by the workspace path (brittle, depends on store internals). The
cwd-scoped `--resume` path is recommended because it avoids both ID capture and store-internal
coupling.

## Tool permissions

Kiro distinguishes tools the agent may attempt from tools pre-approved for unattended
execution. Two flags control approval without a human:

- `--trust-all-tools` (`-a`): auto-approve every tool call. Use only inside a hardened sandbox.
- `--trust-tools=<names>`: auto-approve only the named tools, for example
  `--trust-tools=read,grep`; an empty value (`--trust-tools=`) trusts nothing (local
  `chat --help`).

### Built-in tool catalog

Tool names and default permissions, from the published reference and cross-checked against
symbol counts in the 2.4.2 binary (local `strings` probe). The rebrand simplified names and
kept the old names as aliases, so both work with `--trust-tools`.

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

The migration mapping is `fs_read` to `read`, `fs_write` to `write`, `use_aws` to `aws`,
`execute_bash` to `shell`, and `report_issue` to `report`. For least-privilege headless runs,
a read-only profile such as `--trust-tools=read,grep,glob` is the natural starting point; add
`write` and `shell` only when the workflow requires file edits or command execution, and only
inside a sandbox.

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
probed Kiro Pro account the default model was `auto` (`rate_multiplier` 1.0), and the list
included `claude-opus-4.7` and `claude-opus-4.6` (2.2), `claude-sonnet-4.6`/`4.5`/`4` (1.3),
`claude-opus-4.5` (2.2), `claude-haiku-4.5` (0.4), `deepseek-3.2` and `minimax-m2.5` (0.25),
`minimax-m2.1` (0.15), `glm-5` (0.5), and `qwen3-coder-next` (0.05). The `rate_multiplier`
scales credit cost per turn, so the cheapest model is far cheaper than the Claude options.
Treat the exact set as account- and date-specific, not a stable contract; the adapter SHOULD
read `--list-models --format json` at configuration time rather than hardcode IDs. Because
`/model` is unavailable headless, the adapter MUST pin the model with `--model` per turn, or
set `chat.defaultModel` before launch.

Sortie's `AgentEvent.Model` field expects a model identifier such as
`claude-sonnet-4-20250514`. The headless path does not print the resolved model identifier in
machine-readable form, so the adapter can populate `Model` only from the value it passed via
`--model`, not from anything Kiro reports back.

## MCP integration

Kiro CLI is an MCP client. MCP servers are managed with the `mcp` subcommand
(`add`, `remove`, `list`, `import`, `status`) and configured via JSON files. The global
configuration lives at `~/.kiro/settings/mcp.json` after the rebrand (previously
`~/.aws/amazonq/mcp.json`), with a workspace-level `mcp.json` also supported. The config uses
the standard `mcpServers` object (the binary references the `mcpServers` key and a legacy
`useLegacyMcpJson` toggle, local `strings` probe).

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
loaded: a deliberately broken server placed at `<cwd>/.kiro/settings/mcp.json` did not appear
in `mcp list`, and `kiro-cli chat --no-interactive --require-mcp-startup` ran the turn to
completion and exited 0 rather than 3 (authenticated probe). The same symptom is reported in
[amazon-q-developer-cli#3603](https://github.com/aws/amazon-q-developer-cli/issues/3603) and
[#3650](https://github.com/aws/amazon-q-developer-cli/issues/3650), where it occurs under IAM
Identity Center while Builder ID is reported to work, which points to a profile-entitlement
gate rather than a missing config.

Adapter consequence: under the unattended `KIRO_API_KEY` path, MCP injection does not function
and `--require-mcp-startup` exit 3 is unreachable, so `StartSessionParams.MCPConfigPath` has no
effect. The `mcp import` mechanism and the exit-3 behavior apply only when MCP is enabled,
which on this evidence requires an auth path whose profile the backend authorizes (Builder ID
reported to work; not verified here).

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
| `kiro-cli chat --no-such-flag` (pristine auth state) | 1 | Subcommand-level error, observed once before any device registration was cached |
| `kiro-cli mcp list` (unauthenticated) | 1 | `You are not logged in` |
| `kiro-cli chat --no-interactive ...` (no credential at all) | hangs | Enters interactive device login; killed by external timeout |
| `kiro-cli whoami` (valid `KIRO_API_KEY`) | 0 | Prints `Authenticated with API key` and the account email (authenticated probe) |
| `kiro-cli chat --list-models --format json` (valid key) | 0 | Prints the model JSON and exits (authenticated probe) |
| `kiro-cli chat --no-interactive ...` (valid key, success) | 0 | Answer on stdout, `▸ Credits: … • Time: …` trailer on stderr (authenticated probe) |
| `kiro-cli chat --no-interactive ...` (invalid `KIRO_API_KEY`) | **0** | Empty stdout, `Authentication failed.` on stderr, no credits trailer. Fails fast, does not hang (authenticated probe) |

Conflict, then reconciliation: the docs fold "invalid arguments" into exit code 1, but the
binary returns the `clap` usage-error code 2 for an unrecognized top-level flag or subcommand
(local probe). The reconciliation that fits the observations is that the top-level `clap`
parser uses code 2 for malformed invocations, while a subcommand's own argument and semantic
errors exit 1 (a pristine `kiro-cli chat --no-such-flag` exited 1, local probe), which is the
case the docs describe. The docs do not mention code 2 at all. The adapter SHOULD treat any
non-zero exit as failure and not depend on the specific value to distinguish argument errors
from runtime errors.

**Exit code 0 does not mean the turn succeeded.** A successful turn and an invalid-credential
turn both exit 0. With an invalid `KIRO_API_KEY`, `chat --no-interactive` produces empty
stdout, prints `Authentication failed. Your API key may be invalid or expired.` to stderr,
emits no `▸ Credits:` trailer, and exits 0 (authenticated probe). This conflicts with the
documented "exit 1 = authentication error". The invalid-credential case is distinct from the
no-credential case: an invalid key fails fast as just described, whereas no credential at all
triggers the interactive device-login hang. The adapter consequence is concrete: an exit-0
turn with empty stdout and no credits trailer is a silent failure, not an empty success. The
adapter MUST NOT map exit 0 to `turn_completed` unconditionally; it MUST require a positive
success signal (the `▸ Credits:` trailer on stderr, or non-empty answer stdout) and treat
`Authentication failed.` on stderr as `turn_failed`. Validating the key in `StartSession` with
a free `whoami` or `--list-models` call catches an invalid key before any turn runs.

Second observation, and a nondeterminism worth recording: once the CLI has a cached device
registration (written to the `auth_kv` table after any login attempt), an unauthenticated
`chat` invocation resumes that pending device authorization and blocks on the login flow
before surfacing a flag error. In that state, `kiro-cli chat --no-such-flag`,
`kiro-cli chat --model` (missing value), and `kiro-cli chat --agent-engine bogus` all printed
the device code and hung rather than returning a parse-error code (repeated local probes). In
a pristine state the same unknown-flag invocation exited 1. The adapter MUST NOT rely on Kiro
to validate flags with a clean exit code; it MUST guarantee a credential before launch and
enforce an external timeout.

### Failure signals available to the adapter

Because there is no structured output, failure detection on the headless path rests on the
following signals, in priority order:

1. The `▸ Credits: … • Time: …` trailer on stderr. This is the one reliable positive signal
   that a turn actually executed (authenticated probe). Its presence with exit 0 indicates
   success; its absence with exit 0 indicates the turn did not run (most commonly an
   authentication failure).
2. The `Authentication failed.` line on stderr. Present with exit 0 and empty stdout, it marks
   an invalid-credential turn that the adapter MUST classify as `turn_failed`, not as an empty
   success.
3. Process exit code. Non-zero indicates failure; the specific value is not a reliable
   category (see conflict above). Exit 0 is not by itself a success signal (see the
   authenticated finding above). Exit 3 would indicate MCP startup failure under
   `--require-mcp-startup`, but is unreachable under `KIRO_API_KEY` auth, where MCP is disabled
   (see MCP integration).
4. Signal termination. SIGTERM (143) or SIGKILL (137) indicates the orchestrator cancelled the
   turn; map to `turn_cancelled`.
5. stdout/stderr text. The transcript may contain error prose, including quota messages. The
   binary defines quota error types `DailyRequestCount`, `MonthlyRequestCount`, and
   `InsufficientModelCapacity` (local `strings` probe), which surface as upstream failures.
   Text classification is brittle and SHOULD be a last resort.

Suggested mapping to `domain.TurnResult.ExitReason`:

| Kiro evidence | Sortie `ExitReason` |
| ------------- | ------------------- |
| Exit 0 with a `▸ Credits:` trailer on stderr | `turn_completed` |
| Exit 0 with empty stdout, no credits trailer, `Authentication failed.` on stderr | `turn_failed` (auth) |
| Exit 1 or other non-zero (not a signal) | `turn_failed` |
| Exit 3 (`--require-mcp-startup`) | `turn_failed` (MCP startup) |
| SIGTERM or SIGKILL | `turn_cancelled` |
| Binary not found on `PATH` (exit 127) | `startup_failed` |
| No-credential launch that hangs | Prevent by preflight; if it occurs, the orchestrator timeout yields `turn_cancelled` |

The turn-disposition rule the adapter family shares treats a zero exit code with no positive
signal as a failed turn, and the positive signal is ordinarily a per-turn token count. Headless
Kiro reports no token counts at all, so it expresses the same guard in a different currency: the
`▸ Credits:` trailer is the adapter's sole positive success signal, and its absence with exit 0 is
the adapter's only evidence that nothing was produced. Non-empty answer stdout is deliberately not
read as a second, looser signal, because Kiro's stdout is an unstructured transcript with no field
distinguishing an answer from an error message.

## Token usage and cost reporting

This section answers the core question in the research issue: whether token usage or cost is
reported anywhere on the headless output path.

Finding: **the headless path does not report token counts.** It reports an abstract "credits"
cost and elapsed time as human-readable text.

- The first-party headless announcement does not mention token usage or cost reporting.
- The cost line of the form `▸ Credits: 0.01 • Time: 1s` was confirmed directly on stderr
  (authenticated probe), matching the shape the independent hands-on report observed. It
  carries an abstract credits figure and elapsed time, and no input or output token counts.
  This is the central finding for token accounting and it is now double-sourced: the headless
  path surfaces credits, never tokens.
- Token and context usage are available only through interactive slash commands. The binary
  defines `/usage`, `/context`, `/model`, `/tools`, and `/compact` (local `strings` probe),
  and the chat docs describe `/context show` displaying "per-file token usage". None of these
  are available in `--no-interactive` mode.
- Token counts exist internally as telemetry sent to AWS (the binary defines telemetry fields
  including `output_token_size`, `conversation_id`, and `message_id`, local `strings` probe),
  but telemetry is not exposed on stdout.

### Consequences for `EventTokenUsage` and budget tracking

Sortie's `domain.TokenUsage` carries `InputTokens`, `OutputTokens`, `TotalTokens`, and
`CacheReadTokens`, and `EventTokenUsage` exists to carry these normalized counters. The
orchestrator computes deltas across turns and accumulates session totals.

The Kiro headless adapter cannot populate these fields:

- There are no input or output token counts on the headless path, so `InputTokens`,
  `OutputTokens`, `TotalTokens`, and `CacheReadTokens` cannot be filled with real values.
- The only cost signal is the "credits" figure, which is an abstract billing unit. Sortie's
  `TokenUsage` has no credits or cost field, so credits do not map onto the existing model.
- Therefore the Kiro adapter SHOULD NOT emit `EventTokenUsage`, and `TurnResult.Usage` will be
  the zero value. Any budget enforcement based on token counts will be inert for the Kiro
  adapter.

Options the adapter implementation MAY consider (each a tradeoff, none free):

1. Emit no token usage and document Kiro as a no-token-accounting adapter. Lowest complexity;
   budget-by-tokens does not apply.
2. Parse the credits trailer from stderr as a cost proxy for observability only. This requires
   a new cost field in the domain model and a stable trailer format, which #5423 shows is not
   yet contracted. Not recommended until the format stabilizes.
3. Wait for, or upstream, the `--output-format json` feature in #5423, which proposes a
   structured envelope but no token counts in its current draft. This would solve parsing, not
   token accounting.

The honest conclusion for M22: the Kiro adapter is a no-token-usage adapter on the headless
path, and budget tracking by tokens is not feasible for it. This MUST be stated in the adapter
and in any operator-facing documentation.

## Timeout enforcement

Kiro CLI has no native per-turn timeout flag (local `chat --help`). The orchestrator owns
turn duration through `AgentConfig.TurnTimeoutMS` and enforces it by killing the subprocess.
Recommended sequence on timeout:

1. Send SIGTERM to the `kiro-cli` process group.
2. Wait a short grace period for clean exit.
3. Send SIGKILL if the process is still alive.

Stall detection via `AgentConfig.StallTimeoutMS` is weaker for Kiro than for protocol-based
adapters, because the headless transcript has no per-event signal the orchestrator can use to
reset a stall timer. The turn timeout is the primary control. The login-hang hazard makes this
timeout mandatory rather than optional.

## Concurrency and session isolation

Each headless turn is an independent `kiro-cli` subprocess. Sessions are scoped to the working
directory (local `chat --help`), and Sortie runs one agent session per workspace per issue, so
two turns for the same issue never share a cwd concurrently. The shared local state across
processes is the SQLite store at `~/.local/share/kiro-cli/data.sqlite3` and the configuration
under `~/.kiro`. Two processes in different workspace directories keep separate conversation
sets because `conversations_v2` is keyed by the workspace path. Concurrent writes within a
single workspace are not characterized here; Sortie runs one session per workspace, so the
case does not arise.

## Adapter implications

The evidence supports a clear shape for the Kiro adapter:

- It is a synchronous, launch-per-turn adapter. `EventStream()` returns `nil`; events are
  delivered through the `RunTurn` `OnEvent` callback.
- `StartSession` validates the credential (`KIRO_API_KEY` present, or a usable SSO token) and
  records the workspace path. It does not launch a long-lived process.
- `RunTurn` launches `kiro-cli chat --no-interactive` with the rendered prompt, the chosen
  `--model`, a `--trust-tools` allowlist (or `--trust-all-tools` inside a sandbox),
  `--wrap never`, and `--resume-id` on continuation turns. It reads stdout as an opaque
  transcript, captures it into `AgentEvent.Message` for observability, and determines
  `ExitReason` from the process exit and signal status.
- The adapter emits `EventSessionStarted` (with the session ID once known), optional
  `EventNotification` or `EventOtherMessage` carrying transcript summaries, and a terminal
  `turn_completed`, `turn_failed`, or `turn_cancelled`. It does not emit `EventTokenUsage`
  (no token data) and cannot emit reliable `EventToolResult` events (no per-tool structure on
  stdout).
- `StopSession` is a no-op for a clean turn, and on cancellation it ensures the subprocess is
  terminated.

If structured headless output ships later (tracking #5423), the adapter can be upgraded to
parse `--output-format json` and emit richer events, but token accounting still depends on
the upstream adding token counts, which the current proposal does not.

## Differences from the existing adapters

| Aspect | Claude Code | Codex CLI | OpenCode `run` | Kiro CLI |
| ------ | ----------- | --------- | -------------- | -------- |
| Binary | `claude` | `codex` | `opencode` | `kiro-cli` |
| Lineage | Anthropic | OpenAI | OpenCode | Rebranded Amazon Q Developer CLI |
| Integration | Subprocess per turn | Persistent JSON-RPC app-server | Subprocess per turn (in-process server) | Subprocess per turn |
| Headless output | `--output-format stream-json` (JSONL) | JSON-RPC notifications (JSONL) | `--format json` (CLI projection) | Plain text transcript, no structured output |
| Token usage | `usage` in result event | `usage` in `turn/completed` | `step_finish.tokens` (step-scoped) | None on headless path (credits only) |
| Auth | `ANTHROPIC_API_KEY`, Bedrock, Vertex | `CODEX_API_KEY`, ChatGPT session | Provider env vars and config | `KIRO_API_KEY` (Pro+), Builder ID, IAM Identity Center |
| Permission bypass | `--dangerously-skip-permissions` | `approvalPolicy: "never"` | `--dangerously-skip-permissions` | `--trust-all-tools` or `--trust-tools` |
| Session resume | `--resume <id>` | `thread/resume` | `--session <id>` | `--resume-id <id>` (directory-scoped) |
| Models | Claude family | OpenAI family | provider/model | Backend-served set (Claude, DeepSeek observed) |
| MCP config | `--mcp-config <path>` | `.codex/mcp.json`, `config.toml` | `opencode.json` | `~/.kiro/settings/mcp.json`, workspace `mcp.json` |
| Cancellation | Signal | `turn/interrupt` then signal | Signal | Signal (no native interrupt) |

The Kiro adapter is closest to the OpenCode `run` adapter in shape, but with strictly less
information available on stdout: OpenCode `run --format json` at least emits a CLI JSON
projection, while Kiro emits only a human transcript.

## Known limitations

- No structured event stream in headless mode. stdout carries the assistant answer (and
  tool-progress lines when tools run); there is no machine-readable envelope. This is the
  central limitation for any wrapper.
- No token-count reporting on the headless path. Only an abstract "credits" cost and elapsed
  time are surfaced, and only as text. `EventTokenUsage` cannot be populated; token-based
  budget tracking does not apply.
- `--no-interactive` does not bypass authentication. With **no** `KIRO_API_KEY` and no cached
  SSO token, `chat` blocks on an interactive device-login flow with no self-timeout (local
  probe). An **invalid** `KIRO_API_KEY` instead fails fast (exit 0, `Authentication failed.` on
  stderr, empty stdout), which is a silent non-success the adapter must detect (authenticated
  probe).
- No native per-turn timeout. The orchestrator must enforce timeouts by killing the process
  (local `chat --help`).
- Interactive-only token and model controls. `/usage`, `/context`, and `/model` are
  unavailable headless, so the model MUST be pinned with `--model` or `chat.defaultModel`.
- Exit-code granularity is coarse. Documented codes are 0, 1, and 3, with `clap` returning 2
  for argument errors that the docs do not list. Non-zero means failure; the value is not a
  reliable category.
- MCP is unavailable under `KIRO_API_KEY` auth. The `GetProfile` gate returns 403, MCP is
  disabled, a configured `mcp.json` is not loaded, and `--require-mcp-startup` exit 3 is
  unreachable. MCP reportedly works under Builder ID; not verified here.
- The shape of a tool-using turn is not characterized here. Only no-tool turns were exercised;
  where tool-progress lines land relative to stdout and stderr rests on the hands-on report.
- Exit behavior on quota exhaustion (`DailyRequestCount`, `MonthlyRequestCount`) and on a
  mid-turn upstream error is not characterized; those conditions were not induced.

## Documented conflicts and drift

| Topic | One source says | Authoritative source says | Impact |
| ----- | --------------- | ------------------------- | ------ |
| `--output-format json` for chat | Some web summaries present it as shipped | Binary `chat --help` and `strings` have no such flag in 2.4.2; it is open feature request [#5423](https://github.com/kirodotdev/Kiro/issues/5423) | High; adapters must not depend on structured headless output |
| Exit code for invalid arguments | Docs fold invalid args into code 1 | Binary returns `clap` code 2 for an unknown top-level flag (local probe) | Medium; treat any non-zero as failure |
| Version numbering | Source repo tags releases on a `1.x` line (for example [v1.19.7](https://github.com/aws/amazon-q-developer-cli/releases/tag/v1.19.7)) | Installed distribution reports `kiro-cli 2.4.2` (local probe) | Low; anchor runtime claims to 2.4.2 and the docs |
| Chat-command flag validation | A pristine `chat --no-such-flag` exits 1 (local probe) | Once a device registration is cached, the same invocation hangs on the login flow instead of erroring (repeated local probes) | Medium; preflight the credential, do not depend on flag-error exit codes |
| Exit code for authentication failure | Docs map authentication error to exit 1 | An invalid `KIRO_API_KEY` exits **0** with `Authentication failed.` on stderr and empty stdout (authenticated probe) | High; exit 0 is not a success signal, require the credits trailer |
| Credits and time trailer stream | The hands-on report shows the cost line within the printed transcript | The `▸ Credits: … • Time: …` trailer is emitted on **stderr** (authenticated probe) | Medium; read the trailer from stderr, treat stdout as the answer |
| Credits and time trailer format | Hands-on report shows `▸ Credits: 0.05 • Time: 6s` | A second observation matches the shape, `▸ Credits: 0.01 • Time: 1s` (authenticated probe); format stable, values vary | Low; the shape is stable, the numbers are not a contract |
| Headless session enumeration | `--list-sessions` lists saved sessions for the directory | It returns empty for a conversation created by a headless `--no-interactive` turn (authenticated probe) | Medium; use cwd-scoped `--resume`, the ID is not enumerable via the CLI |

## Summary: adapter implementation checklist

1. StartSession: verify the credential. Require `KIRO_API_KEY` (or confirm a usable SSO token)
   before any `chat` launch, and validate it is not merely present but usable with a free
   `kiro-cli whoami` or `kiro-cli chat --list-models` call, because an invalid key produces a
   silent exit-0 empty turn rather than an error. Record the workspace path as cwd. Do not
   start a long-lived process.
2. RunTurn (turn 1): launch `kiro-cli chat --no-interactive` with the rendered prompt passed
   after `--`, plus `--model`, a `--trust-tools` allowlist (or `--trust-all-tools` inside a
   sandbox), and `--wrap never`. Capture stdout and stderr.
3. RunTurn (turn 2+): launch in the same workspace directory and add `--resume` to continue
   the cwd-scoped conversation. This needs no session ID and is the recommended path; both
   `--resume` and `--resume-id <conversation_id>` were confirmed to retain context, but the
   session ID is not enumerable via `--list-sessions` on the headless path.
4. Output handling: treat stdout as the assistant answer (plus tool-progress text when tools
   run) and read the `▸ Credits:` trailer and warnings from stderr. Strip ANSI for logs. Store
   a summary in `AgentEvent.Message`. Do not attempt to parse a structured result.
5. Completion and failure: map exit 0 **with a `▸ Credits:` trailer on stderr** to
   `turn_completed`; exit 0 with empty stdout and `Authentication failed.` on stderr to
   `turn_failed`; non-zero to `turn_failed`; exit 3 under `--require-mcp-startup` to an MCP
   startup failure; and SIGTERM or SIGKILL to `turn_cancelled`. Exit 127 maps to
   `startup_failed` (binary not found). Do not treat a bare exit 0 as success.
6. Token usage: emit none. Leave `TurnResult.Usage` zero. Document Kiro as a no-token-accounting
   adapter.
7. Model: pin with `--model` per turn, or set `chat.defaultModel` before launch. `/model` is
   unavailable headless.
8. MCP: unavailable under `KIRO_API_KEY` auth. The `GetProfile` gate returns 403 and MCP is
   disabled, so `MCPConfigPath` has no effect and `--require-mcp-startup` is inert. If a
   workflow needs MCP, it requires an auth path the backend authorizes for the profile (Builder
   ID reported to work). When MCP is enabled, load servers with `kiro-cli mcp import --file
   "$MCPConfigPath" workspace` (there is no per-launch `--mcp-config` flag).
9. Permissions: prefer a least-privilege `--trust-tools` allowlist over `--trust-all-tools`.
10. Timeout: enforce `TurnTimeoutMS` by killing the process group. This is the only backstop
    against the login hang and the absence of a native timeout.
11. StopSession: no-op on clean exit; on cancellation, ensure the subprocess is terminated.

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
