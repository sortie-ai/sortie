# Claude Code CLI: Adapter Research Notes

> Claude Code CLI v2.x (npm `@anthropic-ai/claude-code`), researched March 2026.
> Reference for implementing the Claude Code `AgentAdapter`.

---

## Overview

Claude Code is an agentic coding tool that runs as a Node.js process in the terminal. It reads
a codebase, executes tools (file edits, shell commands, searches), and produces code changes
autonomously. Sortie treats it as a subprocess: launch it with a prompt, read structured output
from stdout, and terminate it when done.

The primary integration surface is the **CLI in non-interactive ("headless") mode** using the
`-p` (print) flag. Anthropic also provides TypeScript and Python SDKs (`@anthropic-ai/claude-agent-sdk`
and `claude-agent-sdk` respectively), but Sortie's Go adapter uses the CLI subprocess approach per
architecture Section 10.7 (Local Subprocess Launch Contract).

---

## Installation and Prerequisites

Claude Code is an npm package:

```bash
npm install -g @anthropic-ai/claude-code
```

After installation the `claude` binary is available on `$PATH`. The adapter's `agent.command`
config field defaults to `claude` but can be overridden to point to a specific path or wrapper.

**Runtime requirements:**

- Node.js (bundled or host-provided)
- A valid Anthropic API key (`ANTHROPIC_API_KEY` environment variable)
- Alternatively, AWS Bedrock or Google Vertex AI credentials via environment variables

---

## Authentication

Claude Code authenticates against the Anthropic API (or a compatible provider).

| Method                 | Environment Variables                                                                   | Notes                                                   |
| ---------------------- | --------------------------------------------------------------------------------------- | ------------------------------------------------------- |
| Anthropic API (direct) | `ANTHROPIC_API_KEY`                                                                     | Default. Standard Anthropic API key.                    |
| AWS Bedrock            | `CLAUDE_CODE_USE_BEDROCK=1`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` | Cross-region inference profile IDs for model selection. |
| Google Vertex AI       | `CLAUDE_CODE_USE_VERTEX=1`, `ANTHROPIC_VERTEX_PROJECT_ID`, `CLOUD_ML_REGION`            | GCP credentials via ADC or explicit env vars.           |
| LLM Gateway / Proxy    | `ANTHROPIC_BASE_URL` or provider-specific base URL env vars                             | For LiteLLM, custom proxies, etc.                       |

### Config mapping

| Sortie config field           | Value                                                 |
| ----------------------------- | ----------------------------------------------------- |
| `agent.kind`                  | `claude-code`                                         |
| `agent.command`               | `claude` (or full path to the binary)                 |
| `claude-code.permission_mode` | Pass-through adapter config (see Permissions section) |

The adapter does **not** manage API keys directly. The Anthropic API key must be present in
the environment of the Sortie process (or passed through via hook env). The adapter inherits
the parent process environment when spawning the subprocess.

---

## CLI Flags Reference

The adapter constructs a `claude` invocation using these flags. Flags marked **(required)**
are always set by the adapter; others are conditional.

### Core flags

| Flag                                 | Description                                                                                                             | Adapter usage                                                                           |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `-p <prompt>`                        | Non-interactive (headless) mode. Passes the prompt and exits when done.                                                 | **(required)** Every turn invocation uses this.                                         |
| `--output-format stream-json`        | Newline-delimited JSON on stdout. Each line is a JSON object.                                                           | **(required)** For real-time event parsing.                                             |
| `--verbose`                          | Include internal events (tool calls, system messages) in stream output.                                                 | **(required)** Needed for full event visibility.                                        |
| `--max-turns <N>`                    | Maximum number of agentic turns within a single CLI invocation. Not in `claude --help` v2.1.76; may be SDK/action only. | Set to `1` for single-turn control, or omit to let Claude decide. See Turn Model below. |
| `--max-budget-usd <amount>`          | Maximum dollar spend for this invocation (print mode only).                                                             | Optional. Cost safety backstop. Exits with error when exceeded.                         |
| `--model <model>`                    | Override the model (e.g., `claude-sonnet-4-6`, `claude-opus-4-6`).                                                      | Optional. Adapter passes through if configured.                                         |
| `--fallback-model <model>`           | Automatic fallback model when primary is overloaded (print mode only).                                                  | Optional. Resilience for rate-limited deployments.                                      |
| `--effort <level>`                   | Reasoning effort: `low`, `medium`, `high`, `max`.                                                                       | Optional. Controls depth of reasoning.                                                  |
| `--allowedTools <tools>`             | Space-separated list of pre-approved tools. Supports prefix matching: `"Bash(git diff *)"`.                             | Optional. For tool restriction.                                                         |
| `--disallowedTools <tools>`          | Tools to remove from model context entirely.                                                                            | Optional.                                                                               |
| `--tools <tools>`                    | Restrict available built-in tools. `""` = none, `"default"` = all.                                                      | Optional. Limits the tool palette.                                                      |
| `--append-system-prompt <text>`      | Append text to the system prompt. Preserves built-in capabilities.                                                      | Optional. For adapter-injected instructions.                                            |
| `--system-prompt <text>`             | Replace entire default system prompt.                                                                                   | Optional. **Caution:** removes built-in tool instructions.                              |
| `--system-prompt-file <path>`        | Replace system prompt from file.                                                                                        | Optional.                                                                               |
| `--append-system-prompt-file <path>` | Append to system prompt from file.                                                                                      | Optional.                                                                               |
| `--json-schema <schema>`             | JSON Schema for structured output validation (print mode only).                                                         | Optional. Forces output to conform to a schema.                                         |
| `--mcp-config <path>`                | Path to MCP server configuration JSON.                                                                                  | Optional. For tool extensions (e.g., `tracker_api`).                                    |
| `--strict-mcp-config`                | Only use MCP servers from `--mcp-config` (ignore workspace MCP configs).                                                | Optional. For controlled MCP environments.                                              |
| `--add-dir <dirs>`                   | Additional directories for tool access beyond the cwd.                                                                  | Optional. Multi-repo workflows.                                                         |
| `--debug [filter]`                   | Enable debug logging with optional category filter (e.g., `"api,hooks"`).                                               | Optional. For troubleshooting.                                                          |
| `--include-partial-messages`         | Include partial message deltas in stream-json output.                                                                   | Optional. Provides token-by-token streaming for text deltas.                            |
| `--agents <json>`                    | Define custom subagents via JSON.                                                                                       | Optional. Advanced multi-agent patterns.                                                |
| `--agent <name>`                     | Use a specific named agent for the session.                                                                             | Optional. Agent routing.                                                                |

### Session management flags

| Flag                           | Description                                                         | Adapter usage                                                          |
| ------------------------------ | ------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `--continue` / `-c`            | Resume the most recent conversation in the working directory.       | Used for continuation turns (turn 2+).                                 |
| `--resume <session_id>` / `-r` | Resume a specific conversation by session ID.                       | Used for continuation turns when explicit session targeting is needed. |
| `--session-id <uuid>`          | Use a specific UUID for the session (instead of auto-generated).    | Optional. For deterministic session ID assignment.                     |
| `--fork-session`               | When resuming, create a new session ID instead of reusing original. | Optional. For branching conversations.                                 |
| `--no-session-persistence`     | Don't save session to disk (print mode only).                       | Optional. For ephemeral runs that don't need history.                  |
| `--name <name>` / `-n`         | Display name for the session.                                       | Optional. For human-readable session identification.                   |

### Permission flags

| Flag                                   | Description                                                                                    | Adapter usage                                                                                                                                 |
| -------------------------------------- | ---------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `--dangerously-skip-permissions`       | Bypass all permission prompts. No user confirmation required.                                  | **(required for headless)** Without this, the process may hang waiting for interactive approval. Must only be used in sandboxed environments. |
| `--permission-mode <mode>`             | Set permission mode: `default`, `acceptEdits`, `dontAsk`, `bypassPermissions`, `plan`, `auto`. | Alternative to `--dangerously-skip-permissions`. `bypassPermissions` is equivalent.                                                           |
| `--permission-prompt-tool <tool>`      | MCP tool to handle permission prompts programmatically in non-interactive mode.                | Optional. Enables programmatic approval decisions instead of blanket bypass.                                                                  |
| `--allow-dangerously-skip-permissions` | Enable bypassing as an option without activating it.                                           | Optional. Pre-authorization for sandboxed environments.                                                                                       |

> **Security warning:** `--dangerously-skip-permissions` allows arbitrary command execution.
> Per Anthropic's guidance, this should only be used in sandboxed environments without internet
> access, inside an externally enforced sandbox (for example a locked-down container or VM with
> restricted filesystem and network access). Sortie's workspace isolation and hook system operate
> inside that sandbox as additional defense-in-depth, but do not replace external isolation.

### Input flags

| Flag                      | Description                                                              | Adapter usage                                                        |
| ------------------------- | ------------------------------------------------------------------------ | -------------------------------------------------------------------- |
| `--input-format <format>` | Input format (print mode only): `text` (default), `stream-json`.         | Optional. `stream-json` enables real-time streaming input via stdin. |
| `--replay-user-messages`  | Re-emit user messages from stdin on stdout (requires `stream-json` I/O). | Optional. For bidirectional streaming protocols.                     |

### Config/settings flags

| Flag                          | Description                                                  | Adapter usage                                      |
| ----------------------------- | ------------------------------------------------------------ | -------------------------------------------------- |
| `--setting-sources <sources>` | Comma-separated setting sources: `user`, `project`, `local`. | Optional. Control which settings files are loaded. |
| `--settings <file-or-json>`   | Additional settings JSON file or inline JSON.                | Optional. Inject adapter-specific settings.        |

---

## Subprocess Invocation

Per architecture Section 10.7, the adapter launches:

```
sh -c '<agent.command> -p "$1" --output-format stream-json --verbose --dangerously-skip-permissions ${2:+--resume "$2"}' -- "$prompt" "$session_id"
```

> **Shell safety:** The prompt must not be interpolated directly into the `sh -c` string.
> Pass it as a positional parameter (`$1`) to avoid injection via shell metacharacters
> in user-controlled issue content.

### Process settings

| Setting           | Value                                               | Rationale                                                |
| ----------------- | --------------------------------------------------- | -------------------------------------------------------- |
| Working directory | Workspace path (`StartSessionParams.WorkspacePath`) | Agent must operate in the issue workspace.               |
| Stdout            | Pipe (read by adapter)                              | `stream-json` output parsed line by line.                |
| Stderr            | Pipe (read by adapter, logged)                      | Diagnostic output, not structured.                       |
| Environment       | Inherited from Sortie process                       | `ANTHROPIC_API_KEY` and other auth vars must be present. |
| Max line size     | 10 MB                                               | Safe buffering per architecture doc recommendation.      |

### First turn vs. continuation turns

| Turn                   | Invocation                                                                                                                     |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| Turn 1 (new session)   | `claude -p "<full_prompt>" --output-format stream-json --verbose --dangerously-skip-permissions`                               |
| Turn 2+ (continuation) | `claude -p "<continuation_prompt>" --output-format stream-json --verbose --dangerously-skip-permissions --resume <session_id>` |

The `--resume <session_id>` flag continues the conversation in the same session, preserving
the full message history from prior turns. The `session_id` is captured from the JSON output
of the first turn.

**Alternative:** `--continue` resumes the most recent conversation in the cwd. Since Sortie
controls the workspace directory per issue, `--continue` would also work. However,
`--resume <session_id>` is more explicit and avoids ambiguity if multiple sessions exist
in the same workspace.

**Deterministic session IDs:** The `--session-id <uuid>` flag allows the adapter to assign a
known UUID before the first turn, eliminating the need to parse the session ID from output.
This simplifies the turn-1 → turn-2 handoff.

**Ephemeral runs:** `--no-session-persistence` avoids writing session history to disk,
reducing I/O overhead for fire-and-forget runs that don't need continuation.

---

## Output Format: `stream-json`

With `--output-format stream-json`, Claude Code writes one JSON object per line to stdout
(newline-delimited JSON / JSONL). Each line is independently parseable.

### Message types

The stream produces messages conforming to a union type:

`UserMessage | AssistantMessage | SystemMessage | ResultMessage | StreamEvent | TaskMessage`

### Event categories

| `type` field   | Description                                               | Adapter mapping                        |
| -------------- | --------------------------------------------------------- | -------------------------------------- |
| `system`       | System/init messages (session start, retries, compaction) | `session_started` or `notification`    |
| `assistant`    | Complete assistant message (text, tool calls)             | `notification` / `tool_use` tracking   |
| `user`         | User-role message carrying tool execution results         | `tool_result` (via content blocks)     |
| `result`       | Final result message when the turn/session completes      | `turn_completed` or `turn_failed`      |
| `stream_event` | Streaming delta (with `--include-partial-messages`)       | Ignored or used for stall detection    |

> **Note (v2.1.x change):** Earlier Claude Code versions emitted `tool_use` and `tool_result`
> as separate top-level event types *or* as content blocks within `assistant` messages. As of
> v2.1.x, the canonical flow is: `assistant` message with `ToolUseBlock` content → `user`
> message with `ToolResultBlock` content. The adapter handles both the legacy (assistant-only)
> and current (assistant + user) patterns.

### SystemMessage subtypes

| Subtype            | Description                                            | Adapter action                               |
| ------------------ | ------------------------------------------------------ | -------------------------------------------- |
| `init`             | First message. Contains `session_id` and `cwd`.        | Extract `session_id`, emit `session_started` |
| `api_retry`        | API retry in progress.                                 | Log; update stall timer                      |
| `compact_boundary` | Context was compacted (conversation truncated to fit). | Log as `notification`                        |

**Init/system event:**

```json
{
  "type": "system",
  "subtype": "init",
  "session_id": "abc123",
  "cwd": "/path/to/workspace"
}
```

**API retry event:**

```json
{
  "type": "system",
  "subtype": "api_retry",
  "attempt": 1,
  "max_retries": 5,
  "retry_delay_ms": 1000,
  "error_status": 429,
  "error": "rate_limit",
  "uuid": "...",
  "session_id": "..."
}
```

Retry error categories: `authentication_failed`, `billing_error`, `rate_limit`, `invalid_request`,
`server_error`, `max_output_tokens`, `unknown`.

### ResultMessage (turn/session completion)

The result message is always the final line in the stream. It contains comprehensive metadata:

```json
{
  "type": "result",
  "subtype": "success",
  "result": "I've implemented the changes...",
  "session_id": "abc123",
  "is_error": false,
  "total_cost_usd": 0.0234,
  "duration_ms": 45000,
  "duration_api_ms": 38000,
  "num_turns": 3,
  "usage": {
    "input_tokens": 15000,
    "output_tokens": 3200,
    "cache_read_input_tokens": 8000,
    "cache_creation_input_tokens": 2000
  },
  "stop_reason": "end_turn"
}
```

**Result subtypes:**

| `subtype`                             | Meaning                                                              | Adapter mapping  |
| ------------------------------------- | -------------------------------------------------------------------- | ---------------- |
| `success`                             | Turn completed normally.                                             | `turn_completed` |
| `error_max_turns`                     | `--max-turns` limit reached.                                         | `turn_failed`    |
| `error_max_budget_usd`                | `--max-budget-usd` limit reached.                                    | `turn_failed`    |
| `error_during_execution`              | Runtime error during agent execution.                                | `turn_failed`    |
| `error_max_structured_output_retries` | Structured output (`--json-schema`) validation failed after retries. | `turn_failed`    |

**Result event fields:**

| Field               | Type    | Description                                         |
| ------------------- | ------- | --------------------------------------------------- |
| `type`              | string  | Always `"result"`.                                  |
| `subtype`           | string  | Result category (see table above).                  |
| `result`            | string? | Text result (only on `success`).                    |
| `structured_output` | any?    | Parsed JSON (only when `--json-schema` used).       |
| `session_id`        | string  | Session UUID.                                       |
| `is_error`          | boolean | `true` for any error subtype.                       |
| `total_cost_usd`    | number  | Total cost for this invocation.                     |
| `duration_ms`       | number  | Wall-clock duration.                                |
| `duration_api_ms`   | number  | Time spent in API calls only.                       |
| `num_turns`         | integer | Number of internal agentic turns executed.          |
| `usage`             | object  | Token usage breakdown (see below).                  |
| `stop_reason`       | string? | `"end_turn"`, `"max_tokens"`, `"refusal"`, or null. |

**Usage object fields:**

| Field                         | Type    | Description                      |
| ----------------------------- | ------- | -------------------------------- |
| `input_tokens`                | integer | Total input tokens consumed.     |
| `output_tokens`               | integer | Total output tokens generated.   |
| `cache_read_input_tokens`     | integer | Tokens served from prompt cache. |
| `cache_creation_input_tokens` | integer | Tokens written to prompt cache.  |

### Message content blocks

Assistant and user messages contain an array of content blocks:

| Block type        | Key fields                           | Appears in    | Description                       |
| ----------------- | ------------------------------------ | ------------- | --------------------------------- |
| `TextBlock`       | `text`                               | `assistant`   | Text output from the model.       |
| `ThinkingBlock`   | `thinking`, `signature`              | `assistant`   | Extended thinking (when enabled). |
| `ToolUseBlock`    | `id`, `name`, `input`                | `assistant`   | Tool call request.                |
| `ToolResultBlock` | `tool_use_id`, `content`, `is_error` | `user`        | Tool execution result.            |

> **v2.1.x:** `ToolResultBlock` moved from `assistant` messages to `user` messages.
> Legacy streams may still embed `ToolResultBlock` inside `assistant` messages; the
> adapter handles both layouts.

### StreamEvent (partial messages)

Only emitted when `--include-partial-messages` is set. Wraps raw Claude API streaming events:

```json
{
  "type": "stream_event",
  "event": {
    "type": "content_block_delta",
    "delta": { "type": "text_delta", "text": "partial text..." }
  },
  "uuid": "...",
  "session_id": "...",
  "parent_tool_use_id": null
}
```

Claude API streaming event types within `event`:

| Event type            | Description                                                         |
| --------------------- | ------------------------------------------------------------------- |
| `message_start`       | Start of a new message.                                             |
| `content_block_start` | Start of a text or tool_use block.                                  |
| `content_block_delta` | Incremental text (`text_delta`) or tool input (`input_json_delta`). |
| `content_block_stop`  | End of content block.                                               |
| `message_delta`       | Message-level updates (stop_reason, usage).                         |
| `message_stop`        | End of message.                                                     |

A complete message cycle in the stream follows this sequence:

1. `message_start`
2. One or more `content_block_start` / `content_block_delta` / `content_block_stop` sequences
3. `message_delta` (contains stop_reason, usage)
4. `message_stop`
5. `AssistantMessage` (complete message with all content blocks)
6. After all tool execution and final response: `ResultMessage`

### Task messages (background agent tasks)

When Claude Code spawns background tasks (subagents, background bash), these messages appear:

| Message type              | Key fields                            | Description                                                       |
| ------------------------- | ------------------------------------- | ----------------------------------------------------------------- |
| `TaskStartedMessage`      | `task_id`, `description`, `task_type` | Task spawned. Types: `local_bash`, `local_agent`, `remote_agent`. |
| `TaskProgressMessage`     | `task_id`, `usage`, `last_tool_name`  | Periodic progress update.                                         |
| `TaskNotificationMessage` | `task_id`, `status`, `summary`        | Task completed. Status: `completed`, `failed`, `stopped`.         |

### Parsing strategy

The adapter reads stdout line by line. For each line:

1. Parse as JSON. If parsing fails → emit `malformed` event.
2. Check `type` field:
   - `"system"` with `"init"` subtype → extract `session_id`, emit `session_started`.
   - `"system"` with `"api_retry"` subtype → log retry info, update stall timer.
   - `"system"` with `"compact_boundary"` subtype → log context compaction.
   - `"result"` → extract all fields. Check `subtype` and `is_error`:
     - `subtype == "success"` and `is_error == false` → emit `turn_completed`.
     - Any other subtype or `is_error == true` → emit `turn_failed`.
     - Extract `usage` for `token_usage` event.
   - `"assistant"` → emit `notification` or `other_message` with content summary.
     Inspect content blocks for `ToolUseBlock` entries (populate in-flight map).
     Legacy streams may also contain `ToolResultBlock` here.
   - `"user"` → inspect content blocks for `ToolResultBlock` entries. Correlate
     each `tool_use_id` with the in-flight map to emit `tool_result` events with
     the originating tool name and execution duration.
   - `"stream_event"` → update stall detection timer. Optionally emit `notification`
     for text deltas.
3. Any event with recognizable token/cost data → emit `token_usage` event.

### Token usage extraction

The result message provides direct token counts in the `usage` field. The adapter normalizes
these into:

```
TokenUsage{
    InputTokens:  usage.input_tokens,
    OutputTokens: usage.output_tokens,
    TotalTokens:  usage.input_tokens + usage.output_tokens,
}
```

Additional available data:

| Source         | Fields                                                                                    |
| -------------- | ----------------------------------------------------------------------------------------- |
| Result event   | `total_cost_usd`, `duration_ms`, `duration_api_ms`, `num_turns`, `usage.*`                |
| Result `usage` | `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens` |

The `cache_read_input_tokens` and `cache_creation_input_tokens` fields are useful for cost
analysis (cached tokens are cheaper) but are not required for the normalized `token_usage` event.

---

## Session Lifecycle Mapping

Architecture Section 10.2 defines the session lifecycle. Here is how Claude Code maps to it:

### `StartSession`

Architecture Sections 10.1 and 10.2 define `StartSession` as the operation that "launches or
connects" to the agent. For Claude Code, the _session_ is disk-persisted and identified by
`--session-id`, while the OS subprocess is short-lived and created per turn. This adapter
treats `StartSession` as establishing and validating the logical Claude Code session, while
deferring creation of the Node.js subprocess until `RunTurn`.

1. Record the workspace path and configuration.
2. Perform preflight validation:
   - Resolve and normalize the workspace path.
   - Enforce workspace path containment rules.
   - Validate that the Claude Code CLI command is resolvable on `$PATH`.
3. Optionally assign a deterministic Claude Code session ID (for example a UUID) that will be
   passed via `--session-id <uuid>` on the first `RunTurn`. This avoids needing to parse the
   session ID from output before the second turn and makes retries idempotent.
4. Initialize and return a `Session`:
   - `ID`: the chosen session ID if pre-assigned, or empty (to be populated after the first
     turn if Claude Code allocates it).
   - `Internal`: adapter-internal state (workspace path, config snapshot, chosen session ID).
     No OS subprocess is spawned at this point; that happens in `RunTurn` while resuming this
     logical session.

### `RunTurn`

1. Build the CLI command:
   - Turn 1: `claude -p "<prompt>" --output-format stream-json --verbose --dangerously-skip-permissions [--session-id <uuid>]`
   - Turn 2+: append `--resume <session_id>` to continue the conversation.
2. Launch subprocess with `sh -c <command>`, cwd = workspace path.
3. Read stdout line by line, parse each JSON event, and deliver to `OnEvent` callback.
4. On each event, update the stall detection timer.
5. When the process exits:
   - **Exit code 0:** Parse the final `result` event. Check `subtype`:
     - `"success"` → return `TurnResult{ExitReason: turn_completed}`.
     - `"error_max_turns"` or `"error_max_budget_usd"` → return `TurnResult{ExitReason: turn_failed}`.
     - `"error_during_execution"` → return `TurnResult{ExitReason: turn_failed}`.
   - **Exit code 1:** General error → return `TurnResult{ExitReason: turn_failed}` with error details.
   - **Exit code non-zero (other):** Return `TurnResult{ExitReason: turn_failed}`.
   - **Killed by signal (timeout/cancellation):** Return `TurnResult{ExitReason: turn_cancelled}`.
6. Extract `session_id` from the init or result event and store it in the `Session.Internal`
   state for use in subsequent `--resume` invocations.

### `StopSession`

1. If a subprocess is running, send `SIGTERM`.
2. Wait briefly (e.g., 5 seconds) for clean exit.
3. If still running, send `SIGKILL`.
4. Clean up file descriptors and goroutines.

### `EventStream`

Return `nil`. The Claude Code adapter uses the synchronous `OnEvent` callback model, not
the async channel model.

---

## Turn Model

Claude Code has its own internal concept of "turns" (agentic loops within a single CLI
invocation). The `--max-turns` flag controls how many internal turns Claude Code executes
before returning.

**Default behavior when `--max-turns` is omitted:** Claude Code runs until the model
decides it is done (the agent loop completes naturally). There is no hardcoded internal
turn limit — the agent continues executing tool calls and producing responses until it
emits `end_turn`. In practice this means the only bounds are the cost budget
(`--max-budget-usd`) and the adapter's `turn_timeout_ms` deadline in Sortie.

> **Note:** `--max-turns` is documented in the GitHub Actions `claude_args` reference but
> does not appear in `claude --help` as of CLI v2.1.76. It may be handled via the SDK
> layer or settings rather than a direct CLI flag. Verify availability before relying on it
> in the adapter; if unavailable, rely on `--max-budget-usd` and `turn_timeout_ms` as the
> primary safety bounds.

**Sortie's turn model is distinct from Claude Code's internal turns.** Sortie's "turn" is
one CLI invocation (one `RunTurn` call). Within that invocation, Claude Code may execute
multiple internal agentic loops.

Two strategies:

### Strategy A: Single Sortie turn = single Claude Code invocation (recommended)

- Omit `--max-turns` (agent runs until done) or set it explicitly if available.
- Claude Code runs its full agentic loop within one invocation.
- Sortie calls `RunTurn` once per Sortie turn. Claude Code decides when it's done.
- After each Sortie turn, the orchestrator re-checks tracker state and decides whether to
  continue.
- The result event's `num_turns` field reveals how many internal turns Claude Code executed.
- Use `--max-budget-usd` as a cost safety backstop alongside `turn_timeout_ms`.

### Strategy B: One-turn-per-invocation (fine-grained control)

- Set `--max-turns 1` on each invocation.
- Each `RunTurn` call runs exactly one Claude Code internal turn.
- Sortie has maximum control over the loop but incurs subprocess launch overhead per turn
  and may interrupt Claude Code's multi-step plans.

**Recommendation:** Strategy A. Let Claude Code run its agentic loop to completion within
each Sortie turn. Sortie's `agent.turn_timeout_ms` provides the safety backstop. This
minimizes subprocess overhead and allows Claude Code's planner to execute multi-step
operations atomically.

---

## Timeout Enforcement

| Timeout                  | Source      | Enforcement                                                                                                                                                                                         |
| ------------------------ | ----------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.turn_timeout_ms`  | WORKFLOW.md | Adapter sets a deadline on the subprocess. On expiry, send SIGTERM → wait → SIGKILL. Map to `turn_cancelled`.                                                                                       |
| `agent.read_timeout_ms`  | WORKFLOW.md | Not directly applicable to subprocess model (no request/response cycle during the turn). Could apply to initial subprocess startup: if no output within `read_timeout_ms`, consider startup failed. |
| `agent.stall_timeout_ms` | WORKFLOW.md | Enforced by the **orchestrator**, not the adapter. The orchestrator monitors `last_agent_timestamp` from events. If the gap exceeds `stall_timeout_ms`, the orchestrator calls `StopSession`.       |

### Context cancellation

The adapter must respect `context.Context` cancellation:

- `RunTurn` receives a context. If the context is cancelled (e.g., due to tracker
  reconciliation finding the issue is terminal), the adapter must kill the subprocess
  promptly.
- Use `cmd.Process.Signal(syscall.SIGTERM)` followed by a grace period, then
  `cmd.Process.Kill()`.

---

## Permission and Approval Policy

Per architecture Section 10.4, Sortie adopts a high-trust posture:

| Policy                    | Implementation                                                                                                                                                                                                                                                     |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Auto-approve commands     | `--dangerously-skip-permissions` bypasses all prompts.                                                                                                                                                                                                             |
| Auto-approve file changes | Same flag covers file edits.                                                                                                                                                                                                                                       |
| User input required       | The `-p` flag runs non-interactively. If Claude Code somehow requests user input (which should not happen in headless mode with `--dangerously-skip-permissions`), the process would stall. The adapter's turn timeout catches this. Map to `turn_input_required`. |
| Unsupported tool calls    | Claude Code handles tool routing internally. Unknown MCP tools return failure and the session continues.                                                                                                                                                           |

### Permission modes

| Mode                | Behavior                                                                     |
| ------------------- | ---------------------------------------------------------------------------- |
| `default`           | Tools not in `allowedTools` trigger approval prompt; no callback means deny. |
| `acceptEdits`       | Auto-approves file edits (Read, Edit, Write); others follow default rules.   |
| `plan`              | No tool execution; Claude produces a plan only.                              |
| `dontAsk`           | Never prompts. Pre-approved tools run, everything else denied silently.      |
| `bypassPermissions` | All tools run without asking. Cannot run as root on Unix.                    |
| `auto`              | Automatic permission handling.                                               |

### Programmatic permission handling: `--permission-prompt-tool`

Instead of blanket `--dangerously-skip-permissions`, the adapter could use
`--permission-prompt-tool <mcp_tool_name>` to delegate permission decisions to an MCP tool.
This enables fine-grained, programmatic approval without human interaction. The MCP tool
receives the permission request and returns allow/deny.

This is relevant for deployments that want selective approval rather than blanket bypass.

### Alternative: `--allowedTools` for restricted operation

Instead of `--dangerously-skip-permissions`, the adapter could use `--allowedTools` to
pre-approve a specific set of tools:

```bash
claude -p "..." --allowedTools "Edit" "Read" "Bash(git *)" "Bash(make *)" "Grep" "Glob"
```

This provides finer-grained control but requires curating the tool list per use case.
The adapter could expose this via the `claude-code.allowed_tools` pass-through config.

---

## Error Detection and Mapping

### Process exit codes

| Exit Code                | Meaning                                              | Adapter mapping                                   |
| ------------------------ | ---------------------------------------------------- | ------------------------------------------------- |
| 0                        | Success (check `subtype`/`is_error` in result event). If no `result` event was received and cumulative output tokens are zero, treated as `turn_failed` (no-output safety heuristic). | `turn_completed` or `turn_failed` based on result / no-output heuristic |
| 1                        | General error                                        | `turn_failed`                                     |
| Non-zero (other)         | Unexpected failure                                   | `turn_failed`                                     |
| 127                      | `claude` binary not found                            | `agent_not_found`                                 |
| Signal (SIGTERM/SIGKILL) | Killed by adapter or OS                              | `turn_cancelled`                                  |

### Result event `subtype` and `is_error` fields

Even with exit code 0, the result event may have `is_error: true` or a non-success `subtype`.
The adapter must check both the exit code and the result event fields.

| `subtype`                             | `is_error` | Adapter mapping  | Description                                   |
| ------------------------------------- | ---------- | ---------------- | --------------------------------------------- |
| `success`                             | `false`    | `turn_completed` | Normal completion.                            |
| `error_max_turns`                     | `true`     | `turn_failed`    | `--max-turns` limit reached.                  |
| `error_max_budget_usd`                | `true`     | `turn_failed`    | `--max-budget-usd` limit reached.             |
| `error_during_execution`              | `true`     | `turn_failed`    | Runtime error (API failure, tool crash, etc). |
| `error_max_structured_output_retries` | `true`     | `turn_failed`    | `--json-schema` validation exhausted retries. |

### Error category mapping (per architecture Section 10.5)

| Condition                                 | Error category          |
| ----------------------------------------- | ----------------------- |
| `claude` binary not found on `$PATH`      | `agent_not_found`       |
| Workspace path invalid or not a directory | `invalid_workspace_cwd` |
| No output within `read_timeout_ms`        | `response_timeout`      |
| Turn exceeds `turn_timeout_ms`            | `turn_timeout`          |
| Process exits non-zero                    | `port_exit`             |
| Result event `is_error: true`             | `turn_failed`           |
| Process killed by signal                  | `turn_cancelled`        |
| Agent requests user input                 | `turn_input_required`   |

### API retry errors

The `api_retry` system event provides visibility into transient failures that Claude Code
handles internally:

| `error` value           | Description                                              |
| ----------------------- | -------------------------------------------------------- |
| `rate_limit`            | API rate limit (429). Claude Code retries automatically. |
| `server_error`          | API server error (5xx). Retried.                         |
| `authentication_failed` | Invalid API key. Fatal after retries.                    |
| `billing_error`         | Billing/quota issue. Fatal.                              |
| `invalid_request`       | Malformed request. Fatal.                                |
| `max_output_tokens`     | Output truncated. May retry with continuation.           |
| `unknown`               | Unclassified error.                                      |

The adapter should log these for debugging but does not need to act on them — Claude Code
handles retries internally. If retries are exhausted, the process exits with code 1.

---

## Session Storage

Claude Code persists sessions to disk at:

```
~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
```

Where `<encoded-cwd>` is the absolute workspace path with non-alphanumeric characters
replaced by `-`.

This is relevant for:

- **Continuation:** `--resume <session_id>` reads from this path.
- **Cleanup:** Sortie may want to clean up session files when removing workspaces.
- **Disk usage:** Long sessions can accumulate significant JSONL files.
- **Ephemeral mode:** `--no-session-persistence` skips writing entirely.

---

## Hooks Integration

Claude Code supports lifecycle hooks (`.claude/hooks.json`) that can intercept tool calls,
validate actions, and inject context. Sortie does **not** manage Claude Code's hook system
directly — these are workspace-level configurations that the coding agent or `after_create`
hook can set up.

However, the adapter should be aware that:

- Hooks can block tool calls (exit code 2 from hook → tool is denied).
- Hooks can add latency to tool execution (timeout per hook: up to 60s default).
- The `Stop` hook can prevent session completion if it returns exit code 2 (the session
  continues rather than stopping).

The adapter does not need to parse or manage hooks, but hook-induced delays should be
accounted for in timeout calculations.

---

## MCP Server Configuration

The `--mcp-config <path>` flag points Claude Code to a JSON file that declares MCP servers
for the session. The file uses the standard MCP configuration format with a top-level
`mcpServers` object. Each key is a server name; each value declares the transport type,
command, arguments, and optional environment variables.

### File format

```json
{
  "mcpServers": {
    "my-tool-server": {
      "type": "stdio",
      "command": "/usr/local/bin/my-tool",
      "args": ["serve"],
      "env": {}
    }
  }
}
```

| Field     | Type     | Required | Description                                                        |
| --------- | -------- | -------- | ------------------------------------------------------------------ |
| `type`    | string   | No       | Transport type: `"stdio"` (default if omitted) or `"http"`.        |
| `command` | string   | Yes      | Executable to launch for stdio servers.                            |
| `args`    | string[] | No       | Arguments passed to the command.                                   |
| `env`     | object   | No       | Environment variables set for the server process. Keys are         |
|           |          |          | variable names; values are strings. Used for non-secret config.    |

Claude Code reads the file at agent startup and spawns each declared server as a child
process with the specified command and args. The server inherits the agent's environment,
merged with any variables in the `env` field.

### `--strict-mcp-config`

When passed alongside `--mcp-config`, Claude Code ignores MCP server declarations from
workspace-level `.mcp.json` files and only uses servers from the specified config file.
This prevents workspace-controlled MCP servers from interfering with Sortie-managed tools.

### Sortie adapter usage

The worker writes a temporary `.sortie/mcp.json` to the workspace directory containing the
`sortie mcp-server` stdio declaration. If the operator also specifies `claude-code.mcp_config`
in WORKFLOW.md, the worker merges both server sets into a single file — Claude Code accepts
only one `--mcp-config` path. A name collision on the `sortie-tools` key fails the attempt.
Credential values are never written to this file — they reach the MCP server through
inherited environment variables. See ADR-0009 for the full merge algorithm.

---

## OpenTelemetry Integration

Claude Code supports OpenTelemetry for monitoring:

```bash
export CLAUDE_CODE_ENABLE_TELEMETRY=1
export OTEL_METRICS_EXPORTER=otlp
export OTEL_LOGS_EXPORTER=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
```

Available metrics and events:

| Name                        | Type    | Description                                                                                  |
| --------------------------- | ------- | -------------------------------------------------------------------------------------------- |
| `claude_code.session`       | Counter | Incremented at session start                                                                 |
| `claude_code.lines_of_code` | Counter | Lines added/removed (attr: `type`)                                                           |
| `claude_code.cost.usage`    | Counter | Cost per API request (attr: `model`)                                                         |
| `claude_code.tokens`        | Counter | Tokens per API request (attr: `type`, `model`)                                               |
| `claude_code.api_request`   | Event   | Per-request: `input_tokens`, `output_tokens`, `cache_read_tokens`, `cost_usd`, `duration_ms` |
| `claude_code.tool_result`   | Event   | Per-tool: `tool_name`, `success`, `duration_ms`                                              |

Sortie may optionally enable telemetry in the subprocess environment for external monitoring,
but the adapter's primary event source is the `stream-json` stdout output, not OTel.

---

## Adapter-Specific Pass-Through Config

Per architecture Section 5.3.5, the adapter reads pass-through config from the `claude-code`
sub-object in WORKFLOW.md:

| Config key                        | Type    | Description                                                                                                                                                 |
| --------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `claude-code.permission_mode`     | string  | Permission mode: `default`, `acceptEdits`, `dontAsk`, `bypassPermissions`, `plan`, `auto`. Overrides the default `--dangerously-skip-permissions` behavior. |
| `claude-code.model`               | string  | Model override (e.g., `claude-sonnet-4-6`). Maps to `--model`.                                                                                              |
| `claude-code.fallback_model`      | string  | Fallback model for rate-limit resilience. Maps to `--fallback-model`.                                                                                       |
| `claude-code.max_turns`           | integer | Claude Code internal max turns per invocation. Maps to `--max-turns`.                                                                                       |
| `claude-code.max_budget_usd`      | number  | Cost cap per invocation. Maps to `--max-budget-usd`.                                                                                                        |
| `claude-code.effort`              | string  | Reasoning effort: `low`, `medium`, `high`, `max`. Maps to `--effort`.                                                                                       |
| `claude-code.allowed_tools`       | string  | Space-separated tool list. Maps to `--allowedTools`.                                                                                                        |
| `claude-code.disallowed_tools`    | string  | Space-separated denied tool list. Maps to `--disallowedTools`.                                                                                              |
| `claude-code.system_prompt`       | string  | Additional system prompt text. Maps to `--append-system-prompt`.                                                                                            |
| `claude-code.mcp_config`          | string  | Path to MCP config JSON. Maps to `--mcp-config`.                                                                                                            |
| `claude-code.session_persistence` | boolean | If `false`, passes `--no-session-persistence`. Default: `true`.                                                                                             |

---

## Example Adapter Invocation

### Turn 1 (new session)

```bash
prompt='You are working on issue PROJ-123. Fix the authentication bug described below.

## Issue: Authentication fails on expired tokens

The login endpoint returns 500 when the JWT token is expired instead of 401...

## Instructions
1. Read the relevant code files
2. Implement the fix
3. Run tests to verify'

sh -c 'claude -p "$1" \
  --output-format stream-json \
  --verbose \
  --dangerously-skip-permissions' -- "$prompt"
```

Working directory: `/var/sortie/workspaces/PROJ-123`

### Turn 2 (continuation)

```bash
prompt='Continue working on the issue. The previous turn made progress but tests are still failing. Focus on fixing the remaining test failures.'
session_id='abc123-session-id'

sh -c 'claude -p "$1" \
  --output-format stream-json \
  --verbose \
  --dangerously-skip-permissions \
  --resume "$2"' -- "$prompt" "$session_id"
```

Working directory: `/var/sortie/workspaces/PROJ-123`

### With cost cap and model override

```bash
sh -c 'claude -p "$1" \
  --output-format stream-json \
  --verbose \
  --dangerously-skip-permissions \
  --model claude-sonnet-4-6 \
  --fallback-model claude-haiku-4-5-20251001 \
  --max-budget-usd 5.00 \
  --max-turns 50' -- "$prompt"
```

---

## SDK Alternative (Reference Only)

Anthropic provides TypeScript and Python SDKs for programmatic Claude Code usage. Both
internally spawn the Claude Code CLI as a subprocess with `--input-format stream-json
--output-format stream-json --include-partial-messages` and communicate over stdin/stdout.

### TypeScript SDK (`@anthropic-ai/claude-agent-sdk`)

```typescript
import { query } from "@anthropic-ai/claude-agent-sdk";

for await (const message of query({
  prompt: "Find and fix the bug in auth.py",
  options: {
    allowedTools: ["Read", "Edit", "Bash"],
    permissionMode: "acceptEdits",
    cwd: "/path/to/project",
    model: "claude-sonnet-4-6",
    maxTurns: 30,
    maxBudgetUsd: 5.0,
  },
})) {
  if (message.type === "system" && message.subtype === "init") {
    // session_id available
  }
  if (message.type === "result") {
    // message.subtype, message.result, message.total_cost_usd, message.usage
  }
}
```

Key options: `abortController`, `cwd`, `model`, `allowedTools`, `disallowedTools`,
`permissionMode`, `maxTurns`, `maxBudgetUsd`, `mcpServers`, `systemPrompt`,
`includePartialMessages`, `resume`, `continue`, `forkSession`, `sessionId`,
`persistSession`, `hooks`, `canUseTool`, `agents`, `effort`, `env`,
`spawnClaudeCodeProcess`.

### Python SDK (`claude-agent-sdk`)

```python
from claude_agent_sdk import query, ClaudeAgentOptions, ResultMessage

async for message in query(
    prompt="Fix the bug",
    options=ClaudeAgentOptions(
        allowed_tools=["Read", "Edit", "Bash"],
        permission_mode="acceptEdits",
        cwd="/path/to/project",
        max_turns=30,
    ),
):
    if isinstance(message, ResultMessage):
        print(message.result)
```

Multi-turn via `ClaudeSDKClient`:

```python
async with ClaudeSDKClient(options=options) as client:
    await client.query("Analyze the auth module")
    async for message in client.receive_response():
        print(message)
    await client.query("Now refactor it")  # same session
```

Sortie does **not** use the SDKs because the adapter is implemented in Go and the
architecture mandates subprocess-based integration (Section 10.7). The CLI provides a
clean, language-agnostic integration surface. The SDK documentation is useful as a
reference for expected message types and session behavior, as the SDK and CLI share the
same underlying protocol.

---

## Summary: Adapter Implementation Checklist

1. **StartSession:** Store workspace path and config. Optionally pre-assign session ID via
   `--session-id`. Do not launch subprocess yet.
2. **RunTurn (turn 1):** Launch `claude -p <prompt> --output-format stream-json --verbose
--dangerously-skip-permissions [--session-id <uuid>]` with cwd = workspace. Parse JSONL
   stdout. Capture `session_id` from init event.
3. **RunTurn (turn 2+):** Same as turn 1 but append `--resume <session_id>`.
4. **Event parsing:** Map `stream-json` event types to normalized `AgentEvent` types. Handle
   all message types: `system`, `assistant`, `result`, `stream_event`.
5. **Result handling:** Check both `subtype` and `is_error` in result messages. Map subtypes
   to appropriate exit reasons.
6. **Token tracking:** Extract `usage` object from result events. Normalize to
   `{input_tokens, output_tokens, total_tokens}`.
7. **Cost tracking:** Extract `total_cost_usd` and `duration_ms` from result events.
8. **Timeout:** Enforce `turn_timeout_ms` via context deadline + SIGTERM/SIGKILL.
9. **StopSession:** Kill subprocess if running (SIGTERM → grace → SIGKILL).
10. **Error mapping:** Check exit code, result `subtype`, and `is_error` field. Map to
    architecture error categories.
11. **Session continuity:** Use `--resume <session_id>` for multi-turn conversations.
12. **Permissions:** Default to `--dangerously-skip-permissions` for headless operation.
13. **Cost safety:** Optionally set `--max-budget-usd` as a per-invocation cost cap.
14. **API retries:** Log `api_retry` system events for visibility; Claude Code handles
    retries internally.
