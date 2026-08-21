# OpenCode CLI: adapter research notes

> **CLI:** OpenCode, binary `opencode`, npm package `opencode-ai`.
> **Pinned version:** v1.15.12 on Linux.
> **Serves:** the OpenCode `AgentAdapter` in `internal/agent/opencode`.
> **Instruments:** the installed binary; `npx -y opencode-ai@latest` for the v1.14.x anchors
> below; the upstream repository at the `v1.14.25` tag; the rendered docs at `opencode.ai`.
> **Coverage:**
>
> - v1.15.12, 2026-05-29: the `run` flag surface, session storage layout, and dir-scoped resume.
> - v1.14.50, 2026-05-14: the logical-failure exit code, stderr content, and duplicated `error` events. Not re-observed on v1.15.12.
> - v1.14.25, 2026-04-26: every other locally observed claim, including the credential commands, `serve` port binding, `session list` ordering, `export` payloads, permission warnings on stderr, and parallel-session isolation. Not re-observed since.
> - Source-code and raw-docs links point at the immutable `v1.14.25` tag, so quoted code and line numbers stay valid. The rendered docs track the latest release and can lead the tagged source; "Documented conflicts and drift" records where the two disagree.

## Overview

OpenCode exposes three relevant automation surfaces:

| Surface | Transport | What it does | Adapter relevance |
| ------- | --------- | ------------ | ----------------- |
| `opencode run` | stdout/stderr plus an internal or attached HTTP server | Non-interactive one-shot execution | Closest match to Claude/Copilot launch-per-turn adapters |
| `opencode serve` | HTTP + SSE | Headless server exposing sessions, messages, permissions, files, tools, and `/doc` OpenAPI | Cleaner programmatic surface than scraping CLI JSON |
| `opencode acp` | stdin/stdout nd-JSON | ACP server | Unused; the payloads are an open question |

`opencode run` is a thin client over the same server APIs `opencode serve` exposes. Without
`--attach`, `run` bootstraps an in-process server and points the SDK at
`Server.Default().app.fetch(...)`. With `--attach`, it points the SDK at an existing server URL.

That architectural detail matters. `opencode run --format json` is not the canonical OpenCode
event bus. It is a CLI-specific projection emitted by `run.ts`. The canonical bus is the
server SSE stream at `/event`.

## Installation and prerequisites

OpenCode ships as the `opencode` binary and is installed from the `opencode-ai` package or
platform-specific packages.

```bash
curl -fsSL https://opencode.ai/install | bash
npm install -g opencode-ai
brew install anomalyco/tap/opencode
```

Adapter-relevant prerequisites:

| Item | Requirement | Evidence |
| ---- | ----------- | -------- |
| OpenCode binary | `opencode` on `PATH` | README, CLI docs |
| Provider credentials | Auth file, environment variables, `.env`, or provider config | Providers docs, CLI docs, SDK types |
| Working directory | Any project directory; `run --dir` overrides cwd, `--attach` treats it as remote-server path | CLI docs, `run.ts` source |
| Headless use | `opencode run` works without a TTY | Binary probe |

## Authentication and provider configuration

OpenCode does not have a single vendor-specific auth flow. It delegates model access to
configured providers through Models.dev. Credentials can come from several places.

### Credential sources

| Source | Mechanism | Notes |
| ------ | --------- | ----- |
| Interactive credential store | `opencode providers login` or `opencode auth login`, stored in `~/.local/share/opencode/auth.json` | `auth` is an alias of `providers`; see "Documented conflicts and drift". |
| Environment variables | Provider-specific env vars such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `AWS_*`, `GITLAB_TOKEN`, `CLOUDFLARE_*`, `GOOGLE_CLOUD_PROJECT`, `VERTEX_LOCATION`, and many others | Loaded at startup alongside credentials and project `.env` files (CLI docs, providers docs). |
| `opencode.json` provider config | `provider.<id>.options.apiKey`, `baseURL`, headers, model overrides, routing options | Useful for proxy gateways, local models, or custom OpenAI-compatible providers (providers docs, SDK types). |
| Server API | `PUT /auth/{id}` | Programmatic credential injection when integrating through `serve` instead of the CLI wrapper (server and SDK docs). |

### Adapter-relevant observations

- Browser and device-code flows exist for some providers, including GitHub Copilot, OpenAI, and GitLab Duo. They block on human interaction, so an unattended run depends on environment variables, config injection, or pre-populated auth storage instead (providers docs, CLI docs). The adapter injects no credential: `buildRunEnv` passes the parent environment through unchanged apart from the five variables it manages.
- Provider fallback is not a universal CLI flag. It is provider-specific configuration. For example, OpenRouter and Vercel AI Gateway support routing and fallback policies inside `opencode.json` model options (providers docs).
- The server and SDK surfaces expose providers and default models directly through `/provider`, `/provider/auth`, and `/config/providers`, which is cleaner than parsing CLI text (server and SDK docs).

### Adapter-relevant environment variables

The full env var surface is documented in the raw `cli.mdx` and exposed via `flag.ts` (both
linked under Sources). The subset below is the one relevant to a deterministic, unattended
adapter.

`buildManagedEnv` (`internal/agent/opencode/command.go`) owns five of these variables. It sets
`OPENCODE_AUTO_SHARE=false`, `OPENCODE_DISABLE_AUTOUPDATE=true`, `OPENCODE_DISABLE_LSP_DOWNLOAD=true`,
`OPENCODE_DISABLE_AUTOCOMPACT` from the `disable_autocompact` passthrough key (default `true`), and
`OPENCODE_PERMISSION` when the workflow configures `allowed_tools` or `denied_tools`.
`shouldDropManagedEnv` strips all five from the inherited environment before the managed values are
appended, so an operator-side value never survives into the subprocess. The adapter sets none of the
other variables in this table.

| Variable | Type | Purpose | Adapter use |
| -------- | ---- | ------- | ----------- |
| `OPENCODE_CONFIG` / `OPENCODE_CONFIG_DIR` / `OPENCODE_CONFIG_CONTENT` | string | Point OpenCode at a config file, directory, or inline JSON config | Not set. Injects provider or permission config without mutating the repo. |
| `OPENCODE_PERMISSION` | string | Inline JSON permission config; `JSON.parse`d and deep-merged into the resolved `opencode.json` `permission` field at startup, not replacing it | Set from the tool policy. See "OPENCODE_PERMISSION env var format" below. |
| `OPENCODE_AUTO_SHARE` | boolean | When truthy, sessions are automatically shared on completion | Set to `false`, so an operator shell cannot leak session URLs into Sortie runs. |
| `OPENCODE_DISABLE_AUTOUPDATE` | boolean | Disable self-update | Set to `true`, which pins the version in CI and container environments. |
| `OPENCODE_DISABLE_AUTOCOMPACT` | boolean | Disable automatic context compaction between steps | Set from `disable_autocompact`, default `true`. Compaction rewrites prior turns and changes the totals `opencode export` reports. |
| `OPENCODE_DISABLE_LSP_DOWNLOAD` | boolean | Disable automatic LSP server downloads | Set to `true`, which keeps air-gapped and network-restricted runs working. |
| `OPENCODE_DISABLE_MODELS_FETCH` | boolean | Disable fetching the Models.dev catalogue | Not set. Enables fully offline runs. |
| `OPENCODE_DISABLE_PRUNE` | boolean | Disable storage pruning of old data | Not set. Relevant only to long-term local session history. |
| `OPENCODE_DISABLE_DEFAULT_PLUGINS` | boolean | Disable default plugins | Not set. See "Plugin and prompt contamination". |
| `OPENCODE_DISABLE_CLAUDE_CODE`, `OPENCODE_DISABLE_CLAUDE_CODE_PROMPT`, `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS` | boolean | Disable reading `.claude` prompt and skills content | Not set. `OPENCODE_DISABLE_CLAUDE_CODE=true` implies the other two via `flag.ts` derivation, and `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=true` also implies `OPENCODE_DISABLE_EXTERNAL_SKILLS=true`. |
| `OPENCODE_ENABLE_QUESTION_TOOL` | boolean | Enable the `question` tool, which surfaces an interactive question to the user | Not set, so the tool stays off. Complements the `question` permission. |
| `OPENCODE_SERVER_PASSWORD` / `OPENCODE_SERVER_USERNAME` | string | Basic auth for `serve` and `web`; also used by `run --attach` when `--password` is omitted | Not set. The adapter does not use `--attach`. |

## Relevant CLI commands and flags

### `opencode run`

`opencode run [message..]` is the non-interactive CLI entry point.

The flag set below is the one `opencode run --help` prints. `buildRunArgs`
(`internal/agent/opencode/command.go`) emits a fixed subset: `run --format json --dir <ws>`,
then conditionally `--session`, `--model`, `--agent`, `--variant`, `--thinking`, `--pure`,
`--dangerously-skip-permissions`, then `--` and the prompt.

| Flag | Short | Meaning | Adapter use |
| ---- | ----- | ------- | ----------- |
| `--command` |  | Run a slash command instead of a freeform prompt | Not used. |
| `--continue` | `-c` | Resume the last root session | Not used. The adapter resumes by explicit session ID. |
| `--session` | `-s` | Resume a specific session ID | Set from the persisted `sessionID`, which makes continuation deterministic. Resume is dir-scoped; see "Fresh session vs continuation". |
| `--fork` |  | Fork the resumed session first | Not used. Creates a child session before continuing. |
| `--share` |  | Share session on completion | Not used. The adapter sets `OPENCODE_AUTO_SHARE=false` instead. |
| `--model` | `-m` | Model in `provider/model` form | Set from the `model` passthrough key. |
| `--agent` |  | Primary agent name | Set from the `agent` passthrough key. `run.ts` validates the name against the available agents; a subagent name falls back to the default agent with a warning. |
| `--file` | `-f` | Attach files or directories to the prompt | Not used. |
| `--format` |  | `default` or `json` | Always `json`, which is what makes stdout parseable. |
| `--title` |  | Explicit session title | Not used. Without it the session title is the truncated prompt. |
| `--attach` |  | Target an existing `serve` instance | Not used. Would avoid a server cold start per turn. |
| `--password` | `-p` | Basic-auth password for `--attach` | Not used. Falls back to `OPENCODE_SERVER_PASSWORD` (`run.ts` source). |
| `--username` | `-u` | Basic-auth username for `--attach` | Not used. Falls back to `OPENCODE_SERVER_USERNAME`, then `opencode` (CLI docs). |
| `--dir` |  | Local cwd override, or remote path when attached | Always set to the issue workspace. Also scopes session resume. |
| `--port` |  | Local server port when not attached | Not used. For the `0` sentinel semantics see "`opencode serve`". |
| `--variant` |  | Provider-specific reasoning variant such as `high`, `max`, or `minimal` | Set from the `variant` passthrough key. |
| `--thinking` |  | Emit reasoning blocks | Set from the `thinking` passthrough key. Affects CLI output only. |
| `--dangerously-skip-permissions` |  | Auto-approve permissions that are not explicitly denied | Set from `dangerously_skip_permissions`, default `true`, which keeps unattended runs from stalling on tool prompts. |
| `--pure` |  | Run without external plugins | Set from the `pure` passthrough key, default `false`. Present in shipped help output but omitted from the CLI docs page. |
| `--print-logs` |  | Print logs to stderr during the run | Not used. Diagnostic only. |
| `--log-level` |  | Set log verbosity, one of DEBUG, INFO, WARN, ERROR | Not used. Diagnostic only. |
| `--replay` / `--replay-limit` |  | Replay recent history when resuming an interactive run | Not used. Targets interactive resume, not `--format json` automation. |

Separately, the top-level `opencode [project]` command documents a `--prompt` flag in the CLI
docs and in root help. That is a TUI and root flag rather than part of the `run` surface:
`opencode run --prompt ...` prints `run` help and exits `1`. Positional `message..` is the
non-interactive prompt input, and `buildRunArgs` passes the prompt that way.

Example invocation for a headless turn:

```bash
opencode run \
  --format json \
  --session "$SESSION_ID" \
  --model anthropic/claude-sonnet-4-20250514 \
  --dangerously-skip-permissions \
  -- "Implement the requested fix"
```

Use `--` before the prompt so shell wrappers can pass the full prompt as one positional
argument.

### `opencode serve`

`opencode serve` starts the headless HTTP server.

| Flag | Meaning | Notes |
| ---- | ------- | ----- |
| `--port` | Listen port | An effective port of `0` means try `4096` first, then fall back to an ephemeral port if `4096` is busy. Both `run --port` and `serve --port` share these semantics. |
| `--hostname` | Listen address | Docs default to `127.0.0.1`. |
| `--mdns` / `--mdns-domain` | mDNS discovery | Not relevant to a launch-per-turn adapter. |
| `--cors` | Additional CORS origins | Only matters for browser clients. |

Shared network options define `port` with a default of `0`, but both the Node and Bun server
adapters interpret `0` as a sentinel: they attempt to bind `4096` first, then fall back to an
ephemeral port only if `4096` is unavailable (server docs, `network.ts`, and the Node and Bun
server adapter sources). `opencode serve` with no flags binds `http://127.0.0.1:4096`.

### Session and provider helper commands

| Command | Use |
| ------- | --- |
| `opencode session list --format json -n N` | Enumerate recent sessions. Output is newest-first. |
| `opencode export [sessionID] [--sanitize]` | Export session data as JSON. Without arguments it exports the most recent session in the current directory; with `sessionID` it exports that exact session. `--sanitize` redacts sensitive transcript and file data. `queryExportUsage` (`internal/agent/opencode/parse.go`) runs `export --sanitize <sessionID>` after the turn subprocess exits, so token usage is recovered without leaking tool-output bodies into logs. |
| `opencode models` | List the model catalog, one `provider/model` slug per entry. `queryModelNotFound` (`internal/agent/opencode/parse.go`) runs it to restore an unknown-model diagnostic; see "Example: logical failure". |
| `opencode providers list` | Enumerate configured provider credentials. This is the primary command name in root help and in `providers --help`. |
| `opencode auth list` | Alias for `providers list`. `auth --help` keeps `auth`-prefixed subcommands under an `opencode providers` header. |

## Subprocess behavior

### `run` is not a standalone agent protocol

`opencode run` follows this control flow (`run.ts` source):

1. Parse CLI flags and optional attached files.
2. Create or resume a session through the SDK.
3. Subscribe to the server event stream.
4. Send the prompt via `sdk.session.prompt(...)` or `sdk.session.command(...)`.
5. Convert selected server events into a custom stdout JSON envelope when `--format json` is set.
6. Stop when the server reports `session.status.type == "idle"`.

Without `--attach`, the command does not spawn an external `serve` child process. It boots the
server in-process and routes SDK calls through an internal fetch function backed by
`Server.Default().app.fetch(...)`.

### Fresh session vs continuation

| Mode | Mechanism | Notes |
| ---- | --------- | ----- |
| Fresh | `sdk.session.create({ title, permission: rules })` | Session ID is server-generated and looks like `ses_...`. |
| `--session <id>` | Resume exact session ID | Deterministic resume path, but dir-scoped. |
| `--continue` | `sdk.session.list()` then first root session | `session list --format json` returns newest-first, so `--continue` resumes the most recent root session. The docs do not promise that ordering. |
| `--fork` with resume | `sdk.session.fork({ sessionID })` | Creates a child session before continuing. |

Resume is dir-scoped. `opencode run --session <id>` replays a session only when the run
executes with the same `--dir` (project directory) the session was created in. Resuming the
same session ID under a different `--dir` exits `0` with no JSON events on stdout: same-dir
resume emits events, different-dir resume emits nothing. This is consistent with sessions being
stored in a single global database (see "Session storage") and keyed to their originating
project directory. The adapter satisfies the constraint by construction: `buildRunArgs` passes
`state.target.WorkspacePath` as `--dir` on every turn, and that path is the issue workspace
resolved once per session by `agentcore.ResolveLaunchTarget`. Resuming a session under a
different workspace produces a silent empty turn rather than an error, which the adapter would
report as `turn_failed` through the zero-work row rather than as a resume fault.

When `run` creates a new session it injects three deny rules: `question=*`, `plan_enter=*`, and
`plan_exit=*` (`run.ts` source). Resumed sessions reuse the existing session state instead of
re-creating these rules.

### Working directory handling

- When not attached, `--dir` calls `process.chdir(args.dir)` before bootstrapping the local server. Invalid paths terminate immediately with exit code `1` (`run.ts` source).
- When attached, `--dir` is passed to the SDK as the remote directory selector instead of changing the local process cwd (`run.ts` source).
- The server and SDK surfaces also accept `directory` query parameters on many APIs, which makes `serve` a better fit when a single OpenCode backend serves multiple workspaces (server and SDK docs).
- The adapter sets both: `RunTurn` assigns `cmd.Dir` to the workspace path and `buildRunArgs` passes the same path as `--dir`, so the subprocess cwd and the session's project directory cannot diverge.

### Session storage

Sessions are persisted in a single global SQLite database at
`~/.local/share/opencode/opencode.db`, not in a per-project store under the workspace. The same
directory holds the credential store (`auth.json`) and other shared state.

Two consequences for the adapter:

- Sessions created in any workspace are visible to the same OS user from any other workspace. Isolation between concurrent issues comes from distinct server-generated session IDs, not from separate storage files.
- Resume stays dir-scoped at replay time even though storage is global: the session row exists regardless of cwd, but `run --session <id>` emits events only when invoked under the originating `--dir`.

## Permissions and tool access control

OpenCode permission control is config-driven. Each rule resolves to `allow`, `ask`, or `deny`
(permissions docs).

### Permission keys

The `permissions.mdx` page, rendered as the public permissions docs, documents 14 tool and
safety keys:

- `read`
- `edit`
- `glob`
- `grep`
- `bash`
- `task`
- `skill`
- `lsp`
- `question`
- `webfetch`
- `websearch`
- `codesearch`
- `external_directory`
- `doom_loop`

The runtime schema in `config/permission.ts` declares two more keys the docs page does not
surface, `list` and `todowrite`, and accepts any additional string key through a
`Schema.StructWithRest` catchall, so OpenCode validates more permission keys than the public
documentation lists. `knownPermissionKeys` (`internal/agent/opencode/command.go`) carries all 16
and uses them for one purpose only: when the workflow sets `allowed_tools`, every known key
outside that set is denied explicitly. Keys the workflow names are forwarded verbatim whether or
not they are known, with `logUnknownPermissionKey` recording the unknown ones at debug level, so
a configuration OpenCode accepts is never rejected by the adapter.

### Defaults

The documented defaults are:

| Permission | Default |
| ---------- | ------- |
| Most tools | `allow` |
| `external_directory` | `ask` |
| `doom_loop` | `ask` |
| `read` on env files | `deny` for `*.env` and `*.env.*`; `allow` for `*.env.example` (rule order matters; last matching pattern wins). |

### `OPENCODE_PERMISSION` env var format

OpenCode loads `OPENCODE_PERMISSION` at config-merge time, in
[`config/config.ts` lines 656-658](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/config/config.ts#L656-L658):

```ts
if (Flag.OPENCODE_PERMISSION) {
  result.permission = mergeDeep(result.permission ?? {}, JSON.parse(Flag.OPENCODE_PERMISSION))
}
```

Two consequences for adapters:

1. The value is parsed as JSON and must conform to the same schema as the `permission` field in `opencode.json`, defined in `config/permission.ts`.
2. The result is deep-merged into the resolved `opencode.json` `permission`, not used as a replacement. An adapter-supplied policy stacks on top of any operator-side `permission` block instead of overriding it.

Three accepted forms (all valid for `opencode.json` `permission` and therefore valid for
`OPENCODE_PERMISSION` as JSON-encoded values):

1. **Shorthand string**, applied to all keys via `*` normalization (`"allow"` becomes `{"*":"allow"}` internally):
   ```bash
   OPENCODE_PERMISSION='"deny"'
   ```
2. **Flat key→action map** (most common adapter form):
   ```bash
   OPENCODE_PERMISSION='{"read":"allow","edit":"deny","bash":"deny","external_directory":"deny"}'
   ```
3. **Granular key→{pattern→action} map** (when path/argument scoping matters):
   ```bash
   OPENCODE_PERMISSION='{"bash":{"git *":"allow","rm *":"deny","*":"deny"}}'
   ```

Action values are `"allow" | "ask" | "deny"` (see `config/permission.ts`). Pattern syntax: `*`
matches zero or more characters, `?` exactly one, `~` and `$HOME` expand to the user home;
rules are evaluated by pattern match with the last matching rule winning (see
`permissions.mdx`).

Merge semantics drive two adapter behaviors. `shouldDropManagedEnv` removes any inherited
`OPENCODE_PERMISSION` before the subprocess launches, so the operator's value cannot
pre-pollute the merge result. And `buildPermissionPolicy` emits the flat key-to-action form
covering every key it decides: the keys named in `allowed_tools` map to `allow`, the keys named
in `denied_tools` map to `deny`, and when `allowed_tools` is non-empty every remaining known key
maps to `deny`. Keys outside that policy fall through to the operator's
`~/.config/opencode/opencode.json` or to OpenCode defaults. When the workflow sets neither list,
the adapter sets no `OPENCODE_PERMISSION` at all and OpenCode's own resolution applies.
`parsePassthroughConfig` rejects a configuration that names the same key in both lists.

### Headless behavior of `run`

`run.ts` handles permission requests this way:

| Condition | CLI behavior |
| --------- | ------------ |
| `--dangerously-skip-permissions` set | Reply `once` to permission requests that are not explicitly denied |
| No bypass flag | Print a human warning and reply `reject` |

The full `Reply` schema accepts `once | always | reject`; `run.ts` only emits `once` and
`reject`, so the third value (`always`, which would whitelist a tool's suggested patterns for
the rest of the session) is not reachable through `opencode run` (`permission/index.ts`
source).

A rejection prints this pair:

```text
! permission requested: external_directory (/etc/*); auto-rejecting
{"type":"tool_use", ... "state":{"status":"error","error":"The user rejected permission to use this specific tool call."}}
```

The warning goes to stderr, and the JSON envelope goes to stdout: `run.ts` calls
`UI.println(...)` before replying `reject`, and `println` writes through `print`, which calls
`process.stderr.write` (`cli/ui.ts`, v1.14.25 tag; not re-observed on the pinned v1.15.12
binary, since reproducing it needs an authenticated run). `opencode run --format json`'s stdout
stream stays JSON-clean whether or not a permission prompt occurs. The adapter recognizes the
warning from the turn's collected stderr lines once the subprocess has exited:
`isPermissionWarning` (`internal/agent/opencode/opencode.go`) matches the
`! permission requested:` prefix against each stderr line and emits a notification. Any stdout
line that fails to parse as an event is a `domain.EventMalformed` event; the permission warning
is not one of those, because it never arrives on stdout.

## Output format: `opencode run --format json`

The CLI docs describe `--format json` as "raw JSON events". Source inspection of `run.ts` and
live runs show a narrower, CLI-defined envelope instead. The emitted objects have this
top-level shape:

```json
{
  "type": "step_start | tool_use | text | reasoning | step_finish | error",
  "timestamp": 1777197446593,
  "sessionID": "ses_236c713fcffel8QozOz4ca0AYK",
  "...": "type-specific payload"
}
```

### Stdout event types

| `type` | Payload | When emitted | Evidence |
| ------ | ------- | ------------ | -------- |
| `step_start` | `part: StepStartPart` | Start of a model step | `run.ts` source, binary probe |
| `tool_use` | `part: ToolPart` | When a tool part reaches `completed` or `error` | `run.ts` source, binary probe |
| `text` | `part: TextPart` | Completed text part | `run.ts` source, binary probe |
| `reasoning` | `part: ReasoningPart` | Completed reasoning part, only when `--thinking` is set | `run.ts` source |
| `step_finish` | `part: StepFinishPart` | End of a model step | `run.ts` source, binary probe |
| `error` | `error: EventSessionError.properties.error` | When the server emits `session.error` for this session | `run.ts` source, SDK types, binary probe |

The parser mirrors this set: `parse.go` declares one `raw*Part` struct per payload, and
`RunTurn`'s event switch handles exactly these six types, emitting `domain.EventMalformed` for
any other `type` value.

What the CLI does not emit in JSON mode:

- No `session_started` event
- No raw `message.updated` or `message.part.updated` server events
- No `permission.asked` event
- No `session.status` or `session.idle` event
- No final result or summary envelope

### Example: simple one-step turn

```json
{"type":"step_start","timestamp":1777197446593,"sessionID":"ses_236c713fcffel8QozOz4ca0AYK","part":{"id":"prt_dc938f5be001xlQ2FdVcM0ybM8","messageID":"msg_dc938ecbe001pHUOguAaJY92Pz","sessionID":"ses_236c713fcffel8QozOz4ca0AYK","snapshot":"45865d3017876fc42b80fa16e317d109a7008c30","type":"step-start"}}
{"type":"text","timestamp":1777197446597,"sessionID":"ses_236c713fcffel8QozOz4ca0AYK","part":{"id":"prt_dc938f5c3001Xf6Jb1dJzX7Po6","messageID":"msg_dc938ecbe001pHUOguAaJY92Pz","sessionID":"ses_236c713fcffel8QozOz4ca0AYK","type":"text","text":"\n\nHello","time":{"start":1777197446595,"end":1777197446596}}}
{"type":"step_finish","timestamp":1777197446660,"sessionID":"ses_236c713fcffel8QozOz4ca0AYK","part":{"id":"prt_dc938f5c600183OklHsapPOT69","reason":"stop","messageID":"msg_dc938ecbe001pHUOguAaJY92Pz","sessionID":"ses_236c713fcffel8QozOz4ca0AYK","type":"step-finish","tokens":{"total":16267,"input":14406,"output":21,"reasoning":0,"cache":{"write":0,"read":1840}},"cost":0}}
```

### Example: tool call

```json
{"type":"tool_use","timestamp":1777197461503,"sessionID":"ses_236c6de07ffeMCaCIVqcZsSjBi","part":{"type":"tool","tool":"read","callID":"call_function_1hg9s1exw5vv_1","state":{"status":"completed","input":{"filePath":"/home/ubuntu/work/sortie/README.md"},"output":"<path>/home/ubuntu/work/sortie/README.md</path>\n<type>file</type>\n<content>\n1: <p align=\"center\">\n...","metadata":{"preview":"<p align=\"center\">...","truncated":false,"loaded":[]},"title":"README.md","time":{"start":1777197461489,"end":1777197461502}},"id":"prt_dc9392fd2001HeyUJbUUYfz0Ez","sessionID":"ses_236c6de07ffeMCaCIVqcZsSjBi","messageID":"msg_dc93922a40015YBm8bwcEdTQXV"}}
```

Tool payloads can be large. The `read` tool embeds the returned file content directly in
`state.output`, so one JSON line can carry a whole file body: the CLI writes each event as
`JSON.stringify(...) + EOL` in `run.ts`. The adapter's stdout scanner
(`startOpenCodeReader`) therefore starts at a 64 KiB buffer and grows to `maxLineBytes`, which
is 10 MiB.

### Example: logical failure

An invalid model produces this (v1.14.50 anchor):

```text
{"type":"error","timestamp":1778777973456,"sessionID":"ses_1d8921de2ffeMg3sLcTUjJxwoX","error":{"name":"UnknownError","data":{"message":"Model not found: nonexistent/nonexistent."}}}
{"type":"error","timestamp":1778777973456,"sessionID":"ses_1d8921de2ffeMg3sLcTUjJxwoX","error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details."}}}
EXIT:1
```

Two `error` events reach stdout for this single failure. The first carries the actionable
diagnostic from the `session.error` event stream; the second is the generic HTTP 500 envelope
the in-process server emits when the underlying defect was not declared in the API error schema.
Both arrive wrapped as `{"name":"UnknownError","data":{"message":...}}`.

`RunTurn`'s event loop assigns `runtime.terminalError` on every `error` event, so the last one
wins and the masked message rather than the diagnostic is what would otherwise reach the
`turn_failed` event. `isMaskedServerError` (`internal/agent/opencode/opencode.go`) recognizes
that placeholder by exact match on `Unexpected server error. Check server logs for details.`,
and `finalizeExitedTurn` then calls `queryModelNotFound`, which reports
`Model not found: <model>` when the configured model is absent from the `opencode models`
catalog. A masked failure from any other cause keeps the placeholder message.

Stderr is empty for this failure and the process exits `1`. Neither signal is load-bearing:
`finalizeExitedTurn` derives the terminal failure from the `error` event alone, so a logical
failure maps to `turn_failed` whatever the exit code, and the adapter collects stderr through
`procutil.StderrCollector` and logs it at warn level instead of classifying its text.

## Turn completion, failure detection, and `TurnResult` mapping

Sortie's turn model lives in `internal/domain/agent.go`. `domain.TurnResult` carries
`SessionID`, a normalized `ExitReason`, the session's run-cumulative `Usage`, and
`UsageMeasured`, which distinguishes an unknown spend from a zero one.

### Completion detection

`run.ts` stops reading events when it sees `session.status.type === "idle"` on the underlying
server event stream. It does not print that status transition to stdout.

Practical implication:

- Process exit is the only explicit end-of-turn signal on the CLI surface.
- `step_finish` with `reason == "stop"` often coincides with normal completion, but it is not a distinct final-result event.
- Multi-step turns can emit several `step_finish` events before process exit.

### Failure detection

| Signal | Reliability | Notes |
| ------ | ----------- | ----- |
| Stdout `{"type":"error", ...}` | Authoritative | Emitted from `session.error`. A single failure can emit several `error` events; see "Example: logical failure" |
| `tool_use` with `part.state.status == "error"` | Important, but not always terminal | Permission rejection and tool failures land here |
| stderr text | Diagnostic only when present | Empty for a `session.error` failure |
| Process exit code | Informative, not load-bearing | See "Documented conflicts and drift" |

The adapter maps that evidence onto domain events as follows. `RunTurn` emits the per-event
rows; `finalizeExitedTurn` fills an `agentcore.TurnEvidence` and hands the terminal decision to
`agentcore.DecideTurn`, which is shared by every agent adapter.

| Domain event | OpenCode evidence |
| ------------ | ----------------- |
| `session_started` | The first envelope carrying a `sessionID`, applied by `applySessionEvent`. A later envelope carrying a different ID is a session id mismatch, which fails the turn |
| `tool_result` | Each `tool_use` event, using `part.tool` for the name, `part.state.time` for the duration, and `part.state.status == "error"` for the error flag |
| `notification` | `step_start`, the text of a `text` part truncated to 500 runes, `step_finish` with its reason, and permission-warning lines from stderr |
| `other_message` | Each `reasoning` part |
| `malformed` | An unparseable payload for a known `type`, an unrecognized `type`, and any non-JSON stdout line; the permission warning never reaches this stream |
| `token_usage` | The `opencode export` figures, never `step_finish.part.tokens`. See "Token usage" |
| `turn_completed` | The process exits `0`, no `error` event arrived, and at least one `text`, `reasoning`, or `tool_use` part was parsed during the turn |
| `turn_failed` | Any `error` event (`ErrTurnFailed`); a non-zero exit with no `error` event (`ErrPortExit`); an exit-`0` turn with no assistant part parsed (`ErrTurnFailed`); a stdout read error or session id mismatch (`ErrResponseError`); or a timeout waiting for the first JSON event (`ErrResponseTimeout`) |
| `turn_cancelled` | Context cancellation or `StopSession`. `run --format json` emits no cancel envelope, so the signal is the adapter's own |

The `text`, `reasoning`, or `tool_use` part is the per-turn work signal: `RunTurn` sets
`assistantOutputSeen`, and `finalizeExitedTurn` passes it as `agentcore.WorkPresent` or
`agentcore.WorkAbsent`. An exit-`0` run that parsed envelopes but produced no assistant output,
such as a `step_start`/`step_finish` pair bracketing nothing, therefore takes the shared
zero-work row and reports `turn_failed`.

### Token usage

`StepFinishPart.tokens` are step-scoped, not a final turn summary. In a two-step run the token
breakdown changes between the tool-call step and the final text step instead of accumulating
monotonically:

- Tool step: `{"input":14412,"output":58,"cache":{"read":1840}}`
- Final step: `{"input":1446,"output":149,"cache":{"read":16240}}`

`queryExportUsage` therefore recovers authoritative usage with
`opencode export --sanitize <sessionID>` after the subprocess exits rather than reading
`step_finish.part.tokens` from stdout. The export carries
per-assistant-message `info.tokens {total, input, output, reasoning, cache{read, write}}`. The
per-message `cache.write` field reaches no `step_finish` token count on stdout at all: a captured
three-message session showed `cache.write` values of 16586, 15, and 16618 across the messages the
stdout stream never surfaces. `tokens.total` includes both cache and reasoning tokens and is not
`input + output`, so `parseExportOutput` computes its own figures instead of reading
`tokens.total`: `input + cache.read + cache.write` for `input_tokens`, `output + reasoning` for
`output_tokens`.

The export's session aggregate, at the top-level `info.tokens` of the export payload, equals the
sum over every assistant message in the session: a captured session with per-message totals
16593, 16609, and 16626 reported a session aggregate whose components (`input` 9, `output` 14,
`reasoning` 0, `cache.read` 16586, `cache.write` 33219) sum to 49828, the sum of the three message
totals. `parseExportOutput` ignores that aggregate and sums the run's own assistant messages,
because the aggregate spans the whole session including turns from an earlier run, while the
figure the adapter reports is run-scoped. It selects messages by `info.sessionID` and, on a
resumed session, by `info.time.created` at or after the run's start. `finalizeExitedTurn` skips
the update when the export yields nothing, so a failed export cannot lower a figure the run has
already reported.

## Concurrency and session isolation

Two `opencode run --format json` commands launched in parallel in the same workspace produce
distinct session IDs and complete independently:

- `ses_236c5a996ffeWzz4OuQinQRiAj`
- `ses_236c5ba76ffeL6MNEglFHLGLXv`

Session isolation therefore rests on the server-generated ID, which is why the adapter persists
the exact `sessionID` and passes `--session <id>` on continuation turns instead of relying on
`--continue` and the `session.list()` ordering.

## Edge cases and operational notes

### Network interruptions and rate limiting

The server event model includes `session.status` values of `busy`, `idle`, and
`retry { attempt, message, next }` (SDK v2 types). The plugin docs also list `session.status`,
`session.idle`, and `session.error` as first-class events.

`run --format json` does not surface those server status events, so retry and backoff timing is
visible on the server SSE surface only. The adapter reads no stall or retry signal; its only
liveness control is the first-JSON-event read timeout in `RunTurn`, which defaults to 30 seconds
and is overridden by `AgentConfig.ReadTimeoutMS`. The timer stops once the first JSON envelope
arrives, so a mid-turn stall does not trip it.

### Output-length and context-limit failures

The server error union includes `MessageOutputLengthError`, `MessageAbortedError`, `APIError`,
`ProviderAuthError`, and `UnknownError` (SDK v2 types). Those errors reach the CLI as
`type: "error"` envelopes. `rawRunError` captures the structured object, and
`rawRunErrorMessage` reads `data.message` first, then `name`, so the adapter never classifies
stderr text.

### External-directory access

The documented defaults make `external_directory` an `ask` permission (permissions docs). In
unattended `run` usage without `--dangerously-skip-permissions`, this produces a non-JSON
warning line and a `tool_use` error part.

### Plugin and prompt contamination

OpenCode loads default plugins, project plugins, global plugins, and `.claude` prompt and skill
content unless explicitly disabled (CLI docs, plugin docs). The adapter disables none of it: it
sets no `OPENCODE_DISABLE_DEFAULT_PLUGINS` and no `OPENCODE_DISABLE_CLAUDE_CODE*` variable, and
`--pure` is opt-in through the `pure` passthrough key.

## Fit of `run` as an adapter surface

`opencode run --format json` carries what a launch-per-turn adapter needs, but it is not a
lossless wire protocol. It hides server status events, omits a final result envelope, mixes
human-readable text into stderr during permission rejection, and can repeat an `error` event for
one failure. `opencode serve` exposes explicit session, message, permission, and event APIs with
documented schemas and an OpenAPI spec (server and SDK docs), which is the surface those gaps
would be closed on.

## Documented conflicts and drift

| Topic | Docs say | Shipped CLI / source say | Impact |
| ----- | -------- | ------------------------ | ------ |
| Auth command name | `opencode auth ...` | Root help promotes `opencode providers ...`; `auth` is an alias whose own help renders `auth` subcommands under an `opencode providers` header | Low; no parser should depend on human help text |
| Network port default wording | Server docs describe `4096` | Shared CLI options expose `0` in help and config, while the Bun and Node adapters treat `0` as "try `4096` first, then fall back to an ephemeral port" | High for `--attach`; set `--port` explicitly |
| `run --format json` | "raw JSON events" | CLI-emitted projection from `run.ts`, not raw SSE | High for adapters |
| Permissions in JSON mode | Not called out | Permission rejection prints a plain-text warning to stderr; stdout carries the JSON envelope alone | Low for parsers, the streams are separate |
| Exit codes | Not documented | The code is unstable across releases: `0` on a logical failure in v1.14.25, `1` in v1.14.50 | High for failure handling; the adapter keys failure on the stdout `error` event instead |
| `--pure` flag | Not on docs page | Present in shipped help output | Medium for deterministic runs |
| Permission keys | `permissions.mdx` lists 14 keys | `config/permission.ts` accepts 16 explicitly (adding `list` and `todowrite`) and any other string key via a `StructWithRest` catchall | Medium; a strict whitelist against the documented set would break on configurations OpenCode itself accepts |
| `OPENCODE_PERMISSION` precedence | Listed in the env-var table, no merge semantics specified | [`config/config.ts` lines 656-658](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/config/config.ts#L656-L658) call `mergeDeep(opencode.json.permission, JSON.parse(env))`, so the env var stacks on top of operator config and never replaces it | High; an inherited value bleeds into the merged policy unless the launcher scrubs it |

## Open questions

- Whether the logical-failure shape (exit `1`, empty stderr, two `error` events) still holds on the pinned binary. Repeat the invalid-model run on v1.15.12 and capture stdout, stderr, and the exit code separately.
- What `run --format json` shows while the server is in `session.status.type == "retry"`. Drive a provider into rate limiting and capture stdout for the duration of the backoff.
- The output shape of `opencode models`. `queryModelNotFound` treats the catalog as whitespace-separated `provider/model` slugs, and no probe backs that. Run `opencode models` on the pinned binary.
- Which failures other than an unknown model arrive as the masked generic server error. The adapter package comment attributes the masking to OpenCode 1.16.0 and later, which is above the pinned version, so both the version boundary and the set of masked causes are unverified here. Probe 1.16.x with an invalid model, a revoked credential, and an aborted message.
- Whether the newest-first ordering of `session.list()` is guaranteed rather than incidental. Read the session list implementation at the pinned tag.
- Whether default plugins or `.claude` prompt and skill content change a headless run. Run one prompt with and without `--pure` in a repo that carries `.claude` content and diff the event streams.
- The nd-JSON payloads of `opencode acp`. Capture one ACP session over stdin and stdout.

## Sources

Rendered docs (`opencode.ai`):

- [CLI docs](https://opencode.ai/docs/cli/)
- [Permissions docs](https://opencode.ai/docs/permissions/)
- [Providers docs](https://opencode.ai/docs/providers/)
- [Server docs](https://opencode.ai/docs/server/)
- [SDK docs](https://opencode.ai/docs/sdk/)
- [Plugin events docs](https://opencode.ai/docs/plugins/)

Source code and raw docs, pinned at the `v1.14.25` tag:

- [`run.ts`](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/cli/cmd/run.ts)
- [`network.ts`](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/cli/network.ts)
- [Node server adapter (`adapter.node.ts`)](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/server/adapter.node.ts)
- [Bun server adapter (`adapter.bun.ts`)](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/server/adapter.bun.ts)
- [`permission/index.ts`](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/permission/index.ts)
- [`config/permission.ts`](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/config/permission.ts)
- [`config/config.ts`](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/config/config.ts)
- [`flag/flag.ts`](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/opencode/src/flag/flag.ts)
- [SDK v2 types (`types.gen.ts`)](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/sdk/js/src/v2/gen/types.gen.ts)
- [README](https://github.com/anomalyco/opencode/blob/v1.14.25/README.md)
- [`cli.mdx` (raw docs)](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/web/src/content/docs/cli.mdx)
- [`permissions.mdx` (raw docs)](https://github.com/anomalyco/opencode/blob/v1.14.25/packages/web/src/content/docs/permissions.mdx)
