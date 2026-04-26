# OpenCode CLI: adapter research notes

> OpenCode CLI v1.14.25 (`opencode`, npm `opencode-ai`), researched April 2026.
> Reference for implementing the OpenCode `AgentAdapter`.
>
> Primary sources: [CLI docs][cli-docs], [permissions docs][permissions-docs], [providers docs][providers-docs], [server docs][server-docs], [plugin events docs][plugins-docs], [run command source][run-src], [permission event source][permission-src], [SDK v2 types][sdk-v2-types], [published README][readme-src], and local probes of `npx -y opencode-ai@latest` v1.14.25 on Linux on 2026-04-26.
>
> Source links below mostly point at OpenCode's `dev` branch. When shipped v1.14.25 behavior differs from docs or source, this note calls that drift out explicitly.

## Overview

OpenCode exposes three relevant automation surfaces:

| Surface | Transport | What it does | Adapter relevance |
| ------- | --------- | ------------ | ----------------- |
| `opencode run` | stdout/stderr plus an internal or attached HTTP server | Non-interactive one-shot execution | Closest match to Claude/Copilot launch-per-turn adapters |
| `opencode serve` | HTTP + SSE | Headless server exposing sessions, messages, permissions, files, tools, and `/doc` OpenAPI | Cleaner programmatic surface than scraping CLI JSON |
| `opencode acp` | stdin/stdout nd-JSON | ACP server | Exists, but this note does not reverse-engineer the ACP payloads |

Source inspection shows that `opencode run` is a thin client over the same server APIs exposed by `opencode serve`. Without `--attach`, `run` bootstraps an in-process server and points the SDK at `Server.Default().app.fetch(...)`. With `--attach`, it points the SDK at an existing server URL.[run-src][server-docs][sdk-docs]

That architectural detail matters. `opencode run --format json` is not the canonical OpenCode event bus. It is a CLI-specific projection emitted by `run.ts`. The canonical bus is the server SSE stream at `/event`.[run-src][server-docs]

## Installation and prerequisites

OpenCode ships as the `opencode` binary and is installed from the `opencode-ai` package or platform-specific packages.[readme-src][cli-docs]

```bash
curl -fsSL https://opencode.ai/install | bash
npm install -g opencode-ai
brew install anomalyco/tap/opencode
```

Adapter-relevant prerequisites:

| Item | Requirement | Evidence |
| ---- | ----------- | -------- |
| OpenCode binary | `opencode` on `PATH` | [readme-src][cli-docs] |
| Provider credentials | Auth file, environment variables, `.env`, or provider config | [providers-docs][cli-docs][sdk-v2-types] |
| Working directory | Any project directory; `run --dir` overrides cwd, `--attach` treats it as remote-server path | [cli-docs][run-src] |
| Headless use | `opencode run` works without a TTY | Observed locally in v1.14.25 |

## Authentication and provider configuration

OpenCode does not have a single vendor-specific auth flow. It delegates model access to configured providers through Models.dev. Credentials can come from several places.

### Credential sources

| Source | Mechanism | Notes |
| ------ | --------- | ----- |
| Interactive credential store | `opencode providers login` or `opencode auth login`, stored in `~/.local/share/opencode/auth.json` | Docs still present `auth`; shipped root help promotes `providers`, while `auth` remains an alias and alias-specific help still prints `auth`-prefixed subcommands.[cli-docs] Observed locally in v1.14.25. |
| Environment variables | Provider-specific env vars such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `AWS_*`, `GITLAB_TOKEN`, `CLOUDFLARE_*`, `GOOGLE_CLOUD_PROJECT`, `VERTEX_LOCATION`, and many others | Loaded at startup alongside credentials and project `.env` files.[cli-docs][providers-docs] |
| `opencode.json` provider config | `provider.<id>.options.apiKey`, `baseURL`, headers, model overrides, routing options | Useful for proxy gateways, local models, or custom OpenAI-compatible providers.[providers-docs][sdk-v2-types] |
| Server API | `PUT /auth/{id}` | Programmatic credential injection when integrating through `serve` instead of the CLI wrapper.[server-docs][sdk-docs] |

### Adapter-relevant observations

- Browser and device-code flows exist for some providers, including GitHub Copilot, OpenAI, and GitLab Duo. They are not suitable for unattended orchestration. Prefer environment variables, config injection, or pre-populated auth storage.[providers-docs][cli-docs]
- Provider fallback is not a universal CLI flag. It is provider-specific configuration. For example, OpenRouter and Vercel AI Gateway support routing and fallback policies inside `opencode.json` model options.[providers-docs]
- The server and SDK surfaces expose providers and default models directly through `/provider`, `/provider/auth`, and `/config/providers`, which is cleaner than parsing CLI text.[server-docs][sdk-docs]

### Adapter-relevant environment variables

| Variable | Purpose | Notes |
| -------- | ------- | ----- |
| `OPENCODE_CONFIG` / `OPENCODE_CONFIG_DIR` / `OPENCODE_CONFIG_CONTENT` | Point OpenCode at a config file, config directory, or inline JSON config | Useful when Sortie wants to inject provider or permission config without mutating the repo.[cli-docs] |
| `OPENCODE_PERMISSION` | Inline JSON permission config | Complements `--dangerously-skip-permissions`.[cli-docs][permissions-docs] |
| `OPENCODE_SERVER_PASSWORD` / `OPENCODE_SERVER_USERNAME` | Basic auth for `serve` and `web`; also used by `run --attach` when `--password` is omitted | [server-docs][cli-docs][run-src] |
| `OPENCODE_DISABLE_DEFAULT_PLUGINS` | Disable default plugins | Reduces implicit behavior in headless runs.[cli-docs] |
| `OPENCODE_DISABLE_CLAUDE_CODE`, `OPENCODE_DISABLE_CLAUDE_CODE_PROMPT`, `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS` | Disable loading `.claude` prompt and skills content | Relevant if Sortie needs strict prompt isolation.[cli-docs] |

## Relevant CLI commands and flags

### `opencode run`

`opencode run [message..]` is the non-interactive CLI entry point.[cli-docs]

| Flag | Short | Meaning | Adapter use |
| ---- | ----- | ------- | ----------- |
| `--command` |  | Run a slash command instead of a freeform prompt | Optional |
| `--continue` | `-c` | Resume the last root session | Useful, but see resume caveat below |
| `--session` | `-s` | Resume a specific session ID | Preferred for deterministic continuation |
| `--fork` |  | Fork the resumed session first | Optional branch semantics |
| `--share` |  | Share session on completion | Usually disable for automation |
| `--model` | `-m` | Model in `provider/model` form | Primary model selector |
| `--agent` |  | Primary agent name | Validated against available agents; subagents fall back to default with a warning.[run-src] |
| `--file` | `-f` | Attach files or directories to the prompt | Optional |
| `--format` |  | `default` or `json` | `json` is required if scraping stdout |
| `--title` |  | Explicit session title | Useful when automation wants deterministic session names instead of truncated prompts |
| `--attach` |  | Target an existing `serve` instance | Avoids server cold start per turn |
| `--password` | `-p` | Basic-auth password for `--attach` | Falls back to `OPENCODE_SERVER_PASSWORD`.[run-src] |
| `--dir` |  | Local cwd override, or remote path when attached | Useful for remote-server routing |
| `--port` |  | Local server port when not attached | Effective port `0` means try `4096` first, then fall back to an ephemeral port if `4096` is busy; shipped `run --help` phrases this as "defaults to random port" |
| `--variant` |  | Provider-specific reasoning variant such as `high`, `max`, or `minimal` | Secondary model control |
| `--thinking` |  | Emit reasoning blocks | Only affects CLI output |
| `--dangerously-skip-permissions` |  | Auto-approve permissions that are not explicitly denied | Required for clean unattended runs in many tooling scenarios |
| `--pure` |  | Run without external plugins | Present in shipped v1.14.25 help output, but omitted from the CLI docs page. Observed locally in v1.14.25. |

Separately, the top-level `opencode [project]` command documents a `--prompt` flag in the CLI docs and shipped root help. That is a TUI/root flag rather than the documented `run` surface. In local v1.14.25 probing, `opencode run --prompt ...` printed `run` help and exited with code `1`, so adapters should treat positional `message..` as the stable non-interactive prompt input.[cli-docs] Observed locally in v1.14.25.

Example invocation for a headless turn:

```bash
opencode run \
  --format json \
  --session "$SESSION_ID" \
  --model anthropic/claude-sonnet-4-20250514 \
  --dangerously-skip-permissions \
  -- "Implement the requested fix"
```

Use `--` before the prompt so shell wrappers can pass the full prompt as one positional argument.

### `opencode serve`

`opencode serve` starts the headless HTTP server.[server-docs][cli-docs]

| Flag | Meaning | Notes |
| ---- | ------- | ----- |
| `--port` | Listen port | Runtime semantics: an effective port of `0` means try `4096` first, then fall back to an ephemeral port if `4096` is busy |
| `--hostname` | Listen address | Docs default to `127.0.0.1`.[server-docs] |
| `--mdns` / `--mdns-domain` | mDNS discovery | Usually irrelevant for Sortie |
| `--cors` | Additional CORS origins | Only matters for browser clients |

Shared network options define `port` with a default of `0`, but both the Node and Bun server adapters interpret `0` as a sentinel: they attempt to bind `4096` first, then fall back to an ephemeral port only if `4096` is unavailable.[server-docs][network-src][node-adapter-src][bun-adapter-src] A local v1.14.25 `opencode serve` probe bound to `http://127.0.0.1:4096` with no flags. Observed locally in v1.14.25.

### Session and provider helper commands

| Command | Use |
| ------- | --- |
| `opencode session list --format json -n N` | Enumerate recent sessions. The observed output is newest-first. Observed locally in v1.14.25. |
| `opencode export [sessionID]` | Export session data as JSON. Useful for offline debugging or fixtures.[cli-docs] |
| `opencode providers list` | Enumerate configured provider credentials. This is the primary command name in shipped root help and in `providers --help`. Observed locally in v1.14.25. |
| `opencode auth list` | Alias for `providers list`. The docs still use `auth`, and `auth --help` keeps `auth`-prefixed subcommands under an `opencode providers` header. Observed locally in v1.14.25. |

## Subprocess behavior

### `run` is not a standalone agent protocol

Source inspection shows this control flow inside `opencode run`:[run-src]

1. Parse CLI flags and optional attached files.
2. Create or resume a session through the SDK.
3. Subscribe to the server event stream.
4. Send the prompt via `sdk.session.prompt(...)` or `sdk.session.command(...)`.
5. Convert selected server events into a custom stdout JSON envelope when `--format json` is set.
6. Stop when the server reports `session.status.type == "idle"`.

Without `--attach`, the command does not spawn an external `serve` child process. It boots the server in-process and routes SDK calls through an internal fetch function backed by `Server.Default().app.fetch(...)`.[run-src]

### Fresh session vs continuation

| Mode | Mechanism | Notes |
| ---- | --------- | ----- |
| Fresh | `sdk.session.create({ title, permission: rules })` | Session ID is server-generated and looks like `ses_...`. Observed locally in v1.14.25. |
| `--session <id>` | Resume exact session ID | Deterministic resume path |
| `--continue` | `sdk.session.list()` then first root session | In practice, `session list --format json` returns newest-first, so `--continue` resumes the most recent root session today. That ordering is observed locally, not promised by the docs. |
| `--fork` with resume | `sdk.session.fork({ sessionID })` | Creates a child session before continuing |

Source inspection also shows that `run` injects three deny rules when it creates a new session: `question=*`, `plan_enter=*`, and `plan_exit=*`.[run-src] That is source-derived behavior. Resumed sessions reuse the existing session state instead of re-creating these rules.

### Working directory handling

- When not attached, `--dir` calls `process.chdir(args.dir)` before bootstrapping the local server. Invalid paths terminate immediately with exit code `1`.[run-src]
- When attached, `--dir` is passed to the SDK as the remote directory selector instead of changing the local process cwd.[run-src]
- The server and SDK surfaces also accept `directory` query parameters on many APIs, which makes `serve` a better fit when a single OpenCode backend serves multiple workspaces.[server-docs][sdk-docs]

## Permissions and tool access control

OpenCode permission control is config-driven. Each rule resolves to `allow`, `ask`, or `deny`.[permissions-docs]

### Permission keys

Documented tool and safety keys include:[permissions-docs]

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

### Defaults

The documented defaults are:[permissions-docs]

| Permission | Default |
| ---------- | ------- |
| Most tools | `allow` |
| `external_directory` | `ask` |
| `doom_loop` | `ask` |
| `read` on `.env*` files | `deny`, except `.env.example` |

### Headless behavior of `run`

`run.ts` handles permission requests this way:[run-src][permissions-docs]

| Condition | CLI behavior |
| --------- | ------------ |
| `--dangerously-skip-permissions` set | Reply `once` to permission requests that are not explicitly denied |
| No bypass flag | Print a human warning and reply `reject` |

Observed with v1.14.25:

```text
! permission requested: external_directory (/etc/*); auto-rejecting
{"type":"tool_use", ... "state":{"status":"error","error":"The user rejected permission to use this specific tool call."}}
```

That warning is written to stdout before the JSON envelope. This means `opencode run --format json` is not actually JSON-clean unless the prompt avoids permission prompts or the adapter uses `--dangerously-skip-permissions`. Observed locally in v1.14.25, and consistent with the `run.ts` source branch that calls `UI.println(...)` before replying `reject`.[run-src]

## Output format: `opencode run --format json`

The CLI docs describe `--format json` as "raw JSON events".[cli-docs] Source inspection and live runs show a narrower, CLI-defined envelope instead. The emitted objects have this top-level shape:[run-src]

```json
{
  "type": "step_start | tool_use | text | reasoning | step_finish | error",
  "timestamp": 1777197446593,
  "sessionID": "ses_236c713fcffel8QozOz4ca0AYK",
  "...": "type-specific payload"
}
```

### Observed stdout event types

| `type` | Payload | When emitted | Evidence |
| ------ | ------- | ------------ | -------- |
| `step_start` | `part: StepStartPart` | Start of a model step | [run-src] and observed locally |
| `tool_use` | `part: ToolPart` | When a tool part reaches `completed` or `error` | [run-src] and observed locally |
| `text` | `part: TextPart` | Completed text part | [run-src] and observed locally |
| `reasoning` | `part: ReasoningPart` | Completed reasoning part, only when `--thinking` is set | [run-src] |
| `step_finish` | `part: StepFinishPart` | End of a model step | [run-src] and observed locally |
| `error` | `error: EventSessionError.properties.error` | When the server emits `session.error` for this session | [run-src][sdk-v2-types] and observed locally |

What the CLI does **not** emit in JSON mode:

- No `session_started` event
- No raw `message.updated` or `message.part.updated` server events
- No `permission.asked` event
- No `session.status` or `session.idle` event
- No final result or summary envelope

### Example: simple one-step turn

Observed locally in v1.14.25:

```json
{"type":"step_start","timestamp":1777197446593,"sessionID":"ses_236c713fcffel8QozOz4ca0AYK","part":{"id":"prt_dc938f5be001xlQ2FdVcM0ybM8","messageID":"msg_dc938ecbe001pHUOguAaJY92Pz","sessionID":"ses_236c713fcffel8QozOz4ca0AYK","snapshot":"45865d3017876fc42b80fa16e317d109a7008c30","type":"step-start"}}
{"type":"text","timestamp":1777197446597,"sessionID":"ses_236c713fcffel8QozOz4ca0AYK","part":{"id":"prt_dc938f5c3001Xf6Jb1dJzX7Po6","messageID":"msg_dc938ecbe001pHUOguAaJY92Pz","sessionID":"ses_236c713fcffel8QozOz4ca0AYK","type":"text","text":"\n\nHello","time":{"start":1777197446595,"end":1777197446596}}}
{"type":"step_finish","timestamp":1777197446660,"sessionID":"ses_236c713fcffel8QozOz4ca0AYK","part":{"id":"prt_dc938f5c600183OklHsapPOT69","reason":"stop","messageID":"msg_dc938ecbe001pHUOguAaJY92Pz","sessionID":"ses_236c713fcffel8QozOz4ca0AYK","type":"step-finish","tokens":{"total":16267,"input":14406,"output":21,"reasoning":0,"cache":{"write":0,"read":1840}},"cost":0}}
```

### Example: tool call

Observed locally in v1.14.25:

```json
{"type":"tool_use","timestamp":1777197461503,"sessionID":"ses_236c6de07ffeMCaCIVqcZsSjBi","part":{"type":"tool","tool":"read","callID":"call_function_1hg9s1exw5vv_1","state":{"status":"completed","input":{"filePath":"/home/ubuntu/work/sortie/README.md"},"output":"<path>/home/ubuntu/work/sortie/README.md</path>\n<type>file</type>\n<content>\n1: <p align=\"center\">\n...","metadata":{"preview":"<p align=\"center\">...","truncated":false,"loaded":[]},"title":"README.md","time":{"start":1777197461489,"end":1777197461502}},"id":"prt_dc9392fd2001HeyUJbUUYfz0Ez","sessionID":"ses_236c6de07ffeMCaCIVqcZsSjBi","messageID":"msg_dc93922a40015YBm8bwcEdTQXV"}}
```

Tool payloads can be large. The `read` tool embeds the returned file content directly in `state.output`, which means one JSON line can contain the whole file body. Adapters should use a generous line buffer when parsing stdout. This is consistent with the CLI's `JSON.stringify(...) + EOL` implementation and observed local output from `read README.md`.[run-src]

### Example: logical failure

Observed locally in v1.14.25 with an invalid model:

```text
ProviderModelNotFoundError: ProviderModelNotFoundError
...
{"type":"error","timestamp":1777197598202,"sessionID":"ses_236c4ba84ffeKJLGiwxfHIx8Au","error":{"name":"UnknownError","data":{"message":"Model not found: nonexistent/nonexistent."}}}
EXIT:0
```

The stack trace was written to stderr, the CLI emitted an `error` JSON object on stdout, and the process exit code was still `0`. Observed locally in v1.14.25. The source path explains why: `run` calls `process.exit(1)` only for CLI setup failures or unhandled loop exceptions, not for `session.error` events.[run-src]

## Turn completion, failure detection, and `TurnResult` mapping

Sortie's turn model lives in [internal/domain/agent.go](../internal/domain/agent.go). `TurnResult` needs `SessionID`, a normalized `ExitReason`, and cumulative token usage.

### Completion detection

`run.ts` stops reading events when it sees `session.status.type === "idle"` on the underlying server event stream. It does not print that status transition to stdout.[run-src]

Practical implication:

- Process exit is the only explicit end-of-turn signal on the CLI surface.
- `step_finish` with `reason == "stop"` often coincides with normal completion, but it is not a distinct final-result event.
- Multi-step turns can emit several `step_finish` events before process exit.

### Failure detection

| Signal | Reliability | Notes |
| ------ | ----------- | ----- |
| Stdout `{"type":"error", ...}` | Better than exit code | Emitted from `session.error` |
| `tool_use` with `part.state.status == "error"` | Important, but not always terminal | Permission rejection and tool failures land here |
| stderr text | Diagnostic only | Can contain stack traces or human-readable errors |
| Process exit code | Weak | Observed locally: invalid model still exited `0` |

For a Sortie adapter built on `opencode run`, a sensible normalization rule is:

| Sortie normalized outcome | OpenCode evidence |
| ------------------------ | ----------------- |
| `session_started` | First successfully parsed JSON envelope carrying `sessionID`, or session ID known from a server/API response |
| `tool_result` | Each `tool_use` event, using `part.tool`, `part.state.status`, and `part.state.time` |
| `notification` / `other_message` | Default-mode-only prose is not available in JSON mode; optional if adapter also captures stderr |
| `turn_completed` | Process exits after a normal run and no terminal `error` was observed |
| `turn_failed` | Any terminal `error` event, or a process-level CLI/setup failure |
| `turn_cancelled` | Prefer the server API surface, which exposes `session.abort` and `session.status`; `run --format json` does not emit a dedicated cancel envelope |
| `token_usage` | Do not treat `step_finish.part.tokens` as authoritative turn totals without extra logic |

### Token usage caveat

`StepFinishPart.tokens` are step-scoped, not a final turn summary. In a two-step run, the observed token breakdown changed between the tool-call step and the final text step instead of monotonically accumulating:

- Tool step: `{"input":14412,"output":58,"cache":{"read":1840}}`
- Final step: `{"input":1446,"output":149,"cache":{"read":16240}}`

By contrast, the server's `AssistantMessage` type includes per-message `cost` and `tokens` fields that are better candidates for authoritative turn totals.[sdk-v2-types] `run --format json` does not emit the final `AssistantMessage` envelope directly.[run-src]

For precise `TurnResult.Usage`, prefer one of these approaches:

1. Integrate against `serve` and use the server/SDK session APIs directly.
2. Use `run` for execution, then query the session's final message through the server/API surface before returning.

## Concurrency and session isolation

Observed locally in v1.14.25, two `opencode run --format json` commands launched in parallel in the same workspace produced distinct session IDs and completed independently:

- `ses_236c5a996ffeWzz4OuQinQRiAj`
- `ses_236c5ba76ffeL6MNEglFHLGLXv`

`opencode session list --format json -n 10` returned sessions in newest-first order for the same project directory. That makes `--continue` workable today, because the current implementation picks the first root session from `session.list()`. It is still safer for a Sortie adapter to persist the exact `sessionID` and use `--session <id>` on continuation turns. Observed locally in v1.14.25 and consistent with `run.ts`.[run-src]

## Edge cases and operational notes

### Network interruptions and rate limiting

The server event model includes `session.status` values of `busy`, `idle`, and `retry { attempt, message, next }`.[sdk-v2-types] The plugin docs also list `session.status`, `session.idle`, and `session.error` as first-class events.[plugins-docs]

`run --format json` does not surface those server status events. That means:

- retry/backoff timing is visible on the server SSE surface, not on CLI JSON stdout
- if Sortie needs live stall/retry visibility, `serve` is the better integration surface

This point is source-derived from the SDK types and plugin event docs. It was not observed in a live rate-limit run during this research session.

### Output-length and context-limit failures

The server error union includes `MessageOutputLengthError`, `MessageAbortedError`, `APIError`, `ProviderAuthError`, and `UnknownError`.[sdk-v2-types] Those errors can appear through the CLI as `type: "error"` envelopes or stderr diagnostics. An adapter should capture the structured error object when present and avoid relying on stderr text classification alone.

### External-directory access

The documented defaults make `external_directory` an `ask` permission.[permissions-docs] In unattended `run` usage without `--dangerously-skip-permissions`, this produces a non-JSON warning line and a `tool_use` error part. Observed locally in v1.14.25.

### Plugin and prompt contamination

OpenCode can load default plugins, project plugins, global plugins, and `.claude` prompt/skill content unless explicitly disabled.[cli-docs][plugins-docs] For deterministic orchestration, test whether you need one or more of:

- `--pure`
- `OPENCODE_DISABLE_DEFAULT_PLUGINS=1`
- `OPENCODE_DISABLE_CLAUDE_CODE=1`
- `OPENCODE_DISABLE_CLAUDE_CODE_PROMPT=1`
- `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1`

## Adapter implications

The evidence above supports two practical conclusions:

- `opencode run --format json` is usable for a launch-per-turn adapter.
- It is not a lossless wire protocol. It hides server status events, omits a final result envelope, can mix human text into stdout during permission rejection, and does not reliably signal logical failure via non-zero exit codes.[run-src] Observed locally in v1.14.25.

`opencode serve` is the cleaner long-term surface because it exposes explicit session, message, permission, and event APIs with documented schemas and an OpenAPI spec.[server-docs][sdk-docs]

If Sortie wants maximum symmetry with the existing Claude/Copilot launch-per-turn adapters, `opencode run` can work, but only with stricter parsing rules and explicit session tracking. If Sortie wants the lowest integration risk, a persistent `opencode serve` subprocess plus HTTP/SSE integration is a better fit.

## Documented conflicts and drift

| Topic | Docs say | Shipped CLI / source say | Impact |
| ----- | -------- | ------------------------ | ------ |
| Auth command name | `opencode auth ...` | Root help promotes `opencode providers ...`; `auth` remains an alias, and alias help still renders `auth` subcommands under an `opencode providers` header | Low; parser should not depend on human help text |
| Network port default wording | Server docs describe `4096` | Shared CLI options expose `0` in help/config, while the Bun/Node adapters treat `0` as "try `4096` first, then fall back to an ephemeral port" | High for `--attach`; always set `--port` explicitly |
| `run --format json` | "raw JSON events" | CLI-emitted projection from `run.ts`, not raw SSE | High for adapters |
| Permissions in JSON mode | Not called out | Permission rejection prints a plain-text warning to stdout before JSON | High for parsers |
| Exit codes | Not documented | Observed logical failure with exit code `0` | High for failure handling |
| `--pure` flag | Not on docs page | Present in shipped help output | Medium for deterministic runs |

[cli-docs]: https://opencode.ai/docs/cli/
[permissions-docs]: https://opencode.ai/docs/permissions/
[providers-docs]: https://opencode.ai/docs/providers/
[server-docs]: https://opencode.ai/docs/server/
[sdk-docs]: https://opencode.ai/docs/sdk/
[plugins-docs]: https://opencode.ai/docs/plugins/
[readme-src]: https://github.com/anomalyco/opencode/blob/dev/README.md
[network-src]: https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/cli/network.ts
[run-src]: https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/cli/cmd/run.ts
[node-adapter-src]: https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/server/adapter.node.ts
[bun-adapter-src]: https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/server/adapter.bun.ts
[permission-src]: https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/permission/index.ts
[sdk-v2-types]: https://github.com/anomalyco/opencode/blob/dev/packages/sdk/js/src/v2/gen/types.gen.ts
