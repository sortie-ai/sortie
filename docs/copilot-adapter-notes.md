# GitHub Copilot CLI: adapter research notes

> GitHub Copilot CLI v1.0.13 (npm `@github/copilot`, binary `copilot`), researched March 2026.
> Instruments: the official documentation listed below, headless runs of the installed binary
> with `-p --output-format json` and stdout captured, and reads of the on-disk session-state
> journal. Reference for the Copilot CLI `AgentAdapter` in `internal/agent/copilot`.
>
> Coverage: the flag surface, the JSONL event schema, session-id capture, autopilot behavior,
> and session-state token accounting are anchored to v1.0.13. The `--additional-mcp-config`
> file syntax and the exit-0 outcome of a config parse failure are anchored to v1.0.21 (April
> 2026). No other surface has been re-observed on v1.0.21.
>
> Primary sources: [CLI command reference][cli-ref], [CLI programmatic reference][cli-prog],
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
   [architecture Section 10.7](architecture/10-agent-adapter-contract.md#107-local-subprocess-launch-contract) (Local Subprocess Launch Contract).
2. **Agent Client Protocol (ACP)** via `copilot --acp` ([CLI command reference][cli-ref]),
   which starts an ACP server. The official documentation carries no protocol details for it.
3. **TypeScript SDK** (`@github/copilot-sdk`), which internally communicates with the CLI process
   via JSON-RPC. Not usable from a Go adapter directly, but serves as reference for session
   behavior and event types.

Sortie's Go adapter uses the CLI subprocess approach (surface 1). The ACP and SDK surfaces are
documented here only as reference for expected behavior.

---

## Installation and prerequisites

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

The CLI resolves authentication tokens in a precedence order with fallback on failure. The
official documentation ([authenticate-copilot-cli][auth-ref]) states the order as
`COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN` (in order of precedence).

The CLI implements try-and-fallback, not exclusive selection. Setting `COPILOT_GITHUB_TOKEN` to
an invalid token while `GH_TOKEN` or `GITHUB_TOKEN` hold valid tokens does not cause failure:
the CLI falls back to the next source. So the precedence order matters only when all sources
hold valid but different tokens.

| Priority | Method                       | Environment Variable / Mechanism         | Notes                                                              |
| -------- | ---------------------------- | ---------------------------------------- | ------------------------------------------------------------------ |
| 1        | Copilot-specific env var     | `COPILOT_GITHUB_TOKEN`                   | Highest priority. Dedicated to Copilot CLI.                        |
| 2        | GitHub env var (primary)     | `GH_TOKEN`                               | Per official docs. Shared with `gh` CLI.                           |
| 3        | GitHub env var (secondary)   | `GITHUB_TOKEN`                           | Per official docs. Common in CI environments.                      |
| 4        | OAuth keychain               | System keychain / credential store       | From interactive `/login` device flow.                              |
| 5        | `gh` CLI fallback            | `gh auth token`                          | Uses the `gh` CLI's stored credential if available.                |

The precedence order is immaterial to the adapter: `checkAuth` in
`internal/agent/copilot/copilot.go` checks that at least one source is present, not which one
the CLI will select. It accepts a non-empty `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or
`GITHUB_TOKEN`, and otherwise falls back to a successful `gh auth status` (logged at WARN).
With no source at all it returns `agent_not_found` before any turn runs.

[auth-ref]: https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/authenticate-copilot-cli

### Supported token types

| Token type                    | Prefix          | Notes                                               |
| ----------------------------- | --------------- | --------------------------------------------------- |
| OAuth token (device flow)     | `gho_`          | Created via interactive `/login`.                   |
| Fine-grained PAT              | `github_pat_`   | Requires the "Copilot Requests" permission scope.   |
| GitHub App user-to-server     | `ghu_`          | For GitHub App integrations.                         |
| Classic PAT                   | `ghp_`          | Acceptance is unresolved. See "Open questions".      |

### Config mapping

| Sortie config field        | Value                                        |
| -------------------------- | -------------------------------------------- |
| `agent.kind`               | `copilot-cli`                                |
| `agent.command`            | `copilot` (or full path to the binary)       |

The adapter does not manage GitHub tokens directly. The token must be present in the
environment of the Sortie process (or passed through via hook env). The subprocess inherits the
parent process environment.

Organization and enterprise restrictions: if the user's Copilot access is provided via an
organization or enterprise, the administrator must enable the Copilot CLI policy in organization
settings. The CLI fails to authenticate when this policy is disabled.

---

## CLI flags reference

`buildArgs` in `internal/agent/copilot/command.go` constructs the argument vector from these
flags. Flags marked (always) appear on every invocation; the rest are conditional.

### Core flags

| Flag                               | Description                                                                                        | Adapter usage                                                    |
| ---------------------------------- | -------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `-p <prompt>` / `--prompt`         | Non-interactive (headless) mode. Passes the prompt and exits when done.                            | (always) Carries the rendered turn prompt.                       |
| `-s` / `--silent`                  | Suppress stats and decoration, outputting only the agent's response ([CLI programmatic reference][cli-prog]).  | (always) Prevents non-JSON text from polluting stdout.           |
| `--output-format json`             | JSONL on stdout. Each line is a JSON object.                                                       | (always) For structured event parsing.                           |
| `--model <model>`                  | Override the AI model (e.g., `claude-sonnet-4.5`, `gpt-5`).                                       | From `copilot-cli.model`.                                        |
| `--agent <name>`                   | Use a specific custom agent for the session.                                                       | From `copilot-cli.agent`.                                        |
| `--no-ask-user`                    | Disable the `ask_user` tool. Agent works autonomously without requesting user input ([`copilot --help`][cli-help-ref]). | (always) Prevents stalls waiting for user input.                 |
| `--additional-mcp-config <json>`   | Add MCP server configuration for the session (inline JSON or `@<path>` to JSON file).              | Tool extensions. See "MCP server configuration".                 |
| `--disable-builtin-mcps`           | Disable all built-in MCP servers.                                                                  | From `copilot-cli.disable_builtin_mcps`.                         |
| `--disable-mcp-server <name>`      | Disable a specific built-in MCP server.                                                            | Not passed by the adapter.                                       |
| `--no-custom-instructions`         | Disable loading custom instructions from workspace files.                                          | From `copilot-cli.no_custom_instructions`.                       |
| `--secret-env-vars <vars>`         | Redact the values of specified environment variables in output.                                     | Not passed by the adapter.                                       |
| `--share <path>`                   | Export session transcript to markdown file on completion (prompt mode only).                        | Not passed by the adapter.                                       |
| `--experimental`                   | Enable experimental features.                                                                      | From `copilot-cli.experimental`. Feature set unenumerated; see "Open questions". |

### Session management flags

| Flag                           | Description                                                         | Adapter usage                                                          |
| ------------------------------ | ------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `--resume <session_id>`        | Resume a specific conversation by session ID.                       | Continuation turns with a known session ID.                            |
| `--continue`                   | Resume the most recent conversation in the working directory.       | Continuation turns after a turn produced no session ID.                |

Both cases are described in "Session continuation".

### Permission flags

| Flag                       | Description                                                                                                    | Adapter usage                                                                                                                           |
| -------------------------- | -------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `--allow-all` / `--yolo`   | Grant all permissions: tools, paths, and URLs. Agent operates without any approval prompts.                    | Passed when no tool-scoping key is configured. Without it, and without scoped grants, the process hangs waiting for interactive approval. |
| `--allow-all-tools`        | Allow all tools without confirmation, but still require path/URL approval.                                      | Not passed by the adapter.                                                                                                              |
| `--allow-all-paths`        | Disable path verification for file operations.                                                                  | Not passed by the adapter.                                                                                                              |
| `--allow-all-urls`         | Disable URL verification for fetch operations.                                                                  | Not passed by the adapter.                                                                                                              |
| `--allow-tool <tools>`     | Allow specific tools without confirmation. Supports glob patterns (e.g., `"bash(git *)"`, `"edit_file"`).      | From `copilot-cli.allowed_tools`.                                                                                                       |
| `--deny-tool <tools>`      | Deny specific tools. Takes precedence over `--allow-tool`. Supports glob patterns.                              | From `copilot-cli.denied_tools`.                                                                                                        |

`--allow-all` and `--yolo` allow arbitrary command execution and file modification. Per
[architecture Section 10.4](architecture/10-agent-adapter-contract.md#104-approval-tools-and-user-input-policy), Sortie adopts a high-trust posture where approval requests must not leave a
run stalled, so `buildArgs` grants `--allow-all` whenever the workflow configures none of
`allowed_tools`, `denied_tools`, `available_tools`, or `excluded_tools`. Configuring any of
those switches the invocation to scoped grants instead, because `--allow-all` would override
them. Workspace isolation and the hook system operate as additional defense in depth.

### Tool filter flags

| Flag                          | Description                                                                         | Adapter usage                                     |
| ----------------------------- | ----------------------------------------------------------------------------------- | ------------------------------------------------- |
| `--available-tools <tools>`   | Restrict the set of tools available to the agent. Only listed tools are accessible. | From `copilot-cli.available_tools`.                |
| `--excluded-tools <tools>`    | Remove specific tools from the available set.                                       | From `copilot-cli.excluded_tools`.                 |

Tool names follow the CLI's tool vocabulary ([CLI command reference: tool availability][cli-ref]).
Built-in tools include: `bash`, `view`, `edit_file` (shown as `edit` in some CLI docs,
backed by `apply_patch`), `create`, `apply_patch`, `glob`, `grep`, `web_fetch`, `ask_user`,
`task`, `report_intent`, `show_file`, `store_memory`, `task_complete`, `exit_plan_mode`.

### Autopilot flags

| Flag                                | Description                                                                                               | Adapter usage                                                          |
| ----------------------------------- | --------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `--autopilot`                       | Enable autopilot mode. Agent continues working through steps autonomously until task completion.           | (always) Without it the agent stops after one response.                |
| `--max-autopilot-continues <count>` | Limit the number of autonomous continuation steps. Prevents runaway loops.                                | (always) From `copilot-cli.max_autopilot_continues`, default 50.       |

Autopilot mode is distinct from `--allow-all`. `--allow-all` grants permission for tool
execution. `--autopilot` controls whether the agent continues working through multi-step tasks
without waiting for user input between steps. Both are needed for fully autonomous headless
operation.

### Config and settings flags

| Flag                    | Description                                      | Adapter usage                                                  |
| ----------------------- | ------------------------------------------------ | -------------------------------------------------------------- |
| `--config-dir <path>`   | Set the configuration directory.                | Not passed by the adapter.                                     |
| `--log-dir <path>`      | Set the log output directory. Log file names contain the session ID. | Not passed by the adapter.                 |

---

## Subprocess invocation

Per [architecture Section 10.7](architecture/10-agent-adapter-contract.md#107-local-subprocess-launch-contract), each turn is one subprocess. `agentcore.ForkPerTurnSession.RunTurn`
execs the resolved binary directly with the argument vector `buildArgs` returns, on every
platform. No shell is involved, so a prompt carrying shell metacharacters from user-controlled
issue content cannot be reinterpreted. In SSH mode (`LaunchTarget.RemoteCommand` non-empty) the
same vector is quoted into an `ssh` invocation by `sshutil.BuildSSHArgs`.

```
copilot -p <prompt> --output-format json -s --autopilot --no-ask-user \
  --max-autopilot-continues <N> [--resume <session_id> | --continue] \
  [--model <model>] [--agent <name>] \
  [--allow-all | --allow-tool <tools> --deny-tool <tools> --available-tools <tools> --excluded-tools <tools>] \
  [--additional-mcp-config <value>] [--disable-builtin-mcps] [--no-custom-instructions] [--experimental]
```

The flags after `--max-autopilot-continues` appear in that order, each one conditional on the
workflow configuration described in "Adapter-specific pass-through config".

`procutil.SetProcessGroup` puts the child in its own process group; on Windows it also sets
`CREATE_NEW_PROCESS_GROUP` and `procutil.AssignProcess` attaches the child to a Job Object for
process tree termination.

### Process settings

| Setting           | Value                                               | Rationale                                                  |
| ----------------- | --------------------------------------------------- | ---------------------------------------------------------- |
| Working directory | Workspace path (`StartSessionParams.WorkspacePath`) | Agent must operate in the issue workspace.                 |
| Stdout            | Pipe (read by adapter)                              | JSONL output parsed line by line.                          |
| Stderr            | Pipe (drained by `procutil.NewStderrCollector`)     | Diagnostic output, not structured.                         |
| Environment       | Inherited from Sortie process                       | GitHub token and other auth vars must be present.          |
| Max line size     | 10 MB (`stdoutScannerMaxTokenSize`)                 | A longer line ends the scan and fails the turn.            |

### Session continuation

The `result` event, the last JSONL line before process exit, carries the session ID at the top
level:

```json
{"type":"result","timestamp":"...","sessionId":"aa778ea0-6eab-4ce9-b87e-11d6d33dab4f","exitCode":0,"usage":{...}}
```

The adapter's `OnFinalize` hook stores that `sessionId` in its session state, and `buildArgs`
passes it as `--resume <session_id>` on the next turn. Resuming preserves the full message
history from prior turns.

When a turn ends with no `result` event carrying a `sessionId` and no ID is known from an
earlier turn, the adapter sets `fallbackToContinue` and the next turn passes `--continue`
instead, which resumes the most recent conversation in the working directory. That is
unambiguous here because Sortie isolates one workspace per issue. Scanning the session-state
directory is not a session-discovery path: with `max_concurrent_agents > 1` the most recently
written directory is not necessarily this session's, so the adapter opens that tree only by
known session ID (`sessionStateRoot` and `readSessionUsage` in
`internal/agent/copilot/sessionstate.go`).

---

## Output format: `--output-format json`

The [CLI command reference][cli-ref] documents `--output-format=FORMAT` where FORMAT is `text`
(the default) or `json`. Under `json` the CLI writes one JSON object per line to stdout
(newline-delimited JSON, JSONL), and each line is independently parseable.

### JSONL event schema

GitHub does not publish this schema; it comes from captured stdout.

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

**Event types:**

| Event type                        | Ephemeral | `data` fields                                                                              | Adapter mapping           |
| --------------------------------- | --------- | ------------------------------------------------------------------------------------------ | ------------------------- |
| `session.warning`                 | yes       | `warningType`, `message`                                                                   | WARN log and `notification` carrying `message` |
| `session.mcp_server_status_changed` | yes     | `serverName`, `status`                                                                     | DEBUG log only            |
| `session.mcp_servers_loaded`      | yes       | `servers` (array of `{name, status, source, error?}`)                                      | DEBUG log only            |
| `session.tools_updated`           | yes       | `model`                                                                                    | DEBUG log only            |
| `session.info`                    | yes       | `infoType`, `message`                                                                      | `notification` carrying `message` |
| `session.task_complete`           | no        | `summary`, `success`                                                                       | `notification` carrying `summary` |
| `user.message`                    | no        | `content`, `transformedContent`, `attachments`, `agentMode`?, `interactionId`              | DEBUG log only            |
| `assistant.turn_start`            | no        | `turnId`, `interactionId`                                                                  | `notification` carrying the event type |
| `assistant.message_delta`         | yes       | `messageId`, `deltaContent`                                                                | empty `notification`, which resets the orchestrator's stall timer |
| `assistant.message`              | no        | `messageId`, `content`, `toolRequests` (array), `interactionId`, `outputTokens`            | `token_usage` when `outputTokens` is present, plus a `notification` summarizing content or requested tool names |
| `assistant.turn_end`             | no        | `turnId`                                                                                   | `notification` carrying the event type |
| `tool.execution_start`           | no        | `toolCallId`, `toolName`, `arguments`                                                      | `notification` naming the tool; starts the duration timer in `agentcore.ToolTracker` |
| `tool.execution_complete`        | no        | `toolCallId`, `model`, `interactionId`, `success`, `result`, `toolTelemetry`               | `tool_result` with tool name, duration, and the negated `success` as `ToolError` |
| `result`                          | no        | *(top-level)* `sessionId`, `exitCode`, `usage{premiumRequests, totalApiDurationMs, sessionDurationMs, codeChanges{linesAdded, linesRemoved, filesModified}}` | Held as the turn's terminal record; see "Determining turn completion" |

**Example output** (simple task, autopilot mode):

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

**Example with tool use** (read file task):

```jsonl
{"type":"assistant.message","data":{"messageId":"...","content":"","toolRequests":[{"toolCallId":"toolu_vrtx_...","name":"view","arguments":{"path":"/tmp/copilot-test/main.go"},"type":"function","intentionSummary":"view the file..."}],"interactionId":"...","outputTokens":102},"id":"...","timestamp":"...","parentId":"..."}
{"type":"tool.execution_start","data":{"toolCallId":"toolu_vrtx_...","toolName":"view","arguments":{"path":"/tmp/copilot-test/main.go"}},"id":"...","timestamp":"...","parentId":"..."}
{"type":"tool.execution_complete","data":{"toolCallId":"toolu_vrtx_...","model":"claude-opus-4.6","interactionId":"...","success":true,"result":{"content":"1. package main\n2. ","detailedContent":"..."},"toolTelemetry":{"properties":{"command":"view"},"metrics":{"resultLength":19}}},"id":"...","timestamp":"...","parentId":"..."}
```

Three surfaces share one event vocabulary. The on-disk `events.jsonl` under the session-state
root uses the same `"type"` field and event type names as stdout JSONL, and the SDK's session
event types (`user.message`, `assistant.message`, `tool.execution_start`, and the rest) are the
same names again rather than a separate format.

An event type outside the table above is emitted as an `other_message` event carrying the type
name, so an unrecognized event neither fails the turn nor is silently dropped.

### Parsing strategy

`agentcore.ForkPerTurnSession` reads stdout line by line and hands each line to the adapter's
`ParseLine` hook. A line that fails to parse as JSON produces a `malformed` event and the scan
continues. A parsed line is dispatched on its `"type"` field to the mapping in the table above.
The `result` line is returned to the skeleton as the turn's terminal record instead of being
emitted, and the skeleton keeps the last such record for finalization.

### Determining turn completion

The process exits when the agent completes its work, so a turn has two outcome signals: the
`result` event, which carries `exitCode`, `sessionId`, and `usage`, and the process exit code.

`OnFinalize` builds `agentcore.TurnEvidence` from both and delegates the decision to
`agentcore.FinalizeTurn`. A `result` event is authoritative: an `exitCode` of 0 reports
`TerminalSuccess`, and any other value, including an absent field, reports `TerminalFailure`
regardless of the process exit code. With no `result` event the
decision falls to the process exit and to the work evidence, which for this adapter is whether
any `assistant.message` reported a positive `outputTokens` for the turn. See "Process exit
codes" for the resulting dispositions.

### Token usage extraction

stdout carries no input token counts anywhere: the `result` event's `usage` object
(`premiumRequests`, `totalApiDurationMs`, `sessionDurationMs`, `codeChanges`) and the
`assistant.message` event's `data.outputTokens` are the only token-shaped fields on the JSONL
stream, and neither reports input tokens. The adapter uses `data.outputTokens`, summed across
the turn's `assistant.message` events, as an in-turn output estimate only.

The runtime's own accounting lives in a session-state journal on disk:
`<COPILOT_HOME or ~/.copilot>/session-state/<session id>/events.jsonl`. The adapter reads this
file after the subprocess exits. The last line whose `type` is `session.shutdown` carries the
authoritative totals for the session:

```json
{
  "type": "session.shutdown",
  "data": {
    "tokenDetails": {"input": {"tokenCount": 10}, "cache_read": {"tokenCount": 154053}, "cache_write": {"tokenCount": 38948}, "output": {"tokenCount": 596}},
    "modelMetrics": {
      "claude-sonnet-5": {
        "requests": {"count": 5},
        "usage": {"inputTokens": 193011, "outputTokens": 596, "cacheReadTokens": 154053, "cacheWriteTokens": 38948, "reasoningTokens": 149}
      }
    }
  }
}
```

`modelMetrics` is preferred when present, summed across every model entry; `tokenDetails` is
the fallback. The two shapes agree by construction: `tokenDetails.input.tokenCount +
tokenDetails.cache_read.tokenCount + tokenDetails.cache_write.tokenCount` equals
`modelMetrics.<model>.usage.inputTokens` (10 + 154053 + 38948 = 193011 in the example above).

The journal is session-cumulative across every process invocation that resumes the session with
`--resume`: a second turn's `session.shutdown` record reports totals inclusive of the first
turn's spend, not just the second turn's own contribution. `sessionState.recoverUsage` recovers
a single run's contribution by reading the record that predates the run (its own baseline) and
subtracting it from the current record.

The journal read is skipped, leaving the output-only provisional figure standing, when the
session ID is unknown or fails the path-segment check `sessionIDPattern`, when the session runs
in SSH mode, when the file exceeds 64 MB or holds a line over 10 MB (`readSessionUsage` returns
`errSessionStateCapExceeded`), when the file holds no `session.shutdown` record yet, or when a
resumed run has already missed its first read attempt and can no longer separate its own spend
from the prior session's.

The adapter normalizes a `session.shutdown` record into:

```
TokenUsage{
    InputTokens:     modelMetrics.<model>.usage.inputTokens summed across models,
    OutputTokens:    modelMetrics.<model>.usage.outputTokens summed across models,
    TotalTokens:     InputTokens + OutputTokens,
    CacheReadTokens: modelMetrics.<model>.usage.cacheReadTokens summed across models,
}
```

OTel spans ([CLI command reference: OTel monitoring][cli-ref]) also expose token counts, and the
adapter does not read them. See "OpenTelemetry integration".

---

## Session lifecycle mapping

[Architecture Section 10.2](architecture/10-agent-adapter-contract.md#102-session-lifecycle) defines the session lifecycle. `CopilotAdapter` in
`internal/agent/copilot/copilot.go` maps onto it as follows.

### `StartSession`

The Copilot CLI session is disk-persisted under the session-state root and identified by a
UUID, while the OS subprocess is short-lived and created per turn. `StartSession` therefore
establishes the logical session and spawns nothing.

`agentcore.ResolveLaunchTarget` resolves and normalizes the workspace path under the
containment rules, and resolves the `copilot` command (from `agent.command`, defaulting to
`copilot`) to an absolute path. In local mode the adapter then runs `copilot --version` under a
5-second timeout as a canary; a failure returns `agent_not_found` with a message pointing at
the Node.js 22+ requirement, which is the only check that requirement gets. `checkAuth` runs
next, as described in "Token resolution order". SSH mode skips both.

The returned `domain.Session` carries `ID` set to `StartSessionParams.ResumeSessionID`, which
is empty for a new session and populated when a prior attempt is being resumed, and `Internal`
set to the adapter's `sessionState` (launch target, session ID, agent config, MCP config path,
usage accumulator, and per-turn scan state).

### `RunTurn`

`RunTurn` resets the per-turn scan state and delegates to
`agentcore.ForkPerTurnSession.RunTurn`, which builds the argument vector from `buildArgs`,
launches the subprocess with cwd set to the workspace path, emits `session_started` with the
PID and the current session ID, scans stdout, and calls the adapter's `OnFinalize` hook once
the process exits. Each emitted event updates the orchestrator's stall reference timestamp.
`OnFinalize` stores the `sessionId` from the `result` event, recovers token usage, and returns
the `TurnResult`.

### `StopSession`

`StopSession` delegates to `ForkPerTurnSession.Stop`, which signals the process group
gracefully (`SIGTERM` on POSIX, `CTRL_BREAK_EVENT` on Windows), waits up to 5 seconds for the
turn goroutine to finish its cleanup, and then force-terminates the process tree (`SIGKILL` to
the group on POSIX, `TerminateJobObject` on Windows). With no subprocess running it returns
`nil` immediately.

### `EventStream`

Returns `nil`. The adapter delivers events synchronously through the `OnEvent` callback, not
through a channel.

---

## Turn model

Copilot CLI has its own internal concept of autonomous continuations controlled by autopilot mode.
The `--max-autopilot-continues` flag limits how many autonomous steps the agent takes within a
single CLI invocation.

In autopilot mode the agent continues working through steps until it determines the task is
complete, encounters a problem, or reaches the continuation limit. Without `--autopilot`, the
agent completes one interaction cycle and exits.

| Aspect                        | With `--autopilot`                                        | Without `--autopilot`                                |
| ----------------------------- | --------------------------------------------------------- | ---------------------------------------------------- |
| `user.message.data.agentMode` | `"autopilot"`                                             | absent                                               |
| After first response          | Sends autopilot continuation prompt, continues working    | Exits immediately with `result` event                |
| Continuation prompt           | "You have not yet marked the task as complete using the task_complete tool..." | N/A                                   |
| `session.info` events         | `infoType: "autopilot_continuation"` with premium request count | absent                                        |
| `task_complete` tool          | Agent calls `task_complete` to signal completion          | Not invoked; process exits after first response      |

Without `--autopilot` the agent produces one response and exits rather than running a
multi-step agentic loop, so the flag is what makes a non-trivial headless task possible. That
is why `buildArgs` passes it unconditionally.

Sortie's turn model is distinct from Copilot CLI's internal continuations. A Sortie turn is one
CLI invocation (one `RunTurn` call), and within that invocation Copilot CLI runs its own
agentic loop to completion, bounded by `--max-autopilot-continues`. After each turn the
orchestrator re-checks tracker state and decides whether to run another.

---

## Timeout enforcement

| Timeout                  | Source      | Enforcement                                                                                                                                                                                           |
| ------------------------ | ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.turn_timeout_ms`  | WORKFLOW.md | Reaches the adapter on `domain.AgentConfig`, and no code path reads it. See "Open questions".                                                                                                          |
| `agent.read_timeout_ms`  | WORKFLOW.md | The adapter enforces no read deadline. The worker uses this value only to bound its best-effort `StopSession` call.                                                                                    |
| `agent.stall_timeout_ms` | WORKFLOW.md | Enforced by the orchestrator, not the adapter. `reconcileStalled` in `internal/orchestrator/reconcile.go` compares `LastAgentTimestamp` against the threshold and cancels the worker's context.        |

`--max-autopilot-continues <N>` is a step-based limit rather than a time-based one. Each
continuation consumes one or more premium requests, so the limit bounds both runaway execution
and API cost.

### Context cancellation

`RunTurn` receives the worker's context. On cancellation, whether from stall reconciliation or
from the tracker reporting the issue terminal, `exec.CommandContext` signals the process group
gracefully through `procutil.SignalGraceful` and force-terminates the tree after the 5-second
`WaitDelay`. The skeleton reports the turn as `turn_cancelled` with `ErrTurnCancelled`,
carrying whatever usage had accumulated.

---

## Permission and approval policy

Per [architecture Section 10.4](architecture/10-agent-adapter-contract.md#104-approval-tools-and-user-input-policy), Sortie adopts a high-trust posture:

| Policy                    | Implementation                                                                                                                                                                                           |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Auto-approve all actions  | `--allow-all` / `--yolo` bypasses all permission prompts for tools, paths, and URLs. Passed under the condition described in "Permission flags".                                                         |
| User input suppression    | `--no-ask-user` disables the `ask_user` tool ([`copilot --help`][cli-help-ref]). The agent makes decisions autonomously. Passed on every invocation, so a turn never ends in `turn_input_required`.       |
| Autopilot permissions     | When entering autopilot mode without `--allow-all`, the CLI prompts for permissions. In headless mode (`-p`) that prompt stalls, so an operator who configures tool scoping must scope it wide enough to cover the run. |
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

## Error detection and mapping

### Process exit codes

Copilot CLI documents no per-failure-mode exit codes, so any non-zero value means only
"failure" and stderr carries the diagnosis. `procutil.EmitWarnLines` logs the collected stderr
lines at WARN whenever a turn ends in an error.

| Outcome                                                | Exit reason      | Error category    |
| ------------------------------------------------------ | ---------------- | ----------------- |
| `result` event with `exitCode` 0                       | `turn_completed` | none              |
| `result` event with non-zero `exitCode`                | `turn_failed`    | `turn_failed`     |
| No `result` event, process exits 0, output tokens seen  | `turn_completed` | none              |
| No `result` event, process exits 0, no output tokens    | `turn_failed`    | `turn_failed`     |
| No `result` event, process exits non-zero               | `turn_failed`    | `port_exit`       |
| Exit 127 (`copilot` not found)                          | `turn_failed`    | `agent_not_found` |
| Killed by signal                                        | `turn_cancelled` | `turn_cancelled`  |
| Stdout line over 10 MB, or another scanner error        | `turn_failed`    | `port_exit`       |

The zero-output row is the safety heuristic: a process that exits 0 having produced nothing is
reported as a failure rather than a silent success, and `FinalizeTurn` logs "agent exited
without producing output, treating as failure".

### Authentication failures

| Condition                                  | Behavior                                                  | Adapter mapping         |
| ------------------------------------------ | --------------------------------------------------------- | ----------------------- |
| No token source present                    | `checkAuth` rejects the session before any launch          | `agent_not_found`       |
| Token present but rejected by the API      | CLI fails during the API call and exits non-zero            | `port_exit`             |
| Organization policy disables Copilot CLI   | CLI fails during authentication                            | `port_exit`             |

### Error category mapping (per [architecture Section 10.5](architecture/10-agent-adapter-contract.md#105-timeouts-and-error-mapping))

`StartSession` produces these categories before any subprocess runs:

| Condition                                       | Error category          |
| ----------------------------------------------- | ----------------------- |
| Workspace path invalid or not a directory       | `invalid_workspace_cwd` |
| `copilot` not resolvable, or `agent.command` empty | `agent_not_found`    |
| `copilot --version` canary fails                | `agent_not_found`       |
| No GitHub authentication source                 | `agent_not_found`       |

Turn outcomes use the categories in the exit-code table above. `response_timeout`,
`turn_timeout`, and `turn_input_required` are unreachable for this adapter: it enforces no read
or turn deadline, and `--no-ask-user` is passed on every invocation.

### Known issues in the wild

From [github/copilot-cli issues](https://github.com/github/copilot-cli/issues):

- **Session file corruption** ([#2012](https://github.com/github/copilot-cli/issues/2012)):
  raw U+2028/U+2029 characters in `events.jsonl` break `JSON.parse()` on `--resume`, which
  breaks session continuation.
- **Subprocess I/O deadlock** ([#1838](https://github.com/github/copilot-cli/issues/1838)):
  in Nix and direnv environments the CLI hangs on a subprocess I/O deadlock. Stall
  reconciliation is what ends such a turn.
- **Headless server fd leaks** ([#2389](https://github.com/github/copilot-cli/issues/2389)):
  when running as a headless server, kqueue file descriptors leak and the bash tool stops
  working after prolonged use.
- **Authentication failures without output** ([#2184](https://github.com/github/copilot-cli/issues/2184)):
  the CLI fails to start without any output when there is a login issue. The turn ends on the
  process exit, which the zero-output row maps to `turn_failed`.
- **sessionStart hook fires after userPromptSubmitted** ([#2201](https://github.com/github/copilot-cli/issues/2201)):
  the `sessionStart` hook fires after `userPromptSubmitted`, not before.
- **`--additional-mcp-config` requires the `@` prefix for file paths**: a bare file path is
  parsed as inline JSON and fails with `Invalid JSON in --additional-mcp-config`. The
  documented syntax for file input is `@<path>` (`github/copilot-cli#428`).
- **Config parsing errors exit 0 without output**: when Copilot CLI fails to parse
  `--additional-mcp-config`, it exits 0 without emitting any JSONL events. The zero-output row
  maps this to `turn_failed`, and the WARN-level stderr log carries the parse failure.

---

## Session storage

Copilot CLI persists sessions to disk under `<COPILOT_HOME or ~/.copilot>/session-state/<session
id>/`. Each session directory contains:

| File/Directory    | Description                                                        |
| ----------------- | ------------------------------------------------------------------ |
| `events.jsonl`    | Full event log for the session (all messages and tool calls).      |
| `workspace.yaml`  | Workspace metadata (cwd, git root, repository, branch).           |
| `plan.md`         | The agent's implementation plan (if plan mode was used).           |
| `checkpoints/`    | Checkpoints for infinite session context compaction.               |
| `files/`          | Files tracked by the session.                                      |

`--resume <session_id>` reads the conversation back from this directory, and the adapter reads
`events.jsonl` for token totals as described in "Token usage extraction". Long sessions
accumulate checkpoint data here; Sortie removes workspaces but not session-state directories.

### Infinite sessions and context compaction

Copilot CLI uses "infinite sessions" by default. When the context window approaches capacity:

1. Background compaction starts at ~80% context usage (configurable via SDK).
2. Processing blocks at ~95% context usage until compaction completes.
3. Compaction summarizes older conversation history, preserving recent context.

So a single Copilot CLI session runs indefinitely without hitting context limits, and the
adapter manages no context window of its own.

---

## Hooks integration

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

Sortie does not manage Copilot CLI's hook system. Hook files are workspace-level configuration
that the coding agent or an `after_create` workspace hook sets up. They still change what a
turn does: a `preToolUse` hook returning `"deny"` blocks a tool call, an `agentStop` hook
returning `"block"` prevents the session from completing, and each hook adds latency to tool
execution (30 seconds per hook by default), which lengthens the wall-clock turn a stall timeout
has to accommodate.

---

## MCP server configuration

The `--additional-mcp-config <json>` flag adds MCP server declarations for the session.
It accepts inline JSON or `@<path>` referencing a JSON file. The `@` prefix is required
for file paths: a bare path is parsed as inline JSON and fails. The format follows the
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
the `env` field. Unlike `--mcp-config` in Claude Code, `--additional-mcp-config` is additive:
it supplements rather than replaces Copilot CLI's built-in MCP servers (`github-mcp-server`,
`playwright`, `fetch`, `time`).

### Disabling built-in MCP servers

Use `--disable-builtin-mcps` to disable all built-in MCP servers, or
`--disable-mcp-server <name>` to disable a specific one.

### Sortie adapter usage

`orchestrator.GenerateMCPConfig` writes `.sortie/mcp.json` into the workspace directory, and
`buildArgs` passes that path as `--additional-mcp-config @<path>`. If the operator also sets
`copilot-cli.mcp_config` in WORKFLOW.md, the worker merges both server sets into that one file;
a name collision on the reserved `sortie-tools` key fails the attempt. Credential values are
never written to the file, and reach the MCP server through inherited environment variables
instead. See ADR-0009 for the full merge algorithm.

When no generated config exists, `formatMCPConfigValue` passes an operator `mcp_config` value
through on its own: inline JSON (a value starting with `{`) and a value already prefixed with
`@` go through unchanged, and any other value is treated as a file path and given the `@`
prefix.

---

## OpenTelemetry integration

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

The adapter neither sets these variables nor parses OTel output: its event source is the JSONL
stdout stream and its token source is the session-state journal, both of which need no extra
runtime configuration. An operator who exports one of the variables into Sortie's environment
gets OTel in the subprocess as well, since the subprocess inherits that environment, and the
`invoke_agent` span then carries the per-request `gen_ai.usage.input_tokens` and
`gen_ai.usage.output_tokens` breakdown that the JSONL stream does not.

---

## Adapter-specific pass-through config

Per [architecture Section 5.3.5](architecture/05-workflow-specification.md#535-agent-object), the adapter reads pass-through config from the `copilot-cli`
sub-object in WORKFLOW.md:

| Config key                                | Type    | Description                                                                      |
| ----------------------------------------- | ------- | -------------------------------------------------------------------------------- |
| `copilot-cli.model`                       | string  | Model override (e.g., `claude-sonnet-4.5`, `gpt-5`). Maps to `--model`.         |
| `copilot-cli.max_autopilot_continues`     | integer | Maximum autonomous continuation steps. Maps to `--max-autopilot-continues`. A value of zero or less is replaced by 50. |
| `copilot-cli.agent`                       | string  | Custom agent name. Maps to `--agent`.                                            |
| `copilot-cli.allowed_tools`               | string  | Space-separated tool allow list. Maps to `--allow-tool`.                         |
| `copilot-cli.denied_tools`                | string  | Space-separated tool deny list. Maps to `--deny-tool`.                           |
| `copilot-cli.available_tools`             | string  | Space-separated available tools restriction. Maps to `--available-tools`.        |
| `copilot-cli.excluded_tools`              | string  | Space-separated excluded tools. Maps to `--excluded-tools`.                      |
| `copilot-cli.mcp_config`                  | string  | MCP server configuration (inline JSON or file path). Merged into the generated config, or passed through as described in "MCP server configuration". Maps to `--additional-mcp-config`. |
| `copilot-cli.disable_builtin_mcps`        | boolean | If `true`, passes `--disable-builtin-mcps`. Default: `false`.                   |
| `copilot-cli.no_custom_instructions`      | boolean | If `true`, passes `--no-custom-instructions`. Default: `false`.                 |
| `copilot-cli.experimental`                | boolean | If `true`, passes `--experimental`. Default: `false`.                           |

---

## ACP alternative (reference only)

The [CLI command reference][cli-ref] lists `--acp` as "Start the Agent Client Protocol server."
ACP is an open standard for client-agent communication. `copilot --acp` starts that server
process. The official documentation carries no ACP protocol format, message types, session
management API, or transport mechanism for Copilot CLI, so the surface is unusable as an
integration target on the evidence available. See "Open questions".

### ACP versus subprocess tradeoffs

| Aspect             | Subprocess (`-p`)                           | ACP (`--acp`)                                |
| ------------------ | ------------------------------------------- | -------------------------------------------- |
| Process lifecycle  | New subprocess per turn                     | Long-running process across turns            |
| Startup overhead   | Node.js startup per turn (~1-2s)            | One startup, then low-latency messages       |
| Documentation      | Well-documented flags and behavior          | Flag exists; protocol undocumented           |
| Session management | `--resume <session_id>` per invocation     | Unknown                                      |
| Error recovery     | Process crash = turn failure, clean restart | Process crash = all sessions lost            |
| Complexity         | Simple: spawn, read, kill                   | Unknown: protocol details not published      |

---

## SDK alternative (reference only)

GitHub provides a TypeScript SDK (`@github/copilot-sdk`, v0.2.0) for programmatic control of
Copilot CLI via JSON-RPC. The SDK internally spawns the CLI and communicates over stdio or TCP.
It is labeled a technical preview.

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
| Session events            | `user.message`, `assistant.message`, `assistant.message_delta`, `tool.execution_start`, `tool.execution_complete`, `session.idle` | The shared vocabulary described in "JSONL event schema".        |
| Infinite sessions         | Background compaction at configurable thresholds.           | Confirms Copilot CLI handles context limits internally.         |
| Multiple sessions         | Independent sessions with different models.                 | Confirms per-issue session isolation.                            |
| Streaming                 | `assistant.message_delta` for incremental text.            | Useful for stall detection.                                      |
| Custom tools              | `defineTool()` with Zod schemas, handler callbacks.        | Not applicable for subprocess model.                             |
| System message override   | `customize` (per-section) or `replace` mode.               | Reference for prompt injection behavior.                         |

Sortie does not use the SDK: the adapter is written in Go and the architecture mandates
subprocess-based integration ([Section 10.7](architecture/10-agent-adapter-contract.md#107-local-subprocess-launch-contract)), for which the CLI is the language-agnostic surface.
The SDK documentation stays useful as a reference for event types and session behavior, because
the SDK and CLI share the same underlying agent engine.

---

## Fleet mode

Copilot CLI's `/fleet` command breaks implementation plans into independent subtasks and executes
them in parallel using subagents, each with its own context window. Parallel execution happens
inside the CLI, so the adapter sees a `/fleet` session as one turn regardless of the internal
parallelism, and Sortie's own concurrency model (per-issue sessions bounded by
`max_concurrent_agents`) is unaffected. Subagents default to a low-cost model that the prompt
can override per subtask, each subagent interaction consumes premium requests independently, and
the `subagentStop` hook can intercept and block subagent completion.

---

## Differences from the Claude Code adapter

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

## Open questions

- Whether a classic PAT (`ghp_*`) authenticates the CLI. One observation has a classic PAT
  failing authentication, and no official documentation states such a restriction. Settle it by
  running one headless turn per token type with every other source unset, and comparing the
  stderr messages.
- Exit codes per failure mode (rejected credential, upstream API error, backend timeout). An
  authentication failure emits `session.warning` and `session.mcp_server_status_changed`, then
  errors to stderr and exits non-zero; the other modes have not been induced. Settle it by
  forcing each failure and recording exit code and stderr.
- Whether `--resume` accepts a session ID taken from the `result` event on every path,
  including after a turn that ended in failure. Settle it by running a two-turn session with a
  forced failure on the first turn and checking that the second turn's `user.message` history
  includes the first prompt.
- How `--max-autopilot-continues` interacts with the process exit code when the limit is
  reached. Settle it by running a task that cannot finish within a limit of 1 and reading the
  `result` event's `exitCode` and the process exit code.
- Whether Copilot CLI ever removes session-state directories on its own. Settle it by listing
  the session-state root before and after a long-running session and a CLI restart.
- What `--experimental` enables. The flag surfaces no feature list in `copilot --help`. Settle
  it by diffing `copilot --help` and the JSONL event stream of one identical turn run with and
  without the flag.
- ACP protocol details for `--acp`: transport, message format, and session management. Settle
  it by starting `copilot --acp` under a timeout with stdout captured and inspecting the first
  frames it emits, or by finding an upstream schema dump.
- Which token source the CLI selects when all sources hold valid but different tokens. The
  fallback-on-failure behavior makes this immaterial to the adapter, which only checks that one
  source exists. Settle it by setting three valid tokens for distinct accounts and reading back
  the authenticated identity.
- Whether `agent.turn_timeout_ms` is meant to bind this adapter. The value reaches
  `domain.AgentConfig.TurnTimeoutMS` and no code reads it, so a turn is bounded only by
  `stall_timeout_ms` and worker cancellation. Settle it against the agent adapter contract in
  `docs/architecture/10-agent-adapter-contract.md`, not by probing the CLI.

[cli-help-ref]: https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference "GitHub Copilot CLI command reference (includes flags such as --no-ask-user)"

---

## Sources

[cli-ref]: https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference
[cli-prog]: https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-programmatic-reference
[hooks-ref]: https://docs.github.com/en/copilot/reference/hooks-configuration
[hooks-about]: https://docs.github.com/en/copilot/concepts/agents/coding-agent/about-hooks
[hooks-tut]: https://docs.github.com/en/copilot/tutorials/copilot-cli-hooks
