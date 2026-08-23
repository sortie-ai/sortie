# OpenAI Codex CLI: adapter research notes

> OpenAI Codex CLI (npm `@openai/codex`, binary `codex`) on Linux x86_64. Reference for the
> `domain.AgentAdapter` implementation in `internal/agent/codex`, registered under kind `codex`.
>
> Coverage. The configuration surface, including the approval policy and the sandbox modes,
> follows OpenAI's published [configuration schema](https://developers.openai.com/codex/config-schema.json),
> which tracks the current release. The approval value set is confirmed on **v0.147.0** and
> **v0.149.0**, and the flag surface is anchored to **v0.147.0**, both on the research host. MCP
> server configuration and app-server project trust are read from the upstream sources for
> **v0.147.0** and **v0.149.0**; those releases do not behave identically on that surface, and each
> difference is named at the claim rather than folded into a single version. The app-server
> transcripts, the `codex exec` JSONL samples, and the `userAgent` values quoted in the examples
> date from **v0.121.0** and have not been re-captured since, so a shape shown in an example may lag
> the current release even where the surrounding claim does not. Hosted-tool and model-tier behavior
> and the feature-flag surface were recorded against v0.134.0 in May 2026 and have not been
> re-observed, which makes them the weakest claims in this document. Two schemas cover this ground
> and are authoritative for different surfaces: the published configuration schema for what an
> operator may write in a configuration file, and the protocol schema each release generates for
> values that travel on a request. They agree almost everywhere. Where they disagree, the surface
> the value crosses governs, and the divergence is named at the claim. The dynamic-tool response
> payload is exercised end to end against **v0.149.0** on the research host.
>
> Claims about Sortie's own code name a Go symbol and were verified against the tree.
> Primary sources are linked under "Sources" at the end.

---

## Overview

Codex CLI is an agentic coding tool from OpenAI that runs as a native Rust binary. It reads a
codebase, executes tools (shell commands, file edits, MCP tool calls, and web searches), and
produces code changes autonomously.

Codex exposes two non-interactive surfaces:

1. **App-server mode** (`codex app-server`) communicating over JSON-RPC 2.0 on stdio (JSONL).
   It provides full lifecycle control: thread management, turn execution, approval handling,
   dynamic tool registration, and session resume. The same protocol powers the Codex VS Code
   extension.
2. **Non-interactive CLI** (`codex exec`) with `--json` for JSONL output on stdout. It carries
   no approval routing, no dynamic tool injection, and no mid-turn steering.

`CodexAdapter` in `internal/agent/codex` uses the app-server surface. Unlike the Claude Code,
Copilot, and OpenCode adapters, which fork one subprocess per turn, it launches
`codex app-server` once in `StartSession` and keeps it alive for the whole session; each turn is
a `turn/start` request on one persistent thread. The `codex exec` surface is described at the
end of this document for reference; no adapter code path reaches it.

---

## Installation and prerequisites

Codex CLI is distributed through multiple channels:

```bash
# npm
npm install -g @openai/codex

# Homebrew (macOS)
brew install --cask codex

# Direct binary download (Linux x86_64)
# https://github.com/openai/codex/releases/latest
# codex-x86_64-unknown-linux-musl.tar.gz
```

After installation the `codex` binary is available on `$PATH`. `agentcore.ResolveLaunchTarget`
falls back to the command `codex app-server` when `agent.command` is unset, splits the command
on whitespace, and resolves the first token through `exec.LookPath`; an operator can point
`agent.command` at a specific path or wrapper script instead.

**Runtime requirements:**

- The `codex` binary, a statically linked native Rust build that needs no Node.js runtime.
- A valid OpenAI API key (`CODEX_API_KEY` or ChatGPT session credentials).

A Git repository is not among them for this adapter. The trusted-directory refusal lives in the
`codex exec` wrapper, above the app-server layer, which is why `--skip-git-repo-check` is an
`exec` flag and appears in no configuration surface. `thread/start` against a non-git `cwd`
succeeds and reports `gitInfo: null`.

**Supported platforms:** Linux (x86_64, arm64), macOS (x86_64, Apple Silicon), Windows (native
or WSL2).

---

## Authentication

Codex CLI supports three authentication modes.

| Mode                          | Mechanism                                              | Notes                                                       |
| ----------------------------- | ------------------------------------------------------ | ----------------------------------------------------------- |
| API key (recommended for CI)  | `CODEX_API_KEY` environment variable                   | Standard OpenAI API key. Billed at API rates.               |
| ChatGPT managed               | Browser-based OAuth via `codex login`                  | Uses ChatGPT subscription credits (Plus/Pro/Enterprise).    |
| ChatGPT external tokens       | Host-supplied `idToken` + `accessToken` via app-server | For embedded integrations that own the auth lifecycle.      |

### Config mapping

| Sortie config field  | Value                                              |
| -------------------- | -------------------------------------------------- |
| `agent.kind`         | `codex`                                            |
| `agent.command`      | `codex app-server` (or full path to the binary)    |

`CODEX_API_KEY` must be present in the environment of the Sortie process. `StartSession` sets
`cmd.Env = os.Environ()`, so the subprocess inherits it. In SSH mode `buildSSHRemoteCmd`
prepends `CODEX_API_KEY=<shell-quoted value>` to the remote command.

For headless and CI environments where browser login is unavailable:

1. Set `CODEX_API_KEY` as an environment variable (preferred).
2. Alternatively, authenticate on a machine with a browser via `codex login`, then copy
   `~/.codex/auth.json` to the headless host.

### App-server authentication sequence

`authenticateIfNeeded` runs after the initialization handshake and reads the auth state:

```json
{"method": "account/read", "id": 1, "params": {"refreshToken": false}}
```

A non-null `result.account` means the app-server already holds valid credentials and the
function returns. When `result.account` is `null` and `CODEX_API_KEY` is set, it logs in:

```json
{"method": "account/login/start", "id": 2, "params": {"type": "apiKey", "apiKey": "sk-..."}}
```

It then waits up to `readTimeout(state)` for the `account/login/completed` notification and
fails the session with `domain.ErrResponseError` unless it carries `success: true`. When
`result.account` is `null` and `CODEX_API_KEY` is empty, the function returns without logging
in and the session proceeds unauthenticated.

**Credential storage:** Codex caches login details in `~/.codex/auth.json` (plaintext) or the
OS keychain (configurable via `cli_auth_credentials_store` in `config.toml`). The adapter
treats this as an opaque implementation detail.

---

## App-server protocol

### Transport

The app-server communicates via JSONL over stdio (one JSON object per line, newline-delimited).
Each message follows JSON-RPC 2.0 conventions with the `"jsonrpc": "2.0"` header omitted on
the wire.

- **Requests** (client → server): contain `method`, `params`, and `id`.
- **Responses** (server → client): echo `id` with either `result` or `error`.
- **Notifications** (server → client): contain `method` and `params`, no `id`.

An experimental WebSocket transport (`--listen ws://IP:PORT`) exists but is not used by the
adapter.

### Launching the app-server

`StartSession` execs the resolved binary directly through `exec.CommandContext`, with no shell
in between: `LaunchTarget.Command` is the absolute path to `codex` and `LaunchTarget.Args` holds
the remaining tokens of `agent.command`, so the default yields `codex app-server`. When
`StartSessionParams.SSHHost` is set the local command is `ssh` and the codex command runs on the
remote host. The subprocess receives:

| Setting           | Value                                               | Rationale                                                 |
| ----------------- | --------------------------------------------------- | --------------------------------------------------------- |
| Working directory | Workspace path (`StartSessionParams.WorkspacePath`) | Agent must operate in the issue workspace.                |
| Stdout            | Pipe (read by adapter)                              | JSONL output parsed line by line.                         |
| Stdin             | Pipe (written by adapter)                           | JSON-RPC requests sent as JSONL.                          |
| Stderr            | Pipe (read by `procutil.StderrCollector`, logged)   | Diagnostic output, not structured.                        |
| Environment       | Inherited from Sortie process                       | `CODEX_API_KEY` and other auth vars must be present.      |
| Max line size     | 1 MB                                                | `bufio.Scanner` buffer ceiling; a longer line fails the read. |

### Initialization handshake

`initializeHandshake` sends `initialize` and then the `initialized` notification. Requests sent
before initialization are rejected.

```json
{"method": "initialize", "id": 1, "params": {
  "clientInfo": {
    "name": "sortie_orchestrator",
    "title": "Sortie Orchestrator",
    "version": "0.1.0"
  },
  "capabilities": {
    "experimentalApi": true
  }
}}
```

Response:

```json
{"id": 1, "result": {"userAgent": "codex/0.121.0", "platformFamily": "linux", "platformOs": "linux"}}
```

Then:

```json
{"method": "initialized", "params": {}}
```

`initializeHandshake` always requests `capabilities.experimentalApi: true`, which is what
unlocks the `dynamicTools` field on `thread/start`. Sending tools without that capability draws
`-32600` with the message `thread/start.dynamicTools requires experimentalApi capability`.

---

## Thread and turn lifecycle

### Core primitives

| Primitive | Description                                                                |
| --------- | -------------------------------------------------------------------------- |
| Thread    | A conversation between user and agent. Contains turns. Persisted to disk.  |
| Turn      | A single user request and the agent work that follows. Contains items.     |
| Item      | A unit of input or output (message, command, file change, tool call).      |

### Session lifecycle mapping

| Sortie lifecycle event | App-server action                               |
| ---------------------- | ----------------------------------------------- |
| `StartSession`         | Launch `codex app-server`, `initialize`, `account/read`, `thread/start` or `thread/resume` |
| `RunTurn` (turn 1)     | `turn/start` with prompt and configuration      |
| `RunTurn` (turn 2+)    | `turn/start` on the same thread                 |
| `StopSession`          | Close stdin, SIGTERM, grace period, SIGKILL     |

### Starting a thread

```json
{"method": "thread/start", "id": 10, "params": {
  "model": "gpt-5.4",
  "cwd": "/var/sortie/workspaces/PROJ-123",
  "approvalPolicy": "never",
  "sandbox": "workspace-write",
  "dynamicTools": [
    {
      "name": "tracker_api",
      "description": "Execute queries and mutations against the configured issue tracker.",
      "inputSchema": {
        "type": "object",
        "required": ["operation"],
        "properties": {
          "operation": {"type": "string"},
          "issue_id": {"type": "string"},
          "target_state": {"type": "string"}
        }
      }
    }
  ]
}}
```

Response:

```json
{"id": 10, "result": {"thread": {"id": "thr_abc123", "preview": "", "ephemeral": false, "modelProvider": "openai", "createdAt": 1745000000}}}
```

Followed by a notification:

```json
{"method": "thread/started", "params": {"thread": {"id": "thr_abc123"}}}
```

`startThread` sends `cwd`, `approvalPolicy` (default `never`), and `sandbox` (default
`workspace-write`) on every call, adds `model` and `personality` when the pass-through config
sets them, and adds `dynamicTools` when the tool registry is non-empty. It reads `thread.id`
from the response, fails the session when that field is empty, then waits up to
`readTimeout(state)` for the `thread/started` notification and returns the thread ID even if
that notification never arrives. `domain.Session.ID` carries the thread ID.

`dynamicTools` requires the `experimentalApi` capability. It registers client-side tools that
Codex invokes through `item/tool/call` during turns, which is how Sortie exposes `tracker_api`
without running an MCP server. `buildDynamicTools` emits one entry per tool in
`domain.ToolRegistry`, each carrying the tool's `Name()`, `Description()`, and `InputSchema()`.

### Starting a turn

```json
{"method": "turn/start", "id": 30, "params": {
  "threadId": "thr_abc123",
  "input": [{"type": "text", "text": "<rendered prompt>"}],
  "cwd": "/var/sortie/workspaces/PROJ-123",
  "sandboxPolicy": {
    "type": "workspaceWrite",
    "writableRoots": ["/var/sortie/workspaces/PROJ-123"],
    "networkAccess": false
  },
  "model": "gpt-5.4",
  "effort": "medium"
}}
```

Response:

```json
{"id": 30, "result": {"turn": {"id": "turn_456", "status": "inProgress", "items": [], "error": null}}}
```

`RunTurn` sends `threadId`, `input`, and `cwd` on every call. It adds `sandboxPolicy` on the
first turn of the session and on any turn when `codex.turn_sandbox_policy` is configured, and
adds `model` and `effort` when the pass-through config sets them. `turn/start` also accepts an
`approvalPolicy` override, which the adapter never sends, so the thread-level policy governs the
whole session. The `turn.id` from the response is the interrupt target.

### Continuation turns

For turn 2 onward, `RunTurn` sends another `turn/start` on the same thread. No resume argument
is needed; the thread maintains full conversation history.

```json
{"method": "turn/start", "id": 31, "params": {
  "threadId": "thr_abc123",
  "input": [{"type": "text", "text": "<continuation prompt>"}],
  "cwd": "/var/sortie/workspaces/PROJ-123"
}}
```

### Resuming a previous session

When `StartSessionParams.ResumeSessionID` is non-empty, `resumeThread` sends:

```json
{"method": "thread/resume", "id": 11, "params": {"threadId": "thr_abc123"}}
```

The response matches `thread/start`. History is restored from the thread's JSONL rollout file.
When the resume request fails, `StartSession` logs a warning and falls back to `startThread`,
so the session continues on a fresh thread with no history rather than failing.

---

## Event stream

After `turn/start`, `RunTurn` reads JSONL notifications from stdout until `turn/completed`
arrives. The `turn/completed` payload carries the final status for both successful and failed
turns. Any notification whose method is not listed below becomes a `domain.EventOtherMessage`
carrying the method name.

### Turn events

| Notification          | Description                                       | Adapter mapping                  |
| --------------------- | ------------------------------------------------- | -------------------------------- |
| `turn/started`        | Turn begins. Contains turn ID.                    | `session_started` on the session's first turn, `notification` afterwards |
| `turn/completed`      | Turn finished. Contains final status.             | Terminal event, see "Turn completion" |
| `thread/tokenUsage/updated` | Token usage snapshot for the thread.        | `token_usage`                    |
| `turn/diff/updated`   | Aggregated diff across file changes.              | Debug log only, no event         |
| `turn/plan/updated`   | Agent's plan update.                              | `notification`                   |

### Item events

| Notification             | Description                                    | Adapter mapping        |
| ------------------------ | ---------------------------------------------- | ---------------------- |
| `item/started`           | New item begins (command, message, tool call). | `notification` summarizing type and item ID; tool-shaped items also open an `agentcore.ToolTracker` entry |
| `item/completed`         | Item finished with final state.                | `tool_result` when the item ID is tracked; `notification` with the first 200 runes for an `agentMessage` |
| `item/agentMessage/delta`| Streaming text delta for agent message.        | Empty `notification`   |
| `item/commandExecution/outputDelta` | Streaming command output.            | Empty `notification`   |
| `item/tool/call`         | Dynamic tool invocation request.               | Dispatched to `domain.ToolRegistry`, see "Dynamic tool calls" |

Every emitted event advances `RunningEntry.LastAgentTimestamp` in the orchestrator, which is
what the stall detector reads. The empty-message notifications on the two delta methods exist
for that reason alone.

### Item types

| `item.type`            | Description                                      | Notes                                  |
| ---------------------- | ------------------------------------------------ | -------------------------------------- |
| `userMessage`          | User prompt (echoed back).                       | No special handling; summarized as a notification. |
| `agentMessage`         | Agent's text response.                           | Text emitted as a notification on `item/completed`. |
| `reasoning`            | Model reasoning output (when supported).         | No special handling.                   |
| `commandExecution`     | Shell command execution.                         | Tool-tracked under the `command` field, or under the item type when that field is empty. |
| `fileChange`           | Proposed or applied file edits.                  | Tool-tracked under the item type name. |
| `mcpToolCall`          | MCP tool invocation.                             | Tool-tracked under the item type name. |
| `dynamicToolCall`      | Client-side dynamic tool invocation.             | Tool-tracked under the item type name. |
| `webSearch`            | Web search request.                              | No special handling.                   |
| `contextCompaction`    | History compaction event.                        | No special handling.                   |

### Turn completion

The `turn/completed` notification contains the final turn state:

```json
{"method": "turn/completed", "params": {
  "turn": {
    "id": "turn_456",
    "status": "completed",
    "items": [...],
    "error": null
  }
}}
```

`turn.status` values:

| Status           | Adapter mapping                                                                                                            | Description                                       |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| `completed`      | `turn_completed`                                                                                                            | Agent finished normally.                          |
| `interrupted`    | `turn_cancelled` when the turn's context was already cancelled; `turn_failed` when the context was still live               | Cancelled via `turn/interrupt`, or an interruption Sortie did not request. |
| `failed`         | `turn_failed`                                                                                                                | Error during turn execution.                      |
| any other status | `turn_failed`                                                                                                                | An unrecognized status is treated as a failure.   |

On failure, `turn.error` contains `{message, codexErrorInfo?, additionalDetails?}`. The adapter
parses `message` and `codexErrorInfo`.

The persistent subprocess exits once at session end, not once per turn, so `RunTurn` has no
per-turn process exit to report to `agentcore.DecideTurn`. It builds every `TurnEvidence` with
`Work: agentcore.WorkUnobservable` and leaves `ExitObserved` false, which makes the shared
decision's zero-work row unreachable for this adapter: a missing `turn/completed` means the
stdout channel closed first, and that path already finalizes as `domain.ErrPortExit`.

---

## Token usage tracking

The `turn/completed` notification carries no `usage` member. The JSON Schema the binary
generates for itself (`codex app-server generate-json-schema`) defines `TurnCompletedNotification`
as `{threadId, turn}` and `Turn` as `{id, items, status, error, startedAt, completedAt,
durationMs}`; there is no `usage` field anywhere in that shape.

Token usage arrives separately, on `thread/tokenUsage/updated`:

```json
{"method": "thread/tokenUsage/updated", "params": {
  "threadId": "019fe79e-9f78-7da1-8b6c-b15b630dff3f",
  "turnId": "019fe79e-bd70-70e1-8d79-7ec812b4289d",
  "tokenUsage": {
    "total": {"totalTokens": 27458, "inputTokens": 27428, "cachedInputTokens": 13696, "outputTokens": 30, "reasoningOutputTokens": 18},
    "last":  {"totalTokens": 13727, "inputTokens": 13722, "cachedInputTokens": 13696, "outputTokens": 5,  "reasoningOutputTokens": 0},
    "modelContextWindow": 258400
  }
}}
```

`total` is thread-cumulative: it spans every turn of the thread, including turns from an earlier
run when the thread is resumed. It accumulates as `total(n) = total(n-1) + last(n)`, so a fresh
thread's first notification reports `total` equal to `last`. `RunTurn` recovers this run's own
contribution by subtracting a baseline captured at the first notification whose `turnId` matches
the run's current turn: `baseline = total - last` at that first notification, and every
subsequent matching notification reports `total - baseline`. A notification carrying a different
`turnId` raises the baseline to the componentwise maximum through `maxUsage` and emits nothing.

A notification whose `tokenUsage` object is absent from the wire payload is distinguishable
from one reporting all-zero counts, because `tokenUsageUpdatedParams.TokenUsage` is a pointer.
The absent case emits no event and leaves `sessionState.usageMeasured` untouched, so
`domain.TurnResult.UsageMeasured` stays false until a real measurement arrives.

Normalization by `normalizeBreakdown` to `domain.TokenUsage`:

| Codex field (`total` or `last` breakdown) | Sortie field      | Notes                                    |
| ----------------------- | ----------------- | ---------------------------------------- |
| `inputTokens`            | `input_tokens`    | Already inclusive of `cachedInputTokens`. |
| `outputTokens`           | `output_tokens`   | Already inclusive of `reasoningOutputTokens`. |
| `inputTokens + outputTokens` | `total_tokens` | Computed by the adapter, not read from `totalTokens`. |
| `cachedInputTokens`   | `cache_read_tokens` | Subset of `input_tokens`.              |

---

## Approval and sandbox policy

### Sandbox modes

Codex enforces OS-level sandboxing (Seatbelt on macOS, bwrap + seccomp on Linux).

| Sandbox mode          | `thread/start` sandbox | `sandboxPolicy.type` | Description                                    |
| --------------------- | ---------------------- | -------------------- | ---------------------------------------------- |
| Read-only             | `read-only`            | `readOnly`           | No file writes, no network.                    |
| Workspace write       | `workspace-write`      | `workspaceWrite`     | Writes allowed within workspace root. No network by default. |
| Danger full access    | `danger-full-access`   | `dangerFullAccess`   | No sandbox. Full filesystem and network access. |
| External sandbox      | `external-sandbox`     | `externalSandbox`    | Codex skips its sandbox; external enforcement assumed. |

The app-server uses kebab-case for the `sandbox` field on `thread/start` and camelCase for
`sandboxPolicy.type` on `turn/start`. `normalizeSandbox` and `denormalizeSandbox` translate
`codex.thread_sandbox` into whichever form the endpoint expects, and pass through a value
already in the target form.

`buildSandboxPolicy` defaults the turn policy to `workspaceWrite` with `writableRoots` set to
the workspace path and `networkAccess: false`, then copies `codex.turn_sandbox_policy` over the
result, so an operator override can replace any key including those two. Running
`dangerFullAccess` inside an externally sandboxed container (Docker with a restricted filesystem
and network) is the alternative when workspace write is too narrow.

### Approval policies

`AskForApproval` accepts three kebab-case strings or one object form. The strings:

| Policy         | Value           | Behavior                                                                                                          |
| -------------- | --------------- | ------------------------------------------------------------------------------------------------------------------ |
| Never ask      | `"never"`       | Never ask. Failures return to the model immediately and are never escalated to a user.                            |
| Unless trusted | `"untrusted"`   | Auto-approve only known-safe commands that read files. Everything else asks.                                      |
| On request     | `"on-request"`  | The model decides when to ask.                                                                                    |

The object form takes a single `granular` member whose booleans allow or reject each approval
category rather than surfacing it: `sandbox_approval`, `rules`, and `mcp_elicitations` are
required, and `request_permissions` and `skill_approval` are optional. A `false` field rejects
that category automatically instead of showing it.

Values and behaviors above come from OpenAI's published configuration schema
([config-schema.json](https://developers.openai.com/codex/config-schema.json)) and the protocol
schema the app-server generates for itself. Those two surfaces disagree on `untrusted`: the protocol
schema each release generates declares it, while the published configuration schema lists only
`on-request` and `never` among its strings. The v0.149.0 source describes `untrusted` as an internal
policy for projects marked untrusted; the v0.147.0 source describes the same variant by what it
auto-approves, as the table does. Because `approvalPolicy` travels on `thread/start`, the protocol
schema governs the value set here. A fourth string, `on-failure`, carries a second surface split on
the same field, by a different mechanism. Two distinct types share the name `AskForApproval`: the
configuration type behind `config.toml`, and the wire type `thread/start` deserializes into, from
which the generated schema and the TypeScript declarations derive. The configuration type maps
`on-failure` onto `on-request` through an alias, so the value is accepted in a configuration file.
The wire type carries no alias, so the same string is rejected on the wire. Both definitions are
identical across v0.147.0 and v0.149.0, so this split is a property of the surface rather than of
the release. It matters in practice because `codex.approval_policy` reaches the wire field and not
the agent's own configuration file: a value an operator has seen work in their `config.toml` is
rejected when Sortie sends it. The value dates from v0.121.0, where `--ask-for-approval` marked it
deprecated in help text. A value outside the accepted set is rejected at `thread/start` with
`-32600`:

```json
{"code": -32600, "message": "Invalid request: unknown variant `on-failure`, expected one of `untrusted`, `on-request`, `granular`, `never`"}
```

That enumeration lists the wire type's declared variants rather than everything it would accept,
because an alias never appears in it. The wire type declares no alias, so on this field the two
coincide. The message also shows the field enforcing what it declares: the server resolved the field
and rejected the value rather than failing to parse the frame. It names four variants because the
object arm is flattened into the same list under its tag, `granular`, which is the same surface as
three strings plus the object form.

`startThread` sends `approvalPolicy: "never"` unless `codex.approval_policy` overrides it, and
`typeutil.StringFrom` accepts only the string form, so the `granular` object cannot be expressed
through `codex.approval_policy` and silently falls back to `never`. In CLI mode the equivalent of
`"never"` is `--ask-for-approval never`, not `--full-auto`, which selects `on-request` with a
workspace-write sandbox. Sending `"never"` permits arbitrary command execution within the sandbox
boundary, so OpenAI's guidance is to use it only in a sandboxed environment. Sortie's workspace
isolation and hook system operate inside that sandbox as defense in depth and do not replace
container-level isolation.

### Approval requests

The protocol schema `codex-cli 0.147.0` generates for itself declares ten server-to-client
JSON-RPC requests, each with `id`, `method`, and `params` required. `RunTurn` recognizes seven of
them as a request only a person could answer and refuses every one: an unattended run has no one
to grant the permission or supply the answer any of the seven asks for.

| Method | What it asks | `RunTurn`'s reply |
| --- | --- | --- |
| `item/commandExecution/requestApproval` | Run a shell command. | Result `{"decision": "decline"}`, documented as "User denied the command. The agent will continue the turn." |
| `item/fileChange/requestApproval` | Change a file. | Result `{"decision": "decline"}`, documented as "User denied the file changes. The agent will continue the turn." |
| `applyPatchApproval` | Change a file; the legacy form of `item/fileChange/requestApproval`. | Result `{"decision": {"denied": {"rejection": "sortie refuses requests that only a person could answer"}}}`, the `ReviewDecision` variant documented as "the agent should not execute it, but it should continue the session and try something else." |
| `execCommandApproval` | Run a shell command; the legacy form of `item/commandExecution/requestApproval`. | Same reply as `applyPatchApproval`. |
| `mcpServer/elicitation/request` | Answer a question an MCP server addressed to a person. | Result `{"action": "decline"}`, which the response schema declares; the `content` member is left absent, which the schema documents as null on a decline. |
| `item/permissions/requestApproval` | Widen the agent's filesystem or network access. | The response schema declares no denial value and requires a `permissions` grant object, so no result can express a refusal. `RunTurn` writes the JSON-RPC error `{"code": -32001, "message": "sortie refuses requests that only a person could answer"}` instead. |
| `item/tool/requestUserInput` | Answer a question. | The response schema requires an `answers` member, so any result would be an answer rather than a refusal. `RunTurn` writes the same JSON-RPC error as `item/permissions/requestApproval`. |
| `item/tool/call` | Invoke a registered dynamic tool. | Out of this class; answered by `handleToolCall`, described under "Dynamic tool calls". |
| `account/chatgptAuthTokens/refresh` | Refresh a stored credential. | Out of this class; falls to the default arm of the notification switch, which emits a `domain.EventOtherMessage` carrying the method name and sends no response. |
| `attestation/generate` | Produce an attestation. | Out of this class; same default-arm handling as `account/chatgptAuthTokens/refresh`. |

The `-32001` code and its message are both pinned rather than derived: the schema declares `code`
as a required `int64` and `message` as a required string on the error object, enumerating no
values for either, and the app-server acknowledges no client response, so an unaccepted code
produces silence rather than an error. The bounded cancellation wait described below is the
backstop for that silence.

A permission request that `RunTurn` refuses in the continuable form lets the agent proceed by
another route within the same turn. A refused `mcpServer/elicitation/request`,
`item/permissions/requestApproval`, or `item/tool/requestUserInput` ends the turn immediately with
the human-input-required outcome, because none of those three response schemas offers a result
that both refuses and lets the turn continue.

The default `approvalPolicy: "never"` keeps the app-server from asking most of these questions in
the first place. Setting `codex.approval_policy` to any other value only changes how often the
app-server sends these requests. It does not change how any of them is answered: the seven
recognized methods keep the replies listed in the table above, which differ from one another,
and `item/tool/call`, `account/chatgptAuthTokens/refresh` and `attestation/generate` are not
in this class at all.

---

## Timeout enforcement

| Timeout           | Source                          | Enforcement                                        |
| ----------------- | ------------------------------- | -------------------------------------------------- |
| Read timeout      | `agent.read_timeout_ms`, default 30s | `readTimeout(state)` bounds the waits for the `account/login/completed` and `thread/started` notifications, and, once a turn's context is cancelled, how long `RunTurn` waits for `turn/completed` before returning. |
| Turn deadline     | The context passed to `RunTurn` | Cancellation triggers `turn/interrupt`, then the read timeout above bounds the wait for the runtime's own response to it. |
| Stall timeout     | `agent.stall_timeout_ms`        | `reconcileStalled` in `internal/orchestrator` cancels the worker context; each emitted event postpones it. |

The adapter reads only `ReadTimeoutMS` from `domain.AgentConfig`. It derives no deadline of its
own from `agent.turn_timeout_ms`; the turn's deadline is whatever context the orchestrator
passes to `RunTurn`. The `initialize`, `account/read`, `thread/start`, and `thread/resume`
responses are bounded by that same context, not by the read timeout.

On context cancellation `RunTurn` sends `turn/interrupt` once. `sendRequest` takes no context
and writes the frame straight to the app-server's stdin, so the cancelled parent cannot drop it
and no deadline bounds the write. `RunTurn` builds a two-second context at that call site and
never passes it in:

```json
{"method": "turn/interrupt", "id": 99, "params": {"threadId": "thr_abc123", "turnId": "turn_456"}}
```

It then keeps reading until `turn/completed` arrives, stdout closes, or `readTimeout(state)`
elapses, whichever comes first; past that bound `RunTurn` returns with `TerminalCancelled` rather
than waiting indefinitely for a `turn/interrupt` the app-server may never acknowledge. The
escalation to SIGTERM, a five-second grace period, and SIGKILL belongs to `StopSession`, not to
the interrupt path.

---

## Error detection and mapping

### Turn failure events

When a turn fails, the server emits a `turn/completed` notification with `status: "failed"` and
an `error` object:

```json
{"method": "turn/completed", "params": {
  "turn": {
    "id": "turn_456",
    "status": "failed",
    "items": [],
    "error": {
      "message": "Context window exceeded",
      "codexErrorInfo": "ContextWindowExceeded"
    }
  }
}}
```

### Error category mapping

`mapCodexErrorInfo` maps the `codexErrorInfo` string to a `domain.AgentErrorKind`. Any value
outside this table, including an empty one, maps to `turn_failed`.

| `codexErrorInfo`                    | `domain.AgentErrorKind` | Description                          |
| ----------------------------------- | ----------------------- | ------------------------------------ |
| `ContextWindowExceeded`             | `turn_failed`           | Token limit exceeded.                |
| `UsageLimitExceeded`                | `turn_failed`           | API usage quota exhausted.           |
| `HttpConnectionFailed`              | `turn_failed`           | Upstream API 4xx/5xx.                |
| `ResponseStreamConnectionFailed`    | `turn_failed`           | SSE or WebSocket stream disconnect.  |
| `ResponseStreamDisconnected`        | `turn_failed`           | Mid-stream disconnect.               |
| `ResponseTooManyFailedAttempts`     | `turn_failed`           | Retry budget exhausted.              |
| `Unauthorized`                      | `response_error`        | Invalid or expired API credentials.  |
| `BadRequest`                        | `response_error`        | Malformed request.                   |
| `SandboxError`                      | `turn_failed`           | Sandbox enforcement failure.         |
| `InternalServerError`               | `turn_failed`           | Server-side error.                   |
| `Other`                             | `turn_failed`           | Catch-all.                           |

### Failures outside the turn payload

The subprocess outlives the turn, so `RunTurn` never inspects a process exit code. It reports
`domain.ErrPortExit` when stdout closes before `turn/completed`, when a stdout read fails, when
the `turn/start` write fails, and when the context is already done at entry. A `turn/start`
response carrying a JSON-RPC `error` finalizes the turn as `turn_failed`.

A missing binary is caught before launch: `agentcore.ResolveBinary` returns
`domain.ErrAgentNotFound` when `exec.LookPath` cannot resolve the first token of
`agent.command`, and again when that token contains whitespace.

---

## Dynamic tool calls

`StartSession` passes every tool in `domain.ToolRegistry` to `thread/start` as a dynamic tool,
named by the tool's `Name()`. The examples below use `tracker_api`, the name
`trackerapi.TrackerAPITool` reports.

A `dynamicTools` entry is a union tagged on `type`. The function arm carries `name`,
`description`, `inputSchema` and an optional `deferLoading`; the namespace arm carries `name`,
`description` and a nested `tools` list. The generated schema marks `type` required on both arms.
`buildDynamicTools` emits no `type` and depends on the server not enforcing it, which is a
forward-compatibility risk that one field would retire.

When the agent invokes a tool, the app-server sends an `item/tool/call` request:

```json
{"method": "item/tool/call", "id": 50, "params": {
  "threadId": "<thread id>",
  "turnId": "<turn id>",
  "callId": "<call id>",
  "namespace": null,
  "tool": "tracker_api",
  "arguments": {
    "operation": "fetch_issue",
    "issue_id": "PROJ-123"
  }
}}
```

`DynamicToolCallParams` requires `threadId`, `turnId`, `callId`, `tool` and `arguments`, and
carries a nullable `namespace`. The decoder reads `tool` and `arguments`; the protocol declares
the rest and the adapter does not model them. `namespace` is non-null only for namespace-form
tools, which the adapter does not emit.

`handleToolCall` looks the name up in the registry and runs `AgentTool.Execute` in its own
goroutine so the event read loop keeps draining stdout. `toolResultFor` builds the response:

```json
{"id": 50, "result": {
  "success": true,
  "output": "{\"id\":\"123\",\"title\":\"Fix auth bug\",\"state\":\"In Progress\"}",
  "contentItems": [{"type": "inputText", "text": "{\"id\":\"123\",\"title\":\"Fix auth bug\",\"state\":\"In Progress\"}"}]
}}
```

`DynamicToolCallResponse` declares exactly two required members, `contentItems` and `success`.
The `output` string sent alongside them is outside the declared type; the first-party client
sends it too and the current release accepts a response carrying all three, so it is an accepted
extension rather than part of the contract. A `contentItems` entry is a union tagged on `type`
with three arms: `inputText` with `text`, `inputImage` with `imageUrl`, and `inputAudio` with
`audioUrl`. The adapter emits only `inputText`. A tool
that returns an error responds with `success: false` and the error text as `output`, and emits
a `domain.EventToolResult` with `ToolError` set. When the tool name is not registered, or no
registry was configured, the response is `success: false` with `unsupported tool: <name>` and
the adapter emits `domain.EventUnsupportedToolCall`. Unparseable params draw a
`success: false` response and no event.

`RunTurn` waits for every in-flight tool goroutine before it finalizes the turn, so a tool
result never arrives after the terminal event.

---

## Session storage

Codex persists thread history as JSONL files on disk. The default storage location is:

```
~/.codex/sessions/<thread-id>/rollout.jsonl
```

Archived threads move to:

```
~/.codex/sessions/archived/<thread-id>/rollout.jsonl
```

The adapter does not read or write these files. Thread resume goes through the app-server
`thread/resume` method, which loads the rollout itself. In a containerized deployment,
`~/.codex/sessions/` has to be a mounted volume for resume to survive a container restart.

---

## Hooks integration

Codex hooks are configured via `hooks.json` files located at:

- `~/.codex/hooks.json` (user-level)
- `<repo>/.codex/hooks.json` (project-level)

Hooks are gated behind a feature flag:

```toml
[features]
codex_hooks = true
```

### Supported hook events

| Event               | Matcher target    | Description                                    |
| ------------------- | ----------------- | ---------------------------------------------- |
| `SessionStart`      | `source`          | Fires on session startup or resume.            |
| `PreToolUse`        | `tool_name`       | Before a tool executes (currently Bash only).  |
| `PostToolUse`       | `tool_name`       | After a tool executes (currently Bash only).   |
| `UserPromptSubmit`  | not supported     | Before a user prompt is sent to the model.     |
| `Stop`              | not supported     | When a turn finishes. Can trigger continuation.|

Hooks run as shell commands with JSON on stdin and JSON/text on stdout. Each hook receives
`session_id`, `transcript_path`, `cwd`, `hook_event_name`, and `model` as common fields.

Returning `{"decision": "block", "reason": "..."}` from a `Stop` hook tells Codex to continue
the turn with the `reason` as a new user prompt, which is continuation logic outside the
orchestrator's view.

Sortie drives continuation through repeated `RunTurn` calls. The adapter writes no hook
configuration and reads no hook output, so hooks the operator places in the workspace run
without adapter involvement.

---

## MCP server configuration

Codex declares MCP servers in the `mcp_servers` table of `config.toml`. The user-level file
lives at `CODEX_HOME/config.toml`, which defaults to the `.codex` directory in the invoking
user's home; entries are written there directly or through `codex mcp add`, injected per
invocation with `-c mcp_servers.<name>.command=...` overrides, or reached by pointing
`CODEX_HOME` at another directory. A project marked trusted, meaning it carries
`trust_level = "trusted"` under `[projects."<path>"]` in the user-level config, also gets an
`mcp_servers` table read from a `config.toml` file inside that project's own `.codex`
directory: confirmed empirically on a running v0.149.0 binary, where an untrusted project's
`.codex/config.toml` contributes nothing to `codex mcp list` and a trusted one's does.

That trust is not necessarily something the operator grants. On `thread/start` the app-server
records it unprompted whenever the request carries a `cwd`, the project has no `trust_level` yet,
and the sandbox permits writing the working directory: it writes `trust_level = "trusted"` for the
resolved Git root, or for the working directory when that is not a repository, into the user-level
`config.toml` and reloads the configuration before the thread starts. Which sandbox value counts
differs by release. v0.147.0 tests the requested sandbox mode, so a requested `workspace-write` or
`danger-full-access` grants trust outright, even where a managed constraint reduces the effective
permission to read-only. v0.149.0 tests the effective permission profile after those constraints, so
a request reduced to read-only grants nothing; v0.147.0 is the more permissive of the two. The entry
persists, so the write lands the first time a project is seen rather than on every run. The
interactive TUI asks first (it prompts `Do you trust the contents of this directory?`), while the
app-server surface the adapter drives has no equivalent prompt. Sortie sends both a `cwd` and a
`workspace-write` sandbox on `thread/start`, so a normal run takes this path. The reload happens
before the thread starts, so a `.codex/config.toml` arriving with the checkout is live in the same
run that grants the trust. Arriving with the checkout is the only route under this sandbox:
`workspace-write` makes the workspace's own `.codex` directory a read-only entry, so the agent
cannot write that file itself. An explicit `trust_level = "untrusted"` recorded for the path blocks
both the grant and the project-local layer, because the grant is skipped whenever any trust level is
already present. The sandbox gates the grant, not the loading. A path already marked trusted keeps
loading its project-local configuration and starting the MCP servers that configuration declares
under a `read-only` sandbox, on v0.147.0 and v0.149.0 alike, because the
sandbox is consulted only when deciding whether to record trust for a path that has none yet. Sortie
derives the workspace path from the issue identifier and reuses it, so one earlier `workspace-write`
run for an issue arms every later run at that path. A project that is neither trusted nor granted
trust contributes nothing either: the loader gates project-local configuration on a positive trust
decision, and reports an unlisted path and an explicitly untrusted one with different diagnostics.

MCP servers declared in that layer run as child processes of the app-server and are not confined
by the `sandbox` value sent on `thread/start`. That value governs the commands the agent
executes, not the transports it connects to: the local launcher builds the configured command
directly, and the executor path starts it with no sandbox.

Codex reads no project-level `.codex/mcp.json` in either case. The only `.mcp.json` the binary
recognizes is plugin-scoped: a package carrying a `.codex-plugin/plugin.json` manifest may
ship one holding an `mcpServers` object, which the plugin manager loads. That is a packaging
artifact, not a file an operator points Codex at.

When an enabled MCP server is configured with `required = true` and fails to initialize, session
initialization fails rather than continuing without that server: `thread/start` returns a JSON-RPC
internal error reading `error creating thread: Fatal error: Failed to initialize session: required
MCP servers failed to initialize: <name>: <cause>`, and `thread/resume` returns the same text
behind an `error resuming thread:` prefix. The key defaults to `false`; under that default Codex
starts the thread without the server and reports the failure in an
`mcpServer/startupStatus/updated` notification. Its parameters require `name` and `status` and
may carry `threadId`, `error` and `failureReason`; `status` runs over `starting`, `ready`,
`failed` and `cancelled`, moving from `starting` to `failed` for a server that does not come up,
and `failureReason` has the single value `reauthenticationRequired`. Both v0.147.0 and
v0.149.0 carry this path.

Sortie reaches Codex tools through dynamic tool registration on `thread/start` instead, which
needs no separate server process. `StartSession` copies
`StartSessionParams.MCPConfigPath` into `sessionState.mcpConfigPath` and nothing reads it
again: the adapter writes no MCP config, passes no MCP argument, and leaves whatever the
operator has placed in the workspace untouched. The Claude Code and Copilot adapters, by
contrast, forward that path as a CLI argument.

---

## OpenTelemetry integration

Codex supports opt-in OTel export configured via `config.toml`:

```toml
[otel]
environment = "prod"
exporter = { otlp-http = {
  endpoint = "https://otel.example.com/v1/logs",
  protocol = "binary"
}}
log_user_prompt = false
```

Event categories include `codex.conversation_starts`, `codex.api_request`, `codex.tool_decision`,
`codex.tool_result`, and `codex.sse_event`.

The adapter does not configure OTel directly. If the operator sets `OTEL_EXPORTER_OTLP_ENDPOINT`
or configures `[otel]` in `config.toml`, Codex exports telemetry independently of Sortie's
observability pipeline.

---

## Adapter-specific pass-through config

The workflow YAML front matter supports a `codex:` block forwarded to the adapter without
core validation. `parsePassthroughConfig` reads it into `passthroughConfig`; a missing or
wrong-typed key falls back to the zero value.

```yaml
codex:
  approval_policy: never
  thread_sandbox: workspaceWrite
  model: gpt-5.4
  effort: medium
  personality: concise
  turn_sandbox_policy:
    networkAccess: true
```

| Config key                  | Type    | Description                                                               |
| --------------------------- | ------- | ------------------------------------------------------------------------- |
| `codex.approval_policy`     | string  | Approval policy for the thread. Maps to `approvalPolicy` on `thread/start`. Default `never`. |
| `codex.thread_sandbox`      | string  | Thread sandbox mode. Maps to `sandbox` on `thread/start`. Default `workspace-write`. |
| `codex.turn_sandbox_policy` | map     | Per-turn sandbox policy override. Merged over `sandboxPolicy` on `turn/start`. |
| `codex.model`               | string  | Model override (e.g., `gpt-5.4`). Maps to `model` on `thread/start` and `turn/start`. |
| `codex.effort`              | string  | Reasoning effort: `low`, `medium`, `high`. Maps to `effort` on `turn/start`.|
| `codex.personality`         | string  | Personality preset. Maps to `personality` on `thread/start`.              |

The turn timeouts are not part of this block. `agent.turn_timeout_ms`, `agent.read_timeout_ms`,
and `agent.stall_timeout_ms` are core config fields, described under "Timeout enforcement".

`typeutil.StringFrom` reads `approval_policy`, so only the three string values
(`"never"`, `"untrusted"`, `"on-request"`) survive parsing. The object form the schema also
accepts, a `granular` member whose booleans decide each approval category, is dropped by
`parsePassthroughConfig`, and the thread falls back to `never`. Written out, the form that
cannot be expressed here is:

```yaml
# Accepted by Codex, rejected by parsePassthroughConfig.
approval_policy:
  granular:
    sandbox_approval: true
    rules: true
    mcp_elicitations: true
```

Symphony configures the same policy with a differently-named map, keyed `reject`, so a snippet
copied from that project does not describe this wire format either.

---

## Full session lifecycle

The message ordering across one session, with the `userAgent` value as captured on v0.121.0:

```
# 1. Launch app-server
$ codex app-server
# (adapter reads/writes JSONL on stdin/stdout)

# 2. Initialize
→ {"method":"initialize","id":1,"params":{"clientInfo":{"name":"sortie_orchestrator","title":"Sortie Orchestrator","version":"0.1.0"},"capabilities":{"experimentalApi":true}}}
← {"id":1,"result":{"userAgent":"codex/0.121.0","platformFamily":"linux","platformOs":"linux"}}
→ {"method":"initialized","params":{}}

# 3. Authenticate (if needed)
→ {"method":"account/read","id":2,"params":{"refreshToken":false}}
← {"id":2,"result":{"account":{"type":"apiKey"},"requiresOpenaiAuth":false}}

# 4. Start thread
→ {"method":"thread/start","id":10,"params":{"model":"gpt-5.4","cwd":"/var/sortie/workspaces/PROJ-123","approvalPolicy":"never","sandbox":"workspace-write","dynamicTools":[{"name":"tracker_api","description":"Issue tracker operations","inputSchema":{"type":"object","required":["operation"],"properties":{"operation":{"type":"string"},"issue_id":{"type":"string"},"target_state":{"type":"string"}}}}]}}
← {"id":10,"result":{"thread":{"id":"thr_abc123"}}}
← {"method":"thread/started","params":{"thread":{"id":"thr_abc123"}}}

# 5. Start turn 1
→ {"method":"turn/start","id":30,"params":{"threadId":"thr_abc123","input":[{"type":"text","text":"Fix the authentication bug..."}],"cwd":"/var/sortie/workspaces/PROJ-123"}}
← {"id":30,"result":{"turn":{"id":"turn_001","status":"inProgress","items":[],"error":null}}}
← {"method":"turn/started","params":{"turn":{"id":"turn_001"}}}
← {"method":"item/started","params":{"item":{"id":"item_1","type":"commandExecution","command":"bash -lc cat src/auth.py","status":"in_progress"}}}
← {"method":"item/completed","params":{"item":{"id":"item_1","type":"commandExecution","status":"completed","exitCode":0}}}
← {"method":"item/started","params":{"item":{"id":"item_2","type":"agentMessage","text":"I found the bug..."}}}
← {"method":"item/completed","params":{"item":{"id":"item_2","type":"agentMessage","text":"I've fixed the authentication bug."}}}
← {"method":"thread/tokenUsage/updated","params":{"threadId":"thr_abc123","turnId":"turn_001","tokenUsage":{"total":{"totalTokens":15500,"inputTokens":15000,"cachedInputTokens":0,"outputTokens":500,"reasoningOutputTokens":0},"last":{"totalTokens":15500,"inputTokens":15000,"cachedInputTokens":0,"outputTokens":500,"reasoningOutputTokens":0}}}}
← {"method":"turn/completed","params":{"turn":{"id":"turn_001","status":"completed","items":[...],"error":null}}}

# 6. Start turn 2 (continuation)
→ {"method":"turn/start","id":31,"params":{"threadId":"thr_abc123","input":[{"type":"text","text":"Run the test suite to verify the fix."}],"cwd":"/var/sortie/workspaces/PROJ-123"}}

# 7. Stop session (close stdin, SIGTERM)
```

---

## The `codex exec` surface

No adapter code path launches `codex exec`. The surface is recorded here because it is the
other non-interactive entry point Codex ships, and its wire format shares nothing with the
app-server's: snake_case item types, a `usage` member on `turn.completed`, and dotted event
type names.

```bash
CODEX_API_KEY=sk-... codex exec \
  --full-auto \
  --sandbox workspace-write \
  --json \
  "Fix the authentication bug described in the issue" \
  2>/dev/null
```

JSONL output on stdout when `--json` is enabled:

```jsonl
{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Done."}}
{"type":"turn.completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122}}
```

Session resume with `codex exec`:

```bash
codex exec resume --last "Continue working on the remaining test failures"
codex exec resume <session_id> "Pick up where you left off"
```

The `codex exec` surface carries no dynamic tool registration, no mid-turn steering, and no
programmatic approval routing.

---

## Concurrency

Multiple `codex app-server` instances can run simultaneously, each in a different workspace
directory. Each instance is an independent process with its own thread state. There is no
shared state between instances beyond the filesystem.

Codex's internal sandbox enforcement is per-process, so two instances writing to the same
workspace directory would conflict. The orchestrator keys `State.Running` by issue ID and
refuses to dispatch an issue that already has an entry there, which keeps one agent session per
workspace.

---

## Runtime constraints

- **No Git repository gate on this surface.** The trusted-directory refusal belongs to the
  `codex exec` wrapper, so `--skip-git-repo-check` guards a path the adapter never takes.
  `thread/start` accepts a non-git `cwd` and reports `gitInfo: null`, and a workspace that no
  hook has cloned into is the ordinary case rather than a failure.
- **Context compaction.** Codex handles context window limits internally through background
  compaction, surfaced as `contextCompaction` item events. No adapter action follows.
- **Protected paths.** In `workspaceWrite` mode, the `.git`, `.agents`, and `.codex`
  directories inside writable roots are read-only. The agent cannot modify them directly.
- **Model and hosted-tool compatibility.** The app-server injects a `tool_search` hosted tool
  into its default tool set on every turn. `gpt-5.4-nano` rejects that tool with HTTP 400
  (`invalid_request_error` on the `tools` param) and the turn fails, so `gpt-5.4-mini` is the
  lowest viable tier. `codex features list` reports `tool_search` as `removed`, so it is not a
  user-togglable feature and `--disable tool_search` does not suppress the injection.

---

## Differences from Claude Code adapter

| Aspect                    | Claude Code                                                   | Codex CLI                                                     |
| ------------------------- | ------------------------------------------------------------- | ------------------------------------------------------------- |
| Binary                    | `claude` (npm `@anthropic-ai/claude-code`)                   | `codex` (npm `@openai/codex`, native Rust binary)             |
| Runtime                   | Node.js                                                       | Rust (native binary; no Node.js required)                     |
| Authentication            | `ANTHROPIC_API_KEY` (Anthropic), Bedrock, Vertex              | `CODEX_API_KEY` (OpenAI), ChatGPT session, external tokens    |
| Integration protocol      | CLI subprocess per turn (`-p <prompt>`)                      | Persistent app-server subprocess (JSON-RPC over stdio)         |
| Session continuity        | `--session-id` on the first turn, `--resume <session_id>` after (new subprocess per turn) | Same thread across turns (persistent process) |
| Output format             | `--output-format stream-json` (JSONL)                        | JSON-RPC 2.0 notifications (JSONL)                             |
| Permission bypass         | `--dangerously-skip-permissions`                             | `approvalPolicy: "never"` on `thread/start`                    |
| Sandbox enforcement       | None (relies on external container isolation)                | OS-level (Seatbelt/bwrap/seccomp) plus configurable policies   |
| Dynamic tools             | `--mcp-config` (MCP sidecar required)                        | `dynamicTools` on `thread/start` (no sidecar needed)           |
| Context management        | `compact_boundary` system event                              | `contextCompaction` item plus background auto-compaction       |
| Cost cap                  | `--max-budget-usd <amount>`                                  | Subscription quota or API rate limits                          |
| Internal turn limit       | `--max-turns <N>` when `claude-code.max_turns` is set        | Controlled by orchestrator through separate `turn/start` calls |
| Init event                | `system` type with `init` subtype, contains `session_id`     | `thread/started` notification contains `thread.id`             |
| Result event              | Final `result` message with `subtype`, `is_error`, `usage`   | `turn/completed` notification with `turn.status`; usage arrives separately on `thread/tokenUsage/updated` |
| Hooks location            | `.claude/hooks.json`                                          | `.codex/hooks.json`                                            |
| OTel configuration        | `CLAUDE_CODE_ENABLE_TELEMETRY=1`                             | `[otel]` block in `config.toml`                                |
| MCP config                | `--mcp-config <path>`, `--strict-mcp-config`                 | `mcp_servers` in `config.toml` (`CODEX_HOME`, or a trusted project's own `.codex/config.toml`), `-c` overrides |
| Models                    | Claude family (Sonnet, Opus, Haiku)                           | OpenAI family (GPT-5.4 default), configurable providers        |

## Differences from Copilot adapter

| Aspect                    | Copilot CLI                                                   | Codex CLI                                                     |
| ------------------------- | ------------------------------------------------------------- | ------------------------------------------------------------- |
| Binary                    | `copilot` (npm `@github/copilot`)                            | `codex` (npm `@openai/codex`, native Rust binary)             |
| Integration protocol      | CLI subprocess per turn (`-p <prompt>`)                      | Persistent app-server subprocess (JSON-RPC over stdio)         |
| Authentication            | GitHub token (`GH_TOKEN`, `GITHUB_TOKEN`)                    | `CODEX_API_KEY` (OpenAI), ChatGPT session                     |
| Permission bypass         | `--allow-all --no-ask-user`                                  | `approvalPolicy: "never"` on `thread/start`                    |
| Autonomous continuation   | `--autopilot --max-autopilot-continues <N>`                  | Orchestrator sends separate `turn/start` calls                 |
| Session continuation      | `--resume <session_id>`, `--continue` when no session ID was captured (new subprocess per turn) | Same thread across turns (persistent process) |
| Dynamic tools             | Not supported via CLI flags                                  | `dynamicTools` on `thread/start`                               |
| Sandbox enforcement       | None (relies on external container isolation)                | OS-level (Seatbelt/bwrap/seccomp)                              |
| Approval handling         | Pre-configured via CLI flags                                 | Thread-level `approvalPolicy`; over JSON-RPC the adapter answers `item/tool/call` only |
| Models                    | Multi-provider (Claude, GPT-5, etc.)                         | OpenAI family (GPT-5.4 default), configurable providers        |

---

## Open questions

Each entry names the probe that would settle it.

- Whether `dynamicTools` registered on `thread/start` survive `thread/resume`. Probe: resume a
  thread in a fresh app-server without re-registering, then prompt the agent to call
  `tracker_api` and watch for an `item/tool/call` request.
- Which `codexErrorInfo` value an authentication failure produces, and which one a rate-limit
  exhaustion produces. The mapping table splits them across `response_error` and `turn_failed`
  on the assumption that they arrive as `Unauthorized` and `UsageLimitExceeded`. Probe: run a
  turn with a revoked key, and a turn on an exhausted quota, and read `turn.error`.
- What `thread/start` returns when a thread is already active on the same connection. Probe:
  send a second `thread/start` mid-turn and record the response.
- Whether `turn/interrupt` always produces `status: "interrupted"`. Probe: interrupt at several
  points (before the first item, mid command execution, during a dynamic tool call) and compare
  the resulting `turn/completed` status.
- What the app-server does when an approval request goes unanswered, which is what the adapter
  does whenever `codex.approval_policy` is not `never`. Probe: start a thread with
  `approvalPolicy: "on-request"`, provoke a command approval, send nothing, and watch whether
  the turn blocks indefinitely or times out on its own.
- How the app-server exits on a fatal internal error, and with which exit code. Probe: kill the
  upstream connection mid-turn under `strace`, or feed it a malformed request that trips a
  panic path.
- How OS sandbox enforcement interacts with a containerized deployment. Probe: run a
  `workspace-write` turn inside Docker with and without
  `--security-opt seccomp=unconfined` and compare failures.
- Whether `CODEX_HOME` relocates the thread store away from `~/.codex/sessions/`. Probe: set it
  to a temporary directory, run a turn, and look for the rollout file under both paths.
- The real maximum message size on the stdio transport. The adapter's 1 MB scanner buffer comes
  from Symphony's observed message sizes, not from a documented app-server limit. Probe: request
  a turn whose diff or tool output exceeds 1 MB in one notification and see whether the
  app-server splits it.
- Whether `account/login/start` is required when `CODEX_API_KEY` is already in the subprocess
  environment, or whether `account/read` reports a non-null account on its own. Probe: launch
  the app-server with the variable set and read the `account/read` response before logging in.

---

## Sources

- [Codex documentation](https://developers.openai.com/codex)
- [Non-interactive mode](https://developers.openai.com/codex/noninteractive)
- [App Server protocol](https://developers.openai.com/codex/app-server)
- [Agent approvals & security](https://developers.openai.com/codex/agent-approvals-security)
- [Model Context Protocol](https://developers.openai.com/codex/mcp)
- [Configuration reference](https://developers.openai.com/codex/config-reference)
- [Authentication](https://developers.openai.com/codex/auth)
- [Hooks](https://developers.openai.com/codex/hooks)
- [Codex SDK](https://developers.openai.com/codex/sdk)
- [openai/codex GitHub repository](https://github.com/openai/codex)
- `codex app-server generate-json-schema --out <dir>`, which writes the JSON Schema set for the
  app-server protocol; 39 documents on v0.149.0
- `codex app-server generate-ts --out <dir>`, which writes the equivalent TypeScript declarations

Both are generated from the types the running binary uses, which makes them the authoritative,
version-exact description of the wire protocol and the first source to consult for any
wire-format question. Both require an output directory. They are easy to miss because top-level
help marks their parent `app-server` as experimental.

The generated schema is authoritative for what the app-server declares and unreliable for what it
enforces. The two come apart per field rather than per surface: on one request a field rejects
every value it does not declare, while another is marked required and is not enforced at all.
A `dynamicTools` entry omitting `type` starts a thread, while omitting `name` or `inputSchema`
draws `-32600 Invalid request: missing field '<name>'`. Read enforcement off a probe of the
field, never off the schema.
