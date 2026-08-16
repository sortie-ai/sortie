# Claude Code CLI: adapter research notes

> Claude Code CLI v2.x (npm `@anthropic-ai/claude-code`), researched March 2026.
> Reference for implementing the Claude Code `AgentAdapter`, shipped as `internal/agent/claude`.
>
> Coverage: the flag surface is anchored to `claude --help` on CLI v2.1.76. The `stream-json`
> message shapes, result fields, permission modes, session storage layout, hook behavior,
> OpenTelemetry names, and SDK surface come from Anthropic's Claude Code documentation read in
> March 2026 and have not been re-observed against a later binary. Statements about Sortie's own
> code name the Go symbol that implements them and were verified against the tree. Unsettled
> items are collected under "Open questions"; sources are listed under "Sources".

---

## Overview

Claude Code is an agentic coding tool that runs as a Node.js process in the terminal. It reads
a codebase, executes tools (file edits, shell commands, searches), and produces code changes
autonomously. Sortie treats it as a subprocess: launch it with a prompt, read structured output
from stdout, and terminate it when done. The adapter registers under the agent kind
`claude-code` in the `claude` package `init` function, with `RequiresCommand: true`.

The integration surface is the **CLI in non-interactive ("headless") mode** using the `-p`
(print) flag. Anthropic also provides TypeScript and Python SDKs (`@anthropic-ai/claude-agent-sdk`
and `claude-agent-sdk` respectively), but Sortie's Go adapter uses the CLI subprocess approach per
[architecture Section 10.7](architecture/10-agent-adapter-contract.md#107-local-subprocess-launch-contract) (Local Subprocess Launch Contract).

---

## Installation and prerequisites

Claude Code is an npm package:

```bash
npm install -g @anthropic-ai/claude-code
```

After installation the `claude` binary is available on `$PATH`. The `agent.command` config field
defaults to `claude` (`agentcore.ResolveLaunchTarget(params, "claude")`) and can be overridden to
point to a specific path or wrapper.

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

The adapter does not manage API keys. The Anthropic API key must be present in the environment of
the Sortie process (or passed through via hook env): `agentcore.ForkPerTurnSession.RunTurn` sets
`cmd.Env = os.Environ()`, so the subprocess inherits the parent process environment verbatim.

---

## CLI flags reference

The adapter builds its argument list in `buildArgs`. Flags marked **(always)** are set on every
invocation; the rest are conditional on pass-through config or on the session state, and flags
marked "Not passed" are part of the CLI surface but never appear in a Sortie invocation.

### Core flags

| Flag                                 | Description                                                                                  | Adapter usage                                                                       |
| ------------------------------------ | ---------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| `-p <prompt>`                        | Non-interactive (headless) mode. Passes the prompt and exits when done.                      | **(always)** Carries the rendered turn prompt.                                      |
| `--output-format stream-json`        | Newline-delimited JSON on stdout. Each line is a JSON object.                                | **(always)** The adapter's only event source.                                       |
| `--verbose`                          | Include internal events (tool calls, system messages) in stream output.                      | **(always)** Needed for full event visibility.                                      |
| `--max-turns <N>`                    | Maximum number of agentic turns within a single CLI invocation.                               | Passed when `claude-code.max_turns` is set. Availability is an open question below. |
| `--max-budget-usd <amount>`          | Maximum dollar spend for this invocation (print mode only). Exits with error when exceeded.  | Passed when `claude-code.max_budget_usd` is set.                                     |
| `--model <model>`                    | Override the model (e.g., `claude-sonnet-4-6`, `claude-opus-4-6`).                           | Passed when `claude-code.model` is set.                                              |
| `--fallback-model <model>`           | Automatic fallback model when primary is overloaded (print mode only).                       | Passed when `claude-code.fallback_model` is set.                                     |
| `--effort <level>`                   | Reasoning effort: `low`, `medium`, `high`, `max`.                                             | Passed when `claude-code.effort` is set.                                             |
| `--allowedTools <tools>`             | Space-separated list of pre-approved tools. Supports prefix matching: `"Bash(git diff *)"`.  | Passed when `claude-code.allowed_tools` is set.                                       |
| `--disallowedTools <tools>`          | Tools to remove from model context entirely.                                                  | Passed when `claude-code.disallowed_tools` is set.                                    |
| `--append-system-prompt <text>`      | Append text to the system prompt. Preserves built-in capabilities.                            | Passed when `claude-code.system_prompt` is set.                                       |
| `--mcp-config <path>`                | Path to MCP server configuration JSON.                                                        | Passed with the worker-generated config path; see MCP server configuration.          |
| `--tools <tools>`                    | Restrict available built-in tools. `""` = none, `"default"` = all.                            | Not passed.                                                                          |
| `--system-prompt <text>`             | Replace entire default system prompt. Removes built-in tool instructions.                    | Not passed.                                                                          |
| `--system-prompt-file <path>`        | Replace system prompt from file.                                                              | Not passed.                                                                          |
| `--append-system-prompt-file <path>` | Append to system prompt from file.                                                            | Not passed.                                                                          |
| `--json-schema <schema>`             | JSON Schema for structured output validation (print mode only).                              | Not passed.                                                                          |
| `--strict-mcp-config`                | Restrict MCP servers to those in `--mcp-config`; see MCP server configuration.                | Not passed.                                                                          |
| `--add-dir <dirs>`                   | Additional directories for tool access beyond the cwd.                                        | Not passed.                                                                          |
| `--debug [filter]`                   | Enable debug logging with optional category filter (e.g., `"api,hooks"`).                    | Not passed.                                                                          |
| `--include-partial-messages`         | Include partial message deltas in stream-json output.                                         | Not passed.                                                                          |
| `--agents <json>`                    | Define custom subagents via JSON.                                                             | Not passed.                                                                          |
| `--agent <name>`                     | Use a specific named agent for the session.                                                   | Not passed.                                                                          |

### Session management flags

| Flag                           | Description                                                         | Adapter usage                                                                                  |
| ------------------------------ | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `--session-id <uuid>`          | Use a specific UUID for the session (instead of auto-generated).    | Passed on the first turn of a new session with the UUID minted by `newUUID`.                     |
| `--resume <session_id>` / `-r` | Resume a specific conversation by session ID.                       | Passed on every other turn.                                                                      |
| `--no-session-persistence`     | Don't save session to disk (print mode only).                       | Passed when `claude-code.session_persistence` is `false`.                                        |
| `--continue` / `-c`            | Resume the most recent conversation in the working directory.       | Not passed. `--resume` targets the session explicitly and is unambiguous when several exist. |
| `--fork-session`               | When resuming, create a new session ID instead of reusing original. | Not passed.                                                                                      |
| `--name <name>` / `-n`         | Display name for the session.                                       | Not passed.                                                                                      |

### Permission flags

| Flag                                   | Description                                                                                    | Adapter usage                                                        |
| -------------------------------------- | ---------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `--dangerously-skip-permissions`       | Bypass all permission prompts. No user confirmation required.                                  | Passed when `claude-code.permission_mode` is unset.                  |
| `--permission-mode <mode>`             | Set permission mode: `default`, `acceptEdits`, `dontAsk`, `bypassPermissions`, `plan`, `auto`. | Passed instead when `claude-code.permission_mode` is set.            |
| `--permission-prompt-tool <tool>`      | MCP tool to handle permission prompts programmatically in non-interactive mode.                | Not passed.                                                          |
| `--allow-dangerously-skip-permissions` | Enable bypassing as an option without activating it.                                           | Not passed.                                                          |

### Input flags

| Flag                      | Description                                                              | Adapter usage |
| ------------------------- | ------------------------------------------------------------------------ | ------------- |
| `--input-format <format>` | Input format (print mode only): `text` (default), `stream-json`.         | Not passed.   |
| `--replay-user-messages`  | Re-emit user messages from stdin on stdout (requires `stream-json` I/O). | Not passed.   |

### Config and settings flags

| Flag                          | Description                                                  | Adapter usage |
| ----------------------------- | ------------------------------------------------------------ | ------------- |
| `--setting-sources <sources>` | Comma-separated setting sources: `user`, `project`, `local`. | Not passed.   |
| `--settings <file-or-json>`   | Additional settings JSON file or inline JSON.                | Not passed.   |

---

## Subprocess invocation

`buildArgs` returns an argv slice and `agentcore.ForkPerTurnSession.RunTurn` launches the binary
with `exec.CommandContext`. No shell wraps the invocation, so the prompt reaches the CLI as a
single argv element and shell metacharacters in user-controlled issue content cannot be
interpreted. A first turn of a new session produces:

```
claude -p <prompt> --output-format stream-json --verbose --dangerously-skip-permissions --session-id <uuid>
```

When `StartSessionParams.SSHHost` is set, `agentcore.ResolveLaunchTarget` resolves the local `ssh`
binary instead and `sshutil.BuildSSHArgs` wraps the same argv, the workspace path, and the remote
command for remote execution.

Process-group isolation, the graceful-shutdown sequence, and the 10 MB stdout line ceiling follow
[architecture Section 10.7](architecture/10-agent-adapter-contract.md#107-local-subprocess-launch-contract).

### Process settings

| Setting           | Value                                                            | Rationale                                                                                       |
| ----------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------- |
| Working directory | Workspace path, resolved by `agentcore.ResolveWorkspace`         | Agent must operate in the issue workspace.                                                      |
| Stdout            | Pipe scanned line by line by `bufio.Scanner`                     | `stream-json` output is parsed one line per event.                                              |
| Stderr            | Pipe drained by `procutil.NewStderrCollector`                    | Diagnostic output, not structured. Logged, and re-emitted at warn level on failure paths.       |
| Environment       | `os.Environ()` of the Sortie process                             | `ANTHROPIC_API_KEY` and other auth vars must be present.                                        |
| Max line size     | 10 MB (`stdoutScannerMaxTokenSize`)                              | A longer line ends the scan with `bufio.ErrTooLong` and fails the turn with `port_exit`.        |
| Process group     | `procutil.SetProcessGroup` before start, `procutil.AssignProcess` after | POSIX process group and Windows Job Object, so the whole tree can be signaled and reaped. |

### First turn and continuation turns

| Turn                                                     | Session flag                          |
| -------------------------------------------------------- | ------------------------------------- |
| First turn of a new session                              | `--session-id <uuid>`                 |
| Every later turn, and every turn of a resumed session    | `--resume <session_id>`               |

`StartSession` mints the session UUID with `newUUID` before the first turn, so the adapter never
has to parse an identifier out of the stream before it can continue a conversation. It marks the
session a continuation when `StartSessionParams.ResumeSessionID` is non-empty, in which case even
turn 1 resumes. `--resume` preserves the full message history from prior turns. When the
`system/init` event reports a `session_id`, `ParseLine` adopts the reported value for later turns
and for event correlation.

---

## Output format: `stream-json`

With `--output-format stream-json`, Claude Code writes one JSON object per line to stdout
(newline-delimited JSON / JSONL). Each line is independently parseable.

### Message types

The stream produces messages conforming to a union type:

`UserMessage | AssistantMessage | SystemMessage | ResultMessage | StreamEvent | TaskMessage`

### Event categories

| `type` field   | Description                                               | Adapter mapping                                                                             |
| -------------- | --------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `system`       | System/init messages (session start, retries, compaction) | `session_started` on the `init` subtype, `notification` otherwise                            |
| `assistant`    | Complete assistant message (text, tool calls)             | `token_usage` on a message id's first sighting, tool tracking, and a `notification` summary  |
| `user`         | User-role message carrying tool execution results         | `tool_result` (via content blocks)                                                           |
| `result`       | Final result message when the turn/session completes      | Held as the turn's terminal report and read in `OnFinalize`                                  |
| `stream_event` | Streaming delta (with `--include-partial-messages`)       | Empty `notification`, which only advances the orchestrator's stall clock                     |
| Anything else  | Unrecognized line                                          | `other_message` carrying `type/subtype`                                                      |

`processToolBlocks` scans the content blocks of both `assistant` and `user` events, so a
`tool_result` block is correlated in either position; CLI versions before v2.1 emit it inside the
`assistant` message. Correlation runs through `agentcore.ToolTracker`, which supplies the
originating tool name and the execution duration; an uncorrelated result is reported as tool name
`unknown`. Error text from a failed tool is unwrapped from Claude Code's
`<tool_use_error>` envelope and stripped of ANSI escapes by `stripClaudeMarkup`, then bounded to
2048 bytes by `truncateToolError`, which keeps the first line and the tail.

### System message subtypes

| Subtype            | Description                                            | Adapter action                                                                  |
| ------------------ | ------------------------------------------------------ | --------------------------------------------------------------------------------- |
| `init`             | First message. Contains `session_id` and `cwd`.        | Adopt `session_id`, emit `session_started`, start the API-timing clock          |
| `api_retry`        | API retry in progress.                                 | `notification` formatted by `formatAPIRetry`                                    |
| `compact_boundary` | Context was compacted (conversation truncated to fit). | `notification` carrying `system/compact_boundary`                               |

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

### Result message

The result message is the final line in the stream. It contains comprehensive metadata:

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

**Result subtypes** (their disposition is in Error detection and mapping):

| `subtype`                             | Meaning                                                              |
| ------------------------------------- | -------------------------------------------------------------------- |
| `success`                             | Turn completed normally.                                             |
| `error_max_turns`                     | `--max-turns` limit reached.                                         |
| `error_max_budget_usd`                | `--max-budget-usd` limit reached.                                    |
| `error_during_execution`              | Runtime error during agent execution.                                |
| `error_max_structured_output_retries` | Structured output (`--json-schema`) validation failed after retries. |

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

`rawEvent` decodes `subtype`, `is_error`, `result`, `duration_api_ms`, `usage`, and `modelUsage`
and acts on all of them. It also decodes `total_cost_usd`, `duration_ms`, `num_turns`,
`stop_reason`, and the init event's `cwd`, none of which reach a domain event.
`structured_output` is not decoded.

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

`rawContentBlock` decodes `type`, `text`, `name`, `id`, `tool_use_id`, `is_error`, and `content`.
A `ToolUseBlock` `input` and a `ThinkingBlock` are not decoded. `toolResultText` reads a
`tool_result` block's `content` in both shapes Claude Code emits: a plain JSON string, and an
array of typed content objects from which the first `text` entry is taken.

### Stream events (partial messages)

Only emitted when `--include-partial-messages` is set, which `buildArgs` never passes. Wraps raw
Claude API streaming events:

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

`ParseLine` has no arm for these messages; they fall to the default arm and become
`other_message` events. Their wire-level `type` values are an open question below.

### Parsing strategy

`agentcore.ForkPerTurnSession.RunTurn` scans stdout line by line and hands each line to the
adapter's `ParseLine` hook, which:

1. Unmarshals the line into a `rawEvent` via `parseEvent`. On a JSON error the skeleton emits a
   `malformed` event carrying the raw line truncated to 500 runes, and continues with the next
   line.
2. Switches on `type`:
   - `"system"` with `"init"`: adopt `session_id`, emit `session_started` with the subprocess PID,
     and start the API-timing clock.
   - `"system"` with any other subtype: emit a `notification`.
   - `"assistant"`: record the model, register per-message-id usage, emit `token_usage` on the
     id's first sighting, scan content blocks for `tool_use` and `tool_result`, and emit a
     `notification` summarizing the message (`summarizeAssistant`).
   - `"user"`: scan content blocks for `tool_result` and restart the API-timing clock, because
     the next API call follows tool execution.
   - `"result"`: settle the turn's authoritative usage into the run accumulator and return the
     event to the skeleton as the turn's terminal report.
   - `"stream_event"`: emit an empty `notification`.
   - anything else: emit `other_message` carrying `type/subtype`.

Every line other than `result` returns nil, so the last `result` event seen is what `OnFinalize`
reads. Emitted events carry a UTC timestamp; `orchestrator.HandleAgentEvent` advances the run
entry's `LastAgentTimestamp` from it, which is what the stall detector reads. The adapter keeps no
stall timer of its own.

### Token usage extraction

Assistant events repeat one message id (`message.id`) for every event of the same model
request; the adapter deduplicates by that id and emits at most one `token_usage` event per id.
Streamed usage is a snapshot, not a final count: a later event carrying the same id can report a
larger usage object than an earlier one as the response continues to generate, so
`componentwiseMaxUsage` keeps the componentwise maximum per id.

The terminal `result` event's top-level `usage` field excludes sub-agent activity, while its
`modelUsage` map and `total_cost_usd` include it. `usageFromResult` derives the authoritative
per-turn usage from `modelUsage` when present, summed across every model entry, and falls back to
the top-level `usage` object only when `modelUsage` is absent or empty.

`agentcore.RunUsage` holds the session total: assistant events set the in-flight turn's
provisional contribution (`SetTurnProvisional`), the result event settles the turn
(`AddTurn`), and the snapshot never decreases. The terminal event and the `TurnResult` carry that
run-cumulative snapshot, not a per-turn figure.

`usageFromAssistant` normalizes a usage object into:

```
TokenUsage{
    InputTokens:  usage.input_tokens + usage.cache_read_input_tokens + usage.cache_creation_input_tokens,
    OutputTokens: usage.output_tokens,
    TotalTokens:  InputTokens + OutputTokens,
    CacheReadTokens: usage.cache_read_input_tokens,
}
```

Additional available data:

| Source         | Fields                                                                                    |
| -------------- | ----------------------------------------------------------------------------------------- |
| Result event   | `total_cost_usd`, `duration_ms`, `duration_api_ms`, `num_turns`, `usage.*`, `modelUsage.*` |
| Result `usage` | `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens` |
| Result `modelUsage` (per model) | `inputTokens`, `outputTokens`, `cacheReadInputTokens`, `cacheCreationInputTokens`, `costUSD` |

Of the cost figures, `rawEvent` decodes `total_cost_usd` and does not carry it anywhere;
`rawModelUsage` does not decode `costUSD` at all. The adapter reports no cost.

API timing is reported instead: the clock starts on `system/init` and restarts after each `user`
event, and the elapsed time is attached to the next `token_usage` event as `APIDurationMS`,
clamped to a minimum of 1 ms. When no per-request timing was emitted during the turn, `OnFinalize`
falls back to the result event's `duration_api_ms` so the two are never double-counted.

---

## Session lifecycle mapping

[Architecture Section 10.2](architecture/10-agent-adapter-contract.md#102-session-lifecycle) defines the session lifecycle. Here is how Claude Code maps to it:

### `StartSession`

For Claude Code, the _session_ is disk-persisted and identified by `--session-id`, while the OS
subprocess is short-lived and created per turn. `StartSession` establishes the logical Claude Code
session and defers creation of the Node.js subprocess to `RunTurn`. It:

1. Calls `agentcore.ResolveLaunchTarget`, which resolves the workspace path to absolute form and
   checks that it exists and is a directory (`agentcore.ResolveWorkspace`), then resolves the
   `claude` binary on `$PATH` (`agentcore.ResolveBinary`). Workspace root containment is enforced
   by the workspace manager before `StartSession` runs, not here.
2. Assigns the Claude Code session ID: `StartSessionParams.ResumeSessionID` when resuming,
   otherwise a fresh UUID from `newUUID`.
3. Builds `sessionState` (launch target, session ID, agent config, MCP config path, run usage
   accumulator) and constructs the `agentcore.ForkPerTurnSession` that owns the subprocess
   lifecycle.
4. Returns a `domain.Session` whose `ID` is the chosen session ID and whose `Internal` is the
   `sessionState`. No OS subprocess exists at this point.

### `RunTurn`

`RunTurn` resets the per-turn scan state (message-id usage map, last model, API-timing clock, tool
tracker) and delegates to `agentcore.ForkPerTurnSession.RunTurn`, which:

1. Calls `buildArgs` for the argv, then starts the subprocess with cwd set to the workspace path.
2. Scans stdout line by line, delivering normalized events to the `OnEvent` callback.
3. Drains stderr, waits for the process, and reaps any surviving process-group members.
4. Decides the outcome. Cancellation, a stdout scan error, exit code 127, and death by signal are
   decided by the skeleton; every other exit reaches the adapter's `OnFinalize`, which reports the
   terminal outcome to `agentcore.FinalizeTurn`.

The run accumulator (`agentcore.RunUsage`) is constructed once in `StartSession` and is not reset
between turns, so `TurnResult.Usage` is run-cumulative.

### `StopSession`

`StopSession` delegates to `agentcore.ForkPerTurnSession.Stop`, which sends a platform-appropriate
graceful shutdown signal to the process group (POSIX: `SIGTERM`; Windows: `CTRL_BREAK_EVENT`),
waits up to 5 seconds for the turn to finish its own cleanup, and force-terminates the process
group otherwise (POSIX: `SIGKILL`; Windows: `TerminateJobObject`). With no subprocess running it
returns nil immediately.

### `EventStream`

Returns nil. The Claude Code adapter uses the synchronous `OnEvent` callback model, not the async
channel model.

---

## Turn model

Claude Code has its own internal concept of "turns" (agentic loops within a single CLI
invocation). Sortie's turn is one CLI invocation, one `RunTurn` call; within it Claude Code may
execute many internal agentic loops. The result event's `num_turns` field reports how many it ran.

`buildArgs` omits `--max-turns` unless `claude-code.max_turns` is configured, so by default Claude
Code runs until the model decides it is done. There is no hardcoded internal turn limit: the agent
continues executing tool calls and producing responses until it emits `end_turn`. The bounds that
remain are the cost budget (`claude-code.max_budget_usd`) and orchestrator-side cancellation.
Letting the agentic loop run to completion inside one Sortie turn minimizes subprocess overhead
and keeps multi-step operations from being interrupted mid-plan; after each Sortie turn the
orchestrator re-checks tracker state and decides whether to continue.

---

## Timeout enforcement

| Timeout                  | Source      | Enforcement                                                                                                                                                                                |
| ------------------------ | ----------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.turn_timeout_ms`  | WORKFLOW.md | Reaches the adapter as `domain.AgentConfig.TurnTimeoutMS` and is stored in `sessionState`. Nothing on the Claude path reads it; no per-turn deadline is set on the subprocess.              |
| `agent.read_timeout_ms`  | WORKFLOW.md | Bounds session teardown: `orchestrator.stopSessionBestEffort` gives `StopSession` this many milliseconds on a detached context, defaulting to 10 000 ms.                                    |
| `agent.stall_timeout_ms` | WORKFLOW.md | Enforced by the orchestrator. `orchestrator.reconcileStalled` compares `LastAgentTimestamp` against the threshold and cancels the worker's context, which terminates the turn.              |

### Context cancellation

`RunTurn` receives a context; the skeleton passes it to `exec.CommandContext` and installs a
`cmd.Cancel` that sends `SIGTERM` to the process group, with `cmd.WaitDelay` set to the same
5-second grace period before the force kill. When the context is cancelled (for example because
tracker reconciliation found the issue terminal, or the stall detector fired), the turn returns
`turn_cancelled` with `domain.ErrTurnCancelled` and whatever usage accumulated before the kill.

---

## Permission and approval policy

Per [architecture Section 10.4](architecture/10-agent-adapter-contract.md#104-approval-tools-and-user-input-policy), Sortie adopts a high-trust posture:

| Policy                    | Implementation                                                                                                                                                     |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Auto-approve commands     | `--dangerously-skip-permissions` bypasses all prompts.                                                                                                             |
| Auto-approve file changes | Same flag covers file edits.                                                                                                                                       |
| User input required       | The `-p` flag runs non-interactively, so no prompt is expected. A process that blocks anyway produces no output and is cut off by orchestrator-side cancellation.   |
| Unsupported tool calls    | Claude Code handles tool routing internally. Unknown MCP tools return failure and the session continues.                                                            |

`--dangerously-skip-permissions` allows arbitrary command execution. Per Anthropic's guidance it
should only be used in sandboxed environments without internet access, inside an externally
enforced sandbox (for example a locked-down container or VM with restricted filesystem and network
access). Sortie's workspace isolation and hook system operate inside that sandbox as additional
defense-in-depth, but do not replace external isolation. An operator who wants selective approval
instead of blanket bypass sets `claude-code.permission_mode`, which replaces the flag, or
`claude-code.allowed_tools`, which pre-approves a specific set of tools such as
`"Edit Read Bash(git *) Bash(make *) Grep Glob"`.

### Permission modes

| Mode                | Behavior                                                                     |
| ------------------- | ---------------------------------------------------------------------------- |
| `default`           | Tools not in `allowedTools` trigger approval prompt; no callback means deny. |
| `acceptEdits`       | Auto-approves file edits (Read, Edit, Write); others follow default rules.   |
| `plan`              | No tool execution; Claude produces a plan only.                              |
| `dontAsk`           | Never prompts. Pre-approved tools run, everything else denied silently.      |
| `bypassPermissions` | All tools run without asking. Cannot run as root on Unix.                    |
| `auto`              | Automatic permission handling.                                               |

---

## Error detection and mapping

### Process exit and turn disposition

`agentcore.DecideTurn` is the shared decision table; the Claude adapter feeds it evidence and does
not decide the disposition itself. The outcome column gives the emitted event and, where one is
returned, the `domain.AgentError` kind.

| Condition                                                          | Decided by                                        | Outcome                                    |
| ------------------------------------------------------------------ | --------------------------------------------------- | -------------------------------------------- |
| Context cancelled at any point                                     | `agentcore.ForkPerTurnSession.RunTurn`            | `turn_cancelled` / `turn_cancelled`        |
| Stdout line exceeds 10 MB, or the pipe read fails                  | `agentcore.ForkPerTurnSession.RunTurn`            | `turn_failed` / `port_exit`                |
| Exit code 127                                                      | `agentcore.ForkPerTurnSession.RunTurn`            | `turn_failed` / `agent_not_found`          |
| Killed by signal                                                   | `agentcore.ForkPerTurnSession.RunTurn`            | `turn_cancelled` / `turn_cancelled`        |
| Result event with `subtype: "success"` and `is_error: false`       | `OnFinalize` reports `TerminalSuccess`            | `turn_completed`                            |
| Result event with any other subtype, or `is_error: true`           | `OnFinalize` reports `TerminalFailure`            | `turn_failed` / `turn_failed`              |
| No result event, non-zero exit                                     | `agentcore.DecideTurn` row `RowNonZeroExit`       | `turn_failed` / `port_exit`                |
| No result event, exit 0, no assistant output tokens this turn      | `agentcore.DecideTurn` row `RowZeroWork`          | `turn_failed` / `turn_failed`              |
| No result event, exit 0, assistant output tokens present           | `agentcore.DecideTurn` row `RowWorkPresent`       | `turn_completed`                            |

The success message is the result event's `result` text truncated to 500 runes; the failure
message is the result event's `subtype`. Work evidence is this turn's own summed assistant output
tokens, not the run-cumulative figure, which is non-zero on any second turn.

Workspace and binary resolution fail earlier, in `StartSession`: an unusable workspace path yields
`invalid_workspace_cwd` and an unresolvable command yields `agent_not_found`
(`agentcore.ResolveWorkspace`, `agentcore.ResolveBinary`).

The Claude path never produces `response_timeout`, `turn_timeout`, or `turn_input_required`: it
enforces no adapter-side deadline, and `-p` admits no interactive prompt.

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

The adapter emits these as notifications and takes no other action, because Claude Code handles
the retries. If retries are exhausted, the process exits with code 1.

---

## Session storage

Claude Code persists sessions to disk at:

```
~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
```

Where `<encoded-cwd>` is the absolute workspace path with non-alphanumeric characters
replaced by `-`.

This is relevant for:

- **Continuation:** `--resume <session_id>` reads from this path.
- **Disk usage:** long sessions accumulate large JSONL files, and they live under `~/.claude`,
  outside the workspace, so removing a workspace does not remove them.
- **Ephemeral mode:** `--no-session-persistence` skips writing entirely.

---

## Hooks integration

Claude Code supports lifecycle hooks (`.claude/hooks.json`) that can intercept tool calls,
validate actions, and inject context. Sortie does not manage Claude Code's hook system: these are
workspace-level configurations that the coding agent or the `after_create` hook can set up. The
adapter neither parses nor manages them, but their behavior shows up in turn timing and outcomes:

- Hooks can block tool calls (exit code 2 from hook, tool is denied).
- Hooks can add latency to tool execution (timeout per hook: up to 60s default), which counts
  against the stall threshold.
- The `Stop` hook can prevent session completion if it returns exit code 2 (the session
  continues rather than stopping).

---

## MCP server configuration

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
|           |          |          | variable names; values are strings.                                |

Claude Code reads the file at agent startup and spawns each declared server as a child
process with the specified command and args. The server inherits the agent's environment,
merged with any variables in the `env` field.

When `--strict-mcp-config` is passed alongside `--mcp-config`, Claude Code ignores MCP server
declarations from workspace-level `.mcp.json` files and only uses servers from the specified
config file.

### Sortie adapter usage

`orchestrator.GenerateMCPConfig` writes `<workspace>/.sortie/mcp.json` before the session starts,
declaring the `sortie-tools` stdio server that runs the Sortie binary as `mcp-server --workflow
<path>`. The file is written to a temporary path and renamed into place, and `.sortie/.gitignore`
containing `*` is rewritten on every call so the directory stays out of git. If the operator also
specifies `claude-code.mcp_config` in WORKFLOW.md, the worker reads that file and merges the
`sortie-tools` entry into its `mcpServers` object, because Claude Code accepts only one
`--mcp-config` path; a name collision on the `sortie-tools` key fails the attempt. See ADR-0009
for the full merge algorithm.

The `env` block of the generated entry carries the per-session variables (`SORTIE_ISSUE_ID`,
`SORTIE_ISSUE_IDENTIFIER`, `SORTIE_WORKSPACE`, `SORTIE_DB_PATH`, `SORTIE_SESSION_ID`,
`SORTIE_SESSION_AGENT_KIND`, and `SORTIE_ATTEMPT` on retries) layered over every `SORTIE_`-prefixed
variable in the orchestrator's own environment (`orchestrator.CollectSortieEnv`), which includes
tracker credentials. Per-session values win on collision. The file is written with mode 0600.

`buildArgs` passes `--mcp-config` with the generated path whenever the worker produced one, and
falls back to the operator's `claude-code.mcp_config` path only when it did not.

---

## OpenTelemetry integration

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

These variables reach the CLI when they are set in the Sortie process environment, since the
subprocess inherits it. The adapter sets none of them itself, and its event source is the
`stream-json` stdout output, not OTel.

---

## Adapter pass-through config

Per [architecture Section 5.3.5](architecture/05-workflow-specification.md#535-agent-object), the adapter reads pass-through config from the `claude-code`
sub-object in WORKFLOW.md. `parsePassthroughConfig` reads these keys; a missing or wrong-typed key
falls back to the zero value, except `session_persistence`, which defaults to `true`.

| Config key                        | Type    | Description                                                                                                                                                 |
| --------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `claude-code.permission_mode`     | string  | Permission mode: `default`, `acceptEdits`, `dontAsk`, `bypassPermissions`, `plan`, `auto`. Replaces the default `--dangerously-skip-permissions`.           |
| `claude-code.model`               | string  | Model override (e.g., `claude-sonnet-4-6`). Maps to `--model`.                                                                                              |
| `claude-code.fallback_model`      | string  | Fallback model for rate-limit resilience. Maps to `--fallback-model`.                                                                                       |
| `claude-code.max_turns`           | integer | Claude Code internal max turns per invocation. Maps to `--max-turns` when greater than zero.                                                                |
| `claude-code.max_budget_usd`      | number  | Cost cap per invocation. Maps to `--max-budget-usd` when greater than zero.                                                                                 |
| `claude-code.effort`              | string  | Reasoning effort: `low`, `medium`, `high`, `max`. Maps to `--effort`.                                                                                       |
| `claude-code.allowed_tools`       | string  | Space-separated tool list. Maps to `--allowedTools`.                                                                                                        |
| `claude-code.disallowed_tools`    | string  | Space-separated denied tool list. Maps to `--disallowedTools`.                                                                                              |
| `claude-code.system_prompt`       | string  | Additional system prompt text. Maps to `--append-system-prompt`.                                                                                            |
| `claude-code.mcp_config`          | string  | Path to an operator MCP config JSON. Merged by the worker; see MCP server configuration.                                                                    |
| `claude-code.session_persistence` | boolean | If `false`, passes `--no-session-persistence`. Default: `true`.                                                                                              |

---

## Example invocation

`buildArgs` emits flags in a fixed order: `-p`, output format, verbose, the permission flag, the
session flag, then model, fallback model, max turns, max budget, effort, allowed tools,
disallowed tools, system prompt, MCP config, and session persistence. A continuation turn of a
workflow that sets a model, a fallback model, and a cost cap produces this argv (one entry per
line, no shell involved):

```
claude
-p
Continue working on the issue. The previous turn made progress but tests are still failing.
--output-format
stream-json
--verbose
--dangerously-skip-permissions
--resume
3f2a1b8c-5d6e-4f70-9a1b-2c3d4e5f6071
--model
claude-sonnet-4-6
--fallback-model
claude-haiku-4-5-20251001
--max-budget-usd
5
--mcp-config
/var/sortie/workspaces/PROJ-123/.sortie/mcp.json
```

---

## SDK alternative (reference only)

Anthropic provides TypeScript and Python SDKs for programmatic Claude Code usage. Both
internally spawn the Claude Code CLI as a subprocess with `--input-format stream-json
--output-format stream-json --include-partial-messages` and communicate over stdin/stdout. The SDK
documentation is useful as a reference for expected message types and session behavior, because
the SDK and CLI share the same underlying protocol.

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

---

## Open questions

- **Does the CLI accept `--max-turns`?** The flag is documented in the GitHub Actions
  `claude_args` reference and does not appear in `claude --help` on v2.1.76, which leaves open
  whether it is an SDK-and-settings surface rather than a CLI flag. `buildArgs` passes it whenever
  `claude-code.max_turns` is configured, so an unsupported flag would break every turn of such a
  workflow. Probe: run `claude -p 'reply with ok' --max-turns 1 --output-format stream-json
  --verbose` against the pinned binary and read the parser exit code and the first stdout line; a
  rejected flag fails before any JSON is emitted.
- **What `type` values do task messages carry on the wire?** The message shapes are documented by
  their SDK type names (`TaskStartedMessage`, `TaskProgressMessage`, `TaskNotificationMessage`),
  not by the `type` string that `ParseLine` switches on, so they land in the default arm as
  `other_message`. Probe: run a headless turn whose prompt forces a subagent or a
  background bash task and record the `type` field of each resulting stdout line.

---

## Sources

Local probe:

- `claude --help` on CLI v2.1.76 (flag surface and version pin).

Anthropic first-party documentation, read March 2026:

- Claude Code CLI reference: flags, headless (`-p`) mode, permission modes, session storage.
- Claude Code `stream-json` output reference: message union, system subtypes, content blocks,
  result fields and subtypes, task messages.
- Claude Code monitoring reference: OpenTelemetry environment variables, metrics, and events.
- Claude Code hooks reference: hook exit codes and per-hook timeout.
- Claude Code GitHub Actions reference: `claude_args`, source of the `--max-turns` claim.
- `@anthropic-ai/claude-agent-sdk` (TypeScript) and `claude-agent-sdk` (Python) SDK references.
- Model Context Protocol configuration format (`mcpServers` object).
