# GitHub Copilot CLI: Adapter Research Notes

> GitHub Copilot CLI v1.0.x (npm `@github/copilot`, binary `copilot`), researched March 2026.
> Updated April 2026 for v1.0.21 `--additional-mcp-config` path syntax correction.
> Reference for implementing the Copilot CLI `AgentAdapter`.
>
> **Primary sources:** [CLI command reference][cli-ref], [CLI programmatic reference][cli-prog],
> [hooks configuration reference][hooks-ref], [about hooks][hooks-about],
> [hooks tutorial][hooks-tut].

---

## Overview

GitHub Copilot CLI is an agentic coding tool that runs as a Node.js process in the terminal.
It reads a codebase, executes tools (file edits, shell commands, searches), and produces code
changes autonomously. Sortie treats it as a subprocess: launch it with a prompt, read structured
output from stdout, and terminate it when done.

Three integration surfaces exist, in order of relevance to Sortie:

1. **CLI in non-interactive ("headless") mode** using the `-p` (prompt) flag with
   `--output-format json` for JSONL output. This is the primary integration surface per
   architecture Section 10.7 (Local Subprocess Launch Contract).
2. **Agent Client Protocol (ACP)** via `copilot --acp` ([CLI command reference][cli-ref]),
   which starts an ACP server. This is a structured alternative but details are sparse in
   official documentation and the protocol surface is subject to breaking changes.
3. **TypeScript SDK** (`@github/copilot-sdk`), which internally communicates with the CLI process
   via JSON-RPC. Not usable from a Go adapter directly, but serves as reference for session
   behavior and event types.

Sortie's Go adapter uses the CLI subprocess approach (surface 1). The ACP and SDK surfaces are
documented as reference for expected behavior and as potential future integration paths.

---

## Installation and Prerequisites

Copilot CLI is distributed through multiple channels:

```bash
# Install script (macOS and Linux)
curl -fsSL https://gh.io/copilot-install | bash

# Homebrew (macOS and Linux)
brew install copilot-cli

# WinGet (Windows)
winget install GitHub.Copilot

# npm (all platforms)
npm install -g @github/copilot
```

After installation the `copilot` binary is available on `$PATH`. The adapter's `agent.command`
config field defaults to `copilot` but can be overridden to point to a specific path or wrapper.

**Runtime requirements:**

- Node.js 22+ (bundled with the install script and Homebrew installations; required when
  installing via npm)
- An active GitHub Copilot subscription (Individual, Business, or Enterprise)
- A valid GitHub authentication token (see Authentication section)

**Supported platforms:** Linux, macOS, Windows. Windows requires PowerShell v6+.

---

## Authentication

Copilot CLI authenticates against GitHub's Copilot API via a GitHub token.

### Token resolution order

The CLI resolves authentication tokens in a precedence order with **fallback on failure**. The
official documentation ([authenticate-copilot-cli][auth-ref]) states the order as
`COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN` (in order of precedence).

**Experimental observation (v1.0.13):** the CLI implements try-and-fallback, not exclusive
selection. Setting `COPILOT_GITHUB_TOKEN` to an invalid token while `GH_TOKEN` or
`GITHUB_TOKEN` hold valid tokens does not cause failure — the CLI falls back to the next
source. This means the precedence order matters only when **all** sources hold valid but
different tokens.

| Priority | Method                       | Environment Variable / Mechanism         | Notes                                                              |
| -------- | ---------------------------- | ---------------------------------------- | ------------------------------------------------------------------ |
| 1        | Copilot-specific env var     | `COPILOT_GITHUB_TOKEN`                   | Highest priority. Dedicated to Copilot CLI.                        |
| 2        | GitHub env var (primary)     | `GH_TOKEN`                               | Per official docs. Shared with `gh` CLI.                           |
| 3        | GitHub env var (secondary)   | `GITHUB_TOKEN`                           | Per official docs. Common in CI environments.                      |
| 4        | OAuth keychain               | System keychain / credential store       | From interactive `/login` device flow.                              |
| 5        | `gh` CLI fallback            | `gh auth token`                          | Uses the `gh` CLI's stored credential if available.                |

> **Note:** For Sortie's adapter, the precedence order is immaterial: the adapter checks
> that **at least one** token source is present, not which specific one the CLI will select.

[auth-ref]: https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/authenticate-copilot-cli

### Supported token types

| Token type                    | Prefix          | Notes                                               |
| ----------------------------- | --------------- | --------------------------------------------------- |
| OAuth token (device flow)     | `gho_`          | Created via interactive `/login`.                   |
| Fine-grained PAT              | `github_pat_`   | Requires the "Copilot Requests" permission scope.   |
| GitHub App user-to-server     | `ghu_`          | For GitHub App integrations.                         |
| Classic PAT                   | `ghp_`          | **Observed:** failed Copilot CLI authentication in v1.0.13 testing. No official docs confirm this; do not treat as a permanent constraint. |

### Config mapping

| Sortie config field        | Value                                        |
| -------------------------- | -------------------------------------------- |
| `agent.kind`               | `copilot-cli`                                |
| `agent.command`            | `copilot` (or full path to the binary)       |

The adapter does **not** manage GitHub tokens directly. The token must be present in the
environment of the Sortie process (or passed through via hook env). The adapter inherits the
parent process environment when spawning the subprocess.

**Organization and enterprise restrictions:** If the user's Copilot access is provided via an
organization or enterprise, the administrator must enable the Copilot CLI policy in organization
settings. The CLI will fail to authenticate if this policy is disabled.

---

## CLI Flags Reference

The adapter constructs a `copilot` invocation using these flags. Flags marked **(required)** are
always set by the adapter; others are conditional.

### Core flags

| Flag                               | Description                                                                                        | Adapter usage                                                    |
| ---------------------------------- | -------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `-p <prompt>` / `--prompt`         | Non-interactive (headless) mode. Passes the prompt and exits when done.                            | **(required)** Every turn invocation uses this.                  |
| `-s` / `--silent`                  | Suppress stats and decoration, outputting only the agent's response ([CLI programmatic reference][cli-prog]).  | **(required)** Prevents non-JSON text from polluting stdout.     |
| `--output-format json`             | JSONL on stdout. Each line is a JSON object.                                                       | **(required)** For structured event parsing.                     |
| `--model <model>`                  | Override the AI model (e.g., `claude-sonnet-4.5`, `gpt-5`).                                       | Optional. Adapter passes through if configured.                  |
| `--agent <name>`                   | Use a specific custom agent for the session.                                                       | Optional. Agent routing.                                         |
| `--no-ask-user`                    | Disable the `ask_user` tool. Agent works autonomously without requesting user input ([`copilot --help`][cli-help-ref]). | **(required)** Prevents stalls waiting for user input.           |
| `--additional-mcp-config <json>`   | Add MCP server configuration for the session (inline JSON or `@<path>` to JSON file).              | Optional. For tool extensions (e.g., `tracker_api`).             |
| `--disable-builtin-mcps`           | Disable all built-in MCP servers.                                                                  | Optional. For controlled environments.                           |
| `--disable-mcp-server <name>`      | Disable a specific built-in MCP server.                                                            | Optional.                                                        |
| `--no-custom-instructions`         | Disable loading custom instructions from workspace files.                                          | Optional. For deterministic behavior.                            |
| `--secret-env-vars <vars>`         | Redact the values of specified environment variables in output.                                     | Optional. For security when env contains secrets.                |
| `--share <path>`                   | Export session transcript to markdown file on completion (prompt mode only).                        | Optional. For audit trail.                                       |
| `--experimental`                   | Enable experimental features.                                                                      | Optional. See verification items.                                |

### Session management flags

| Flag                           | Description                                                         | Adapter usage                                                          |
| ------------------------------ | ------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `--resume <session_id>`        | Resume a specific conversation by session ID.                       | Used for continuation turns (turn 2+).                                 |
| `--continue`                   | Resume the most recent conversation in the working directory.       | Alternative to `--resume` for continuation turns.                      |

### Permission flags

| Flag                       | Description                                                                                                    | Adapter usage                                                                                                                           |
| -------------------------- | -------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `--allow-all` / `--yolo`   | Grant all permissions: tools, paths, and URLs. Agent operates without any approval prompts.                    | **(required for headless)** Without this, the process hangs waiting for interactive approval.                                           |
| `--allow-all-tools`        | Allow all tools without confirmation, but still require path/URL approval.                                      | Alternative to `--allow-all` for partial permission.                                                                                    |
| `--allow-all-paths`        | Disable path verification for file operations.                                                                  | Optional. For environments where path restrictions are handled externally.                                                              |
| `--allow-all-urls`         | Disable URL verification for fetch operations.                                                                  | Optional.                                                                                                                               |
| `--allow-tool <tools>`     | Allow specific tools without confirmation. Supports glob patterns (e.g., `"bash(git *)"`, `"edit_file"`).      | Optional. For selective tool approval.                                                                                                  |
| `--deny-tool <tools>`      | Deny specific tools. Takes precedence over `--allow-tool`. Supports glob patterns.                              | Optional. For tool restriction.                                                                                                         |

> **Security note:** `--allow-all` / `--yolo` allows arbitrary command execution and file
> modification. Per architecture Section 10.4, Sortie adopts a high-trust posture where
> approval requests must not leave a run stalled. The `--allow-all` flag is appropriate for
> headless operation in sandboxed environments. Sortie's workspace isolation and hook system
> operate as additional defense-in-depth.

### Tool filter flags

| Flag                          | Description                                                                         | Adapter usage                                     |
| ----------------------------- | ----------------------------------------------------------------------------------- | ------------------------------------------------- |
| `--available-tools <tools>`   | Restrict the set of tools available to the agent. Only listed tools are accessible. | Optional. For tool palette restriction.            |
| `--excluded-tools <tools>`    | Remove specific tools from the available set.                                       | Optional. For selectively disabling tools.         |

Tool names follow the CLI's tool vocabulary ([CLI command reference: tool availability][cli-ref]).
Built-in tools include: `bash`, `view`, `edit_file` (shown as `edit` in some CLI docs,
backed by `apply_patch`), `create`, `apply_patch`, `glob`, `grep`, `web_fetch`, `ask_user`,
`task`, `report_intent`, `show_file`, `store_memory`, `task_complete`, `exit_plan_mode`.

### Autopilot flags

| Flag                                | Description                                                                                               | Adapter usage                                                          |
| ----------------------------------- | --------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `--autopilot`                       | Enable autopilot mode. Agent continues working through steps autonomously until task completion.           | **(required for headless)** Without this, agent may stop after one step. |
| `--max-autopilot-continues <count>` | Limit the number of autonomous continuation steps. Prevents runaway loops.                                | **(recommended)** Safety backstop for programmatic use.                |

> **Autopilot mode is distinct from `--allow-all`.** `--allow-all` grants permission for
> tool execution. `--autopilot` controls whether the agent continues working through
> multi-step tasks without waiting for user input between steps. Both are needed for
> fully autonomous headless operation.

### Config/settings flags

| Flag                    | Description                                      | Adapter usage                                                  |
| ----------------------- | ------------------------------------------------ | -------------------------------------------------------------- |
| `--config-dir <path>`   | Set the configuration directory.                | Optional. For isolated config environments.                    |
| `--log-dir <path>`      | Set the log output directory.                    | Optional. Log file names contain the session ID.               |

---

## Subprocess Invocation

Per architecture Section 10.7, the adapter launches:

```
# POSIX (Linux / macOS). On Windows, use cmd.exe /C or PowerShell equivalent.
sh -c 'copilot -p "$1" --output-format json -s --allow-all --autopilot --no-ask-user --max-autopilot-continues "$2" ${3:+--resume "$3"}' -- "$prompt" "$max_continues" "$session_id"
```

> **Shell safety:** The prompt must not be interpolated directly into the `sh -c` string. Pass
> it as a positional parameter (`$1`) to avoid injection via shell metacharacters in
> user-controlled issue content.

### Process settings

| Setting           | Value                                               | Rationale                                                  |
| ----------------- | --------------------------------------------------- | ---------------------------------------------------------- |
| Working directory | Workspace path (`StartSessionParams.WorkspacePath`) | Agent must operate in the issue workspace.                 |
| Stdout            | Pipe (read by adapter)                              | JSONL output parsed line by line.                          |
| Stderr            | Pipe (read by adapter, logged)                      | Diagnostic output, not structured.                         |
| Environment       | Inherited from Sortie process                       | GitHub token and other auth vars must be present.          |
| Max line size     | 10 MB                                               | Safe buffering per architecture doc recommendation.        |

### First turn vs. continuation turns

| Turn                   | Invocation                                                                                                                                       |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| Turn 1 (new session)   | `copilot -p "<prompt>" --output-format json -s --allow-all --autopilot --no-ask-user --max-autopilot-continues <N>`                              |
| Turn 2+ (continuation) | `copilot -p "<prompt>" --output-format json -s --allow-all --autopilot --no-ask-user --max-autopilot-continues <N> --resume <session_id>`        |

The `--resume <session_id>` flag continues the conversation in the same session, preserving the
full message history from prior turns. The session ID is available in the final `result` JSONL
event as the `sessionId` field (confirmed experimentally in v1.0.13).

**Alternative:** `--continue` resumes the most recent conversation in the cwd. Since Sortie
controls the workspace directory per issue, `--continue` would work. However, `--resume
<session_id>` is more explicit and avoids ambiguity if multiple sessions exist in the same
workspace.

**Session ID is in the `result` event.** The `result` event — the last JSONL line before
process exit — contains `"sessionId": "<uuid>"`. This was confirmed in v1.0.13:

```json
{"type":"result","timestamp":"...","sessionId":"aa778ea0-6eab-4ce9-b87e-11d6d33dab4f","exitCode":0,"usage":{...}}
```

The adapter must parse the `result` event and store the `sessionId` for use in subsequent
`--resume` invocations. Historical context: prior to JSONL support, the session ID was not
programmatically discoverable ([github/copilot-cli#442](https://github.com/github/copilot-cli/issues/442)).

> **Concurrency note for `max_concurrent_agents > 1`:** Since the `result` event contains the
> session ID, the adapter has a reliable per-session ID source. The fallback strategy of
> scanning `~/.copilot/session-state/` (mentioned in earlier research) is unreliable with
> concurrent sessions and should not be used. If the `result` event is somehow missing (e.g.,
> the process is killed before emitting it), the adapter can fall back to `--continue` which
> resumes the most recent session in the workspace cwd — this is safe because Sortie uses
> workspace-per-issue isolation.

---

## Output Format: `--output-format json`

With `--output-format json`, Copilot CLI writes one JSON object per line to stdout (newline-
delimited JSON / JSONL). Each line is independently parseable.

> **History:** JSONL output support was added in response to
> [github/copilot-cli#52](https://github.com/github/copilot-cli/issues/52). The
> [CLI command reference][cli-ref] documents `--output-format=FORMAT` where FORMAT is `text`
> (default) or `json` (outputs JSONL: one JSON object per line).

### JSONL event schema (observed v1.0.13)

The following schema was determined experimentally by running Copilot CLI v1.0.13 with
`--output-format json` and capturing stdout. The official documentation does not publish this
schema; what follows is empirical observation.

**Common envelope.** Every event is a JSON object with these top-level fields:

| Field        | Type    | Description                                                            |
| ------------ | ------- | ---------------------------------------------------------------------- |
| `type`       | string  | Event type discriminator (see table below).                            |
| `id`         | string  | UUID of the event.                                                     |
| `timestamp`  | string  | ISO 8601 timestamp (e.g., `"2026-03-30T22:19:20.234Z"`).              |
| `parentId`   | string  | UUID linking to the parent event (forms a tree).                       |
| `data`       | object  | Event-type-specific payload (absent on `result` events).               |
| `ephemeral`  | boolean | If `true`, the event is transient (deltas, status updates). Optional.  |

The `result` event is an exception: it has no `data` or `id` fields and instead carries
`sessionId`, `exitCode`, and `usage` at the top level.

**Observed event types:**

| Event type                        | Ephemeral | `data` fields                                                                              | Adapter mapping           |
| --------------------------------- | --------- | ------------------------------------------------------------------------------------------ | ------------------------- |
| `session.warning`                 | yes       | `warningType`, `message`                                                                   | `notification`            |
| `session.mcp_server_status_changed` | yes     | `serverName`, `status`                                                                     | log only                  |
| `session.mcp_servers_loaded`      | yes       | `servers` (array of `{name, status, source, error?}`)                                      | log only                  |
| `session.tools_updated`           | yes       | `model`                                                                                    | log only                  |
| `session.info`                    | yes       | `infoType`, `message`                                                                      | `notification`            |
| `session.task_complete`           | no        | `summary`, `success`                                                                       | `notification`            |
| `user.message`                    | no        | `content`, `transformedContent`, `attachments`, `agentMode`?, `interactionId`              | log only                  |
| `assistant.turn_start`            | no        | `turnId`, `interactionId`                                                                  | `notification`            |
| `assistant.message_delta`         | yes       | `messageId`, `deltaContent`                                                                | stall timer reset         |
| `assistant.message`              | no        | `messageId`, `content`, `toolRequests` (array), `interactionId`, `outputTokens`            | `assistant_message`       |
| `assistant.turn_end`             | no        | `turnId`                                                                                   | `notification`            |
| `tool.execution_start`           | no        | `toolCallId`, `toolName`, `arguments`                                                      | `tool_use`                |
| `tool.execution_complete`        | no        | `toolCallId`, `model`, `interactionId`, `success`, `result`, `toolTelemetry`               | `tool_result`             |
| `result`                          | no        | *(top-level)* `sessionId`, `exitCode`, `usage{premiumRequests, totalApiDurationMs, sessionDurationMs, codeChanges{linesAdded, linesRemoved, filesModified}}` | `turn_completed` / `turn_failed` |

**Example output** (simple task, autopilot mode, v1.0.13):

```jsonl
{"type":"session.mcp_servers_loaded","data":{"servers":[{"name":"github-mcp-server","status":"connected","source":"builtin"}]},"id":"...","timestamp":"2026-03-30T22:19:18.132Z","parentId":"...","ephemeral":true}
{"type":"session.tools_updated","data":{"model":"claude-opus-4.6"},"id":"...","timestamp":"...","parentId":"...","ephemeral":true}
{"type":"user.message","data":{"content":"Say exactly: hello world","transformedContent":"...","attachments":[],"agentMode":"autopilot","interactionId":"bac81e5a-..."},"id":"...","timestamp":"...","parentId":"..."}
{"type":"assistant.turn_start","data":{"turnId":"0","interactionId":"bac81e5a-..."},"id":"...","timestamp":"...","parentId":"..."}
{"type":"assistant.message_delta","data":{"messageId":"96620e44-...","deltaContent":"hello"},"id":"...","timestamp":"...","parentId":"...","ephemeral":true}
{"type":"assistant.message","data":{"messageId":"96620e44-...","content":"\n\nhello world","toolRequests":[],"interactionId":"bac81e5a-...","outputTokens":6},"id":"...","timestamp":"...","parentId":"..."}
{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"...","timestamp":"...","parentId":"..."}
{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"aa778ea0-6eab-4ce9-b87e-11d6d33dab4f","exitCode":0,"usage":{"premiumRequests":6,"totalApiDurationMs":6866,"sessionDurationMs":12927,"codeChanges":{"linesAdded":0,"linesRemoved":0,"filesModified":[]}}}
```

**Example with tool use** (read file task, v1.0.13):

```jsonl
{"type":"assistant.message","data":{"messageId":"...","content":"","toolRequests":[{"toolCallId":"toolu_vrtx_...","name":"view","arguments":{"path":"/tmp/copilot-test/main.go"},"type":"function","intentionSummary":"view the file..."}],"interactionId":"...","outputTokens":102},"id":"...","timestamp":"...","parentId":"..."}
{"type":"tool.execution_start","data":{"toolCallId":"toolu_vrtx_...","toolName":"view","arguments":{"path":"/tmp/copilot-test/main.go"}},"id":"...","timestamp":"...","parentId":"..."}
{"type":"tool.execution_complete","data":{"toolCallId":"toolu_vrtx_...","model":"claude-opus-4.6","interactionId":"...","success":true,"result":{"content":"1. package main\n2. ","detailedContent":"..."},"toolTelemetry":{"properties":{"command":"view"},"metrics":{"resultLength":19}}},"id":"...","timestamp":"...","parentId":"..."}
```

> **Session storage uses the same vocabulary.** The on-disk `events.jsonl` at
> `~/.copilot/session-state/<session-id>/` uses the same `"type"` field and event type names
> as stdout JSONL. The `"event"` field format observed in
> [github/copilot-cli#2201](https://github.com/github/copilot-cli/issues/2201) appears to be
> from an older CLI version.
>
> **SDK event types match.** The SDK types (`user.message`, `assistant.message`,
> `tool.execution_start`, etc.) turn out to be the *same* vocabulary used by
> `--output-format json`, not a separate format.

> **Implementation note:** The adapter must still handle unknown event types gracefully by
> logging them as `other_message` events. Future CLI versions may add new event types.

### Determining session completion

In the subprocess model with `-p`, the process exits when the agent completes its work.
Completion is signaled by two mechanisms:

1. **`result` JSONL event** — the last line emitted before exit, containing `exitCode`,
   `sessionId`, and `usage` data.
2. **Process exit code** — 0 for success, non-zero for failure.

The adapter should use the `result` event as the primary completion signal (it contains richer
data) and fall back to process exit code if the `result` event is missing (e.g., process was
killed).

### Parsing strategy

The adapter reads stdout line by line. For each line:

1. Parse as JSON. If parsing fails, emit a `malformed` event and continue.
2. Read the `"type"` field as the event discriminator.
3. Based on the event type:
   - `tool.execution_start` / `tool.execution_complete` -> log tool activity, emit `tool_use` /
     `tool_result` with tool name, arguments, and result.
   - `assistant.message` -> emit `assistant_message` with content and output token count.
   - `assistant.message_delta` -> reset stall detection timer.
   - `session.task_complete` -> log task completion summary.
   - `result` -> extract `sessionId` (for `--resume`), `exitCode`, `usage`.
   - `session.*` (ephemeral) -> log as `notification`, do not store.
   - Unknown types -> emit `other_message`.
4. On process exit, check exit code to determine `turn_completed` or `turn_failed`.

### Token usage extraction

Token usage data is available from multiple sources in the JSONL output:

- **`result` event** (end of session): `usage.premiumRequests`, `usage.totalApiDurationMs`,
  `usage.sessionDurationMs`, `usage.codeChanges`. Does **not** include raw input/output token
  counts.
- **`assistant.message` event**: `data.outputTokens` gives per-message output token count.
  No `inputTokens` field observed.
- **OTel spans** ([CLI command reference: OTel monitoring][cli-ref]): `invoke_agent` span
  includes `gen_ai.usage.input_tokens` and `gen_ai.usage.output_tokens`. The `chat` span
  includes per-LLM-request token counts.

The adapter normalizes available data into:

```
TokenUsage{
    InputTokens:  <from OTel if available, else 0>,
    OutputTokens: <sum of assistant.message outputTokens>,
    TotalTokens:  <input + output>,
}
```

> **Gap:** Per-session input token counts are not available in JSONL output — only output
> tokens per `assistant.message` and aggregate `premiumRequests` in the `result` event.
> For full input/output breakdowns, enable OTel with `COPILOT_OTEL_FILE_EXPORTER_PATH`
> and parse the span data.

---

## Session Lifecycle Mapping

Architecture Section 10.2 defines the session lifecycle. Here is how Copilot CLI maps to it:

### `StartSession`

Architecture Sections 10.1 and 10.2 define `StartSession` as the operation that "launches or
connects" to the agent. For Copilot CLI, the session is disk-persisted at
`~/.copilot/session-state/<session-id>/` and identified by a UUID. The OS subprocess is
short-lived and created per turn. This adapter treats `StartSession` as establishing the
logical Copilot CLI session, while deferring creation of the Node.js subprocess until `RunTurn`.

1. Record the workspace path and configuration.
2. Perform preflight validation:
   - Resolve and normalize the workspace path.
   - Enforce workspace path containment rules.
   - Validate that the `copilot` CLI command is resolvable on `$PATH`.
   - Validate that Node.js 22+ is available (Copilot CLI requires it).
3. Verify authentication by checking that at least one of `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`,
   or `GITHUB_TOKEN` is set in the environment, or that `gh auth token` returns a valid token.
4. Initialize and return a `Session`:
   - `ID`: empty (populated after the first turn from the `result` JSONL event's `sessionId`).
   - `Internal`: adapter-internal state (workspace path, config snapshot).
     No OS subprocess is spawned at this point; that happens in `RunTurn`.

### `RunTurn`

1. Build the CLI command:
   - Turn 1: `copilot -p "<prompt>" --output-format json -s --allow-all --autopilot
     --no-ask-user --max-autopilot-continues <N>`
   - Turn 2+: append `--resume <session_id>` to continue the conversation.
2. Launch subprocess with `sh -c <command>`, cwd = workspace path.
3. Read stdout line by line, parse each JSON event, and deliver to `OnEvent` callback.
4. On each event, update the stall detection timer.
5. When the process exits:
   - **Exit code 0:** Return `TurnResult{ExitReason: turn_completed}`.
   - **Exit code non-zero:** Return `TurnResult{ExitReason: turn_failed}` with error details.
   - **Killed by signal (timeout/cancellation):** Return `TurnResult{ExitReason: turn_cancelled}`.
6. Extract `session_id` from the `result` JSONL event (`sessionId` field). Store it in
   `Session.Internal` state for use in subsequent `--resume` invocations.

### `StopSession`

1. If a subprocess is running, send `SIGTERM`.
2. Wait briefly (e.g., 5 seconds) for clean exit.
3. If still running, send `SIGKILL`.
4. Clean up file descriptors and goroutines.

### `EventStream`

Return `nil`. The Copilot CLI adapter uses the synchronous `OnEvent` callback model, not the
async channel model.

---

## Turn Model

Copilot CLI has its own internal concept of autonomous continuations controlled by autopilot mode.
The `--max-autopilot-continues` flag limits how many autonomous steps the agent takes within a
single CLI invocation.

**Default behavior in autopilot mode:** The agent continues working through steps until it
determines the task is complete, encounters a problem, or reaches the continuation limit. Without
`--autopilot`, the agent completes one interaction cycle and exits.

**Experimentally observed difference (v1.0.13):**

| Aspect                        | With `--autopilot`                                        | Without `--autopilot`                                |
| ----------------------------- | --------------------------------------------------------- | ---------------------------------------------------- |
| `user.message.data.agentMode` | `"autopilot"`                                             | absent                                               |
| After first response          | Sends autopilot continuation prompt, continues working    | Exits immediately with `result` event                |
| Continuation prompt           | "You have not yet marked the task as complete using the task_complete tool..." | N/A                                   |
| `session.info` events         | `infoType: "autopilot_continuation"` with premium request count | absent                                        |
| `task_complete` tool          | Agent calls `task_complete` to signal completion          | Not invoked; process exits after first response      |

Without `--autopilot`, the agent produces one response and exits — it does NOT run a multi-step
agentic loop. The `--autopilot` flag is therefore **required** for any non-trivial task in
headless mode.

**Sortie's turn model is distinct from Copilot CLI's internal continuations.** Sortie's "turn" is
one CLI invocation (one `RunTurn` call). Within that invocation, Copilot CLI may execute multiple
autonomous continuation steps.

### Strategy A: Single Sortie turn = single Copilot CLI invocation with autopilot (recommended)

- Set `--autopilot --max-autopilot-continues <N>` where N provides a reasonable safety limit.
- Copilot CLI runs its full agentic loop within one invocation.
- Sortie calls `RunTurn` once per Sortie turn. Copilot CLI decides when it's done.
- After each Sortie turn, the orchestrator re-checks tracker state and decides whether to
  continue.
- Use `agent.turn_timeout_ms` as the time safety backstop.

### Strategy B: Without autopilot (fine-grained control)

- Omit `--autopilot`.
- Each `RunTurn` call runs one interaction cycle.
- Sortie has maximum control over the loop but incurs subprocess launch overhead per turn and
  may interrupt Copilot CLI's multi-step plans.

**Recommendation:** Strategy A. Let Copilot CLI run its agentic loop to completion within each
Sortie turn. Sortie's `agent.turn_timeout_ms` provides the safety backstop. Set
`--max-autopilot-continues` to a reasonable value (e.g., 50) to prevent runaway loops.

---

## Timeout Enforcement

| Timeout                  | Source      | Enforcement                                                                                                                                                                                           |
| ------------------------ | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.turn_timeout_ms`  | WORKFLOW.md | Adapter sets a deadline on the subprocess. On expiry, send SIGTERM → wait → SIGKILL. Map to `turn_cancelled`.                                                                                         |
| `agent.read_timeout_ms`  | WORKFLOW.md | Not directly applicable to subprocess model. Could apply to initial subprocess startup: if no output within `read_timeout_ms`, consider startup failed.                                               |
| `agent.stall_timeout_ms` | WORKFLOW.md | Enforced by the **orchestrator**, not the adapter. The orchestrator monitors `last_agent_timestamp` from events. If the gap exceeds `stall_timeout_ms`, the orchestrator calls `StopSession`.         |

### Max autopilot continues as timeout proxy

`--max-autopilot-continues <N>` acts as a step-based safety limit complementing the time-based
`turn_timeout_ms`. Each continuation consumes one or more premium requests. Setting a reasonable
limit prevents both runaway execution and excessive API cost.

### Context cancellation

The adapter must respect `context.Context` cancellation:

- `RunTurn` receives a context. If the context is cancelled (e.g., due to tracker reconciliation
  finding the issue is terminal), the adapter must kill the subprocess promptly.
- Use `cmd.Process.Signal(syscall.SIGTERM)` followed by a grace period, then
  `cmd.Process.Kill()`.

---

## Permission and Approval Policy

Per architecture Section 10.4, Sortie adopts a high-trust posture:

| Policy                    | Implementation                                                                                                                                                                                           |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Auto-approve all actions  | `--allow-all` / `--yolo` bypasses all permission prompts for tools, paths, and URLs.                                                                                                                    |
| User input suppression    | `--no-ask-user` disables the `ask_user` tool ([`copilot --help`][cli-help-ref]). The agent makes decisions autonomously.                                                                                 |
| Autopilot permissions     | When entering autopilot mode, if `--allow-all` is not set, the CLI prompts for permissions. In headless mode (`-p`), this prompt would stall. `--allow-all` must be used with `--autopilot`.            |
| Unsupported tool calls    | Copilot CLI handles tool routing internally. Unknown tools return failure and the session continues.                                                                                                      |

### Permission system details

Copilot CLI categorizes permissions into:

- **Tool permissions:** Whether specific tools can execute (e.g., `bash`, `edit_file`).
- **Path permissions:** Whether file operations can access specific paths.
- **URL permissions:** Whether the agent can fetch specific URLs.

The `--allow-all` flag grants all three categories. Finer-grained control is available via
`--allow-tool`, `--deny-tool`, `--allow-all-tools`, `--allow-all-paths`, and `--allow-all-urls`.

### Tool filter patterns

Tool permission flags accept patterns in `Kind(argument)` format ([CLI command reference:
tool permission patterns][cli-ref]):

```
--allow-tool='shell(git:*)'   # Allow all git subcommands (git push, git status, etc.)
--allow-tool='write'          # Allow all file writes
--deny-tool='shell(rm -rf *)' # Deny destructive shell commands
```

Supported pattern kinds: `shell`, `write`, `read`, `url`, `memory`, and MCP server names.
The `:*` suffix on `shell` patterns matches the command stem followed by a space, preventing
partial matches. `--deny-tool` takes precedence over `--allow-tool`, even when `--allow-all`
is set.

---

## Error Detection and Mapping

### Process exit codes

| Exit Code                | Meaning                                  | Adapter mapping                                   |
| ------------------------ | ---------------------------------------- | ------------------------------------------------- |
| 0                        | Success (task completed or autopilot ended normally). If no `result` event was received and cumulative output tokens are zero, treated as `turn_failed` (no-output safety heuristic). | `turn_completed` or `turn_failed` (no-output heuristic) |
| Non-zero                 | General error                            | `turn_failed`                                     |
| 127                      | `copilot` binary not found               | `agent_not_found`                                 |
| Signal (SIGTERM/SIGKILL) | Killed by adapter or OS                  | `turn_cancelled`                                  |

> **Gap:** Copilot CLI does not document specific exit codes for different failure modes (unlike
> Claude Code which uses exit code 1 for general errors). The adapter should treat any non-zero
> exit code as `turn_failed` and capture stderr for diagnostics.

### Authentication failures

| Condition                                  | Behavior                                                  | Adapter mapping         |
| ------------------------------------------ | --------------------------------------------------------- | ----------------------- |
| No valid token in environment              | CLI fails to start, outputs error to stderr               | `agent_not_found` or `response_timeout` |
| Token lacks Copilot access                 | CLI fails during API call                                 | `turn_failed`           |
| Organization policy disables Copilot CLI   | CLI fails during authentication                            | `turn_failed`           |
| Classic PAT (`ghp_*`) used                 | Authentication rejected                                    | `turn_failed`           |

### Error category mapping (per architecture Section 10.5)

| Condition                                 | Error category          |
| ----------------------------------------- | ----------------------- |
| `copilot` binary not found on `$PATH`     | `agent_not_found`       |
| Node.js 22+ not available                 | `agent_not_found`       |
| Workspace path invalid or not a directory | `invalid_workspace_cwd` |
| No output within `read_timeout_ms`        | `response_timeout`      |
| Turn exceeds `turn_timeout_ms`            | `turn_timeout`          |
| Process exits non-zero                    | `port_exit`             |
| Turn completed with error                 | `turn_failed`           |
| Process killed by signal                  | `turn_cancelled`        |
| Agent requests user input (if `--no-ask-user` is not set) | `turn_input_required` |

### Known issues in the wild

From [github/copilot-cli issues](https://github.com/github/copilot-cli/issues):

- **Session file corruption** ([#2012](https://github.com/github/copilot-cli/issues/2012)):
  Raw U+2028/U+2029 characters in `events.jsonl` break `JSON.parse()` on `--resume`. This may
  affect session continuation.
- **Subprocess I/O deadlock** ([#1838](https://github.com/github/copilot-cli/issues/1838)):
  In Nix/direnv environments, CLI hangs due to subprocess I/O deadlock. The adapter's turn
  timeout catches this.
- **Headless server fd leaks** ([#2389](https://github.com/github/copilot-cli/issues/2389)):
  When running as a headless server, kqueue file descriptors leak and the bash tool stops
  working after prolonged use.
- **Authentication failures without output** ([#2184](https://github.com/github/copilot-cli/issues/2184)):
  CLI fails to start without any output when there is a login issue. The adapter should treat
  no output within `read_timeout_ms` as `response_timeout`.
- **sessionStart hook fires after userPromptSubmitted** ([#2201](https://github.com/github/copilot-cli/issues/2201)):
  The `sessionStart` hook fires after `userPromptSubmitted`, not before. Provides real examples
  of event format in `events.jsonl`.
- **`--additional-mcp-config` requires `@` prefix for file paths** (confirmed v1.0.21): Bare
  file paths are parsed as inline JSON and fail with `Invalid JSON in --additional-mcp-config`.
  The documented syntax for file input is `@<path>` (`github/copilot-cli#428`). Earlier
  versions (≤1.0.18) had an undocumented fallback that recognized bare paths; this fallback
  was removed. The Sortie adapter now uses the `@` prefix consistently.
- **Config parsing errors exit 0 without output** (observed v1.0.21, fixed by PR #405 for
  the `@` prefix case): When Copilot CLI fails to parse `--additional-mcp-config`, it exits 0
  without emitting any JSONL events. The adapter's no-output heuristic detects this as
  `turn_failed`. Operators should check WARN-level logs for the stderr content explaining the
  parse failure.

---

## Session Storage

Copilot CLI persists sessions to disk at:

```
~/.copilot/session-state/<session-id>/
```

Each session directory contains:

| File/Directory    | Description                                                        |
| ----------------- | ------------------------------------------------------------------ |
| `events.jsonl`    | Full event log for the session (all messages and tool calls).      |
| `workspace.yaml`  | Workspace metadata (cwd, git root, repository, branch).           |
| `plan.md`         | The agent's implementation plan (if plan mode was used).           |
| `checkpoints/`    | Checkpoints for infinite session context compaction.               |
| `files/`          | Files tracked by the session.                                      |

This is relevant for:

- **Continuation:** `--resume <session_id>` reads from this directory.
- **Cleanup:** Sortie may want to clean up session state when removing workspaces.
- **Disk usage:** Long sessions with infinite context compaction accumulate checkpoint data.

### Infinite sessions and context compaction

Copilot CLI uses "infinite sessions" by default. When the context window approaches capacity:

1. Background compaction starts at ~80% context usage (configurable via SDK).
2. Processing blocks at ~95% context usage until compaction completes.
3. Compaction summarizes older conversation history, preserving recent context.

This means a single Copilot CLI session can run indefinitely without hitting context limits.
The adapter does not need to manage context window size — Copilot CLI handles this internally.

---

## Hooks Integration

Copilot CLI supports lifecycle hooks via `.github/hooks/*.json` in the workspace
([hooks configuration reference][hooks-ref], [about hooks][hooks-about]). Each hook file uses
a version 1 schema where hook event names are object keys:

```json
{
  "version": 1,
  "hooks": {
    "preToolUse": [
      {
        "type": "command",
        "bash": "./scripts/pre-tool-policy.sh",
        "powershell": "./scripts/pre-tool-policy.ps1",
        "cwd": ".github/hooks",
        "timeoutSec": 15
      }
    ]
  }
}
```

### Hook events

All 8 events are documented in the [CLI command reference: hook events][cli-ref]:

| Event                    | When it fires                                    | Can modify behavior?                                   |
| ------------------------ | ------------------------------------------------ | ------------------------------------------------------ |
| `sessionStart`           | Session begins or resumes.                       | No. Output ignored.                                    |
| `sessionEnd`             | Session completes or is terminated.              | No. Output ignored.                                    |
| `userPromptSubmitted`    | User submits a prompt.                           | No. Output ignored.                                    |
| `preToolUse`             | Before each tool execution.                      | **Yes.** Can allow, deny, or modify tool arguments.    |
| `postToolUse`            | After each tool execution.                       | No. Output ignored.                                    |
| `agentStop`              | Main agent finishes a turn.                      | **Yes.** Can block and force continuation.             |
| `subagentStop`           | Subagent completes.                              | **Yes.** Can block and force continuation.             |
| `errorOccurred`          | Error during processing.                         | No. Output ignored.                                    |

### Hook input formats

Hooks receive JSON on stdin. Key input schemas from the [hooks configuration reference][hooks-ref]:

**`sessionStart`:**
```json
{"timestamp": 1704614400000, "cwd": "/path/to/project", "source": "new", "initialPrompt": "..."}
```
Where `source` is `"new"`, `"resume"`, or `"startup"`.

**`sessionEnd`:**
```json
{"timestamp": 1704618000000, "cwd": "/path/to/project", "reason": "complete"}
```
Where `reason` is `"complete"`, `"error"`, `"abort"`, `"timeout"`, or `"user_exit"`.

**`preToolUse`:**
```json
{"timestamp": 1704614600000, "cwd": "/path", "toolName": "bash", "toolArgs": "{\"command\":\"git status\"}"}
```

**`postToolUse`:**
```json
{"timestamp": 1704614700000, "cwd": "/path", "toolName": "bash", "toolArgs": "...", "toolResult": {"resultType": "success", "textResultForLlm": "..."}}
```

**`errorOccurred`:**
```json
{"timestamp": 1704614800000, "cwd": "/path", "error": {"message": "Network timeout", "name": "TimeoutError", "stack": "..."}}
```

### Hook responses

Only `preToolUse`, `agentStop`, and `subagentStop` process output. All other hooks have their
output ignored.

**`preToolUse`** hook returns ([CLI command reference: preToolUse decision control][cli-ref]):

```json
{
  "permissionDecision": "deny",
  "permissionDecisionReason": "Destructive operations require approval",
  "modifiedArgs": { "command": "git diff" }
}
```

Where `permissionDecision` is `"allow"`, `"deny"`, or `"ask"`. Only `"deny"` is currently
processed per the [hooks configuration reference][hooks-ref]. `modifiedArgs` substitutes
the original tool arguments.

**`agentStop` / `subagentStop`** hook returns ([CLI command reference: agentStop decision
control][cli-ref]):

```json
{
  "decision": "block",
  "reason": "Task not yet complete based on acceptance criteria"
}
```

Where `decision` is `"block"` (force another agent turn using `reason` as the prompt) or
`"allow"` (let the agent stop).

Sortie does **not** manage Copilot CLI's hook system directly. These are workspace-level
configurations that the coding agent or `after_create` workspace hook can set up. However,
the adapter should be aware that:

- Hooks can block tool calls (`preToolUse` returning `"deny"`).
- Hooks can add latency to tool execution (timeout per hook up to 30s default).
- The `agentStop` hook can prevent session completion if it returns `"block"`.
- Hook-induced delays should be accounted for in timeout calculations.

---

## MCP Server Configuration

The `--additional-mcp-config <json>` flag adds MCP server declarations for the session.
It accepts inline JSON or `@<path>` referencing a JSON file. The `@` prefix is required
for file paths — bare paths are parsed as inline JSON and fail. The format follows the
standard MCP configuration schema with a top-level `mcpServers` object.

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

Copilot CLI reads the configuration at agent startup and spawns each declared server as a
child process. The server inherits the agent's environment, merged with any variables in
the `env` field. Unlike `--mcp-config` in Claude Code, `--additional-mcp-config` is
additive — it supplements rather than replaces Copilot CLI's built-in MCP servers
(`github-mcp-server`, `playwright`, `fetch`, `time`).

### Disabling built-in MCP servers

Use `--disable-builtin-mcps` to disable all built-in MCP servers, or
`--disable-mcp-server <name>` to disable a specific one.

### Sortie adapter usage

The worker writes `.sortie/mcp.json` to the workspace directory and passes it via
`--additional-mcp-config @<path>` (the `@` prefix instructs the CLI to read the file).
If the operator also specifies
`copilot-cli.mcp_config` in WORKFLOW.md, the worker merges both server sets into a single
file. A name collision on the `sortie-tools` key fails the attempt. Credential values are
never written to this file — they reach the MCP server through inherited environment
variables. See ADR-0009 for the full merge algorithm.

---

## OpenTelemetry Integration

Copilot CLI supports OpenTelemetry for monitoring ([CLI command reference: OTel
monitoring][cli-ref]). OTel is off by default with zero overhead. It activates when any of the
following environment variables are set:

| Variable                             | Description                                                              |
| ------------------------------------ | ------------------------------------------------------------------------ |
| `COPILOT_OTEL_ENABLED=true`         | Explicitly enable OTel.                                                  |
| `OTEL_EXPORTER_OTLP_ENDPOINT`       | OTLP endpoint URL. Setting this automatically enables OTel.              |
| `COPILOT_OTEL_FILE_EXPORTER_PATH`   | Write all signals to a JSON-lines file. Setting this enables OTel.       |
| `OTEL_SERVICE_NAME`                 | Service name (default: `github-copilot`).                                |
| `COPILOT_OTEL_SOURCE_NAME`          | Instrumentation scope name (default: `github.copilot`).                  |

### Trace hierarchy

The runtime emits a hierarchical span tree per agent interaction:

| Span type        | Span kind  | Key attributes                                                                      |
| ---------------- | ---------- | ----------------------------------------------------------------------------------- |
| `invoke_agent`   | CLIENT     | `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `github.copilot.cost`   |
| `chat`           | CLIENT     | Per-LLM-request token counts, model, response ID, turn cost                        |
| `execute_tool`   | INTERNAL   | `gen_ai.tool.name`, `gen_ai.tool.call.id`, tool arguments (content capture only)    |

### Metrics

| Metric                                            | Type      | Description                                  |
| ------------------------------------------------- | --------- | -------------------------------------------- |
| `gen_ai.client.operation.duration`                | Histogram | LLM API call and agent invocation duration   |
| `gen_ai.client.token.usage`                       | Histogram | Token counts by type (input/output)          |
| `github.copilot.tool.call.count`                  | Counter   | Tool invocations by tool name and success    |
| `github.copilot.tool.call.duration`               | Histogram | Tool execution latency by tool name          |

### Span events

Lifecycle events recorded on active spans:

| Event name                                   | Description                           |
| -------------------------------------------- | ------------------------------------- |
| `github.copilot.hook.start` / `.end`         | Hook execution lifecycle              |
| `github.copilot.session.compaction_start`     | History compaction began              |
| `github.copilot.session.compaction_complete`  | History compaction completed          |
| `github.copilot.session.shutdown`            | Session shutting down                  |

Sortie may enable OTel in the subprocess environment for external monitoring, but the adapter's
primary event source is the JSONL stdout output. OTel data is supplementary and useful for
per-tool and per-request token breakdowns that JSONL may not provide.

---

## Adapter-Specific Pass-Through Config

Per architecture Section 5.3.5, the adapter reads pass-through config from the `copilot-cli`
sub-object in WORKFLOW.md:

| Config key                                | Type    | Description                                                                      |
| ----------------------------------------- | ------- | -------------------------------------------------------------------------------- |
| `copilot-cli.model`                       | string  | Model override (e.g., `claude-sonnet-4.5`, `gpt-5`). Maps to `--model`.         |
| `copilot-cli.max_autopilot_continues`     | integer | Maximum autonomous continuation steps. Maps to `--max-autopilot-continues`.      |
| `copilot-cli.agent`                       | string  | Custom agent name. Maps to `--agent`.                                            |
| `copilot-cli.allowed_tools`               | string  | Space-separated tool allow list. Maps to `--allow-tool`.                         |
| `copilot-cli.denied_tools`                | string  | Space-separated tool deny list. Maps to `--deny-tool`.                           |
| `copilot-cli.available_tools`             | string  | Space-separated available tools restriction. Maps to `--available-tools`.        |
| `copilot-cli.excluded_tools`              | string  | Space-separated excluded tools. Maps to `--excluded-tools`.                      |
| `copilot-cli.mcp_config`                  | string  | MCP server configuration (inline JSON or file path). The adapter adds the `@` prefix for file paths automatically. Maps to `--additional-mcp-config`. |
| `copilot-cli.disable_builtin_mcps`        | boolean | If `true`, passes `--disable-builtin-mcps`. Default: `false`.                   |
| `copilot-cli.no_custom_instructions`      | boolean | If `true`, passes `--no-custom-instructions`. Default: `false`.                 |
| `copilot-cli.experimental`                | boolean | If `true`, passes `--experimental`. Default: `false`.                           |

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

sh -c 'copilot -p "$1" \
  --output-format json \
  -s \
  --allow-all \
  --autopilot \
  --no-ask-user \
  --max-autopilot-continues 50' -- "$prompt"
```

Working directory: `/var/sortie/workspaces/PROJ-123`

### Turn 2 (continuation)

```bash
prompt='Continue working on the issue. The previous turn made progress but tests are still failing. Focus on fixing the remaining test failures.'
session_id='ec41fbe2-af76-4185-ccb9-a61461234abc'

sh -c 'copilot -p "$1" \
  --output-format json \
  -s \
  --allow-all \
  --autopilot \
  --no-ask-user \
  --max-autopilot-continues 50 \
  --resume "$2"' -- "$prompt" "$session_id"
```

Working directory: `/var/sortie/workspaces/PROJ-123`

### With model override and tool restriction

```bash
sh -c 'copilot -p "$1" \
  --output-format json \
  -s \
  --allow-all \
  --autopilot \
  --no-ask-user \
  --max-autopilot-continues 30 \
  --model gpt-5 \
  --deny-tool "bash(rm -rf *)"' -- "$prompt"
```

---

## ACP Alternative (Reference Only)

The [CLI command reference][cli-ref] lists `--acp` as "Start the Agent Client Protocol server."
ACP is an open standard for client-agent communication.

> **Status:** The `--acp` flag exists but the official documentation provides no details on the
> ACP protocol format, message types, or session management API for Copilot CLI. Sortie's
> initial adapter should use the CLI subprocess model. ACP may be considered for a future
> iteration once documentation matures.

### Starting an ACP server

```bash
copilot --acp
```

This starts a server process. The transport mechanism (stdio, TCP, or other) is not
specified in the CLI documentation.

### ACP vs. subprocess tradeoffs

| Aspect             | Subprocess (`-p`)                           | ACP (`--acp`)                                |
| ------------------ | ------------------------------------------- | -------------------------------------------- |
| Process lifecycle  | New subprocess per turn                     | Long-running process across turns            |
| Startup overhead   | Node.js startup per turn (~1-2s)            | One startup, then low-latency messages       |
| Documentation      | Well-documented flags and behavior          | Flag exists; protocol undocumented           |
| Session management | `--resume <session_id>` per invocation     | Unknown                                      |
| Error recovery     | Process crash = turn failure, clean restart | Process crash = all sessions lost            |
| Complexity         | Simple: spawn, read, kill                   | Unknown: protocol details not published      |

---

## SDK Alternative (Reference Only)

GitHub provides a TypeScript SDK (`@github/copilot-sdk`, v0.2.0) for programmatic control of
Copilot CLI via JSON-RPC. The SDK internally spawns the CLI and communicates over stdio or TCP.

> **Status:** The SDK is in technical preview and may change in breaking ways.

### SDK architecture

```typescript
import { CopilotClient, approveAll } from "@github/copilot-sdk";

const client = new CopilotClient({
    useStdio: true,          // stdio transport (default)
    githubToken: "gho_...",  // Optional: override auth
});
await client.start();

const session = await client.createSession({
    model: "gpt-5",
    onPermissionRequest: approveAll,
});

const result = await session.sendAndWait({
    prompt: "Fix the authentication bug",
});

await session.disconnect();
await client.stop();
```

### Key SDK concepts relevant to adapter design

| Concept                   | SDK behavior                                               | Adapter relevance                                               |
| ------------------------- | ---------------------------------------------------------- | --------------------------------------------------------------- |
| Permission handling       | `onPermissionRequest` callback required on every session.   | Confirms that headless mode needs explicit permission handling. |
| Permission result kinds   | `approved`, `denied-interactively-by-user`, `denied-by-rules`, `denied-by-content-exclusion-policy` | Maps to Sortie's approval policy.     |
| Session events            | `user.message`, `assistant.message`, `assistant.message_delta`, `tool.execution_start`, `tool.execution_complete`, `session.idle` | **Same vocabulary as `--output-format json`.** Confirmed experimentally in v1.0.13: JSONL stdout, session storage `events.jsonl`, and SDK all use the same event type names. |
| Infinite sessions         | Background compaction at configurable thresholds.           | Confirms Copilot CLI handles context limits internally.         |
| Multiple sessions         | Independent sessions with different models.                 | Confirms per-issue session isolation.                            |
| Streaming                 | `assistant.message_delta` for incremental text.            | Useful for stall detection.                                      |
| Custom tools              | `defineTool()` with Zod schemas, handler callbacks.        | Not applicable for subprocess model.                             |
| System message override   | `customize` (per-section) or `replace` mode.               | Reference for prompt injection behavior.                         |

Sortie does **not** use the SDK because the adapter is implemented in Go and the architecture
mandates subprocess-based integration (Section 10.7). The CLI provides a clean, language-agnostic
integration surface. The SDK documentation is useful as a reference for expected event types and
session behavior, as the SDK and CLI share the same underlying agent engine.

---

## Fleet Mode (Informational)

Copilot CLI's `/fleet` command breaks implementation plans into independent subtasks and executes
them in parallel using subagents. Each subagent has its own independent context window.

This is relevant to Sortie because:

- **Parallel execution occurs inside the CLI,** not managed by Sortie. The adapter treats a
  `/fleet`-based session as a single turn regardless of internal parallelism.
- **Subagents use separate model configurations.** By default subagents use a low-cost model,
  but the prompt can override this per-subtask.
- **Premium request consumption increases** with subagent parallelism. Each subagent interaction
  consumes premium requests independently.
- **The `subagentStop` hook** can intercept and block subagent completion.

Sortie's orchestrator manages its own concurrency model (per-issue sessions with
`max_concurrent_agents`). The CLI's internal parallelism via `/fleet` is transparent to the
adapter.

---

## Differences from Claude Code Adapter

| Aspect                    | Claude Code                                                   | Copilot CLI                                                   |
| ------------------------- | ------------------------------------------------------------- | ------------------------------------------------------------- |
| Binary                    | `claude` (npm `@anthropic-ai/claude-code`)                   | `copilot` (npm `@github/copilot`)                             |
| Runtime                   | Node.js                                                       | Node.js 22+                                                   |
| Authentication            | `ANTHROPIC_API_KEY` (Anthropic), Bedrock, Vertex              | GitHub token (`GH_TOKEN`, `GITHUB_TOKEN`, `COPILOT_GITHUB_TOKEN`) |
| Permission bypass         | `--dangerously-skip-permissions`                             | `--allow-all` / `--yolo`                                      |
| User input suppression    | Implicit with `-p` + `--dangerously-skip-permissions`         | Explicit `--no-ask-user` flag                                  |
| Autonomous continuation   | Runs full loop by default with `-p`                          | Requires `--autopilot` flag                                    |
| Output format flag        | `--output-format stream-json`                                | `--output-format json`                                         |
| Session continuation      | `--resume <session_id>` or `--continue`                      | `--resume <session_id>` or `--continue`                        |
| Deterministic session ID  | `--session-id <uuid>` (pre-assign before first turn)         | Not documented; session ID captured from output                |
| Session storage path      | `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`        | `~/.copilot/session-state/<session-id>/`                       |
| Context management        | Context compaction at token limit                             | Infinite sessions with background compaction                   |
| Cost cap                  | `--max-budget-usd <amount>`                                  | No documented CLI flag; controlled by subscription quota       |
| Internal turn limit       | `--max-turns <N>` (may be SDK-only)                          | `--max-autopilot-continues <N>`                                |
| Result event              | Final `result` message with `subtype`, `is_error`, `usage`   | `result` event with `sessionId`, `exitCode`, `usage{premiumRequests, totalApiDurationMs, sessionDurationMs, codeChanges}` |
| Init event                | `system` type with `init` subtype, contains `session_id`     | No init event; `sessionId` is in the final `result` event  |
| Hooks location            | `.claude/hooks.json`                                          | `.github/hooks/*.json`                                         |
| OTel env var              | `CLAUDE_CODE_ENABLE_TELEMETRY=1`                             | `OTEL_EXPORTER_OTLP_ENDPOINT`                                  |
| MCP config                | `--mcp-config <path>`, `--strict-mcp-config`                 | `--additional-mcp-config <json>`, `--disable-builtin-mcps`    |
| Built-in MCP servers      | None by default                                               | `github-mcp-server`, `playwright`, `fetch`, `time`            |
| Models                    | Claude family (Sonnet, Opus, Haiku)                           | Multi-provider: Claude Sonnet 4.5 (default), GPT-5, etc.     |

---

## Summary: Adapter Implementation Checklist

1. **StartSession:** Store workspace path and config. Validate `copilot` binary on `$PATH` and
   Node.js 22+ availability. Verify authentication environment. Do not launch subprocess yet.
2. **RunTurn (turn 1):** Launch `copilot -p <prompt> --output-format json -s --allow-all
   --autopilot --no-ask-user --max-autopilot-continues <N>` with cwd = workspace. Parse JSONL
   stdout. Capture session ID from output.
3. **RunTurn (turn 2+):** Same as turn 1 but append `--resume <session_id>` (from `result`
   event's `sessionId` field).
4. **Event parsing:** Parse `"type"` field. Map `assistant.message` → `assistant_message`,
   `tool.execution_start` → `tool_use`, `tool.execution_complete` → `tool_result`,
   `result` → extract session ID and usage. Log unknown types as `other_message`.
5. **Result handling:** Treat exit code 0 as `turn_completed`, non-zero as `turn_failed`, signal
   death as `turn_cancelled`.
6. **Token tracking:** Extract `outputTokens` from `assistant.message` events and
   `premiumRequests` from the `result` event. For input token counts, enable OTel.
7. **Timeout:** Enforce `turn_timeout_ms` via context deadline + SIGTERM/SIGKILL.
8. **StopSession:** Kill subprocess if running (SIGTERM → grace → SIGKILL).
9. **Error mapping:** Check exit code and stderr output. Map to architecture error categories.
10. **Session continuity:** Use `--resume <session_id>` for multi-turn conversations.
11. **Permissions:** Default to `--allow-all --no-ask-user` for headless operation.
12. **Autopilot:** Default to `--autopilot --max-autopilot-continues <N>` for autonomous task
    completion.
13. **MCP extensions:** Optionally pass `--additional-mcp-config` for tool extensions.

### Verification items

Resolved items (verified experimentally on v1.0.13):

- [x] JSONL event schema: `"type"` field discriminator, event types documented above.
- [x] JSONL events match the SDK vocabulary (`user.message`, `assistant.message`, etc.), not
  the old hook-style format (`"event"` field) from issue #2201.
- [x] Session ID is in the `result` event's `sessionId` field.
- [x] `--no-ask-user` flag exists and works (confirmed in `copilot --help` v1.0.13).
- [x] Without `--autopilot`, the agent exits after one response in `-p` mode.

Remaining items requiring further verification:

- [ ] Exit code behavior for different failure modes (authentication failure, API error, timeout).
  Auth failure observed: emits `session.warning` + `session.mcp_server_status_changed` before
  erroring to stderr (exit code non-zero).
- [ ] Whether `--resume` works reliably with session IDs from the `result` event.
- [ ] Interaction between `--max-autopilot-continues` and process exit code.
- [ ] Session storage cleanup behavior when workspaces are removed.
- [ ] ACP protocol details (`--acp`): transport mechanism, message format, session management.
- [ ] Token resolution order when all sources have valid but different tokens (GH_TOKEN vs
  GITHUB_TOKEN precedence — the CLI falls back on failure so the practical impact is minimal).

[cli-help-ref]: https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference "GitHub Copilot CLI command reference (includes flags such as --no-ask-user)"

---

## Sources

[cli-ref]: https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference
[cli-prog]: https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-programmatic-reference
[hooks-ref]: https://docs.github.com/en/copilot/reference/hooks-configuration
[hooks-about]: https://docs.github.com/en/copilot/concepts/agents/coding-agent/about-hooks
[hooks-tut]: https://docs.github.com/en/copilot/tutorials/copilot-cli-hooks
