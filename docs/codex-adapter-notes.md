# OpenAI Codex CLI: Adapter Research Notes

> OpenAI Codex CLI v0.121.x (npm `@openai/codex`, binary `codex`), researched April 2026.
> Reference for implementing the Codex `AgentAdapter`.
>
> **Primary sources:** [Codex documentation][codex-docs], [Non-interactive mode][ni-mode],
> [App Server protocol][app-server], [Agent approvals & security][approvals],
> [Authentication][auth], [Hooks][hooks], [Codex SDK][sdk],
> [openai/codex GitHub repository][repo].
>
> **Prior art:** OpenAI Symphony (Elixir) uses the app-server JSON-RPC
> protocol exclusively. Sortie's adapter uses the same protocol surface per architecture
> Section 10.7 (Local Subprocess Launch Contract).

---

## Overview

Codex CLI is an agentic coding tool from OpenAI that runs as a native Rust binary with a
Node.js-optional architecture. It reads a codebase, executes tools (shell commands, file edits,
MCP tool calls, web searches), and produces code changes autonomously. Sortie treats it as a
subprocess: launch its app-server mode, send JSON-RPC commands over stdio, read structured
event notifications, and terminate when done.

Two integration surfaces exist, in order of relevance to Sortie:

1. **App-server mode** (`codex app-server`) communicating over JSON-RPC 2.0 on stdio (JSONL).
   This is the primary integration surface. The app-server protocol powers the Codex VS Code
   extension and is the surface used by OpenAI's own Symphony orchestrator. It provides full
   lifecycle control: thread management, turn execution, approval handling, dynamic tool
   registration, and session resume.
2. **Non-interactive CLI** (`codex exec`) with `--json` for JSONL output on stdout. This is
   simpler but offers less control: no approval routing, no dynamic tool injection, no
   mid-turn steering. Suitable for one-shot CI tasks but insufficient for Sortie's multi-turn
   orchestration needs.

Sortie's Go adapter uses the app-server approach (surface 1). The `codex exec` surface is
documented as a fallback reference.

### Architectural difference from Claude Code and Copilot adapters

The Claude Code and Copilot adapters use a "launch-per-turn" model: each `RunTurn` call spawns a
new subprocess with `-p <prompt>` and reads JSONL until the process exits. Session continuity
relies on `--resume <session_id>`.

The Codex adapter uses a **persistent subprocess** model: `codex app-server` is launched once in
`StartSession` and kept alive across turns. Each turn is a `turn/start` JSON-RPC request within
a persistent thread. This matches Symphony's architecture and provides lower per-turn overhead,
richer approval control, and reliable session state.

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

After installation the `codex` binary is available on `$PATH`. The adapter's `agent.command`
config field defaults to `codex app-server` but can be overridden to point to a specific path
or wrapper script.

**Runtime requirements:**

- The `codex` binary (Rust-based, statically linked; no Node.js runtime required for the core
  binary since the rewrite from TypeScript to Rust)
- A valid OpenAI API key (`CODEX_API_KEY` or ChatGPT session credentials)
- A Git repository (Codex requires commands to run inside a Git repo; override with
  `--skip-git-repo-check` if necessary)

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

The adapter does **not** manage API keys directly. `CODEX_API_KEY` must be present in the
environment of the Sortie process. The adapter inherits the parent process environment when
spawning the subprocess.

For headless/CI environments where browser login is unavailable:

1. Set `CODEX_API_KEY` as an environment variable (preferred).
2. Alternatively, authenticate on a machine with a browser via `codex login`, then copy
   `~/.codex/auth.json` to the headless host.

### App-server authentication sequence

When using the app-server protocol, the adapter verifies auth state after initialization:

```json
{"method": "account/read", "id": 1, "params": {"refreshToken": false}}
```

If `result.account` is `null` and `CODEX_API_KEY` is set, the adapter logs in:

```json
{"method": "account/login/start", "id": 2, "params": {"type": "apiKey", "apiKey": "sk-..."}}
```

The adapter waits for `account/login/completed` with `success: true` before proceeding to
thread creation.

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

```
sh -c '$AGENT_COMMAND' -- 
```

Where `$AGENT_COMMAND` defaults to `codex app-server`. The subprocess receives:

| Setting           | Value                                               | Rationale                                                 |
| ----------------- | --------------------------------------------------- | --------------------------------------------------------- |
| Working directory | Workspace path (`StartSessionParams.WorkspacePath`) | Agent must operate in the issue workspace.                |
| Stdout            | Pipe (read by adapter)                              | JSONL output parsed line by line.                         |
| Stdin             | Pipe (written by adapter)                           | JSON-RPC requests sent as JSONL.                          |
| Stderr            | Pipe (read by adapter, logged)                      | Diagnostic output, not structured.                        |
| Environment       | Inherited from Sortie process                       | `CODEX_API_KEY` and other auth vars must be present.      |
| Max line size     | 1 MB                                                | Safe buffering per Symphony's observed message sizes.     |

### Initialization handshake

After launching, the adapter sends `initialize` followed by the `initialized` notification.
Requests sent before initialization are rejected.

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

`capabilities.experimentalApi` enables dynamic tool registration (`dynamicTools` on
`thread/start`) which Sortie uses for the `tracker_api` tool.

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
| `StartSession`         | Launch `codex app-server`, `initialize`, `thread/start` |
| `RunTurn` (turn 1)     | `turn/start` with prompt and configuration      |
| `RunTurn` (turn 2+)    | `turn/start` on the same thread                 |
| `StopSession`          | Close stdin, SIGTERM → grace → SIGKILL          |

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

The adapter records `thread.id` for all subsequent turn operations.

**`dynamicTools`** is an experimental field that requires `experimentalApi` capability.
It registers client-side tools that Codex can invoke during turns. The adapter uses this
to expose `tracker_api` without requiring MCP server configuration.

### Starting a turn

```json
{"method": "turn/start", "id": 30, "params": {
  "threadId": "thr_abc123",
  "input": [{"type": "text", "text": "<rendered prompt>"}],
  "cwd": "/var/sortie/workspaces/PROJ-123",
  "approvalPolicy": "never",
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

The adapter records `turn.id` for timeout enforcement and interrupt capability.

### Continuation turns

For turn 2+, the adapter sends another `turn/start` on the same thread. No `--resume` flag
is needed — the thread maintains full conversation history automatically.

```json
{"method": "turn/start", "id": 31, "params": {
  "threadId": "thr_abc123",
  "input": [{"type": "text", "text": "<continuation prompt>"}],
  "cwd": "/var/sortie/workspaces/PROJ-123"
}}
```

This is simpler than the Claude Code and Copilot adapters, which require launching a new
subprocess per turn with explicit session ID propagation.

### Resuming a previous session

If the app-server process was terminated between Sortie runs but the thread log exists on disk:

```json
{"method": "thread/resume", "id": 11, "params": {"threadId": "thr_abc123"}}
```

The response matches `thread/start`. History is restored from the thread's JSONL rollout file.

---

## Event stream

After `turn/start`, the adapter reads JSONL notifications from stdout until `turn/completed`
is received. The `turn/completed` payload carries the final status for both successful and
failed turns.

### Turn events

| Notification          | Description                                       | Adapter mapping                  |
| --------------------- | ------------------------------------------------- | -------------------------------- |
| `turn/started`        | Turn begins. Contains turn ID.                    | `session_started` (first turn)   |
| `turn/completed`      | Turn finished. Contains final status and usage.   | `turn_completed`                 |
| `turn/diff/updated`   | Aggregated diff across file changes.              | Log for observability            |
| `turn/plan/updated`   | Agent's plan update.                              | `notification`                   |

### Item events

| Notification             | Description                                    | Adapter mapping        |
| ------------------------ | ---------------------------------------------- | ---------------------- |
| `item/started`           | New item begins (command, message, tool call). | `tool_use` / `notification` |
| `item/completed`         | Item finished with final state.                | `tool_result` / `assistant_message` |
| `item/agentMessage/delta`| Streaming text delta for agent message.        | Stall detection timer reset |
| `item/commandExecution/outputDelta` | Streaming command output.            | Stall detection timer reset |

### Item types

| `item.type`            | Description                                      | Notes                                  |
| ---------------------- | ------------------------------------------------ | -------------------------------------- |
| `userMessage`          | User prompt (echoed back).                       | Ignored by adapter.                    |
| `agentMessage`         | Agent's text response.                           | Map to `assistant_message`.            |
| `reasoning`            | Model reasoning output (when supported).         | Log; not mapped to events.             |
| `commandExecution`     | Shell command execution.                         | Map to `tool_use` / `tool_result`.     |
| `fileChange`           | Proposed or applied file edits.                  | Map to `tool_use` / `tool_result`.     |
| `mcpToolCall`          | MCP tool invocation.                             | Map to `tool_use` / `tool_result`.     |
| `dynamicToolCall`      | Client-side dynamic tool invocation.             | Handle `tracker_api` calls.            |
| `webSearch`            | Web search request.                              | Log as `notification`.                 |
| `contextCompaction`    | History compaction event.                        | Log as `notification`.                 |

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

| Status          | Adapter mapping    | Description                          |
| --------------- | ------------------ | ------------------------------------ |
| `completed`     | `turn_completed`   | Agent finished normally.             |
| `interrupted`   | `turn_cancelled`   | Cancelled via `turn/interrupt`.      |
| `failed`        | `turn_failed`      | Error during turn execution.         |

On failure, `turn.error` contains `{message, codexErrorInfo?, additionalDetails?}`.

---

## Token usage tracking

On the app-server JSON-RPC surface, token usage is available from `turn/completed`
and `thread/tokenUsage/updated` notifications.

The `turn/completed` notification includes usage under `params`:

```json
{"method": "turn/completed", "params": {
  "turn": {
    "id": "turn_456",
    "status": "completed",
    "usage": {
      "input_tokens": 24763,
      "cached_input_tokens": 24448,
      "output_tokens": 122
    }
  }
}}
```

Normalization to Sortie's `{input_tokens, output_tokens, total_tokens}`:

| Codex field             | Sortie field      | Notes                                    |
| ----------------------- | ----------------- | ---------------------------------------- |
| `input_tokens`          | `input_tokens`    | Total input tokens (includes cached).    |
| `output_tokens`         | `output_tokens`   | Output tokens generated.                 |
| sum of above            | `total_tokens`    | Computed by adapter.                     |
| `cached_input_tokens`   | —                 | Logged for observability; not in Sortie's model. |

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

The app-server uses **kebab-case** for the `sandbox` field on `thread/start` and **camelCase**
for `sandboxPolicy.type` on `turn/start`. The adapter's WORKFLOW.md config (`thread_sandbox`)
accepts camelCase values and translates to the correct wire format for each endpoint.

For Sortie's headless operation in sandboxed containers, two approaches:

1. **`workspaceWrite`** with `writableRoots` set to the workspace path. This is the default
   and preferred approach, matching Symphony's configuration.
2. **`dangerFullAccess`** inside an externally sandboxed container (Docker with restricted
   filesystem and network). Use only when workspace-write is insufficient.

### Approval policies

| Policy           | App-server value    | Behavior                                           |
| ---------------- | ------------------- | -------------------------------------------------- |
| Never ask        | `"never"`           | Never ask for approval. Auto-approve everything.   |
| On request       | `"onRequest"`       | Ask only when agent explicitly requests it.        |
| Unless trusted   | `"unlessTrusted"`   | Ask for untrusted commands.                        |
| Always ask       | `"always"`          | Ask for every action.                              |

For headless orchestration, the adapter sets `approvalPolicy: "never"` on both `thread/start`
and `turn/start`. This is equivalent to `--full-auto` or `--ask-for-approval never` in CLI
mode.

> **Security note:** `approvalPolicy: "never"` allows arbitrary command execution within the
> sandbox boundary. Per OpenAI's guidance, this should only be used in sandboxed environments.
> Sortie's workspace isolation and hook system operate inside that sandbox as additional
> defense-in-depth, but do not replace external container-level isolation.

### Handling approval requests

When `approvalPolicy` is not `"never"`, the app-server sends approval requests as JSON-RPC
requests to the client:

- `item/commandExecution/requestApproval` — shell command approval
- `item/fileChange/requestApproval` — file edit approval
- `item/tool/requestUserInput` — user input request (can carry approval questions)
- `item/tool/call` — dynamic tool call (client must execute and return result)

The adapter responds with approval decisions:

```json
{"id": 42, "result": {"decision": "acceptForSession"}}
```

Available decisions for command and file approvals: `accept`, `acceptForSession`, `decline`,
`cancel`.

For `item/tool/call` (dynamic tool invocations including `tracker_api`), the adapter executes
the tool and returns the result:

```json
{"id": 43, "result": {
  "success": true,
  "output": "{\"issues\": [...]}",
  "contentItems": [{"type": "inputText", "text": "{\"issues\": [...]}"}]
}}
```

Symphony's implementation auto-approves all command and file change requests (`decision:
"acceptForSession"`) and executes dynamic tools via a configurable executor function. The
Sortie adapter follows the same pattern.

---

## Timeout enforcement

| Timeout           | Source                          | Enforcement                                        |
| ----------------- | ------------------------------- | -------------------------------------------------- |
| Turn timeout      | `agent.turn_timeout_ms`         | Context deadline on the turn read loop.            |
| Read timeout      | `agent.read_timeout_ms`         | Applied to the `initialize` and `thread/start` responses. |
| Stall timeout     | `agent.stall_timeout_ms`        | Reset on any `item/*` notification or delta event.  |

When the turn timeout fires:

1. Send `turn/interrupt`:
   ```json
   {"method": "turn/interrupt", "id": 99, "params": {"threadId": "thr_abc123", "turnId": "turn_456"}}
   ```
2. Wait for `turn/completed` with `status: "interrupted"`.
3. If no response within a grace period, SIGTERM → SIGKILL the subprocess.

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

| `codexErrorInfo`                    | Adapter mapping     | Description                              |
| ----------------------------------- | ------------------- | ---------------------------------------- |
| `ContextWindowExceeded`             | `turn_failed`       | Token limit exceeded.                    |
| `UsageLimitExceeded`                | `turn_failed`       | API usage quota exhausted.               |
| `HttpConnectionFailed`              | `turn_failed`       | Upstream API 4xx/5xx.                    |
| `ResponseStreamConnectionFailed`    | `turn_failed`       | SSE/WS stream disconnect.               |
| `ResponseStreamDisconnected`        | `turn_failed`       | Mid-stream disconnect.                   |
| `ResponseTooManyFailedAttempts`     | `turn_failed`       | Retry budget exhausted.                  |
| `Unauthorized`                      | `agent_auth_error`  | Invalid or expired API credentials.      |
| `BadRequest`                        | `turn_failed`       | Malformed request.                       |
| `SandboxError`                      | `turn_failed`       | Sandbox enforcement failure.             |
| `InternalServerError`               | `turn_failed`       | Server-side error.                       |
| `Other`                             | `turn_failed`       | Catch-all.                               |

### Process exit codes

| Exit scenario        | Adapter mapping      | Description                               |
| -------------------- | -------------------- | ----------------------------------------- |
| Clean shutdown       | —                    | Normal after stdin close or interrupt.     |
| Non-zero exit        | `turn_failed`        | Unexpected failure.                        |
| 127                  | `agent_not_found`    | `codex` binary not found on `$PATH`.       |
| Signal (SIGTERM)     | `turn_cancelled`     | Killed by adapter timeout.                 |
| Signal (SIGKILL)     | `turn_cancelled`     | Force-killed after grace period.           |

---

## Dynamic tool calls (`tracker_api`)

The adapter registers `tracker_api` as a dynamic tool on `thread/start`. When the agent
invokes it, the app-server sends an `item/tool/call` request:

```json
{"method": "item/tool/call", "id": 50, "params": {
  "tool": "tracker_api",
  "arguments": {
    "operation": "fetch_issue",
    "issue_id": "PROJ-123"
  }
}}
```

The adapter dispatches to the configured `TrackerAdapter`, serializes the result, and responds:

```json
{"id": 50, "result": {
  "success": true,
  "output": "{\"id\":\"123\",\"title\":\"Fix auth bug\",\"state\":\"In Progress\"}",
  "contentItems": [{"type": "inputText", "text": "{\"id\":\"123\",\"title\":\"Fix auth bug\",\"state\":\"In Progress\"}"}]
}}
```

The response includes `success` (boolean), `output` (string), and `contentItems` (array).
This matches the dynamic tool response schema observed in Symphony's implementation.

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

The adapter does not interact with these files directly. Thread resume uses the app-server
`thread/resume` method, which handles rollout loading internally.

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

The `Stop` hook is notable: returning `{"decision": "block", "reason": "..."}` tells Codex
to continue the turn with the `reason` as a new user prompt. This enables external
continuation logic without orchestrator involvement.

Sortie's orchestrator manages its own continuation logic through `RunTurn` calls. The
adapter does not rely on Codex hooks for turn continuation but does not interfere with
hooks that the operator configures at the workspace level.

---

## MCP server configuration

Codex discovers MCP servers from project-level `.codex/mcp.json` files. The adapter can
inject additional MCP servers (e.g., `tracker_api` as an MCP sidecar) through Codex's
configuration system.

However, the preferred approach for `tracker_api` is dynamic tool registration on
`thread/start` (see Dynamic tool calls above), which avoids the need for a separate MCP
server process.

If MCP configuration is needed, the adapter writes a temporary `mcp.json` to the workspace's
`.codex/` directory before launching the app-server.

When an enabled MCP server is configured with `required = true` and fails to initialize,
`thread/start` fails instead of continuing without it. The adapter avoids setting
`required = true` on MCP servers unless they are essential for the workflow.

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
core validation:

```yaml
codex:
  approval_policy: never
  thread_sandbox: workspaceWrite
  model: gpt-5.4
  effort: medium
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
```

| Config key                  | Type    | Description                                                               |
| --------------------------- | ------- | ------------------------------------------------------------------------- |
| `codex.approval_policy`     | string or map | Approval policy for thread and turn. Maps to `approvalPolicy`.      |
| `codex.thread_sandbox`      | string  | Thread sandbox mode. Maps to `sandbox` on `thread/start`.                 |
| `codex.turn_sandbox_policy` | map     | Per-turn sandbox policy override. Maps to `sandboxPolicy` on `turn/start`.|
| `codex.model`               | string  | Model override (e.g., `gpt-5.4`). Maps to `model` on `thread/start`.     |
| `codex.effort`              | string  | Reasoning effort: `low`, `medium`, `high`. Maps to `effort` on `turn/start`.|
| `codex.personality`         | string  | Personality preset. Maps to `personality` on `thread/start`.              |

The `approval_policy` field accepts either a simple string (`"never"`, `"onRequest"`,
`"unlessTrusted"`, `"always"`) or a granular rejection map matching Symphony's schema:

```yaml
codex:
  approval_policy:
    reject:
      sandbox_approval: true
      rules: true
      mcp_elicitations: true
```

When `approval_policy` is a map with `reject` keys, the adapter translates it to the
app-server's granular approval policy format. With `reject.sandbox_approval: true`, the
adapter auto-declines sandbox-related approval requests rather than forwarding them to an
operator.

---

## Example adapter invocation

### Full session lifecycle

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
← {"method":"turn/completed","params":{"turn":{"id":"turn_001","status":"completed","items":[...],"error":null},"usage":{"input_tokens":15000,"output_tokens":500}}}

# 6. Start turn 2 (continuation)
→ {"method":"turn/start","id":31,"params":{"threadId":"thr_abc123","input":[{"type":"text","text":"Run the test suite to verify the fix."}],"cwd":"/var/sortie/workspaces/PROJ-123"}}

# 7. Stop session (close stdin, SIGTERM)
```

---

## `codex exec` alternative (reference only)

For one-shot tasks or fallback scenarios, the adapter can use `codex exec`:

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

The `codex exec` surface does not support dynamic tool registration, mid-turn steering, or
programmatic approval routing. It is not the recommended surface for Sortie.

---

## Concurrency

Multiple `codex app-server` instances can run simultaneously, each in a different workspace
directory. Each instance is an independent process with its own thread state. There is no
shared state between instances beyond the filesystem.

Codex's internal sandbox enforcement is per-process. Two instances writing to the same workspace
directory would conflict. Sortie's orchestrator ensures one agent session per workspace per
issue, preventing this scenario.

---

## Known behavioral notes

- **Git repository required:** Codex requires the working directory to be inside a Git
  repository. The adapter ensures workspaces are initialized as Git repos (or passes
  `--skip-git-repo-check` if configured). Symphony's workspace manager handles this via a
  `before_run` hook that initializes Git if needed.
- **Context compaction:** Codex handles context window limits internally via background
  compaction. The adapter receives `contextCompaction` item events when this occurs.
  No adapter action is required.
- **Thread persistence:** Thread JSONL files are written to `~/.codex/sessions/`. In
  containerized deployments, this directory should be mounted as a volume if session resume
  across container restarts is desired.
- **Protected paths:** In `workspaceWrite` mode, `.git`, `.agents`, and `.codex` directories
  within writable roots are read-only. The agent cannot modify these directories directly.
- **Approval policy granularity:** The `approval_policy = {reject: {...}}` map syntax
  (observed in Symphony) allows silently declining specific approval categories rather than
  auto-approving them. With `reject.sandbox_approval: true`, sandbox-related prompts are
  rejected (agent sees a denial) rather than approved.

---

## Differences from Claude Code adapter

| Aspect                    | Claude Code                                                   | Codex CLI                                                     |
| ------------------------- | ------------------------------------------------------------- | ------------------------------------------------------------- |
| Binary                    | `claude` (npm `@anthropic-ai/claude-code`)                   | `codex` (npm `@openai/codex`, native Rust binary)             |
| Runtime                   | Node.js                                                       | Rust (native binary; no Node.js required)                     |
| Authentication            | `ANTHROPIC_API_KEY` (Anthropic), Bedrock, Vertex              | `CODEX_API_KEY` (OpenAI), ChatGPT session, external tokens    |
| Integration protocol      | CLI subprocess per turn (`-p <prompt>`)                      | Persistent app-server subprocess (JSON-RPC over stdio)         |
| Session continuity        | `--resume <session_id>` (new subprocess per turn)            | Same thread across turns (persistent process)                  |
| Output format             | `--output-format stream-json` (JSONL)                        | JSON-RPC 2.0 notifications (JSONL)                             |
| Permission bypass         | `--dangerously-skip-permissions`                             | `approvalPolicy: "never"` on thread/turn                       |
| Sandbox enforcement       | None (relies on external container isolation)                | OS-level (Seatbelt/bwrap/seccomp) + configurable policies      |
| Dynamic tools             | `--mcp-config` (MCP sidecar required)                        | `dynamicTools` on `thread/start` (no sidecar needed)           |
| Context management        | `compact_boundary` system event                              | `contextCompaction` item + background auto-compaction           |
| Cost cap                  | `--max-budget-usd <amount>`                                  | Subscription quota or API rate limits                          |
| Internal turn limit       | `--max-turns <N>` (may be SDK-only)                          | Controlled by orchestrator via separate `turn/start` calls     |
| Init event                | `system` type with `init` subtype, contains `session_id`     | `thread/started` notification contains `thread.id`             |
| Result event              | Final `result` message with `subtype`, `is_error`, `usage`   | `turn/completed` notification with `turn.status`, `usage`      |
| Hooks location            | `.claude/hooks.json`                                          | `.codex/hooks.json`                                            |
| OTel configuration        | `CLAUDE_CODE_ENABLE_TELEMETRY=1`                             | `[otel]` block in `config.toml`                                |
| MCP config                | `--mcp-config <path>`, `--strict-mcp-config`                 | `.codex/mcp.json` (project-level), `config.toml`              |
| Models                    | Claude family (Sonnet, Opus, Haiku)                           | OpenAI family (GPT-5.4 default), configurable providers        |

## Differences from Copilot adapter

| Aspect                    | Copilot CLI                                                   | Codex CLI                                                     |
| ------------------------- | ------------------------------------------------------------- | ------------------------------------------------------------- |
| Binary                    | `copilot` (npm `@github/copilot`)                            | `codex` (npm `@openai/codex`, native Rust binary)             |
| Integration protocol      | CLI subprocess per turn (`-p <prompt>`)                      | Persistent app-server subprocess (JSON-RPC over stdio)         |
| Authentication            | GitHub token (`GH_TOKEN`, `GITHUB_TOKEN`)                    | `CODEX_API_KEY` (OpenAI), ChatGPT session                     |
| Permission bypass         | `--allow-all --no-ask-user`                                  | `approvalPolicy: "never"` on thread/turn                       |
| Autonomous continuation   | `--autopilot --max-autopilot-continues <N>`                  | Orchestrator sends separate `turn/start` calls                 |
| Session continuation      | `--resume <session_id>` (new subprocess per turn)            | Same thread across turns (persistent process)                  |
| Dynamic tools             | Not supported via CLI flags                                  | `dynamicTools` on `thread/start`                               |
| Sandbox enforcement       | None (relies on external container isolation)                | OS-level (Seatbelt/bwrap/seccomp)                              |
| Approval handling         | Pre-configured via CLI flags                                 | Programmatic approval routing via JSON-RPC                     |
| Models                    | Multi-provider (Claude, GPT-5, etc.)                         | OpenAI family (GPT-5.4 default), configurable providers        |

---

## Summary: adapter implementation checklist

1. **StartSession:** Launch `codex app-server` with cwd = workspace. Send `initialize` +
   `initialized`. Verify authentication via `account/read`. Start thread via `thread/start`
   with `approvalPolicy`, `sandbox`, and `dynamicTools` configuration. Record `thread.id`.
2. **RunTurn (turn 1):** Send `turn/start` with rendered prompt and turn configuration.
   Record `turn.id`. Enter event read loop.
3. **RunTurn (turn 2+):** Same as turn 1 on the same `thread.id`. No resume flag needed.
4. **Event parsing:** Read JSONL notifications. Map `item/started` → `tool_use`,
   `item/completed` → `tool_result` or `assistant_message`, `turn/completed` →
   `turn_completed` or `turn_failed`. Handle `item/tool/call` for dynamic tool dispatch.
   Log unknown notification methods as `other_message`.
5. **Approval handling:** When `approvalPolicy != "never"`, respond to
   `item/commandExecution/requestApproval` and `item/fileChange/requestApproval` with
   `{"decision": "acceptForSession"}`. Execute `item/tool/call` requests via
   `DynamicTool` / `TrackerAdapter` dispatch.
6. **Token tracking:** Extract `usage` from `turn/completed`. Normalize to
   `{input_tokens, output_tokens, total_tokens}`.
7. **Timeout:** Enforce `turn_timeout_ms` via context deadline. On timeout, send
   `turn/interrupt` then SIGTERM → grace → SIGKILL.
8. **Stall detection:** Reset stall timer on any `item/*` notification or delta event.
   On stall timeout, treat as turn timeout.
9. **StopSession:** Close stdin pipe. Send SIGTERM. Wait for process exit. SIGKILL after
   grace period.
10. **Error mapping:** Check `turn.status` and `turn.error.codexErrorInfo`. Map to
    architecture error categories. Process exit code 127 → `agent_not_found`.
11. **Session resume:** If app-server was restarted, use `thread/resume` with stored
    `thread.id` to restore conversation history.
12. **Git requirement:** Ensure workspace is a Git repository before launching app-server,
    or configure `--skip-git-repo-check`.

### Verification items

Items requiring experimental verification:

- [ ] `dynamicTools` persistence across `thread/resume` (documented but not verified).
- [ ] Exact `codexErrorInfo` values for authentication failures vs. rate limit exhaustion.
- [ ] Behavior when `thread/start` is called with an already-active thread.
- [ ] Whether `turn/interrupt` reliably produces `status: "interrupted"` under all conditions.
- [ ] Process exit code mapping when app-server encounters a fatal internal error.
- [ ] Interaction between OS sandbox enforcement and containerized deployment (Docker with
  `--security-opt seccomp=unconfined`).
- [ ] Thread storage location customization (whether `CODEX_HOME` env var is respected).
- [ ] Maximum message size on stdio transport (observed 1 MB in Symphony; may vary).
- [ ] `account/login/start` behavior when `CODEX_API_KEY` is already in the environment
  (auto-login vs. explicit login required).

---

## Sources

[codex-docs]: https://developers.openai.com/codex
[ni-mode]: https://developers.openai.com/codex/noninteractive
[app-server]: https://developers.openai.com/codex/app-server
[approvals]: https://developers.openai.com/codex/agent-approvals-security
[auth]: https://developers.openai.com/codex/auth
[hooks]: https://developers.openai.com/codex/hooks
[sdk]: https://developers.openai.com/codex/sdk
[repo]: https://github.com/openai/codex
