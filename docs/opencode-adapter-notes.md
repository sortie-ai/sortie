# OpenCode adapter notes

Working notes for anyone changing Sortie's OpenCode adapter in `internal/agent/opencode`.

Last updated: October 2026

Eligibility: not_qualified

Product conformance: not_qualified

Both verdicts fail on one row: protocol session continuation is a gap below the native reference. In the recorded capture the recall turn kept the session identifier, but its answer did not contain the phrase the seed turn asked the runtime to remember (`same_session_without_recall`). The previous capture of the same profile recorded `confirmed_same_session` for that row. Several load-bearing rows are also unmeasured: permission handling, where the runtime raised no permission request under the asking posture; the policy precondition, where no refusal was answered; the human-input case of protocol retry classification, where no request of either class was raised; and native turn disposition, where the success turn ended with no recognized terminal. Protocol runtime failure is not observed either: the refused-model inducer yields a completed turn, because the runtime replaces an unresolvable model id with its default model. Product conformance is also blocked by session continuation, and waits on permission handling, the policy precondition, the human-input case, and protocol runtime failure. The protocol `limit_reached` case stays unmeasured because the prompt channel cannot carry a request big enough to reach the runtime's limit.

This file is checked against the tracked measurement under `tools/qualify/probe/testdata/opencode/`. Changing a grade row below fails the staleness gate until a fresh run replaces that measurement.

## Where to get the volatile facts

Nothing here pins a flag list, an event vocabulary, a permission key set, or an exit code. Read the flags off `opencode run --help` on the version you target. Take permissions, provider configuration, and the server surface from the docs at `opencode.ai`. Go to the upstream repository when the docs and the binary disagree, which for this CLI happens often. The adapter's `command.go` and `parse.go` define what Sortie actually sends and reads.

## The surface we drive, and what it is not

`opencode run` is a thin client over the same HTTP and event APIs the server mode exposes. Without an attach target it boots a server in-process. This explains every gap in the stdout stream: the JSON that `run` prints is a projection of selected server events, not the real event bus. It has no session-started envelope, no idle transition, no permission event, and no final result envelope. Process exit is the only end-of-turn signal.

If a change needs something the projection hides, such as retry and backoff state, use the server surface with its documented schemas. Do not try to parse more out of `run` output.

If you ever wire a port: the server mode treats port zero as a sentinel, not as "pick any free port". Getting this wrong either attaches to a server this process did not start or misses the one the CLI opened. Check the current semantics, and set the port explicitly on both sides.

## Version check and launch contract

`StartSession` runs the command with `--version` at every session start and accepts only major version 2. Any other major version, or no readable version, refuses the session. The refusal names the package, `@opencode/cli`, because another npm package installs a command with the same `opencode` name.

Two settings are refused before any launch: `opencode.pure`, which the runtime has no switch for, and `opencode.effort` or `opencode.variant` without a plain `opencode.model`, because the runtime reads a variant only as a `#` suffix on the model.

The launch contract:

- The runtime trusts `PWD` ahead of its own working directory, so the adapter sets `PWD` to the verified workspace path on every local launch.
- The runtime reads the prompt from standard input. A positional prompt after `--` sends it to the model twice.
- One inline configuration document in `OPENCODE_CONFIG_CONTENT` carries the tool policy, sharing, compaction, and tool-server declarations. A malformed document is dropped whole and silently, so a marshaling bug here fails open, not loud.
- Every launch adds `--standalone`. Without it the runtime uses a shared background service that keeps the environment of whichever client started it first and outlives Sortie.

On every launch the runtime imports sessions stored by older releases, in the background and unordered with the turn. Resuming a session the import has not reached yet fails the turn. A later launch recovers once the import catches up.

A fresh data root has a one-time race: the first launch wins a schema-creation lock, and every concurrent launch on the same root exits early. At the adapter this looks like any other early exit. It goes away on retry once the schema exists.

## Turn lifecycle

Turns run on `agentcore.ForkPerTurnSession`, the skeleton Claude Code and Copilot CLI use. Launch, output reading, the reap, the stop, the order in which a turn's possible endings win, and the logger belong to the skeleton, so a lifecycle change goes there and reaches every kind. This package supplies only what is OpenCode's own, through the skeleton's hooks:

- `BuildArgs` returns the run argument vector. The prompt is not an argument, so `PromptOnStdin` is set and the skeleton writes the prompt to standard input after the SSH preamble on a remote launch.
- `FirstEventTimeout` is `agentcore.FirstResponseTimeout` of the agent configuration.
- `TurnEnv` returns the scrubbed environment for a local launch and, for a remote one, the managed variables with the inline configuration document.
- `ParseLine` maps each envelope to events. A line that is not JSON is malformed. A session ID that differs from the adopted one returns `agentcore.EndTurn` with `response_error`. A turn-ending `step_finish` and an `error` event return a non-nil result.
- `SettleUsage` hands the turn's step reports and the session export to the usage component, which settles the turn. The skeleton calls it on every ending of a started turn except the mismatch, so a cancelled turn, a read failure, and a missed deadline settle like a clean exit does. On an SSH launch Sortie stopped, the skeleton first waits for the remote runtime and its server to exit, so the export never starts a standalone server while the turn's own is still closing the session database.
- `OnFinalize` raises the permission notices from standard error and builds the turn evidence, including the free-tier refusal clause. It settles no spend.
- `GetUsage` and `GetSessionID` read the session state. `EmitSessionStartID` is nil because `ParseLine` emits `session_started` itself on the first envelope that carries an ID.

`StopSession` calls the skeleton's stop, then deletes the verification session when the session is a credential verification.

The verification request carries a deny-all permission policy, and on release 2.2 it still declares the `skill` tool: the runtime's browser plugin appends a `skill` rule after every configuration source, so no configuration removes the definition. The deny-all rules leave the tool unusable, and the adapter declares that the verification request keeps some tools.

The first-event deadline is the larger of the read timeout and `agentcore.CredentialExchangeBound`. OpenCode prints nothing until the provider starts answering, and the free hosted model can take much longer than the 5-second default read timeout. The deadline stops at the first envelope, so it catches a process that never talks, not a stall in the middle of a turn. Stalls belong to the orchestrator.

The export runs after the reap, on a context that outlives the turn's cancellation, through the shared host-command operation the usage-limits pull also uses, locally or over the session's SSH launch. `agentcore.ReadTimeout` of the agent configuration bounds a turn's whole read: the session's export and one export per subagent session. A session that continues runs one more export when it starts, under the same bound. A slow export delays the turn's result, never the stop.

## Session identity and the dir-scoped resume

A session ID is server-generated and learned from the first envelope that carries one. The adapter adopts it, and a later envelope carrying a different ID is treated as a fault that fails the turn rather than a thing to reconcile.

Resume is scoped to the directory the session was created in. Resuming a valid session ID from another directory does not error: the process exits zero and prints nothing. Sortie then reports an early exit with status 0 and empty stderr, which looks like a runtime that never answered, not like a resume that did not match.

The adapter cannot hit this, because every turn runs in the issue workspace. Tests can: a resume test whose second turn runs in a fresh temporary directory never exercises resume. Run both turns in the same workspace.

Sessions are persisted in one global database under the user's home directory, not in a per-project store inside the workspace. Isolation between concurrent issues comes from distinct server-generated IDs, not from separate storage, and removing a workspace reclaims none of it.

## Permissions

OpenCode's permission control is config-driven. Each key resolves to allow, ask, or deny through an ordered rule list; the last matching rule wins. The documented default is ask, but OpenCode 2.x does not apply it to `external_directory`: with no rule for that key, the runtime reads outside the workspace without asking. So when bypass is off and the tool lists leave `external_directory` undecided, the adapter writes `ask` for it itself, and `opencode run` refuses the access and reports it like any other refused call. A list that names the key sets its action as written.

The adapter sends its policy in the inline configuration document. OpenCode deep-merges that document into the rules from every other configuration source; it does not replace them. So a value inherited from the operator's shell would leak into the result. The adapter removes every variable it manages from the inherited environment before adding its own. Keep that scrub in step with the managed set: a new managed variable that is not scrubbed is a leak.

The emitted policy is closed. When the workflow names allowed tools, every other permission key the adapter knows is denied explicitly. That known-key set is a policy input, not a validation list. The runtime accepts more keys than its docs list, so a key the workflow names is forwarded as written even when the adapter does not know it. Rejecting unknown keys would break configurations OpenCode accepts.

Permission prompting is bypassed by default, because otherwise the runtime refuses every permissioned tool call. Validation warns when a workflow turns that off, and rejects an overlap between the allowed and denied lists.

A refusal appears on stderr as a warning line styled with escape sequences; stdout stays clean JSON. The adapter strips the escape sequences before matching the prefix. Skip that and the match never hits real output. The notice is read from collected stderr after exit and goes through `agentcore.DecideHumanRequest`.

The hosted free-tier model refuses any request whose tool set lacks its shell tool or its read tool. That refusal arrives as a run-level error, not as the permission notice. When the session's own tool policy denied one of those tools, the adapter appends a clause naming it to the vendor's text. This match depends on the vendor's wording. If upstream rewords it, re-capture the fixtures; do not widen the text match.

## Usage accounting

Step-scoped token counts on the stream are not a running total. Between the tool step and the final text step of one turn the numbers move in both directions, so taking the last one is wrong.

The kind declares two sources and the shared usage component does the arithmetic. The adapter parses reports and states what each source means; it subtracts no baseline, adds no figures, and decides no completeness. The live source is the `tokens` of each `step_finish` part, one report per model request, identified by the part's `id`. The records source is the session export, which holds one message per finished request, keyed by the message identity and named by provider and model. The two derive `turn_end` arrival and `per_model` attribution.

The usage component normalizes the counts, adding cache to input and reasoning to output, and holds the greatest reading per request identity, so a report that repeats counts once. The runtime prints every counter in a step report, zeros included, and an export message carries every count. A report or message with a count that is absent, null, or not an integer is unknown, never zero. An unknown step report counts nothing and keeps the turn from settling on step reports. An unknown export message stops the known records before it, so that turn and every later turn of the session are lower bounds.

The final text step of a turn prints no `step_finish`, so the sum of the step reports falls short of the turn's spend and stands as a lower bound. A report whose reason ends the turn would settle the turn from the step reports alone, complete, with live provenance and no per-model split; a captured build prints none, so the export settles every completed turn. A turn the runtime did not end itself, by an `error` event, a Sortie stop, a kill, or a cancel, never settles from the step reports.

Records and step reports count the same requests, so the two are never added. A figure the export recovers replaces the step readings, and the turn settles exact and complete with the records' provenance, split by served model. When the export fails or holds no new message, the step sum stands as a lower bound with live provenance and no split, and the turn carries unaccounted spend. A turn with no step report in that case has no figure and its spend is unaccounted. A later turn that reads the export counts from the records again, so the run's total converges on the export's.

The adapter sums per-message figures and ignores the export's session aggregate, for two reasons. The aggregate covers the whole session, including earlier runs, while Sortie reports per run. And its total also includes cache and reasoning tokens, so it is not input plus output.

A finished message's `cost` is read in US dollars as a per-request amount beside its tokens, under the message's `providerID/id` model, and the records source declares it. The `cost` of a `step_finish` report is not read, because its part identity differs from that of the export message that counts the same request, and neither is the export's `info.cost` aggregate. A free model reports `cost` 0 beside non-zero tokens, and a 0 beside tokens is no amount, so the model's amount is a lower bound. A child session's cost is never added to its parent's: the child's tokens count and mark the model's amount a lower bound.

A message counts only once its `finish` field is set. The runtime saves an assistant message with zero tokens before calling the model, so a turn killed mid-step exports a placeholder, not a measurement. A finished message without tokens after the boundary is an aborted request whose spend is recorded nowhere, so it is unknown like any missing count, while one inside the history stays history.

A resumed session reads the export once when it starts. Every message it holds becomes history by identity and never counts as this run's spend, whatever its creation time. A new session starts with no history. When that read fails, the boundary is unknown, never empty, so the session's history is never counted as this run's spend: each turn until a turn-end export is read stands as a lower bound, that read fixes the boundary after its turn settles, and no later turn of the session covers the session.

A turn that hands work to a subagent runs the work in a child session. The stream shows one `tool_use` for the call, with `state.input.agent` set and the child's session id at `state.metadata.metadata.sessionID`, and no event of the child. The parent's export holds none of the child's messages. The adapter exports every named child session once at the end of the turn, and each child message counts once in the turn that ran it. The turn is complete only when every subagent call completed, named its session, and had its export read. Otherwise the turn is a lower bound, and the children that did read still count. A turn short of a child's records leaves no later turn covering the session, because the session's own records hold no child request.

A turn covers the session only when its figure is the complete total of the session's records. A turn settled on step reports never covers it, and neither does any turn after one that left a child session's records uncounted or after a resumed session fixed its history at a turn's end.

The configuration document disables the runtime's title agent. Its request is billed but recorded in no message, so the sum would miss it.

A failed or empty export never lowers a figure the run has already reported. The step sum stands as a lower bound instead of a zero, and a turn without step reports reports nothing.

The export's invocation and parsing live in `internal/agent/opencode/sessionexport`, shared with the OpenCode declaration on the `agent-client-protocol` kind, so both routes read an export the same way and refuse the same ones: another session's document, and one above 64 MiB. The export is sanitized so tool output never reaches a log, and it also runs over SSH, so remote launches are measured too. A failed export logs one debug record without the output.

## Usage limits

The run stream carries no subscription window. Two other places do.

The pull runs `opencode api --standalone experimental.usage.limits` once a turn that reached the runtime has ended, through the same shared host-command operation the export uses, locally or over the session's SSH launch. The `agent-client-protocol` declaration runs the same command, with the configured command cut after the `opencode` word like its export, locally or over the session's SSH launch, while the session's `opencode acp` process runs. It runs on empty stdin. The `--standalone` flag keeps the call off the host's shared background service: without it the call starts that service, which detaches into its own session on a fixed local port and outlives the call and the process-group stop, so every worker would leave one behind. A cancelled turn and a credential-verification session do not pull. The command exists only on releases that ship the usage-limits subsystem. On an older build it fails or hangs until the bound, and on either kind a command that fails to start, exits non-zero, outlives the bound, or prints no object ends the session's pulls. A failed SSH connection costs that turn's limits only, and the next turn pulls again. Do not gate the pull on a version check.

The answer is a JSON object, with the payload under `data` next to a `location` member. Trimmed:

```json
{
  "data": {
    "workspace": { "id": "wrk_...", "name": "...", "usageURL": "..." },
    "go": {
      "rolling": { "percent": 37.5, "limited": false, "resetsAt": 1793199600000 },
      "weekly": { "percent": 12, "limited": false, "resetsAt": 1793577600000 },
      "monthly": { "percent": 100, "limited": true, "resetsAt": 1794700800000 }
    },
    "budgets": []
  }
}
```

`percent` is the share used, from 0 to 100. `limited` marks an exhausted window. `resetsAt` is epoch milliseconds, so convert before comparing it with Unix seconds. A member of `go` can be null, and it yields no reading. The `budgets` list holds spend budgets in dollars and is not read.

The command answers only for a runtime signed in to the OpenCode console. Without that sign-in the payload is `{"data":null}`, which carries no `go` and ends the session's pulls. The runtime makes the console calls itself with the console credential it holds; Sortie neither reads nor sends that credential.

With `--standalone` the command answers in its own process, so it neither starts the background service nor reads the state of one an operator already runs. A plain `opencode serve` route returns 401 without the service credential that `api` supplies, so do not call the HTTP route directly.

The free hosted model reports its limit in the stream instead. When the limit is hit the run prints an error envelope with `type` set to `provider.free-limit`, status 429, and `retryAfterMs`:

```json
{"type":"error","error":{"type":"provider.free-limit","status":429,"retryAfterMs":3600000}}
```

`retryAfterMs` is milliseconds, relative to the moment of the failure. It derives from the response's `retry-after-ms` or `retry-after` header, which is why a header of 3600 seconds arrives as 3600000. The adapter turns it into one exhausted window that resets that long after the failure the event's `timestamp` records, not after the moment the adapter observed it. The `usage_limit` the turn ends with carries the same instant. Other quota errors, such as `provider.budget-limit` and `provider.quota`, carry no usable window, and they end `usage_limit` all the same.

Both the `opencode` kind and the `agent-client-protocol` kind end the turn as `usage_limit` for every one of `provider.free-limit`, `provider.quota`, `provider.budget-limit`, and `provider.model-budget-limit`, and hold the configured model, because one OpenCode configuration reaches models of several providers and a refusal comes from the provider serving the run's model. `provider.rate-limit` is a throttle and is not a refusal. Both kinds read the names from one shared vocabulary. The reset is the refusal's own delay when its route keeps one, and otherwise the latest `resetsAt` among the exhausted console windows, because the rolling, weekly, and monthly windows meter one subscription and a request passes only when all of them have room. An exhausted window with no future `resetsAt` leaves the refusal with no reset. The `provider.quota` name takes no console window, because any provider can send it, so without a delay it retries on the exponential backoff.

The two routes keep the delay differently. The run stream carries `retryAfterMs` in the envelope. Over the protocol the prompt error is a generic `-32603` that carries only the error name in `data.errorName`, so the kind reads the delay from the last error the session export records after the turn's own user message, in the same export read that settles spend, and uses it only when it names the same error. A refusal recorded before the turn's user message belongs to an earlier turn and is ignored.

## Failure detection

An error envelope on stdout is the authoritative failure signal. The process exit code is not load-bearing and has changed meaning across releases; do not key anything on it.

The error envelope carries `type`, `message`, and a numeric `status` when present as flat members.

One failure can arrive as two error envelopes: the actionable diagnostic the session publishes, and a generic placeholder the run command reports when the underlying fault was not in its API error schema. Their order on the stream is not guaranteed, so the adapter keeps whichever envelope carries detail rather than whichever arrives last. When only the placeholder was seen, it reaches the operator as it is, so a report of the generic message is a signal to reproduce, not a bug in the parse.

Work evidence for the turn is this turn's own parsed assistant parts, text, reasoning, or tool use, never the export figure, which is non-zero on any turn after the first. An exit-zero run that parsed envelopes but produced no assistant output takes the shared zero-work row and is reported as a failure rather than a silent success.

## Sortie's own tools

`StartSession` translates the worker-generated MCP configuration into OpenCode's own server entries under the `mcp` key of the inline configuration document. The runtime rejects the standard `mcpServers` key, so the generated file cannot be handed over as is. The entries add to whatever the operator's own configuration declares. An operator header value is carried by reference: the `mcp` member names the environment variable that carries it, and Sortie sets that variable in the runtime's environment, so the document never holds the value.

The document is set only in the turn's own environment (local) or on the SSH carrier (remote), never in the shared managed-environment builder. So the export and delete helper invocations never carry it and never start a tool server of their own.

On an SSH launch the `mcp` member names Sortie's tool server by the address of the launch's reverse forward and each operator HTTP server by its URL, with header values carried by reference to environment variables Sortie sets, and operator stdio servers are left out. The document depends on the forward's outcome, so it travels on the SSH carrier. A refused or unconfirmed forward carries the document without Sortie's entry, and the session opens with the gap declared.

The `run` output has no MCP startup status. A server that fails to start shows up only as failing tool calls. The runtime starts each server in the background and does not wait for it before the turn begins.

Every tool-server call is reported under one runtime-chosen name, not the tool's own name, because code mode is the only route to a tool server.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_OPENCODE_TEST=1` and skip cleanly without it. The other `SORTIE_OPENCODE_*` variables they read are described in that file. Traps:

- A first launch on a clean machine runs database migrations before the first event, and the free hosted model can be slow to answer. Give the suite a generous read timeout.
- A test that resumes a session must run both turns in the same workspace.
- The scripted-model case isolates `HOME` and the XDG directories, so a version-manager shim that reads `HOME` fails to start. Name the binary by an absolute path.
- OpenCode 2.x offers MCP tools only inside code mode, and it offers a server's tools only from the request after the tool listing. A scripted endpoint has to script an `execute` call and wait for the listing before it answers.

## Entry points

Two routes drive this runtime. The `opencode` kind, described in the sections above, launches `opencode run` once per turn. The generic `agent-client-protocol` kind drives `opencode acp` and keeps one process open for the session. This page records how the protocol route meets the generic adapter and how it compares with the native stream. The sample workflow for the protocol route is `examples/WORKFLOW.agent-client-protocol.opencode.md`.

The `acp` subcommand is the only protocol entry point, and it takes no model switch; the protocol observations below say where the model comes from. The route carries session continuation and Sortie's own tool server over HTTP, and its spend comes from the runtime's session export.

The native entry point is `opencode run --standalone --format json`, with `--session` to resume. `--auto` approves permission requests, and the asking posture leaves it out.

A stored provider login authenticates a run. Every grade on this page was measured under a stored provider credential.

## Load-bearing capability observations

Protocol session continuation blocks both verdicts: it is a gap where the native stream is usable. Permission handling, the human-input case on the protocol surface, the runtime failure case on the protocol surface, and the success turn on the native stream stay unmeasured. Token accounting is a gap on both surfaces, so it does not separate the routes.

No prompt response in the capture carried a token figure, although Sortie reads one from the wire when it is present. Sortie took the figure from the runtime's session export, the same source the native kind settles from. The ceiling stop worked: a live run through Sortie's orchestrator stopped at the figure ("stopped at 53750 token(s) ... token budget hold"). The grade stays a gap because it describes the wire, the way the Gemini notes record theirs.

The native stream carries no token figure on its terminal either. `opencode run --format json` has no terminal envelope: the stream ends with a `text` event or with nothing. A `step_finish` event carries tokens only on a tool step, so the final text step of a turn reports none. Turn disposition on this surface is not observed for that reason: the success turn ended with no end-of-turn terminal the profile can recognize, so there is no disposition to grade.

Retry classification on the native stream is a gap because the run stream has no human-input terminal. The case was induced and the surface reported no outcome, so it counts against the surface. On the protocol surface the same case is not observed, because no request of either class was raised under the posture the probe ran in.

Cancellation passed on the protocol surface, where the turn ended in cancellation after its own context was cancelled. On the native stream SIGINT yields an `error` event, never a cancellation terminal, and the runtime writes its terminal only at exit, so cancellation cannot be induced there. The retryable failure case passed on both surfaces: after one SIGKILL to its group, the launch reported transport loss.

Two cases are declared, not measured, on both surfaces: a turn ending in `runtime_refusal` and a retry classified as `non_retryable_refusal`. A request built to trigger one completed normally, so the case is recorded as `outcome_never_produced`. The runtime failure case is not declared. The runtime reports an endpoint failure as a failed turn on the protocol, but the qualification's refused-model inducer cannot induce one: an unresolvable model id is silently replaced by the default model, so the turn completes and the case is not observed on either surface. The `unknown_outcome` case has no deterministic inducer on either surface, so it carries no obligation. `limit_reached` was not induced on either surface.

Permission handling is not observed. Under the asking posture the runtime raised no permission request, and the policy precondition found no refusal answered, so the probe has nothing to grade. That says nothing about how the runtime behaves when it does ask.

- protocol turn_disposition: Not observed: not_observed
- protocol retry_classification: Not observed: not_observed
- protocol token_ceiling: Observed: gap
- protocol tool_server_delivery: Observed: usable
- protocol session_continuation: Observed: gap
- protocol permission_handling: Not observed: not_observed
- native_stream_json turn_disposition: Not observed: not_observed
- native_stream_json retry_classification: Observed: gap
- native_stream_json token_ceiling: Observed: gap
- native_stream_json session_continuation: Observed: usable

A case that was induced and that the surface reported no outcome for keeps its obligation. It counts against that surface; it is not forgiven.

Gaps on the protocol route outside the capture:

- The documentation site has no page for the protocol route of this runtime. The sample workflow points at the generic reference page.

## Protocol-specific observations

The handshake reports the agent's own name, `OpenCode`, and its version, which the gated suite's identity rule reads. It advertises `loadSession` true, `mcpCapabilities.http` true with `sse` false, and `sessionCapabilities` carrying `close`, `delete`, `fork`, `list`, and `resume`. `authMethods` names one login method, which points at `opencode auth login`.

Tool servers declared in `session/new` reach OpenCode over HTTP.

`opencode acp` takes no model switch. The model comes from OpenCode's configuration, and the qualification passes it through `OPENCODE_CONFIG_CONTENT` as a document with a `model` key holding the model id. An id OpenCode cannot resolve does not fail: the runtime silently selects its default model instead. The refused-model inducer, which names an id no provider serves, was served `openrouter/particle-ai/particle-1`, on this surface and on `run`. That is why the refused-model inducer cannot induce a runtime failure. It also means a mistyped model id runs the turn on a model nobody chose, with nothing in the output marking it.

The title agent makes a billed request that no exported message records. With the model pinned in the environment of the Sortie process, the money run served exactly one model and no request attributed to a title agent. The credential-verification session was deleted through the protocol before it could be re-read, so that check covers the run's session only.

On the release the qualification ran on, the usage-limits pull fails: the `experimental.usage.limits` call does not exist on that release, and the session logs the pull as unsupported. The turn is unaffected. The usage limits section above describes the command on releases that have it.

Session continuation is proven only by the recall answer: a successful load alone does not show that the history came back. On the native stream, resuming through `--session` returned the phrase the seed asked the runtime to remember. On the protocol surface of the recorded capture, the recall turn ran under the seed's own session identifier but its answer did not contain the phrase, which grades as `same_session_without_recall`. The previous capture of the same profile recorded `confirmed_same_session` on the protocol surface.

## Native headless observations

`opencode run --standalone --format json` prints JSON Lines on stdout, one event per line: `step_start`, `tool_use`, `step_finish` on tool steps, and `text`. A tool step's `step_finish` carries every token counter, zeros included, and its `messageID` names an export message with identical tokens. A subagent call prints one `tool_use` whose `tool` is `subagent`; the child's events never reach the stream. There is no run-started envelope and no final result envelope. The stream ends with the final `text` event or with nothing, so the profile's recognizer keys on the first `text` event. A turn that ends without one has no terminal.

`--standalone` is required. Without it, `opencode run` attaches to the host's shared background service, which keeps the environment of the client that started it first. It then ignores the per-launch model variable and serves its own default model on every launch.

A cancelled turn leaves an `error` event, never a cancellation terminal. The stream has no terminal for a request addressed to a person.

The profile treats the plain-JSON surface as absent. The output-format flag accepts a default text format and JSON Lines, and no structured single-document format. A declared absence is checked by running the ordinary headless invocation and finding no structured terminal; a launch that fails to start would prove nothing.

## Workspace trust and process boundary

The containment boundary held under measurement: every launch ran inside the run-scoped root, no project settings applied, and every process-group member observed was the launched command or its descendant. The profile treats `opencode.json`, `.opencode`, and `AGENTS.md` in a workspace as project settings.

## Excluded capability cases

- retry_classification human_input: induced, and the surface reported no outcome (terminal_vocabulary_closed), so the case keeps its obligation
- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer on any runtime, so the case carries no obligation
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

Windows live qualification is unobserved.

- native_stream_json turn_disposition runtime_failure: fixture_induction_failed
- native_stream_json turn_disposition success: fixture_induction_failed
- protocol retry_classification human_input: fixture_induction_failed
- protocol turn_disposition runtime_failure: fixture_induction_failed
