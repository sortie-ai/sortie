# Pi adapter notes

Working notes for anyone changing Sortie's Pi adapter in `internal/agent/pi`.

Last updated: November 2026

## Where to get the volatile facts

Nothing here pins a flag list, an event vocabulary, or a model name. Read the flags off `pi --help` on the version you target. The minimum version is in the workflow reference. Take providers, authentication, sessions, and project trust from the documentation the installed release ships, in its `docs` directory. The JSON stream is described in its `json.md`. The adapter's `command.go` defines what Sortie sends and `parse.go` defines what it reads. The event and delta types `parse.go` accepts are a closed set: a type outside it is reported as a malformed line, so re-derive the set from a live run after a pi upgrade instead of widening it on the assumption that a new type is harmless.

## The surface we drive

One subprocess per turn: `pi -p --mode json`, with the prompt on standard input and the issue workspace as the process working directory. `-p` is the non-interactive mode and `--mode json` writes one JSON object per stdout line. No directory flag is sent.

The prompt is not an argument because pi treats a message argument that begins with `@` as a file to include, and a long prompt can exceed the argument limit. Over SSH the prompt follows the environment preamble on standard input.

A credential verification session adds `--no-session`, so it leaves no session file in pi's session store. A reduced one, which the orchestrator asks for first, also adds `--no-tools`, an empty `--tools` list, `--no-context-files`, `--no-skills`, `--system-prompt` with the shared replacement prompt, and an empty `--append-system-prompt`, so its request carries no tool definitions, no `AGENTS.md` or `CLAUDE.md`, no skills, no built-in system prompt, and no `APPEND_SYSTEM.md`. pi lets a `--tools` list override `--no-tools`, so the empty list follows it and keeps a configured command that carries its own `--tools` from giving the request tools. pi discovers `APPEND_SYSTEM.md` in the user's agent directory, and in the workspace's `.pi` directory when the project is trusted, whatever the other flags say; an explicit `--append-system-prompt` replaces that discovery, and an empty value appends nothing. pi keeps every `--append-system-prompt` value, so a value the configured command passes for it stays in the reduced request; `--system-prompt` and `--tools` take their last occurrence, so the adapter's values replace the command's. The kind declares that its verification request keeps none of these.

Two shapes decide how a line is read:

- `message_update` is delta-only. The CLI strips the cumulative `message` and the `partial` member of its `assistantMessageEvent`, so only the delta's own fields carry information. The authoritative record of a response is the `message` on `message_end`.
- `turn_end` repeats `message_end` for the same assistant response and adds the tool results. `agent_end` carries the whole message list, so reading it for text or usage would count a response several times.

## Turn lifecycle

Turns run on `agentcore.ForkPerTurnSession`, the skeleton the other fork-per-turn kinds use. Launch, output reading, the reap, the stop, and the order in which a turn's endings win belong to the skeleton, so a lifecycle change goes there and reaches every kind. This package supplies what is pi's own, through the skeleton's hooks:

- `BuildArgs` returns the run arguments. `PromptOnStdin` is set.
- `FirstEventTimeout` is `agentcore.ReadTimeout` of the agent configuration: `agent.read_timeout_ms`, 5 seconds unless the workflow sets another value. A process that prints nothing parseable within it is stopped and the turn fails with `response_timeout`. The deadline ends at the first accepted line, so it catches a process that never talks, not a stall during a turn. Stalls belong to the orchestrator's `stall_timeout_ms`.
- `ParseLine` maps each line to events. A line that is not a JSON object with a `type` is returned as an error and the skeleton reports it as malformed. A payload that does not decode, an unknown event type, and an unknown delta type are reported as malformed by the adapter. `agent_end` without a retry pending returns a non-nil result, which marks the run as ended.
- `SettleUsage` settles the turn's responses through the usage ledger into the session total.
- `OnFinalize` builds the turn evidence from the last response's stop reason.
- `GetUsage` and `GetSessionID` read the session state.

pi emits `message_update`, `tool_execution_update`, `turn_start`, `compaction_*`, and `auto_retry_*` events during a turn. Each is reported as an empty notification so the orchestrator's stall timer sees activity during long output or a long tool run.

## Session identity and resume

The session id comes from the `session` header that opens the stream. `StartSession` holds the orchestrator's resume id only when pi's session file for it is present on the session's host, and the header supplies the id otherwise. pi stores each session as `<agent dir>/sessions/<project directory>/<timestamp>_<id>.jsonl`, with the agent directory taken from `PI_CODING_AGENT_DIR` or `~/.pi/agent`, and the check looks only in the directory named for the workspace, because `pi --session <id>` opens a session found under another project directory through a prompt on standard input, where the first line of the turn's prompt would be read as the answer and the turn would end without running. pi derives the directory name from its physical working directory: the path without its leading separator, with every separator and colon replaced by `-`, between `--` marks. A local launch resolves symlinks in the workspace path first. A remote launch uses the workspace path as given, so a symlinked remote workspace finds no file and starts a new session. A workspace path holding `*`, `?`, `[` or `]` gives a directory name the file matcher would read as a wildcard, so the check gives no answer and every resume starts a new session. A resume id with no such file starts a new session, and the session reports that it carried no history, because `pi --session <id>` exits non-zero with `No session found matching` for an id it cannot find. A lost SSH connection during the check fails the start, and the retry resumes the same session. `PI_CODING_AGENT_DIR` belongs in the environment Sortie runs in, or on the remote host for an SSH launch; a value set only inside the agent command is not seen. A `--session-dir` argument, `PI_CODING_AGENT_SESSION_DIR`, or a `sessionDir` setting in the project or user `settings.json` that moves the session files elsewhere makes the check find none, so every resume starts a new session. pi reads `sessionDir` before it resolves project trust, so it applies under `--no-approve` too. Every later turn passes `--session <id>`. `session_started` is emitted once per Sortie session, so a continuation turn does not look like a new session.

The header is checked. A different id on a session that already has one ends the turn with `response_error`.

The header's `cwd` is checked on a local launch: it must be the same directory as the turn's workspace. pi can move a session to the directory the stored session was created in, so a header naming another directory means the turn is not working in the issue workspace. The comparison uses the file system's own identity of the two directories rather than their text, because pi reports the physical path of a workspace reached through a symbolic link, such as `/var` on macOS, which is a link to `/private/var`.

On a remote launch the workspace path names the remote host's file system, which Sortie cannot inspect, so only the session id is checked.

## Permissions

pi's project trust decides whether pi loads the resources a working folder supplies: its `.pi` settings, MCP servers, extensions, skills, prompt templates, and system prompt files. An unattended turn cannot answer pi's trust prompt, so the adapter always passes one of pi's two trust flags and defaults to the one that loads nothing. A repository cannot make the agent load those resources unless the operator opts in.

Context files such as `AGENTS.md` and `CLAUDE.md` load whatever the trust decision is, and pi has no approval prompt per tool call. The workflow reference describes the limits of `pi.approve`; pi's own `docs/security.md` states them for pi.

The JSON stream has no approval request event, so the adapter does no permission classification. A tool call that fails appears as a `tool_execution_end` with `isError` set.

## Usage accounting

The kind registers one live usage source, the run's own output, with per-turn spend that arrives during the turn. Each assistant response is one provider request and carries one `usage` figure on its `message_end`. The request's identity is the response's `responseId`, or its `timestamp` when pi names no id. `turn_end` carries the same response and the same identity, so a response is never counted twice. Two responses with no id and the same millisecond timestamp share an identity and count once, at the larger of their two readings. A response read at `message_end` counts before the tools it requested run, so a turn stopped during a tool keeps the usage of the responses that finished.

The turn's figure is settled from the responses it read, summed per serving model, so a turn that used two models attributes each its own share. Per-request spend would not do this: the usage component attributes the whole figure of such a turn to the model of its last request. Each measured response is still reported as one `token_usage` event as soon as it is read, from `message_end` or from the `turn_end` fallback.

- `reasoning` is a subset of `output` and is not added again.
- `cacheRead` and `cacheWrite` are part of the input pi billed. Each is added into the input total and reported as its own subset counter.
- The model of a response is `provider/model`. pi reports `responseModel` only when the provider answered with a model other than the one requested, as an alias or a router does, and then the model is `provider/responseModel`, so spend is attributed to the model that served the request.
- A response without a usage object, or whose counts are all zero, as a failed retry attempt reports, is not a measurement and reports no figure. A turn that served output and has no measured response is reported as spend not fully accounted for. So is a turn with measured responses and also a response that ended with a stop reason other than `error` or `aborted` and carried no measurement, because that response was a billed request left out of the sum. A response that ended with `error` or `aborted`, carried no measurement and no output is a request the provider did not bill and leaves the turn accounted. With output, it counts as the turn reaching the model and leaves the turn not fully accounted for, because the output was produced and not measured. An empty text block and an empty thinking block are not output. An all-zero reading is not passed to the usage component as a measured zero: a turn that reached the model cannot settle on a reading that adds nothing. An assistant `message_end` or `turn_end`, or a `usage` entry, that does not decode is reported as a malformed line and leaves the turn not fully accounted for, because the request it describes is missing from the sum; an entry of another type that does not decode does not. A `message_end`, or a `turn_end` with no `message_end` before it, that does not decode also discards the stop reason of the earlier responses, and the turn is reported as `turn_outcome_unknown`.
- A response whose usage names no identity, with neither `responseId` nor `timestamp`, cannot be told apart from a repeat of another response. It reports no `token_usage` event and is left out of the turn's figure. The turn's other responses still count, and the turn's spend is reported as not fully accounted for.

pi reports no session total of its own, so the total is the adapter's sum of this process's responses. The per-model split of the total is settled when the turn ends.

A turn is settled once pi ends its run with `agent_end` and no retry pending. A turn that served output and ends without that event, for example because it was stopped, is reported as spend not fully accounted for.

pi keeps a prompt cache warm by resending the request with a one-token output cap, which its default `streaming` cache-warming mode does during a long run, and reports each successful request as an `entry_appended` event whose entry has type `usage` and kind `cache_warm`. The adapter counts it as a request, identified by the entry's `id`, with the entry's provider and model, beside the responses of the turn, so the turn's figure and request count include it.

Compaction is an extra model call that no `message_end` reports. A turn in which pi emits `compaction_start`, or an `entry_appended` event for a `compaction` entry, which an extension commits without `compaction_start`, is reported as spend not fully accounted for, and the responses counted before and after it still settle as the turn's figure, which leaves out the summarization calls. A `compaction_end` that is neither aborted nor carries an `errorMessage` also counts as the turn reaching the model, so a turn that ends after a completed compaction with no assistant output is reported as spend not fully accounted for.

## Usage limits

The kind declares that it does not recognize a usage-limit refusal. pi reports a refused request as an assistant response with stop reason `error` and an `errorMessage` that holds the provider's own text, prefixed with an HTTP status. The status is not reliable evidence: a request through a gateway that refused with an upstream 402 arrived as `404: {...[402]: ...}`. The message carries no structured limit type, no reset instant, and no model scope, and the text differs per provider, so recognizing it would mean matching message text. Such a refusal fails the turn as `turn_failed` with the provider's message. Revisit this when pi's stream carries a structured refusal field.

## SSH

A remote session runs the same `pi -p --mode json ...` command on the remote host, built with the shared helper the other fork-per-turn kinds use, with the same session flag. The launch carries the variables `worker.ssh_pass_env` lists. The kind declares no credential variables of its own. Carried values and the prompt travel on the connection's standard input, so the remote host needs `dd` once a variable is carried.

## Failure detection

`pi --mode json` applies no exit-code check to a failed model response, so a response that stopped with `error` or `aborted` still exits 0. The stop reason of the last assistant response is the evidence. pi retries a failed response inside the same process, with `agent_end` carrying `willRetry` true, so only the last response decides the turn. pi can also start another run in the same process after an `agent_end` with no retry pending. An `agent_start` clears the ended state, so a later run that exits before its own `agent_end` falls to the exit-based rows and its spend is reported as not fully accounted for.

When the run ends with `agent_end` and no retry pending, the last response's stop reason sets the turn's outcome:

| Stop reason | Outcome |
| --- | --- |
| `stop` | Success |
| `length` | `turn_token_limit` |
| `error`, `aborted` | `turn_failed`, with the response's `errorMessage` as the message, or the stop reason when there is none |
| `toolUse` | No outcome of its own: pi ends a run on a tool-use response when every tool of the last batch asks it to terminate, so the exit-based rows decide the turn |
| anything else | `turn_outcome_unknown` |

pi's `agent_end` line repeats every message of the run, including each tool result, and pi caps one tool result at 50 KB. Sortie reads stdout lines up to 10 MB, so a run whose messages add up to more fails with a stdout read error once the work is done.

A run that ended with no assistant response at all, and a run that never reached `agent_end`, fall to the shared exit-based rows. An exit 0 turn with no assistant output and no tool activity fails as a zero-work turn.

Failure modes worth recognizing:

- **Exit 0 with a failed turn.** A rejected model name or an authentication error can end a response with stop reason `error` while the process exits 0. The turn fails as `turn_failed`.
- **Early exit.** A process that starts and exits before it wrote a readable line is reported as an early exit with its exit status and the end of its stderr, so a wrong `agent.command` argument or an option pi rejects shows pi's own usage text. A command that cannot be found at all starts no process and is reported as not found.
- **No first event.** A process that stays alive without printing a parseable line fails with `response_timeout` and the message `timed out waiting for first event`. A credential or provider prompt waiting on a terminal is the usual cause.
- **SSH connection failure.** On a remote launch, exit status 255 before the run ended is a failed connection, not a pi failure.
- **Wrong workspace or session.** The header checks above end the turn with `response_error`.

## Sortie's own tools

The kind declares `MCPDialectNone`. pi loads MCP servers from its own `mcp.json` and from extensions, and the adapter does not pass it the generated `.sortie/mcp.json`. Sortie's tools are neither advertised to nor callable by a pi session, local or remote. The servers an `mcp_config` key names are not delivered either. The workflow reference's MCP paragraph for the `pi` block describes what the preflight reports.
