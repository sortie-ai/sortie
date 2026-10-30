# Agent Client Protocol adapter notes

Working notes for anyone changing Sortie's Agent Client Protocol adapter in `internal/agent/clientprotocol`.

Last updated: 2026-10-29

This kind names no default runtime. The operator's `agent.command` picks the binary and the flag or subcommand that puts it into protocol mode, so these notes cover several runtimes. A runtime's version is never pinned here; the adapter records it per session from the `initialize` handshake. Runtime-specific traps live in `docs/gemini-adapter-notes.md`, `docs/kiro-adapter-notes.md`, `docs/copilot-adapter-notes.md`, and `docs/opencode-adapter-notes.md`.

## Pinned schema artifact

The generated wire types in `wire_gen.go` come from a pinned release of the protocol's stable schema. The tag and commit live in `schemagen/generate.go` and in `PROVENANCE.txt` beside the schema copy under `testdata/`. Changing them is a pin move, not a routine edit.

The adapter deliberately ignores some members of the pinned schema. It names a tool call by its tool kind instead of the optional tool `name`, except that the kinds `other` and `switch_mode` name none. It does not advertise the client capabilities for notice and compaction session updates, and does not read those updates.

A scheduled check reports when the publisher's stable schema releases differ from the pin. It never moves the pin, and it closes its own report on the first run that finds no difference.

## Runtimes

What each runtime showed in its `initialize` handshake:

| Runtime | Protocol entry point | What was observed |
|---|---|---|
| `gemini` | `--acp` (`--experimental-acp` is deprecated) | Driven end to end by the gated suite. Advertises no `sessionCapabilities` object. |
| `kiro-cli` | `acp` subcommand, listed only under `--help-all` | Driven end to end. Empty `sessionCapabilities`; `mcpCapabilities.sse` is false. |
| `copilot` | `--acp` | `agentInfo.name` is `Copilot`. The gated suite runs with `SORTIE_CLIENTPROTOCOL_SPEND_DECLARATION=Copilot`. On a local launch its scripted-model run asserts exact usage per token class and per model, with the settling source and strategy, for completed, tool-calling, resumed, and cancelled turns. Its journal is not written at once, so a killed turn can end with no figure, its amount unknown, never zero. Over SSH the same assertions pass as locally, because the records are read on the worker host. It advertises `mcpCapabilities.http`, so the tool-server SSH scenarios run with `SORTIE_CLIENTPROTOCOL_HTTP_TOOLS=1`, and the SSH kill, cancellation after output, and continuation pass. `sessionCapabilities` has `close` and `list`, not `resume`. |
| `opencode` | `acp` subcommand | `agentInfo.name` is `OpenCode`. The gated suite runs the whole scripted-model contract with `SORTIE_CLIENTPROTOCOL_SPEND_DECLARATION=OpenCode`, `SORTIE_CLIENTPROTOCOL_REPORTED_MODEL_PREFIX=scripted/`, and a `SORTIE_CLIENTPROTOCOL_STATUS_CALL` that scripts the model's `execute` tool, the only route through which OpenCode 2.x offers MCP tools to the model. On a local launch it asserts exact usage per token class and per model, with the settling source and strategy, for completed, tool-calling, resumed, cancelled, and killed turns. Over SSH the same assertions pass as locally, because the session export is read on the worker host. It advertises `mcpCapabilities.http`, so the tool-server SSH scenarios run with `SORTIE_CLIENTPROTOCOL_HTTP_TOOLS=1`. `sessionCapabilities` has `close`, `fork`, `list`, and `resume`. |
| `claude` | none | The CLI exposes no protocol entry point. |
| `codex` | none | `app-server` speaks a different JSON-RPC dialect, the one the `codex` adapter drives. |

A local launch needs up to three command-line switches: one that puts the runtime on the protocol, one that trusts the workspace so declared tool servers are not dropped, and one for a posture that does not ask before running a tool. Runtimes spell them differently, and no shared code branches on which runtime it is.

| Switch | `gemini` | `kiro-cli` | `copilot` | `opencode` |
|---|---|---|---|---|
| Protocol | `--acp` | `acp` subcommand | `--acp` | `acp` subcommand |
| Trust | `--skip-trust` | `-a` | none | none |
| Posture | `--approval-mode yolo` | `-a`, the same switch | `--allow-all-tools` | none |

`kiro-cli` uses one switch for trust and posture, so you cannot take back one without the other. Without `--allow-all-tools`, a `copilot` tool call raises `session/request_permission`, a refusal fails the call, and the turn ends without a follow-up model request. `opencode` takes no model switch either: its model comes from its own configuration, which the qualification passes through `OPENCODE_CONFIG_CONTENT`.

Some runtimes send many notifications under their own vendor method prefix. They are notifications, not requests, so the client answers none of them and depends on none of them.

## Agent message chunks

A runtime streams one reply as many `agent_message_chunk` updates. Chunks of one message share a `messageId`, and a new ID starts a new message. Each text chunk is reported as a notification with an empty message, which keeps the stall timer fresh without replacing the run's last agent message. The text accumulates, capped at 500 runes, and the completed message is reported once, when a chunk with another ID, a user message chunk, or any update reported as an event arrives, or when the turn or the stream ends. Updates that are only logged do not end a message, and neither does a `usage_update` that reports usage windows. The completed message carries the time its last chunk arrived, so a reply a session load replays is not dated into the next turn. A non-text chunk belongs to its message and is reported as its own event.

## Usage reporting

The kind derives `turn_end` arrival and `session_total` attribution for every session, local or remote, because its live source is the protocol messages read at turn end and they name no model. The stable wire carries no spend counter. A `usage_update` session update stays out of every token figure and is otherwise logged at debug level. Under a runtime declaration selected from the handshake name, its `_meta` reports usage windows, without ending the agent message. Claude Agent ACP forwards the SDK's `rate_limit_info` unchanged in `_claude/rateLimit`; a report without `rateLimitType` updates the kind's one `Usage limit` window, and the bridge drops a `rate_limit_event` until the turn has seen token usage, so a refusal on a turn's first request reports no window. The `_meta.quota` block on a prompt result is a presence signal and a lower bound, never the accounting figure.

A runtime's figure comes from the usage declaration its session runs under: the one the handshake names, or the default declaration.

A spend declaration is selected once from the handshake name, like the other declarations, and says how the runtime reports spend. The declaration for `Copilot` pairs a live source with a records source. The live source reads the `usage` member of each prompt result as a counter cumulative over the runtime's process. The counter starts again at zero in a new process, which a session load starts. It holds cache reads and writes inside input and reasoning inside output, and names no model. A member that is absent, null, or not an integer leaves that count unknown, and `totalTokens`, `thoughtTokens`, and the `usage_update` occupancy never enter a figure. The records source reads the `session.usage_record` lines of the session-state journal on the runtime's host, one per model request, each naming the model that served it and an opaque request identity. The journal's location and read caps live in `internal/agent/copilot/sessionstate`, shared with the `copilot-cli` kind. The session id is checked by the path-segment rule of the shared runtime read, which refuses an id that is not a plain file name before any file is opened or any process starts.

The declaration for `OpenCode` also pairs a live source with a records source. The live source reads the `usage` member of each prompt result as the counts of that turn alone, the sum of the turn's steps in this session, so a reading below the previous one is a smaller turn and never a counter restart. Cache reads and writes sit outside input and reasoning sits outside output, so the figure adds cache to input and reasoning to output and reports the cache counts apart. The runtime omits a counter that is zero, so an absent `thoughtTokens`, `cachedReadTokens`, or `cachedWriteTokens` reads as zero, while an absent `inputTokens` or `outputTokens`, and any member that is null or not an integer, leaves that count unknown. The live source names no model. The records source is the session export, run as the runtime's own command in the session's workspace with the launch environment, so it uses the binary and settings the session runs on. The command is the configured one cut after the runtime's name, so a launcher, `env` assignments, or a path before it carry over and `acp` does not; a command that does not name the runtime runs `opencode` from `PATH`. The export yields one record per finished assistant message, keyed by the message id and named by provider and model, which is the alias when an alias is configured. The export's session aggregate is never read, because it includes requests no message records. The export parser lives in `internal/agent/opencode/sessionexport`, shared with the `opencode` kind.

Sortie turns off OpenCode's session title generation on every session it launches, locally and over SSH, by adding `agent.title.disable` to `OPENCODE_CONFIG_CONTENT` and keeping every member the operator set, so those sessions carry no title and the first turn of a created session is complete. The credential check's launch carries the addition beside its deny-all permissions. An operator's own `agent.title.disable` other than `true`, or a value that is not a JSON object, stays as set and makes the session's figures lower bounds, as does a configured command that does not name the runtime, because that launch carries no addition.

A subagent's requests fall outside the parent's prompt result and the parent's export. The transport reads the subagent's start and the session it runs in from the parent's update stream, and at turn end reads that session's own export. Each record counts once, in the turn that ran the subagent, toward the run total and `agent.max_tokens`, and joins the per-model split. The turn is complete only when every subagent it started completed, named its session, and had its export read. A subagent that fails, goes unnamed, or has an export that cannot be read leaves its turn a lower bound.

The declaration for `gemini-cli` also pairs a live source with a records source. The live source reads the `usage` member of each prompt result as the counts of that turn alone, through the mapping the OpenCode declaration shares: `inputTokens` as input with cache reads inside it, `outputTokens` plus `thoughtTokens` as output, `cachedReadTokens` as cache read, and `cachedWriteTokens` as cache write, which the runtime never sends. The runtime omits a counter that is zero, so an absent `thoughtTokens`, `cachedReadTokens`, or `cachedWriteTokens` reads as zero, while an absent `inputTokens` or `outputTokens`, and any member that is null or not an integer, leaves that count unknown. `totalTokens` leaves reasoning out and is never read. The live source names no model, and the per-model block of `_meta.quota` is unread, because it names the requested model and not the one that served a request after a tool call. The records source is the chat journal, a JSON Lines file the runtime writes under `.gemini/tmp/<project>/chats` in `GEMINI_CLI_HOME`, else in the user's home directory. Sortie can compute neither the project directory nor the start minute in the file name, so the transport finds the file by the name pattern `session-*-<first 8 characters of the session id>.jsonl` through the shared runtime read. A session id whose first 8 characters are not ASCII letters, digits, or `-` is refused before any file is opened or any process starts. A matched file counts only when its first line is a header whose `sessionId` equals the session id, so the file each process start writes and another session's file contribute nothing. Each `gemini` line that carries `tokens` is one record, keyed by the message `id` and named by the message `model`: input is `input` plus `tool`, output is `output`, reasoning is `thoughts`, and cache read is `cached`. A line that repeats an `id` counts once, and a model alias the API serves under another name is attributed under the alias. The reader scans lines in file order and skips the header and every other control line. `$rewindTo` and `$patch` remove nothing, because a rewound or compressed request was still billed. A runtime appends a request's line when the request completes, so the request streaming at a cancel or a kill is in no record, and a line that does not decode is an unknown reading, never zero. A session whose current model, from the result of `session/new` or `session/load`, is `auto`, begins with `auto-`, or is absent makes a model request that picks the model and that no source records, so every turn of that session is a lower bound. A session on a concrete model reports complete figures. Web search and web fetch are requests outside the main chat that no source records, as are the loop check, history compression, invalid streams and subagents; dynamic routing escapes only when the operator adds a model definition with `tier: "auto"` under a name that does not start with `auto`; a turn containing one of these requests reads complete.

The default declaration covers a runtime with no declaration of its own, and a declaration that maps no spend, such as the Claude bridge and Kiro CLI declarations, which keep their windows and pulls and read spend under the default mapping. It reads the `usage` member of each prompt result: `inputTokens` as input, `outputTokens` as output, `cachedReadTokens` as cache read, and `cachedWriteTokens` as cache write. `thoughtTokens` and `totalTokens` are not read. The counts are read as cumulative over the runtime's process under the same reset rule, with cache inside input and reasoning inside output, and a missing member or `usage` object is unknown, never zero. The protocol has not settled whether these counts are per turn or cumulative, so the figure is exact for a runtime that reports cumulative counts and never more than the true spend for one that reports per-turn counts. It is therefore always a lower bound, never complete, and its turn carries unaccounted spend. A reading that adds no input and no output tokens on a turn that reached the model is no figure, so Kiro CLI, whose token fields hold zero, leaves such a turn without one, and its credits are not read here. A runtime that reports no usage fields leaves its turns as spend that occurred with the amount unknown. The gated suite runs under the default declaration with `SORTIE_CLIENTPROTOCOL_SPEND_DECLARATION=default`, which registers no declaration for the run and expects the scripted figure marked a lower bound, over SSH too, and the nightly run carries a Copilot CLI row for it.

On every launch, local or remote, the transport reads the records at the start of a continued session, one the runtime loads or resumes, so the requests that session already holds count as history and never as this run's spend, and again at every turn end. A session the runtime creates holds no history and reads nothing at start. It counts each model request once in the run total, whichever source reports it and whichever earlier reads failed, and a model request joins the per-model split at most once. A turn the live figure settles keeps its amount whether the records read splits it by served model, fails, or finds nothing; a failed read leaves that amount unsplit. A turn with no live figure, such as a cancelled turn or a result without `usage`, is settled from the records when they can be read, and ends as spend occurred with the amount unknown when they cannot. For a live source cumulative over the process, a reading below the previous one is a counter restart; a per-turn reading is never one. A restart counts in full, the turn is not complete, and no record counts until a later complete turn fixes a new boundary. A served model no price table carries stays unpriced. A remote read runs on the worker host over a new SSH connection built like the session's own launch, streams the journal line by line under the same size and line caps as a local read, and finds the runtime's home as the worker's shell resolves it, from the variables the launch carried and the worker's own login environment. A records file whose name holds a part Sortie cannot compute is found by a name pattern: one read-only command lists the regular files under the home whose path matches, with the pattern passed as quoted arguments so no wildcard option of the remote shell applies, and each listed file is then streamed as above. A stream cut mid-journal, a dropped connection, and a read that outlasts the agent read timeout are failed reads, never complete ones. Each read moves the whole journal, at most 64 MiB.

A session raises `tokenCounts` to `ours` at the handshake. The first turn that reached the model and ends with no figure, while no earlier turn produced one, lowers it to `gap` for the rest of the session.

A turn that made a model request no figure accounts for is recorded as "spend occurred, amount partly unknown". The issue's summed total then becomes a lower bound, which is different from a measured zero.

A runtime declaration can also pull its usage windows when a turn ends, for a runtime whose limits the wire never carries. When the prompt response arrives and the turn was not cancelled, the pump starts the pull in its own goroutine, publishes the windows ahead of the turn's terminal event, and finalizes the turn once the pull has answered; the pull's bound caps the wait. A credential verification session and a cancelled turn run no pull. The pump sends the declaration's request without naming the runtime. The declaration reaches the connection, the launch environment, and the runtime's home files only through the pull runtime the transport hands it, and the transport reads a home file, and runs a read-only command, on the far host for a remote launch. A declaration can also run the runtime's own command line on its host, in the workspace with the launch environment, and can exchange messages with that command over its standard streams, on the far host for a remote launch. An error on the pull is logged at debug level and drops that turn's windows, and a declaration that finds its source unsupported ends the session's pulls.

A declaration is selected by the name the handshake reports, and a name with no declaration selects the default declaration. When the handshake reports no name, the launch command decides: a command line that names the runtime selects its declaration, so a runtime whose newer engine sends no `agentInfo` still pulls. The command is the configured agent command, which on an SSH worker is the remote command line and never the SSH executable that carries it. A handshake that names a different runtime than the command does selects the handshake's declaration, and the session logs one `Warn` record, `agent runtime differs from its configured command`, with `configured_runtime`, `handshake_runtime`, and `usage_declaration`. The notes of each runtime whose pull the kind runs describe it: `docs/copilot-adapter-notes.md`, `docs/opencode-adapter-notes.md`, `docs/gemini-adapter-notes.md`, and `docs/kiro-adapter-notes.md`.

## Usage-limit refusals

The kind declares that it recognizes a provider usage-limit refusal. The transport consults the usage declaration its session selected and names no runtime: the declaration states which `session/prompt` errors and which notifications of the turn are a refusal or a transient failure, whether the refusal's reset comes from the pull the turn already makes, and which opt-in the `initialize` request carries to receive the notifications. A runtime whose declaration states none keeps today's outcomes: `response_error` for a prompt error, `credential_unverified` for `-32000`, and the stop reason's outcome for a result. Recognition reads only members the runtime itself produces, never message text, `statusCode`, `errorCode`, or a body a provider authored. A recognized refusal ends the turn as `usage_limit`, unless the turn's stop reason is `cancelled`, which stays `cancelled`. A refusal's spend is unknown, never a measured zero, so the run still counts toward `agent.max_sessions`.

What each runtime shows:

| Runtime | Refusal shape | Held on |
|---|---|---|
| OpenCode | A prompt error whose `data.errorName` is `provider.free-limit`, `provider.quota`, `provider.budget-limit`, or `provider.model-budget-limit`. The error code is the generic `-32603` and is not read. `provider.rate-limit` is a throttle and stays `response_error`. | The configured model |
| Copilot CLI | A `session.error` event with `errorType` `quota` or `rate_limit`, which the runtime sends as a notification and then ends the turn with `end_turn`. Any other `errorType`, and any event naming another session, leaves the stop reason's outcome. | The configured model |
| Gemini CLI | A prompt error with code `429`. Codes `503` and `500` stay `response_error`. | The configured model |
| Kiro CLI, `--agent-engine v3` | A prompt error, sent as `-32000`, whose `data.errorType` is `UsageLimitReachedError` or `OverageLimitReachedError`. A `data.retryErrorType` of `THROTTLING` is a throttle and ends `response_error`, not a rejected credential. | The whole kind |
| Kiro CLI, default engine | A `_kiro.dev/error/rate_limit` notification with no `retry_warning` earlier in the turn, followed by a prompt error that is not `-32000`. | The whole kind |

Shared vocabularies back this table. The OpenCode error names live with the `opencode` kind's shared limits package, and the `opencode` kind ends `usage_limit` for the same names. The Copilot `session.error` reading lives in a package the `copilot-cli` kind shares, so both routes read one definition.

The reset instant comes from these sources, tried in this order:

1. The refusal the runtime's own records hold for the turn. OpenCode's protocol error drops the retry delay, so the turn-end records read, the same session export that settles spend, also returns the refused message's error and its retry delay, anchored at the time the runtime recorded the failure. The refusal is used only when it names the same error as the prompt error and carries a delay.
2. The earliest exhausted window of the pull the turn already makes, for refusals that read windows: OpenCode's console-metered names (`provider.free-limit`, `provider.budget-limit`, `provider.model-budget-limit`), Copilot's `quota`, every Gemini `429`, and every Kiro refusal. OpenCode's `provider.quota` never takes a console window, because any provider can send it, and a Copilot `rate_limit` never does, because no quota snapshot reports a rate limit.
3. Zero, which the orchestrator treats like `turn_failed` and retries on the exponential backoff. A records read or a pull that fails, times out, finds nothing, or does not apply lands here, and so does a runtime with no pull.

The OpenCode console windows meter a workspace, not a model, so the configured model filters none of them. Gemini's buckets meter one model each. With `agent-client-protocol.model` set, only that model's exhausted bucket supplies a Gemini reset, so a `429` after the runtime's silent retries of a per-minute throttle finds an unexhausted bucket and keeps zero. Without it the refused model is unknown, because the runtime may route a request to another model than the session shows, and every bucket counts. On this kind `model` names the held model and scopes the reset. It does not select the runtime's model, which `agent.command` does.

What an operator sees and controls:

- Copilot sends `session.error` only to a client that subscribed in `initialize`. Sortie subscribes only when `agent.command` names the `copilot` executable or the `@github/copilot` package, so a wrapper script that hides the name receives no subscription and the refused turn ends successful, as before. A runtime also drops events when its notification queue backs up, which loses the subscription's event the same way.
- Gemini refuses only after its own silent retries, which can run for several minutes before the prompt error arrives. An API-key or Vertex session pulls no bucket, so its reset is zero and the issue retries on backoff.
- Kiro's credit window resets on the billing-cycle date, read as 00:00 UTC. Whether the pull reports credits above the limit for an account with overage enabled is not established. If it does, a daily or hourly refusal on such an account takes the billing-cycle reset and the hold outlasts the real one; the orchestrator caps the wait.
- A reset comes from the runtime host's clock. On an SSH worker, skew between the two hosts shifts the instant, and the orchestrator ignores a past instant and caps a far one.

Refusal shapes that carry no structured member stay unrecognized and keep their old outcomes:

- A Kiro default-engine overage, which arrives as a bare `-32603`.
- A Kiro default-engine throttle or capacity shortage, which a `retry_warning` precedes. A usage limit the service answers at HTTP 429 reads the same way, and so does a refusal that follows a retried throttle in the same turn, because `retry_warning` latches for the turn.
- A Gemini quota hit during chat compression, which arrives as code `500`.
- A Copilot refusal on a launch that sent no subscription.
- A Copilot refusal followed by an automatic switch to another model (`eligibleForAutoSwitch`), which the captures never showed. A turn that switches and finishes would read as refused.

Every fixture behind these rules comes from a real runtime talking to a local stand-in upstream that answered each model request with one refusal. The runtime's frames are its own, while the provider's status, headers, and body are the stand-in's. No real provider refusal was captured on any runtime, so whether a live OpenCode refusal carries a retry delay is not established, and the design treats the delay as optional. The files under `testdata/usage_limit/` carry the provenance of each capture.

## Session close and delete

Teardown sends `session/close` when the handshake advertises `sessionCapabilities.close`. The gated suite drives this path against Copilot CLI and OpenCode, whose handshakes advertise `sessionCapabilities.close`.

When a handshake advertises `sessionCapabilities.delete`, a verification session's teardown sends `session/delete` instead, so the verification conversation does not stay in the runtime's store. OpenCode advertises it, and its credential-verification session was deleted through the protocol. Copilot CLI does not advertise it.

## Credential verification

The `initialize` wait during credential verification is the larger of `agent.read_timeout_ms` and `agentcore.CredentialExchangeBound`, because a runtime may talk to its own backend before it answers. `kiro-cli` with an API key takes far longer than the 5-second default read timeout to answer `initialize`; with a stored device login it answers in well under a second. With the longer wait, a refused key fails its first prompt with a `-32603` error whose `data` names the invalid bearer token.

A `-32000` response to `initialize`, `session/new`, or `session/prompt` means the runtime refused its credential, on a verification session and a working one alike, except where the session's usage declaration reads a `session/prompt` error as a usage-limit refusal or a throttle (see Usage-limit refusals). A runtime that exits before it answers any startup request gets the shared early-exit report, not a credential diagnosis. A connection lost while the runtime stays alive past the observation grace keeps `port_exit`'s transport message. A failed `session/load` or `session/resume` still falls back to a fresh session and is never read as a credential refusal.

## Session load spacing

The adapter holds a `session/load` call until the clock leaves the UTC minute in which this process created the session. The wait runs on the open connection, after the handshake and the negative control, and is bounded at one minute.

It exists because one runtime's `session/load` inside that minute fails and permanently destroys the session's resumability. `docs/gemini-adapter-notes.md` has the evidence.

The session continuation capability record cannot do this job, because the call that would observe the defect is the call that causes it. A per-process record of when this adapter created each session is the only way to keep the load out of that minute.

This wait is load-bearing. Do not trim it as dead time: removing it brings the defect back silently, and nothing notices until a run comes back with no history.

Limits:

- A session created by an earlier process is not covered, because the record lives in memory.
- A wall-clock step backward larger than the deferral can still land the load inside the creation minute, because the deferral is clamped at one minute instead of re-reading the clock.
- On an SSH launch the wait uses the orchestrator host's clock, so clock skew between the hosts larger than the deferral can still land the load inside the runtime host's creation minute.

## What a delivered tool server needs to be callable

A runtime that advertises `mcpCapabilities.http` receives Sortie's tool server as an HTTP server hosted on the orchestrator host, on every launch. The entry names a loopback address and carries a per-session bearer as an `Authorization` header inside the request, which is the only place the protocol lets a header value travel. On a remote launch the address is the remote end of a reverse SSH forward. A runtime without HTTP support receives the stdio entries on a local launch and runs without Sortie's tools on a remote launch, with the gap declared in the opening notice.

`copilot --acp` rejects every stdio entry a client sends, with no error and no notification, so an operator stdio server never reaches it, and the session is told nothing. It accepts HTTP entries. `kiro-cli` at its debug log level writes the `Authorization` value to its log; the bearer is useless once its session ends.

Delivery is not the problem: `session/new` carries the declaration, and the runtime reaches the server and lists its tools. But a runtime that asks the client for consent before a tool call gets a refusal, and the tool is not called. The protocol's server declaration has no trust or approval field, so Sortie cannot mark a server as pre-approved on the wire. The only lever is the runtime's own configuration, reached through `agent.command`: an approval mode that does not ask, or a rule that pre-approves the tools of Sortie's server.

Both `gemini` and `kiro-cli` behave this way: under a posture that asks, the declared server is delivered and its tool is never called. The other runtimes are unmeasured on this point.

`kiro-cli` adds a second condition: with an API-key credential it drops every declared server before any posture question. See `docs/kiro-adapter-notes.md`.

The adapter reports a refused permission request once per session, on two surfaces. The notification reaches the orchestrator's generic event handling, which logs the event type at `Debug` and keeps the message only as the running entry's last agent message, where the next message overwrites it. The `Warn` log record is the only surface that outlives the run.

That record comes from a refused permission request. Under a posture that never asks it never fires, so an untrusted workspace that silently drops the declared servers leaves no signal on this path.

## Reasoning level

The kind forwards no reasoning level. It reads no `effort` key and never sets a session configuration option for one, because the option ids differ per runtime and some runtimes offer none. The operator writes the runtime's own reasoning option in `agent.command`. The registration declares `EffortNotForwarded`, so a workflow that sets `effort` in this kind's block gets `agent.effort.not_forwarded` instead of a silent drop. A completeness test fails when a non-test file of this package reads the key.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_CLIENTPROTOCOL_TEST=1`. Because the kind has no default runtime, `SORTIE_CLIENTPROTOCOL_COMMAND` supplies the launch command, and the suite skips without it. The other coordinates are described in the test file and in `docs/agent-adapter-howto.md`. Traps:

- The scripted tool-server case needs the runtime's trust and posture switches in the command. Without the trust switch `gemini` drops the declared server; without the posture switch it asks, gets refused, and the call never reaches the server.
- `SORTIE_CLIENTPROTOCOL_ASKING_COMMAND`, used by the scripted refusal case, must leave out the posture switch. A runtime that never asks runs the guarded command and the case fails.
- The scripted cases isolate `HOME` and the XDG directories, so a version-manager shim that reads `HOME` fails to start. Name the runtime by an absolute path.
- The client fills in a default for a field it cannot read instead of refusing the message. A change in the shape of a message it already handles therefore does not fail at runtime. The gated suite's shape assertions are the only place that shows up.
- The `(default declaration)` Copilot run names `claude-sonnet-4.5`, a model id Copilot's multiplier table knows. An unknown id, such as `scripted-model`, makes Copilot print a "No token count multiplier is known" notice as agent text before the first `usage_update`.
- Whether the model calls a tool when asked is the model's choice, not a protocol obligation. The live suite logs a missing tool call as a `model-dependent observation` and does not fail on it. It still fails when a tool call arrives without a normalized tool result. The scripted cases drive the tool call deterministically and do fail without it.

The qualification profile in `tools/qualify` drives a live model for minutes and spends real credits. It has its own gate; `docs/agent-adapter-howto.md` describes its coordinates and the `make qualify-*` targets. Things that are not obvious:

- A named authentication variable with no value is a clean skip, because the operator mints a temporary credential for each run. Every other missing or malformed coordinate fails the run, so a typo cannot pass green.
- An offline staleness gate on every push fails when a tracked profile changes without a fresh measurement, or when a runtime's notes disagree with its tracked measurement.
- The nightly live shard compares the tracked profile's `capability_gap_labels` with the live gap labels through `SORTIE_CLIENTPROTOCOL_PROFILE`, so a capability that changes after qualification is caught. A session that goes unmeasured after its first turn counts `token counts` as a gap even though no notice names it.
