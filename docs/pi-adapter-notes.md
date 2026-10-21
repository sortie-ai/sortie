# Pi adapter notes

Working notes for anyone changing Sortie's Pi adapter in `internal/agent/pi`: what `pi -p --mode json` actually writes to stdout, why the exit code is not a failure signal, how session identity and resume work, and why Sortie's own tools do not reach the runtime.

## Where to get the volatile facts

Nothing here pins a flag list or a model name. Read the argument surface off `pi --help` on the version you are targeting, and take the provider and authentication surface from the documentation the installed release ships. The adapter's `command.go` defines what Sortie sends and `parse.go` defines what it reads; the event vocabulary checked there is the 1.3.0 schema, so re-derive it from a live run after any version bump rather than extending the accepted set on the assumption that a new type is harmless.

## Launch contract

- The prompt is written to standard input, not passed on the command line. pi treats a message argument beginning with `@` as a file to include, and Linux limits a single argument string to 128 KiB. Over SSH the prompt follows the environment preamble on standard input.
- A credential-verification run adds `--no-session`, so it leaves no session file in the runtime's session store.

## The surface Sortie drives

The adapter launches one subprocess per turn:

```
pi -p --mode json [--session <id> | --no-session] [--model <name>] \
   [--approve | --no-approve]
```

`-p` is the non-interactive prompt mode, `--mode json` is the machine-readable stream, and the prompt arrives on standard input. The process working directory is already the issue workspace, so no directory flag is sent, and the workspace is not named on the command line. The adapter's own flags, not the workflow's `agent.command`, decide the binary: `agent.command` overrides the program and may carry leading arguments, and the adapter's arguments follow them.

stdout carries one JSON object per line, framed by a session header before anything else:

```
{"type":"session","version":3,"id":"<uuid>","timestamp":"...","cwd":"<dir>"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_start","message":{...}}
{"type":"message_update","assistantMessageEvent":{...}}
{"type":"message_end","message":{...}}
{"type":"tool_execution_start","toolCallId":"...","toolName":"...","args":{...}}
{"type":"tool_execution_update","toolCallId":"...","partialResult":{...}}
{"type":"tool_execution_end","toolCallId":"...","toolName":"...","result":{...},"isError":false}
{"type":"turn_end","message":{...},"toolResults":[...]}
{"type":"agent_end","messages":[...],"willRetry":false}
```

Those are the families the adapter reads. The full 1.3.0 set also contains `agent_settled`, `bash_execution_update`, `queue_update`, `entry_appended`, `session_info_changed`, `thinking_level_changed`, `compaction_start`, `compaction_end`, `auto_retry_start`, `auto_retry_end`, `summarization_retry_scheduled`, `summarization_retry_attempt_start`, and `summarization_retry_finished`. Most of them are framing with nothing for the orchestrator to record, and several only appear in an interactive session. The exception is `tool_execution_end`, covered below.

A line that is not a JSON object carrying a `type`, and a `type` outside that set, are both reported as a malformed line rather than skipped. That is deliberate. A future event the adapter silently dropped could carry the only text, the only usage, or the only failure the turn produced, and a stream that has moved past what this adapter understands is worth an operator's attention rather than a quiet empty turn. The same holds for the `message_update` delta types (`text_start`, `text_delta`, `text_end`, `thinking_start`, `thinking_delta`, `thinking_end`, `toolcall_start`, `toolcall_delta`, `toolcall_end`, `start`, `done`, `error`): a variant outside the set is a fault.

## The two shapes that decide how a line is read

**`message_update` is delta-only.** The CLI strips both the event's own cumulative `message` and the `partial` field inside its `assistantMessageEvent`, so only the delta event's fields carry information. Do not build a message from the deltas: they arrive in pieces, and the authoritative record is the `message` on `message_end`.

**`turn_end` repeats `message_end`.** Both name the same assistant response for one pi turn, and `turn_end` additionally carries the tool results. The adapter treats `message_end` as authoritative for the response's text and stop reason, and `turn_end` as the fallback for a stream that delivered only one of the two; the figure the turn reports about usage is taken from whichever of the two arrives, and a stream carrying both does not count the response twice. `agent_end` repeats neither: it carries the whole message list, so reading it for text or usage would report the same response several times over. That is a reason to ignore it, not a reason to read it differently.

## Exit codes and failure

`pi --mode json` applies no exit-code check to a failed model response. A response that stopped with `stopReason` `error` or `aborted` still exits 0, so the exit code cannot decide whether a turn succeeded. The stop reason on the assistant message is the only evidence: the adapter records the failure of the last assistant response, and the finalize path turns it into a terminal failure with error kind `response_error`, taking the response's `errorMessage` as the message and falling back to naming the stop reason when the runtime carried none. A bounded excerpt of the turn's stderr is appended when there is one, and the collected stderr lines are logged as warnings.

The last response decides because pi retries a response that stopped with `error` inside the same process (`agent_end` with `willRetry` true, `auto_retry_start`, a new `agent_start`): a retry that succeeds makes the turn succeed, and a turn whose retries all fail reports the last error. Usage of every response, including each retry, is summed. A turn that produced no assistant output at all takes the shared zero-work row and is reported as a failure rather than a silent success, which is also where a stream that never produced a JSON event and exited 0 lands.

The first-JSON deadline defaults to thirty seconds and takes the workflow's read timeout when one is configured. It is armed only until the first JSON event arrives, so it guards a subprocess that never starts talking; a mid-turn stall is the orchestrator's `stall_timeout_ms`. Once the subprocess has been reaped, the remaining stdout is drained for a bounded grace so a descendant holding the output handle cannot withhold the turn's result.

The session header is checked, not merely recorded. Its `cwd` must be the turn's own workspace, and on a resumed session its id must be the id Sortie already holds. Either mismatch kills the process and fails the turn with `response_error` naming both values, because a stream that reports a different workspace or a different session is not the session this turn was supposed to continue.

A turn whose process exits on its own as the context is cancelled is reported by its real outcome, a success or a failure, rather than as cancelled.

Each tool result the adapter reports carries a duration and, for a tool that errored, the result text truncated to 500 characters. The duration is the `durationMs` pi's `tool_execution_end` carries (pi 1.2.0 and later), and the adapter's own measurement from the tool's start event when pi omits it.

## Session identity and resume

The session id comes from the header, not from Sortie. `StartSession` stores the orchestrator's resume id when there is one and the header supplies one when there is not, and a `session_started` event is emitted exactly once per Sortie session, even across a resume, so a continuation turn does not look like a new session to the dashboard.

Once an id is known, every later turn of that session passes `--session <id>`. The first turn of a new session carries no such flag. A resumed session therefore re-sends its own id in every header, which is what makes the mismatch check above meaningful rather than a comparison against a value the adapter itself chose.

## Native flags

A key outside the accepted set is refused rather than ignored: pi has no passthrough channel, so an unrecognized key would look configured while doing nothing.

- `model` becomes `--model`.
- `effort` is accepted and not read. The registered kind declares effort as not forwarded, so the orchestrator reports the advisory `agent.effort.not_forwarded` for a workflow that reaches a pi session with `effort` set, and pi runs at its own default thinking level. Accepting the key keeps a workflow shared with other kinds from being refused.
- `mcp_config` is a path to the operator's own MCP server file. It is checked to be a string and is not delivered: the adapter hands pi no MCP configuration (see Sortie's own tools).
- `project_trust` decides between `--approve` and `--no-approve`. The default is `ignore`, which sends `--no-approve` and keeps the workspace's own project-local files and packages out of the run. An unattended turn has no operator to answer a trust prompt, so the permissive value has to be asked for by name rather than inherited from a developer's shell.

## Permissions

pi's own approval prompt cannot be answered by an unattended turn, so the adapter always sends one of two flags and never leaves the choice to pi's defaults. `project_trust` selects between them: `ignore`, the default, sends `--no-approve`, and `approve` sends `--approve`. With `--no-approve` the project-local files and packages of the workspace, which come from the repository an issue's agent works on, are not loaded. `approve` is the operator's explicit decision to run what the workspace declares. Any other value is refused.

The JSON event stream documents no approval request or human-input event, so the adapter performs no refusal or permission classification. A tool call that fails, whatever the reason, appears as a `tool_execution_end` with `isError` set.

## Usage accounting

The registered kind declares turn-end arrival and per-model attribution. Each pi model response carries one `usage` figure on the `turn_end` (or `message_end`) message, and the turn's own figure is the sum of every such response. Two normalizations matter:

- `reasoning` is a subset of `output`, not an addition to it, so it is never summed a second time. A thinking block is also excluded from the text the turn reports, for the same reason: the runtime already counts its tokens inside the response's output usage.
- `cacheRead` is part of the input the provider billed, so it is added into the input total, and it is also reported as its own subset counter. `cacheWrite` is likewise folded into the input total.

The model the turn reports is the model of the last response whose usage was counted, which is the right answer for a turn that ran on more than one model. A run-cumulative total is kept per Sortie session across every turn of that session, and a turn that produced no usage at all reports no figure rather than a zero that would lower an already-reported run.

Compaction is an extra model call, so it is an extra charge, and the adapter does not count it. `compaction_start` and `compaction_end` are recognized as framing events, and a `compaction_end` result's `usage` figure is not read. The usage a turn reports therefore leaves out the summarization calls pi's auto-compaction makes, and a session that compacts reports fewer tokens than the provider billed.

A turn is settled once pi ends its run with `agent_end` and no retry pending. A turn that served output (a delta, a response, a tool result) and ends without that event, for example because it was stopped or timed out, is reported as spend not fully accounted for. The adapter's total is its own sum of this process's turns, not a session total the runtime reports, so a turn never covers the spend of earlier turns.

## SSH

The adapter builds the same ssh argument vector the other fork-per-turn kinds use, from the shared helper, with the same per-turn arguments: a remote session runs the same `pi -p --mode json ...` command on the remote host, in the remote workspace, with the same session flag. `AgentPID` is left unset for a remote turn, since the pid belongs to the ssh process rather than to pi.

A remote launch takes its ssh options from the shared launch target, the same path `opencode` uses. It carries the names listed under `worker.ssh_pass_env` that are set and non-empty in Sortie's environment, minus those under `worker.ssh_disallow_pass_env`, applies the target's host-key checking setting, and unsets a withheld variable for the remote command. The kind declares no credential variables of its own, so nothing is carried that the operator did not list. Carried values and the prompt travel on the ssh connection's standard input, the values first, which is why the remote host needs `dd` once a variable is carried; see the SSH section of the workflow reference.

## Sortie's own tools

The kind registers `MCPInjectionUnsupported`. pi loads MCP servers from its own `mcp.json` and from extensions, and the adapter does not pass it the generated `.sortie/mcp.json`, add a flag for it, or set an environment variable for it. Sortie's tools are therefore neither advertised to nor callable by a pi session, local or remote, and the servers a `mcp_config` key names are not delivered either.

The preflight reports this: `agent.kind.no_tool_channel` warns that the kind has no tool execution channel, and a `mcp_config` key in a `pi` block draws `agent.mcp_config`. Both are warnings, so a workflow that does not need Sortie's tools runs unchanged.

## Failure modes worth recognizing

- **Exit 0 with a failed turn.** A provider failure, such as a rejected model name or an authentication error, can end an assistant response with stop reason `error` while the process exits 0. The turn still fails, as `response_error`, from that stop reason. A report of a failed turn with exit status 0 is this, not an adapter fault.
- **Early exit.** A process that exits before it wrote any readable line is reported as an early exit carrying its exit status and the end of its stderr, so a wrong `agent.command`, a missing binary, or an option pi rejects shows up with pi's own usage text rather than as an empty turn. It is not reported when the turn was cancelled or the session was stopped.
- **No first event.** A subprocess that stays alive without printing a JSON event is killed when the first-JSON deadline passes, and the turn fails with `response_timeout` and the message `timed out waiting for first pi json event`. A credential or provider prompt that waits on a terminal is the usual cause.
- **Empty turn.** An exit-0 turn with no assistant text, reasoning, or tool call takes the shared zero-work row and fails, so it is retried instead of counting as work done.
- **SSH connection failure.** On a remote launch, exit status 255 with no assistant failure on the stream is reported as a failed SSH connection, not as a pi failure. The same status with an assistant failure on the stream is reported as that failure.
- **Wrong workspace or session.** The header check described under Exit codes and failure fails the turn with `response_error` naming both values.

## Verifying a change

The package's own tests drive a fake `pi` written as a shell script, so argument construction, the parser, usage accounting, session resume, and the main exit paths are covered hermetically. There is no live-runtime suite in this package; a change to the event vocabulary is therefore only as trustworthy as the fixtures, so a version bump should re-run the turn against the real binary and diff the stream against the recorded fixtures before the accepted sets are widened.
