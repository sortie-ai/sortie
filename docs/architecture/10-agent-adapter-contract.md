## 10. Agent Adapter Contract

This section defines the interface contract that all agent adapters must satisfy. Adapter-specific protocol details (handshake sequences, JSON-RPC framing, stdio vs. HTTP transport) are documented separately per adapter.

### 10.1 Agent Adapter Interface

An agent adapter must implement the following operations:

- `StartSession(workspace, config, settings) -> Session`
  - Launch or connect to an agent process/service in the given workspace.
  - `settings` is the resolved settings block of the session's agent kind for this attempt (§5.3.9). It is read-only, and absent reads as an empty block.
  - Returns an opaque session handle.
- `RunTurn(session, prompt, issue, on_event) -> TurnResult`
  - Execute one agent turn with the given prompt.
  - Delivers every event to the orchestrator by calling `on_event` during the turn; the returned `TurnResult` carries the turn's outcome, not its events.
  - Returns when the turn completes (success, failure, or timeout).
- `StopSession(session)`
  - Terminate the agent process/service cleanly.

An adapter is constructed once per process, with no settings, and holds none: every setting reaches it as the `settings` input of the session it starts, and the adapter keeps what it parsed in that session's own state for `RunTurn` and `StopSession`. A block the adapter cannot parse fails `StartSession` before anything launches, with the `agent_not_found` error kind wrapping the fault and carrying the text the kind's offline validator reports for the same fault. The orchestrator refuses such a block before it reaches a session (§8.4), so this is the second line. A kind that reads `model` and `effort` reads `model` through the one shared reader, as it reads `effort`, and delivers its session's values on every turn, the first turn of a resumed session included, exactly as for a new session. It never relies on the runtime remembering an earlier value: a runtime may keep a session's model and forget its level, and a resumed session is resumed with the settings the attempt resolved, which can differ from the previous attempt's.

A registered kind declares `Deprecation`, `nil` for a kind that is not deprecated and non-nil naming the replacement kind for one that is, and no built-in kind declares a deprecation. A deprecated kind stays registered and constructs and runs unchanged; the declaration is read-only after registration. A configuration reaching a deprecated kind draws one advisory, reported once as a `sortie validate` warning and once per appearance in the run log, naming the kind and its replacement.

A kind that launches a command declares `RequiresCommand`, and declares `DefaultCommand`, the command it launches when it is given none. The literal a kind's `StartSession` hands the shared launch resolution is that same package value. A kind with a default command needs no `agent.command`; a kind with none, `agent-client-protocol` among them, launches only a command written for it. `mock` and HTTP-based kinds launch no command.

A registered kind declares `EffortForwarding`, what it does with the `effort` key of its own settings block. `forwarded` declares that the kind reads the key through the one shared reader and delivers a non-empty value to its runtime on every turn of every session it starts, credential verification included. `not_forwarded` declares that the kind reads no such key. An undeclared kind fails registry-enumerated completeness. The key means the same in every forwarding kind's block (§5.3.5): the runtime's own level name, delivered exactly as written and judged by the runtime alone, with the runtime's own default when the key is unset. A delivered level outranks every host input that names a level, so the level the versioned workflow names is the level the runtime receives. An adapter whose runtime lets a launch-environment variable outrank its delivery withholds that variable from every launch of a session that sets the level (§10.7). A cap that a runtime's own settings impose still applies. A configuration that reaches a `not_forwarded` kind and sets `effort` in that kind's block draws one advisory, reported once as a `sortie validate` warning and once per appearance in the run log, and it blocks no dispatch. `agent-client-protocol` and `mock` declare `not_forwarded`: the generic kind types none of a runtime's switches, so the operator writes a runtime's own reasoning option in the command written for it, and `mock` launches no runtime.

A kind whose adapter was removed declares a retirement instead: an entry in the retired-kind registry that names a replacement kind, the kind's credential variable names, and a pure, deterministic conversion of the retired kind's command and settings onto the replacement kind. The replacement kind MUST be registered, not deprecated, not itself retired, and not the retired kind. A retired kind has no adapter, so a configuration naming it converts at load rather than constructing one (§6.1). The live registration wins: a name registered both as an adapter and as a retirement runs as the live kind and draws only its deprecation advisory. The declaration keeps its runtime facts, the credential names and the default executable of the removed kind included, in the removed kind's own package, so the core names no runtime.

Built-in agent adapter kinds:

- `claude-code`, `copilot-cli`, `codex`, `opencode`, `mock`, and `agent-client-protocol` are built-in agent adapter kinds.
- `claude-code`, `copilot-cli`, and `opencode` use one local subprocess per turn.
- `codex` uses one persistent local subprocess per session.
- `agent-client-protocol` uses one persistent subprocess per session, launched locally or through the SSH worker, in which a turn is a single open request that ends with a declared stop reason, a third session model beside the two above.
- `mock` launches no process and belongs to none of these three session models.

Built-in adapter summary:

| Kind | Session model | Event surface | Resume and identity | Notable differences |
|---|---|---|---|---|
| `claude-code` | One subprocess per turn | Newline-delimited JSON from `claude -p --output-format stream-json --verbose` | New sessions use `--session-id`; continuation turns use `--resume`; runtime `session_id` may replace the provisional adapter-generated ID | The usage component counts each streamed assistant message's usage provisionally until the per-process, per-model figures of the process's `result` events settle the turn. A turn without that figure settles from the runtime's session journal on its host, read locally or through the shared remote read. A resumed process reports only its own requests. |
| `copilot-cli` | One subprocess per turn | Newline-delimited JSON from an abbreviated `copilot -p --output-format json -s --autopilot --no-ask-user ...` invocation | The adapter assigns every session's own identifier, a v4 UUID minted in `StartSession`; the first turn of a session it created passes `--session-id <uuid>`, and every later turn passes `--resume <uuid>`; no launch ever passes `--continue`, which resumed whichever session the home directory saw most recently | The turn's outcome comes from the runtime's own task-completion report rather than from the terminal event's exit code alone; a turn that ends without that report is reported as an incomplete turn with error kind `turn_incomplete`. The adapter always includes `--max-autopilot-continues` and includes --allow-all unless copilot-cli.allowed_tools is set, in which case that allow-list replaces it, while copilot-cli.denied_tools, copilot-cli.available_tools, and copilot-cli.excluded_tools leave it in place. The usage component counts each turn from the running totals per served model that the runtime's usage checkpoints report on the event stream, cumulative over the session. A resumed session starts from the total of the per-request records its journal already holds. A turn that ends without a successful result event, or that the runtime reports aborted, settles from those records, read locally or through the shared remote read, so a run on an SSH worker is measured like a local one. The runtime does not write those records at once, so a killed turn, or a cancelled one the runtime exits before it journals, can end with no figure, its spend unaccounted. The result event's premium requests are a vendor unit. A credential-verification session (§10.9) adds `--disable-builtin-mcps`, `--max-autopilot-continues 0`, `--available-tools view`, and `--no-custom-instructions`, drops the operator's `--available-tools` and `--excluded-tools`, passes no `--additional-mcp-config`, and is deleted by `StopSession` through a `session.delete` call over the runtime's `--server --stdio` interface. |
| `codex` | One persistent `codex app-server` subprocess per session | JSON-RPC 2.0 over stdio | `ResumeSessionID` maps to `thread/resume`; otherwise the adapter starts a new thread; thread ID is the session ID | Turns are started inside the persistent session with `turn/start`; tool handling is Sortie's tool server, declared through configuration overrides the adapter supplies on the app-server command line: a local launch declares the stdio sidecar, and a remote launch receives the host-side server through the remote execution channel; each override approves its server's tools in advance because the never-ask approval policy would refuse a call that asks; every other approval stays part of the app-server protocol. The usage component counts each request from the thread-cumulative totals of `thread/tokenUsage/updated`: from zero on a thread the adapter starts and, on a resumed thread, from the total the app server repeats once when the thread resumes, which counts nothing. A turn that ends without `turn/completed`, or that the runtime reports interrupted, settles from the per-request records of the thread's rollout file on its host, read locally or through the shared remote read. The reported model comes from the thread operation's response and is updated on a runtime `model/rerouted` reroute. |
| `opencode` | One subprocess per turn | Line-delimited JSON from `opencode run --format json --standalone` with the prompt on standard input | Each session start queries the configured command's version, refusing a version it cannot read and a major other than 2; `ResumeSessionID` maps to `--session <session_id>`; the first observed `sessionID` becomes the session ID; a mismatch is `turn_failed` with error kind `response_error` | The adapter maps `opencode.model`, `opencode.agent`, `opencode.variant`, `opencode.thinking`, and `opencode.dangerously_skip_permissions` to CLI flags, `opencode.effort` and `opencode.variant` filling one variant slot, which folds into `opencode.model` as a `#`-separated suffix and refuses a block that sets both; parses `step_start`, `text`, `reasoning`, `tool_use`, `step_finish`, and `error`; maps plain-text permission warnings to `notification` and unknown output to `malformed`; reads usage from two sources, the `tokens` of each `step_finish` report on the stream as the live source, one report per model request, and `opencode session export --standalone --sanitize <session_id>` as the records source, one record per finished assistant message, read again for each subagent session the turn ran; maps logical `error` events to `turn_failed` even when the process exits with status `0`, naming a `bash` or `read` tool the hosted free tier requires when the session's own tool policy denies it. Tool handling is the same tool server, declared through the runtime's inline configuration document: a local launch declares the stdio sidecar, and a remote launch receives the host-side server through the remote execution channel. |
| `mock` | Launches no process | Canned events for tests | Configured directly by the test rather than recovered from a runtime | Delivers no tool servers |
| `agent-client-protocol` | One persistent subprocess per session, launched locally or through the SSH worker, in which a turn is a single open request that ends with a declared stop reason. This is a third model beside the one-subprocess-per-turn kinds and the persistent-session kind. | Newline-delimited JSON-RPC over stdio, with the runtime named by `agent.command`, and one closed set of session updates normalized at the boundary. | The session identifier comes from the session-creation response; continuation is attempted only when the agent advertised it, is confirmed by observation with a negative control, and falls back to a new session when the advertisement is absent or the observation fails. A load is additionally held until the clock leaves the UTC minute in which this process created the session being loaded. | The stable usage notification reports context occupancy rather than spend, and the optional per-turn extension a prompt result may carry is read as a presence signal and a lower bound only, so no accounting figure comes off the stable line. A figure instead comes from the usage declaration the session runs under or from a measurement source outside the protocol. A usage declaration, selected once from the name the runtime gives in its handshake, says how the runtime reports spend: it reads the usage a prompt result carries as the runtime's own live source and, for a runtime that keeps them, the per-request records on its host as its records source, and one shared usage component counts each model request once whichever source reports it, with the records only splitting by served model an amount the live source settled. A local and a remote launch attach the same sources. A measurement source is offered the launch through one interface the transport asks without naming any runtime; session construction can start the runtime up to twice more before the session is created, so the subprocess the session runs on carries only the settings of the measurement source its handshake confirms. It refuses no runtime build by its version, proves its own applicability from the records that source writes, and supplies the turn's figure through the shared turn-end usage report. The same seam carries per-runtime verification additions, which a credential-verification session of a local launch naming a runtime that has one applies to drop agent context (§10.9); a remote launch and a command line no addition names carry none. A turn that issued a model request no figure was proven to account for, whether none was produced or the one produced fell short of the turn, is recorded as spend occurred, part of the amount unknown. Tool servers are re-expressed into the session-creation request. When the runtime advertises HTTP tool-server support, the request carries Sortie's tool server as an HTTP server on the orchestrator host, on a local launch too, and the operator's HTTP servers beside it. A runtime without that support receives the stdio declaration on a local launch and opens a remote session with the tool-server gap declared. Permission requests are refused inside the protocol by selecting an option of a refusing kind. |

### 10.2 Session Lifecycle

The orchestrator interacts with an agent session as follows:

1. Before the first turn, run the shared credential-verification step (§10.9) unless the worker holds a fresh proof of the same agent setup: a separate session, started, sent one fixed request, and stopped, that proves the runtime completes one model request with its credential.
2. Call `StartSession` for the working session.
3. Call `RunTurn` for each turn. Continuation turns reuse the same session.
4. Call `StopSession` when the worker run is ending.

The session handle and any session identifiers are adapter-specific. The orchestrator treats `session_id` as an opaque string.

The worker accepts a session id from three sources, in this order: the working session's `StartSession` result, each relayed `session_started` event's non-empty `session_id`, and each coding or self-review turn result's non-empty `session_id`. An empty report is ignored, and a later report replaces the currently accepted session id. The credential-verification session's identifiers (§10.9) are never accepted, whether reported through an event or through its own turn result.

A turn result's `session_id`, when non-empty, MUST name only the identifier the adapter would continue the session under as of the end of that turn; it MUST NOT name a session id the session has already replaced.

The accepted session id is what the dispatch identity record (§9.5.2), the worker's exit result, the exit's `session_metadata` row, and a continuation retry's resume identifier all carry. A park record carries the accepted session id of the run it follows when that run had no reaction kind and ended normally, so the first run after a person releases the park resumes it; any other run's session id is not kept, because the retry policy starts a new session after it.

#### 10.2.1 Handoff-Evidence Ownership

The handoff-evidence verdict belongs to the orchestrator. It is computed from the workspace baseline and the positive SCM signals the orchestrator already reads, after an otherwise-eligible normal worker exit. This decision adds no operation, event, result field, or work-classification obligation to the agent adapter interface.

An adapter's turn disposition (§10.8) and the handoff-evidence verdict answer different questions. The former normalizes what one runtime reported about a turn; the latter decides whether the orchestrator observed durable output behind a tracker handoff. A successful adapter result, a non-zero completed-turn count, token usage, or a tool call is diagnostic context only and cannot by itself establish either work observed or absence of work.

A future adapter may contribute a positive work signal. Such a signal can only change an absence or undeterminable case to work observed; it cannot declare absence and does not transfer ownership of the verdict from the orchestrator. This preserves one monotone verdict across present and future runtimes.

### 10.3 Normalized Event Types

The orchestrator expects the following event types from any agent adapter. Adapters map their native protocol events to this normalized set:

- `session_started`: session initialized successfully
- `startup_failed`: session could not be initialized
- `turn_completed`: turn finished successfully
- `turn_failed`: turn finished with failure
- `turn_cancelled`: turn was cancelled
- `turn_ended_with_error`: turn ended due to an error condition
- `turn_input_required`: agent asked for a decision only a person could give, ending the attempt under the shared refusal posture
- `token_usage`: normalized token usage event: `{input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens}`. Optional `model` field (string) identifies the LLM model when available. Optional `api_duration_ms` field (int64, milliseconds) carries per-request or per-turn API response wait time when the adapter can measure it.

Counter definitions: `input_tokens` counts every token sent to the model, including prompt-cache reads and prompt-cache writes. `output_tokens` counts every token the model generated, including reasoning tokens. `cache_read_tokens` is the subset of `input_tokens` served from the prompt cache and MUST NOT be added to any other counter. `cache_write_tokens` counts input tokens written to the prompt cache; it is disjoint from `cache_read_tokens`, is zero when the runtime reports no cache-write count, and MUST NOT be added to any other counter. `total_tokens` is `input_tokens + output_tokens`, computed by the adapter: an adapter MUST NOT pass a vendor total through, and MUST report `0` for a counter its runtime does not expose.

Scope: every reported value is cumulative over the run, meaning the agent session the orchestrator opened with `StartSession`, across every turn of that session, and excludes usage a resumed session accumulated before `StartSession`. The sequence of values an adapter reports within one run is monotonically non-decreasing per component.

Resume: for a runtime whose usage figure is cumulative over its session, the usage component starts a resumed session from the figure the live source repeats once on resume or, when the live source repeats none, from the total of the records the session's records source already holds. A runtime whose per-model counters also span the session reports each served model's running total on the live source, and the component splits the turn from them. The usage component subtracts the starting point from every reading through its resume baseline, which treats a reading of all zeros as no reading and a reading below the previous one as a counter restart: the run figure so far is kept and counting resumes from the estimate. When the starting point is unknown, the baseline is estimated as the first reading less the request's own usage where the runtime reports one, and a counter restart re-derives it the same way. The turn reports `SpendUnaccounted` exactly when the re-derived baseline is non-zero in a counted component (the reading less the request's own usage, or all of the reading when the runtime reports none), because the estimate cannot separate the restored figure from calls the turn's sub-agents made; a re-derived baseline of zero is exact and unflagged, as for a healthy turn after a context-window overflow. A session whose starting point was estimated reports no later figure as covering the session. A continuation whose starting point is known is exact and unflagged.

Emission rules: an adapter SHOULD emit one `token_usage` event per model API request when it can observe request boundaries, and MUST NOT emit more than one `token_usage` event per observed request. An adapter MAY carry the run-cumulative snapshot on a turn-finalization event (`turn_completed`, `turn_failed`, `turn_cancelled`, `turn_ended_with_error`), which the orchestrator accumulates without counting it as an API request. `TurnResult.Usage` MUST carry the same run-cumulative snapshot as the turn's last emitted value.

Measurement semantics: emitting a `token_usage` event asserts that the runtime reported usage for the observed request. An all-zero payload asserts a measurement of zero, which is a legitimate statement distinct from having no measurement at all. An adapter MUST NOT emit the event when the runtime supplied no usage figure for the observed event. This qualifies, rather than repeals, the sentence above requiring an adapter to report `0` for a counter its runtime does not expose: reporting `0` for one counter inside a measurement is a statement about that counter, while emitting no event is a statement about the whole request.

`TurnResult.UsageMeasured` carries the same assertion at turn granularity: it is true once the adapter has observed at least one usage measurement for the session, by the end of that turn, and false otherwise. It is monotone: once true for a session, it MUST remain true for every later turn of that session. An adapter that can never measure usage reports `UsageMeasured` false on every turn and emits no `token_usage` event. An adapter whose ability to measure depends on the run, rather than being permanently absent, reports the property per run rather than as a static adapter property.

`TurnResult.SpendUnaccounted` reports a third state beside a figure and its absence: the turn issued a model request that no figure was proven to account for, either because no source produced one or because the one produced was not proven to reach the whole of the turn, so spend occurred and part of the amount is unknown. A turn that produced no figure at all contributes no number to any total and produces no `token_usage` event. A turn whose figure fell short reports that figure and emits its event as any measured turn does, and only what the figure leaves out stays unknown. Neither state is a measured zero or an estimate, because no arithmetic ever produces a figure for what is missing. It does not qualify `UsageMeasured`, which asserts that at least one measurement exists rather than that the accounting is complete, so both may be true of one session whose turns differ and of one turn whose figure fell short. It is per turn and not monotone. The flag changes neither `Usage` nor `UsageMeasured`: an unsettled turn still reports its partial figure.

One shared rule in `agentcore` sets the flag, and an adapter supplies only its runtime's evidence for each turn. A turn reports `SpendUnaccounted` when its resume baseline is estimated, or when the runtime served it output and the adapter holds no figure the runtime reported as covering the whole turn. Served output is a message or delta, a reasoning item, a tool call, a sub-agent task event, or a usage figure of this turn; a request-start announcement, the user's own or a replayed message, and session setup are not. A figure covers the whole turn only when the runtime's completion signal for the turn implies it, so every cut-short turn is unsettled, one ended to wait for a person's answer included. A sub-agent event's own token count is evidence that output was served and never feeds a figure.

`TurnResult.UsageProvenance` names the source category and the strategy whose figure settled the turn's usage. A source category is a vendor-neutral label from a closed set the usage component owns: the agent protocol's own messages on the session's standard streams, or the runtime's own session records on its host, an entry per model request. The strategy states how the source's reports reach the adapter: live, pulled, or read from records. Only the usage component assigns the value, from the source that settled the turn; an adapter and the orchestrator carry it unread, and the shared turn-spend rule in `agentcore` is the only writer onto a turn result. Its source is empty when the turn settled no figure or its adapter does not account through the usage component. It states which source settled the figure and says nothing of how complete the figure is, which `UsageMeasured` and `SpendUnaccounted` carry.

A turn cancelled after the runtime served output settles from the runtime's session records when they hold an amount for the turn. The records' figure replaces the cancelled reply's live figure and the two are never added, so no request counts twice; the turn carries the records source's provenance and `SpendUnaccounted`, because the request in flight at the cancellation sits in no figure. When the records hold nothing for the turn, because no boundary is known, the read failed, or no record is new, the live reading stands as a lower bound and the turn carries the live source's provenance. A later live reading adds only what neither figure counted. A live source that reports the counts of each model request under its own identity holds the greatest reading per identity, and the held readings sum to the live figure. The reading that settles a turn is the report of the request that ended it. Without that report the records replace the held readings, and with nothing new in the records the held readings stand as a lower bound with the live source's provenance. Such a live source never covers the session. A cancellation before the runtime served output keeps the live path. A records source declares whether a request's record is written when the request completes or only when the turn ends. Records written only when the turn ends cannot settle a killed turn: that turn's spend occurred and its amount is unknown, never zero, and it carries `SpendUnaccounted` and no records provenance.

Two further turn-result fields let the orchestrator stop counting a flag that a later figure covers. `SpendCoverable` marks a completed turn that served output, reported no covering figure, and has no estimated baseline. `CoversSession` marks a turn whose covering figure is the runtime's running session total, or the complete total of the runtime's session records, which hold no child session's requests; either holds what every earlier completed turn spent whether or not the adapter read it. It is never set on a turn with an estimated baseline, and no turn covers the session after a turn left a child session's records uncounted, or after a resumed session fixed its history at a turn's end on a per-turn or per-request live source. Only a completed turn is coverable, because the request in flight at a stop or a failure is missing from the runtime's saved totals, and a turn the shared skeleton builds itself after a cancellation or a launch failure is never coverable. The orchestrator folds the turn results of a run in order. A result of a different session than the previous result clears the run's coverable count. A flagged result adds one to the run's unaccounted count, and one to the coverable count when it is coverable; a flagged result that is not coverable clears the coverable count, because an estimate cuts the session total's link to earlier turns. A `CoversSession` result subtracts the coverable count from the unaccounted count and clears it. The run's resulting count goes onto the run-history row, where a non-zero count makes the issue's summed total a lower bound on what the issue spent rather than the whole of it, in the same way a non-zero unmeasured-session count already does.

Every registered agent kind, and every runtime declaration under the generic protocol kind, registers the usage sources it attaches to each session it runs, local or remote. A live source, the runtime's own output or its protocol messages, is required, and registration refuses a kind or a declaration without one; no reason takes its place. A live source that reports spend only in a vendor unit, such as credits, is a live source. A records source, the runtime's own session records on its host, is optional. A consumer reads the disposition below from the sources a kind attaches, never from a declaration made beside them, so the pair is known at dispatch without the runtime's identity and is the same for a local and a remote session.

`UsageArrival` states when a token figure becomes available: `incremental` (the live source counts tokens after each model request while the turn runs, so one `token_usage` event arrives per observed model API request while the turn's work is still in flight), `turn_end` (at most one figure settles per turn, after the turn's work is over and before the turn's terminal event), or `none` (no attached source counts tokens, because spend is reported in a vendor unit only or not at all). `UsageAttribution` states what a figure attributes to: `per_model` (at least one usage-bearing event of the session names the model that produced it), `session_total` (the sources guarantee no model name, though a figure may still name one when a source the session attaches names it), or `none` (there is no figure to attribute). `none` is derived only, never declared, and arrival and attribution are `none` together exactly. Both vocabularies carry an empty, undeclared zero value distinct from every declared member.

The derivation reads the sources in this order. When the live source counts tokens, the arrival is `incremental` if it reports after each model request and `turn_end` otherwise, and the attribution is `per_model` if that source names the served model or a records source that counts tokens does, and `session_total` otherwise. When only the records source counts tokens, the arrival is `turn_end` and the attribution is `per_model` if that source names the served model. When neither counts tokens, both are `none`.

A remote session attaches the same sources as a local one. The live source arrives on the session's standard streams, and a records source is read on the remote host through the one shared remote-read operation. No kind and no runtime is exempt from measurement because its session is remote.

The generic protocol kind attaches the protocol messages as its live source, read at turn end, so every declaration under it derives `turn_end` and `session_total`; the split by served model that a records source makes still reaches the figure through `UsageByModel` and is priced per model. Declarations exist per runtime because the same field means different things across runtimes, and the pump, the domain model, and the orchestrator never branch on the runtime. A runtime with no declaration of its own runs under a default declaration, and so does a declaration that maps no spend of its own, which keeps every other member it declares. A declaration may also carry a launch addition that turns off a model request the runtime makes on its own and no source records, so the turns of a session are measured as complete, and a reading of child sessions: sessions the runtime runs part of a turn's work in, whose requests the live source does not count and whose own records count in the turn that ran them. A turn that ran a child session is complete only when every child is settled, and a session whose addition cannot take effect is a lower bound throughout. The default declaration reads the standard usage fields of the prompt result as counts cumulative over the runtime's process under the reset rule, with cache inside input and reasoning inside output. That reading is exact when the runtime reports cumulative counts and never more than the true spend when it reports per-turn counts, so a figure read under it is always a lower bound, never complete. A member that is absent or not an integer is unknown, never zero, and a reading that adds no input and no output tokens on a turn that reached the model is no figure. A runtime that reports no usage fields leaves its turns as spend that occurred with the amount unknown.

A declaration is selected from the name the runtime gives in its handshake. A handshake that names a runtime with no declaration selects the default declaration, and one that names no runtime keeps the declaration of the configured command. Where something must know the runtime before the handshake, its identity comes from the configured agent command, never from the SSH executable that launches that command on a remote host, or from the name an earlier handshake of the same launch setup reported; the handshake confirms it. When the handshake names a different runtime than the command does, the declaration the handshake names is selected and the mismatch is reported for that session.

A usage figure MAY carry a per-model breakdown beside its totals: the run-cumulative counts of each model the figure covers, in the same input, output, cache-read, and cache-write classes, covering at most the figure's totals class by class. The breakdown rides on the usage-bearing event (`AgentEvent.UsageByModel`), and an event that carries a run-cumulative snapshot carries that snapshot's breakdown. An adapter whose runtime reports usage split by model MUST carry that split through to the figure rather than summing it into one total under one model name, because runs are priced per model (Section 13.5). The orchestrator folds every usage-bearing event once: each named model is credited what its count adds to what the run already credited it, up to the event's delta, and the rest of the delta goes to the model current when it arrives (the event's model, else the model the run's events named last, else the configured model). The same credits feed the token metric and the run's per-model parts, so the parts sum to the run's totals. The model names an adapter reports stay the runtime's own spelling, because they also label metrics and run history; mapping them onto price-table keys is the pricing component's job, never an adapter's.

A registration MUST reflect the code path that always runs, not an opportunistic path a runtime release can starve. A kind whose authoritative figure comes from a fallback read that the current runtime rarely or never feeds registers the source that does run, not the one that does not.

A kind declaring `turn_end` reports each figure a turn settles as exactly one `token_usage` event, after the turn's work and before its terminal event, carrying the run-cumulative snapshot that the terminal event and `TurnResult.Usage` also carry, the model when the figure's source names one, and no `api_duration_ms`. Its `UsageMeasured` is true from the first turn whose recovery produced a figure to the end of the session. The adapter layer implements this report once for every `turn_end` kind, which supplies the figure and the model and emits no `token_usage` event of its own.
- `tool_result`: a tool call completed. Optional fields: `tool_name` (string, the name the runtime gives the tool, or `{server}.{tool}` for an MCP or dynamic tool; empty when the runtime names none, and never argument text or a protocol item type), `tool_kind` (string, `shell`, `read`, `edit`, or `skill`; empty for any other tool), `tool_argument` (string, the call's one main argument as the runtime sent it: a shell call's command, a read or edit call's path with several joined by `, `, a skill call's skill; empty for any other tool and when the runtime gives none), `duration_ms` (int64), `tool_error` (bool, true when the call returned an error). An adapter maps its runtime's tools to a kind and a main argument and reports no other argument of a call. It does not name a call by its command, title, or protocol item type, and it names an MCP call `{server}.{tool}`. An adapter does not clean, mask, bound, or shorten the name or the argument: the orchestrator does that once for every adapter before it stores, streams, or serves the event. It drops a kind outside the four above together with the argument, removes the `bash -lc` or `sh -c` wrapper from a shell command, removes the run's workspace path from a path, replaces control characters, removes format characters, masks registered secrets (Section 15.3), and cuts the text to 999 runes followed by `…`.
- `notification`: informational message from the agent
- `other_message`: unclassified message
- A frame that carries no text for a person, such as a protocol event type, a JSON-RPC method name, a lifecycle marker, a tool-call tag, or a fixed placeholder, MUST be reported as a `notification` or `other_message` with an empty message, never with the frame's type name or a placeholder. The orchestrator treats an empty one as a liveness signal for stall detection and does not retain it. Agent text, retry and human-request notices, and any message Sortie words itself keep their text. Tool calls are reported by their `tool_result` events.
- `malformed`: unparseable or unrecognized message

Each event should include:

- `event` (enum/string)
- `timestamp` (UTC timestamp)
- `agent_pid` (if available)
- optional `usage` map: `{input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens}`
- optional `model` string: LLM model identifier when available
- payload fields as needed

Token accounting is normalized at the adapter boundary. The orchestrator receives `{input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens}` directly and does not parse adapter-specific payload shapes.

Usage windows: a provider usage window is a quota a provider meters over a span such as five hours or a week and enforces by refusing requests until it resets. An adapter whose runtime reports windows hands the usage component the readings of each report in the runtime's own terms, tagged with the source that carries them; the component normalizes them, and the adapter MUST NOT normalize, name, or merge a window itself. The adapter emits the normalized windows in `AgentEvent.UsageWindows` on a notification with an empty message. A report never removes a window it does not carry: the orchestrator merges the windows of each event into the newest report of every window per agent kind. A window source is live or pull, and its declaration says which. A live source reads the runtime's own output or protocol messages as they arrive. A pull source asks for the windows with a request the adapter makes, either to the runtime itself (a command of its own, or an extension request on the session's own connection) or to the endpoint the runtime itself calls. One policy, owned by the usage component, runs every pull: a pull runs when a turn that reached the runtime ends, whether it succeeded or failed, before the turn's terminal event; it MUST NOT run for a cancelled turn (a turn the adapter ends because the runtime asked for a person included), a turn that timed out, a turn the adapter ends because it cannot read the runtime's output or that output breaks the protocol, or a credential-verification session; it is bounded like an auxiliary launch of the runtime; a pull that fails reports no window and leaves every held window in place; and once the runtime cannot answer a pull in a session, the session pulls no more. A pull asks about the account and sets nothing that enables the runtime's telemetry. A pull reads a credential only where the runtime reads it, in the environment Sortie launches the runtime with or in the runtime's own files on its host, and sends it only to the endpoint the runtime sends it to; Sortie MUST NOT store, log, refresh, or rewrite that credential. A remote session pulls on the remote host: an extension request travels on the session's own connection, a command of the runtime runs through the session's own SSH launch, and a credential file is read on the remote host through the shared remote read, which resolves the remote home; an endpoint request then leaves from Sortie's host. A window may carry an amount, the use and the limit in the vendor's own unit such as credits, which the source declares for each window it reports; the amount is never converted, and the used share is the ratio of the two when the source reports none. A source may report the share of a window that is left rather than used, and its declaration says so. The generic protocol kind reads windows under the per-runtime declaration chosen once per session, by the name the runtime gives in its handshake or, when the handshake gives no name, by the launch command that starts the runtime; a runtime with no declaration reports none. No ceiling reads a window.

### 10.4 Approval, Tools, and User Input Policy

This section covers the approval posture, tool subsystem, and user-input handling for agent sessions.

#### 10.4.1 Approval policy

Every agent run is unattended: no person is present to grant a permission or answer a question while a run is in progress. A request for consent to act is refused in the continuable form the runtime's own protocol offers, where the runtime offers one, so the turn can proceed by another route; a request addressed to a person ends the attempt with the `turn_input_required` outcome. The posture is uniform across every agent adapter and is not an operator-configurable setting.

Policy requirements:

- Each deployment enforces the same refusal posture: a permission request is refused rather than granted, and a request for human input ends the attempt rather than stalling.
- Approval requests and user-input-required events MUST NOT leave a run stalled indefinitely. A configuration value that would let the agent stop and wait for a person is refused before the run, rather than satisfied when it arrives mid-turn.
- Where an adapter kind delivers the tool-registry execution channel (§10.4.3) per session and its runtime asks for consent before invoking a tool, the refusal makes the delivered tools uncallable, and the adapter reports that once per session rather than letting the turn end as an ordinary success.

Unsupported tool calls:

- No adapter routes a tool call: every kind with an execution channel routes calls to the same MCP tool server (§10.4.3), and the tool server answers a name not in the `ToolRegistry` with a JSON-RPC error object, code `-32602`, rather than a result. A result carrying `isError` is reserved for a tool that was found and whose execution failed.
- The session continues after either answer. This is the channel's behavior, not adapter-level behavior; no adapter intercepts or routes a tool call itself.

Ending on a request only a person could answer:

- If the agent asks for something only a person could give, end the run attempt immediately with the `turn_input_required` outcome, distinct from a generic turn failure.

#### 10.4.2 Tool interface contract

All tools that Sortie exposes to agents implement the `AgentTool` interface:

- `Name() string`: stable tool identifier used for matching tool call requests to implementations. MUST be unique within a `ToolRegistry`.
- `Description() string`: human-readable summary suitable for inclusion in agent prompts and MCP `tools/list` responses.
- `InputSchema() json.RawMessage`: JSON Schema describing the tool's expected input. Used for MCP tool registration and prompt-based documentation.
- `Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)`: runs the tool and returns a structured JSON result. The Go `error` return is reserved for internal failures (nil adapter, marshal failure) that indicate programming errors.

Every tool's `Execute` MUST return the uniform result envelope. On success it MUST return `{"success": true, "data": <payload>}`, where `<payload>` is the tool's success object. On a domain failure (missing auth, API failures, invalid input, unreadable local state) it MUST return `{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}`, where `kind` is from the tool's documented closed set (Section 10.4.5) and `message` is a human-readable detail. A domain failure is carried in this envelope with a nil Go `error`; the non-nil Go `error` return signals only an internal marshal failure. All tools marshal both envelopes through one shared helper, so the two shapes are byte-identical at the top level across tools.

#### 10.4.3 Tool registry

`ToolRegistry` is the central registration point for all agent tools.

Invariants:

- Registration is static at build time. All tools are registered during orchestrator initialization, before the first dispatch. No dynamic plugin loading.
- The registry is safe for concurrent reads after construction. Concurrent `Register` + `Get` is a data race; callers MUST NOT call `Register` after passing the registry to the orchestrator.
- Duplicate names panic (programming error, not runtime input).
- The registry feeds both the prompt-time tool advertisement and the runtime execution channel for an agent kind whose declared disposition delivers one; a kind whose declaration delivers none receives neither, which is what keeps the two consistent for every kind. What decides is the declaration, not the kind. The execution channel, where one exists, is the MCP tool server exposed by the `sortie mcp-server` subcommand. On a local launch it is a stdio sidecar the runtime spawns. On a remote launch the server runs on the orchestrator host, never on the worker host, as a child process that serves Streamable HTTP on a loopback port, and the runtime reaches it through a reverse forward carried by the session's own SSH connection. A local protocol session whose runtime advertises HTTP tool-server support reaches the same host-side server over loopback with no forward. Every HTTP request carries a per-session bearer that authorizes only that session's server and stops working when the session closes.
- A remote session settles the forward before the first prompt. A startup probe, one extra SSH connection per session start, reads the forward's outcome, so the opening decision has one path for every kind. A forward the worker host refuses, a forward whose outcome cannot be confirmed, a server process that fails or exits, and a runtime without HTTP tool-server support leave the session running without Sortie's tools. Such a gap never fails the session: it is logged and declared in the session's opening notice, and the worker withholds the tool advertisement from a session that reports at start that its runtime cannot reach Sortie's tool server, so the prompt names no tool the runtime cannot call. A runtime that runs before the first prompt and has not connected to the server within the startup read timeout opens with the gap declared. A runtime that exists only per turn settles its first connection at the first completed turn, and a gap found after the opening notice surfaces in the log only.
- Operator-declared tool servers follow their transport. An HTTP server reaches local and remote launches, with each header value carried by reference through the runtime's environment, or inside the request for a protocol runtime; its URL resolves on the host where the runtime runs. A stdio server reaches local launches only. A remote launch leaves it out, and the opening notice declares the gap.
- Each agent kind declares the tool-server configuration dialect its runtime reads, or declares none with a stated reason. The shared delivery renders every dialect from one server model, and an adapter only places the finished rendering: it never reads the declared servers, the endpoint, or the bearer. The tool server answers every MCP protocol version its protocol library serves, the newest era included, so a runtime that opens with that era connects without falling back.
- The worker generates the tool server configuration once per dispatch. On a local launch the agent runtime decides how many `sortie mcp-server` processes it starts from that configuration and for how long each lives: a runtime that starts one subprocess per turn starts a new tool server process with each turn, while a session-long runtime keeps one process for the whole session. Every tool server process of one dispatch receives the same environment. A remote session has one server process, started on the orchestrator host with the generated environment, which serves every turn of the session; tracker credentials and the host paths the workspace tools read never reach the worker host, and the tools answer as on a local launch.

#### 10.4.4 Tool tiers

Tools are classified by their dependency profile. The tier determines security posture, test strategy, and failure characteristics.

**Tier 1, pure orchestrator state.** These tools read from local session state (a workspace state file or the SQLite database) with zero external calls. They are deterministic and fast. Beyond internal bugs, their only runtime failure mode is the failure envelope of Section 10.4.2 when the local state they read is missing or unreadable. Their `error.kind` values come from the closed Tier 1 set `state_unavailable`, `state_malformed`, and `query_failed`: an absent, symlinked, oversized, or unreadable state file is `state_unavailable`, a present but unparseable state file is `state_malformed`, and a failed database query is `query_failed`.

- `sortie_status` (Section 10.4.5)
- `workspace_history` (Section 10.4.5)
- `cost_budget` (Section 10.4.5)

**Tier 2, external dependencies.** These tools interact with external services (tracker APIs, future SCM APIs) through network calls using orchestrator-managed credentials. They are subject to transport failures, authentication errors, rate limits, and per-tool timeouts.

- `tracker_api` (Section 10.4.5)
- `notify_operator` (Section 10.4.5)

Future tools follow the same classification.

#### 10.4.5 Built-in tools

Sortie registers the built-in tools below in the `ToolRegistry`, subject to each tool's availability conditions. The MCP server (`sortie mcp-server`, per ADR-0009) registers a tool only when its required inputs are present in the session environment.

**`tracker_api` (Tier 2)** executes queries and mutations against the configured issue tracker using the orchestrator's tracker credentials.

Availability: only meaningful when valid tracker auth is configured. When auth is absent, the tool SHOULD NOT be registered.

Project scoping: the tool is scoped to the configured project. An agent working on project PROJ MUST NOT be able to query or mutate issues in unrelated projects through this passthrough tool.

Supported operations:

| Operation | Required fields | Description |
|---|---|---|
| `fetch_issue` | `issue_id` | Fetch a single issue by ID |
| `fetch_comments` | `issue_id` | Fetch comments for an issue |
| `search_issues` | (none) | Return issues in configured active states |
| `transition_issue` | `issue_id`, `target_state` | Transition an issue to a target state |

The `TrackerAdapter.CommentIssue` method exists on the adapter interface but is not yet exposed through `tracker_api`.

Tracker dispatch:

The tool delegates to the configured `TrackerAdapter` implementation. Transport, input shape, and query semantics are adapter-defined.

Result semantics: the tool returns the uniform envelope of Section 10.4.2. On success it returns `{"success": true, "data": <payload>}`, where `<payload>` is the per-operation response object. On a domain failure (API-level error, invalid input, missing auth, or transport failure) it returns `{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}`. The `error.kind` comes from the closed set below.

| `error.kind` | Condition |
|---|---|
| `invalid_input` | Input fails schema decode, carries unknown or trailing fields, or omits a required field for the operation |
| `unsupported_operation` | The requested operation is not one of the four supported operations |
| `project_scope_violation` | The target issue is outside the configured project |
| `tracker_transport_error` | The request was canceled or its deadline was exceeded |
| `internal_error` | An unexpected error that the adapter did not classify |
| `domain.TrackerError` kinds | The adapter returned a classified tracker error; its kind passes through verbatim |

The response payload or error envelope is returned as structured JSON that the agent can inspect in-session.

**`sortie_status` (Tier 1)** returns live session runtime metadata. It reads the worker-maintained session state file `.sortie/state.json` in the workspace and makes no external calls.

Availability: registered when the session workspace path is present in the environment (`SORTIE_WORKSPACE`). The tool takes no input.

Response fields:

| Field | Type | Description |
|---|---|---|
| `turn_number` | integer | Current turn within the session |
| `max_turns` | integer | Configured `agent.max_turns` |
| `turns_remaining` | integer | `max_turns - turn_number`, clamped at 0 |
| `attempt` | integer or null | Retry or continuation attempt number; null on the first run |
| `session_duration_seconds` | number | Wall-clock seconds since the session started |
| `tokens` | object | `input_tokens`, `output_tokens`, `total_tokens`, `cache_read_tokens`, and `cache_write_tokens`. Each member is an integer or null; the five are null together, exactly when the sibling `tokens_measured` is false, and each carries its figure otherwise |
| `tokens_measured` | boolean | True when the five figures in `tokens` are a measurement the session's runtime reported; false when nothing measured them, and also false for a session whose resolved usage arrival is `none`, whatever its runtime reports |

On success the tool returns the response object above under `data`, in the envelope `{"success": true, "data": {...}}` of Section 10.4.2. On failure it returns the failure envelope `{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}`. An absent, symlinked, oversized, or unreadable state file yields `error.kind` `state_unavailable`; a present but unparseable state file, including an invalid `started_at`, yields `state_malformed`.

**`workspace_history` (Tier 1)** returns the most recent completed run attempts for the current issue. It queries the `run_history` table (Section 19.2) through a read-only SQLite connection and makes no external calls.

Availability: registered when the database path and issue ID are present in the environment (`SORTIE_DB_PATH`, `SORTIE_ISSUE_ID`). The tool takes no input.

On success the tool returns, under `data` in the envelope `{"success": true, "data": {...}}` of Section 10.4.2, a JSON object `{issue_id, entries}`, where `entries` lists at most the 10 most recent runs, newest first. Each entry has `attempt`, `agent_adapter`, `started_at`, `completed_at`, `status` (`succeeded`, `failed`, `cancelled`, `ci_failed`, `needs_person`, or `budget_stopped`), and `error` (null unless the run failed). The per-entry `error` is the run's own error and is distinct from the envelope's `error` object.

On failure the tool returns the failure envelope `{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}` with `error.kind` `query_failed`. This happens when the history query fails.

**`cost_budget`** returns cumulative per-issue token spend and the remaining token budget so an agent can self-regulate before the ceiling stops the run in flight or blocks a re-dispatch. It sums the uncached tokens of the issue's `run_history` rows (`total_tokens` less `cache_read_tokens`, floored at zero per row) and adds the uncached tokens of the running session's recorded `session_metadata` row, then makes no external calls. It is the advisory counterpart to the `agent.max_tokens` enforcement: the tool adds the running session's recorded `session_metadata` figure, written at most once per throttled write interval apart from the immediate writes the persistence schema section lists as exceptions, while the in-flight lane adds the session's live in-memory figure, which every usage event raises. The advisory reading therefore trails the enforced figure by up to that interval. The tool never reads rates from WORKFLOW.md and never prices anything: a cost it reads is the recorded `estimated_cost_usd` of the issue's `run_history` rows plus the live estimate the orchestrator writes to the running session's matched `session_metadata` row on the same throttled cadence as its token figures, so the agent's reading and the orchestrator's figure share one source and agree after a rate edit made without a restart.

Availability: registered when the database path and issue ID are present in the environment (`SORTIE_DB_PATH`, `SORTIE_ISSUE_ID`), the same condition as `workspace_history`. The running session's dispatch ID (Section 4.2) arrives through `SORTIE_DISPATCH_ID`. The tool takes no input.

On success the tool returns, under `data` in the envelope `{"success": true, "data": {...}}` of Section 10.4.2, a JSON object with the following fields:

- `used_tokens`: cumulative uncached tokens across the issue's completed sessions, plus the uncached tokens of the running session's recorded `session_metadata` row when a session is in flight. Prompt-cache reads are not counted, so the figure is lower than a token total the agent's runtime may report. The running session's spend is added only when the `session_metadata` row's dispatch ID equals the supplied dispatch ID; an empty dispatch ID never matches. The agent adapter's own session ID cannot serve as this key: the tool server's environment is fixed before most runtimes assign a session ID, and a resumed session keeps its session ID across dispatches, so a session ID could not tell the running dispatch's row from a stale one.
- `budget_tokens`: the configured `agent.max_tokens`. `0` means unlimited.
- `remaining_tokens`: `budget_tokens - used_tokens`, floored at `0`; `null` when the budget is unlimited, so the agent distinguishes "no limit" from "nothing left".
- `used_sessions`: completed sessions for the issue. The running session is not counted, matching how `agent.max_sessions` counts. Continues to count sessions whose spend is unknown, because `agent.max_sessions` counts sessions rather than spend.
- `budget_sessions`: the configured `agent.max_sessions`. `0` means unlimited.
- `unmeasured_sessions`: the count of the issue's completed sessions whose coding agent reported no token usage, so `used_tokens` excludes them rather than counting them as zero spend.
- `used_tokens_complete`: `false` when `unmeasured_sessions` is greater than zero, when a completed session recorded a turn that spent tokens no figure was proven to account for, when the matched `session_metadata` row records a turn of the running session that spent tokens no figure was proven to account for, when no dispatch ID was supplied, or when no `session_metadata` row matched the supplied dispatch ID; `true` otherwise. A `false` value tells the agent that `used_tokens` is a lower bound and `remaining_tokens` an upper bound, rather than exact figures.
- `warning_tokens` and `warning_reached`: present together, and only while `agent.token_warning_percent` is set above `0` in the tool server's own configuration, the same source `budget_tokens` reads. Absent together otherwise, so a deployment that never sets the field gets a result byte-identical to one from before the warning threshold existed. `warning_tokens` is the configured threshold in tokens; `warning_reached` is `true` once `used_tokens` has reached it. Because the tool derives `warning_reached` from its own `used_tokens`, and the event loop writes the usage figure that reaches the threshold to `session_metadata` immediately rather than waiting for the throttled write interval, the two never disagree within one result: a reading taken before that write lands still reports `used_tokens` below the threshold and `warning_reached: false`.
- `used_usd`, `budget_usd`, `remaining_usd`, and `used_usd_complete`: present together, and only while `agent.max_cost_usd` is above `0` in the tool server's own configuration, the same source `budget_tokens` reads. Absent together otherwise, so a deployment that never sets the money ceiling gets a result byte-identical to one from before it existed. `used_usd` is the issue's estimated cost in US dollars: the recorded `estimated_cost_usd` of its completed `run_history` rows plus the live estimate of the running session's matched `session_metadata` row, folded by the same rule the orchestrator's dispatch lanes and in-flight lane use, so the agent's reading and the enforced figure agree except for the interval the throttled write leaves. The running session's part counts only when its row's dispatch ID equals the supplied dispatch ID; a row of another dispatch adds nothing. `budget_usd` is the configured `agent.max_cost_usd`. `remaining_usd` is `0` once `used_usd` reaches `budget_usd`, compared in whole micro-dollars, and `budget_usd - used_usd` otherwise. `used_usd_complete` is `false` when some spend could not be priced or was not fully reported, because a run is recorded `unpriced` or `partial`, or `used_tokens_complete` is false; a `false` value tells the agent that `used_usd` is a lower bound and `remaining_usd` an upper bound. The tool reads only the database and never a rate.

The tool's description tells the agent that prompt-cache reads are not counted in `used_tokens` or `remaining_tokens`, so `used_tokens` is lower than a token total its runtime may report and the two are not to be compared.

When a threshold is in force, the tool's description carries one further added sentence naming `warning_tokens` and `warning_reached` and advising the agent to wrap up or hand off before the ceiling stops the run; the description carries no such sentence when no threshold is configured.

When a money ceiling is in force, the tool's description carries one further added sentence, after the warning sentence when both apply, naming `used_usd`, `budget_usd`, `remaining_usd`, and `used_usd_complete`. It tells the agent that `remaining_usd` is what is left before the ceiling stops this run or blocks a further session, that a `false` `used_usd_complete` makes `used_usd` a lower bound, and that the figure is Sortie's estimate from its price rates, not the provider's bill, so it can differ from a cost the agent's runtime reports and the two are not to be compared. The description carries no such sentence when no money ceiling is configured.

The asymmetry between `used_tokens` (includes the running session) and `used_sessions` (excludes it) is deliberate: a session is discrete and either finished or not, whereas tokens accrue continuously, so an advisory reading that ignored in-flight spend would be useless exactly when the agent consults it. The running session's spend reaches `session_metadata` through throttled incremental writes during the session; at session exit, clearing the row's dispatch ID and inserting the `run_history` row are separate writes, in that order, so no window double counts when both succeed. If the clearing write fails while the `run_history` write still succeeds, the row keeps the exited session's dispatch ID and its last recorded total, so a caller supplying that dispatch ID again would count this run's spend twice; this is an accepted risk rather than one the tool detects or corrects.

On failure the tool returns the failure envelope `{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}` with `error.kind` `query_failed`. This happens when the budget query fails.

This result shape supersedes the five-field description of ADR-0013 (`docs/decisions/0013-agent-cost-budget.md`); `unmeasured_sessions` and `used_tokens_complete` are additions this section now records as current, and the running session is matched by dispatch ID rather than by the session identifier that decision assumed the tool receives.

**`notify_operator` (Tier 2)** sends a real-time notification to an operator-configured channel while a session runs. The agent uses it to escalate a decision it should not make alone, report progress on a long task, or flag a blocker, without terminating the session. The tool resolves the notifier backends that receive `agent.message` and posts to them; it knows nothing about any specific channel.

Availability: registered only when at least one entry of the `notifications` list (Section 5.3.10) receives `agent.message`: a registered notifier kind that lists `agent.message` in `events`, or that writes no `events` and so receives `agent.message` by default. Entries that receive only orchestrator events, and the `tracker_comment` entry, never receive a tool call and do not count toward registration. Both the sidecar and the main process select the receiving entries from the live workflow configuration, so they agree on the tool set. When no entry receives `agent.message`, the tool is not registered and the agent is never offered a tool whose messages reach no destination.

The agent supplies only the message; the system owns the envelope and the tool input cannot set any envelope field. The input schema rejects unknown fields:

| Field | Type | Required | Description |
|---|---|---|---|
| `severity` | string | Yes | One of `info`, `warning`, `critical` |
| `title` | string | Yes | Non-empty short summary |
| `body` | string | Yes | Non-empty notification detail |
| `category` | string | No | One of `decision_needed`, `progress`, `blocked`, `completed`, `other` |

The system-owned envelope carries a generated notification id, an ISO-8601 UTC timestamp, a source (the hostname by default), the issue id and identifier, the dispatch id, the session id, the attempt, the dispatch-frozen agent kind, and the event type, which the tool sets to `agent.message`. The envelope correlates the notification to a dispatch and a session for an operator and a machine consumer. The tool never sets `AgentText`: it is empty on `agent.message`, and the agent's title and body reach every backend as written (Section 10.4.7).

The dispatch id and the session id are distinct identities. The dispatch id is minted once for the current worker attempt and stays the same for every notification and every tool server process of that attempt; it is new for every dispatch, retry, and continuation. The session id is read from the workspace's dispatch identity record (Section 9.5.2) each time an envelope is built, and reflects the latest session id the worker has accepted for the tool server's own dispatch; it stays empty until the worker accepts one, and an agent kind that never reports a session id leaves it empty for the whole dispatch. A record naming a different dispatch id, or no record at all, also yields an empty session id, so a tool server outliving its dispatch cannot post another dispatch's session id.

Rate limiting: the tool enforces a cap, `max_per_session`, where `0` selects the default rather than unlimited. The cap is drawn from the entries that receive `agent.message`, and it counts only the agent's own calls: an orchestrator event never claims a slot, and a slot never suppresses an orchestrator event. The count belongs to the dispatch and is shared by every tool server process that serves it, kept as files in the workspace's notification slot directory (Section 9.5.3): a runtime that starts a new process for each turn shares the same count across every turn instead of restarting it. A retry or a continuation mints a new dispatch id and starts a new count; a session id change never resets it. A call claims its slot before delivery, not once per backend, and counts against the cap from the moment the first backend accepts the notification; when no backend accepted it, the slot is released and the call does not count. A call whose tool server process ends before any backend accepted the notification, for example because the process was killed, does not count either, except on a workspace filesystem that refuses file locks (Section 9.5.3). A call past the cap returns `rate_limited` and sends nothing. A call whose slot cannot be evaluated returns `state_unavailable` and sends nothing; this also covers a tool server started with no dispatch identity.

Result semantics: on success the tool returns `{"success": true, "data": {"delivered": <int>, "notification_id": "<id>"}}`, the uniform success envelope of Section 10.4.2. On a domain failure it returns `{"success": false, "error": {"kind": "...", "message": "..."}}` with `error.kind` in the closed set below. The Go error return is reserved for an internal marshal failure.

| `error.kind` | Condition |
|---|---|
| `invalid_input` | Input fails schema decode, carries unknown or trailing fields, has an out-of-enum `severity` or `category`, or has an empty `title` or `body` |
| `state_unavailable` | The dispatch's notification count could not be established; no backend is called |
| `rate_limited` | Every one of the dispatch's `max_per_session` slots is occupied: counted, or held by a call still in flight (Section 9.5.3) |
| `send_failed` | A backend returned a transport failure, a non-2xx response, or an unparseable response. The message is a redacted category and never echoes the URL, request body, or response body |
| `backend_unavailable` | No backend could be resolved at execution time (defensive; normal operation gates registration on an entry that receives `agent.message`) |

#### 10.4.6 Tools vs. agent-authored files

The tool subsystem (this section) and the `.sortie/status` file protocol (Section 21) are independent communication channels between agents and the orchestrator. They address different concerns, operate on different transports, and have deliberately different failure characteristics. This separation is a design choice, not an implementation accident.

**Communication patterns.**

| Property | Agent tools | `.sortie/status` file |
|---|---|---|
| Direction | Agent <-> Orchestrator (request-response) | Agent -> Orchestrator (one-way advisory) |
| Transport | Tool call (MCP tool server) | Filesystem sentinel file |
| Timing | Synchronous, during a turn | Asynchronous, read after turn completes |
| Purpose | Data access (tracker queries, orchestrator state) | Control flow (retry suppression, soft stop) and the agent's reason for stopping |
| Failure mode | Tool call fails; agent receives error and continues | File absent or unreadable; orchestrator proceeds normally |
| Agent requirement | MCP client or equivalent tool-calling capability | Write a file to disk (`echo "blocked" > .sortie/status`) |

**Why two channels exist.** The channels serve orthogonal roles:

- Tools are the **data plane**: the agent requests information or performs a mutation and receives a structured result within the same turn. The agent needs the response to continue its work.
- The `.sortie/status` file is the **control plane**: the agent advises the orchestrator about task feasibility after the turn completes. The orchestrator uses this signal to suppress continuation retries. The lines after the first, the agent's statement, carry its reason for stopping, which Sortie publishes with the `session.stopped` event (Section 21.1). No response flows back to the agent.

The `notify_operator` tool (Section 10.4.5) is a third direction that the data-plane and control-plane framing above does not cover: agent to operator. Most tools target the orchestrator and return data the agent consumes to continue its turn. `notify_operator` is a tool by transport (it travels the MCP execution channel and returns a delivery result), but its recipient is a human on a configured channel that subscribes to `agent.message`, not the orchestrator, and it changes no orchestrator state. The three patterns are distinct: read or mutate orchestrator-held data (`tracker_api`, `cost_budget`, `sortie_status`, `workspace_history`), advise the orchestrator about feasibility after the turn (`.sortie/status`), and reach the operator in real time during the turn (`notify_operator`). `notify_operator` does not suppress retries, perform a handoff, or release a claim; an agent that needs to alter orchestrator control flow still writes `.sortie/status`.

Collapsing both into a single MCP-based channel was evaluated and rejected during the A2O protocol design (see `docs/agent-to-orchestrator-protocol.md`, Section 5.1, Alternative 2). The MCP approach fails the agent-agnostic requirement: an agent without MCP client support cannot send the control signal. The file-based channel satisfies all six A2O requirements (agent-agnostic, fail-safe, advisory, zero-dependency, forward-compatible, inspectable) simultaneously; no tool-call-based mechanism achieves this.

**Coexistence.** An agent MAY use both channels in the same session. Typical sequence:

1. Agent calls a tool (e.g., `tracker_api.fetch_issue`) to gather context.
2. Agent determines the task requires a human architectural decision.
3. Agent writes `mkdir -p .sortie && echo "blocked" > .sortie/status`.
4. Turn completes; orchestrator reads the status file, suppresses retries, and, where the dispatch drives issue state, parks the issue and applies the parking label.

The two channels do not interact. A tool call cannot write to `.sortie/status` on behalf of the agent, and the `.sortie/status` file cannot trigger tool execution. The orchestrator processes them at different points in the worker lifecycle: tool calls during the turn (via the execution channel), status file after the turn (Section 21.1, read timing per `agent-to-orchestrator-protocol.md` Section 3.1).

**Defense in depth.** The independence of the two channels provides resilience. If the MCP execution channel is unavailable (tool server crash, agent lacks MCP support), the file-based advisory signal still functions. If the filesystem is read-only or the workspace is on a remote host with restricted write access, tool calls still function. Neither channel is a single point of failure for the other.

#### 10.4.7 Notifier adapter family

Outbound notifications are an adapter family, the same shape as the tracker, agent, CI, and SCM families. Two producers feed it: the `notify_operator` tool (Section 10.4.5), a thin Tier 2 wrapper that runs in the sidecar, and the orchestrator, which publishes its own events from the main process. A new channel is a new package behind the existing interface, not a tool rewrite and not a new call site in the orchestrator.

The family has three layers. Message data is the normalized notification: for an orchestrator event, the typed facts the event reports, and for `agent.message`, the agent's own title and body. One renderer builds the title, the body, and every destination's text from that data. Per-kind transport encodes the destination's text in its wire format and sends it. A producer reports facts and builds no text, and a backend builds no text of its own.

The family has the following parts:

- **The `domain.Notifier` interface** exposes one method, `Send(ctx, Notification) error`. A single method keeps every backend interchangeable and lets any producer reuse the family. An implementation applies a per-call timeout and never logs the endpoint URL, the request body, or the response body.
- **The normalized `domain.Notification`** has two layers. The envelope is system-owned and carries the notification id, timestamp, source, issue id and identifier, the display form of the identifier when it differs, dispatch id, session id, attempt, dispatch-frozen agent kind, and the event type. An orchestrator event may also carry a stage transition: the hop decision of a stage event, holding the source rule, the target rule, the chain identifier, the hop count, and, on `stage.not_advanced`, the reason. It is empty on every other event. The message carries `severity` and an optional `category`, and its content: the agent supplies a `title` and a `body` for `agent.message`, and for every other event the orchestrator supplies the event's typed facts, from which the renderer builds the title and the body. An orchestrator event may also carry `AgentText`, agent-authored text that each rendering places after the body, separated by a blank line, and that the webhook carries under its own key. Only `session.stopped` carries it: the agent's statement from `.sortie/status` (Section 21.1), normalized and masked of registered secrets before it enters the message. It is empty on `agent.message` and when the agent gave no reason. The value is self-contained: every field a backend needs rides in it, with no dependency on producer-only state.
- **The event type** is a closed catalog declared in the domain layer. Configuration validation accepts exactly the names the catalog declares, so a valid name and an emitted type cannot drift apart. Each type declares its severity, and every type except `agent.message` is an orchestrator event.
- **The renderer** is a kind-free component that registers no kind. It is the one place that builds a title, a body, or a destination's text, and every destination takes its text from it. It yields the title and the body, the operator rendering (a severity-tagged title line, the body, and the agent text), and the public rendering (the body and the agent text, never any envelope data). A rendering places a non-empty `AgentText` after the body, separated by a blank line, exactly as written. It also holds the one length cut every kind with a message limit applies: the longest prefix within the limit in UTF-16 code units that ends on a rune boundary, with no marker appended.
- **The `registry.Notifiers` registry** maps a `kind` string to a constructor. Backend packages register in `init()`; the sidecar and the main process resolve backends by `kind` at runtime. This mirrors `registry.SCMAdapters` exactly. The registry reserves the kind `tracker_comment`, and a registration under that name is refused.
- **The backend packages** are one per `kind`. v1 ships `webhook` (posts the notification as a JSON object using generic field names), `slack` (posts a Slack-shaped body whose `text` field holds the operator rendering), `telegram` (posts to the Bot API `sendMessage` method with the body keys `chat_id`, an integer or a string, and `text`, the operator rendering cut to 4096 UTF-16 code units; the bot token rides in the endpoint path), and `discord` (posts to the operator's webhook URL with the body keys `content`, the operator rendering cut to 2000 UTF-16 code units, and `allowed_mentions`, whose empty `parse` list turns off every mention; the webhook token rides in the URL). Every HTTP kind builds its client and posts its JSON through one shared component that applies a mandatory per-call timeout and classifies transport and non-2xx errors into a category that omits the URL and payload; a classified error keeps no request URL in its chain, so a secret in the URL never reaches a log. The `webhook` body carries every envelope and message field under snake_case keys, `dispatch_id` and `session_id` included, and adds `event_type` on an orchestrator event, `agent_text` on an event that carries `AgentText`, and `stage` on a stage event. The `stage` object holds `source_rule`, `target_rule`, `chain_id`, `hop_count`, and, on `stage.not_advanced` only, `reason`; every other payload, `agent.message` included, is unchanged. The `slack`, `telegram`, and `discord` text appends a non-empty `AgentText` after the body, separated by a blank line, as written. A body without `AgentText` and an `agent.message` payload are unchanged.
- **The routing component** is a kind-free package that registers no kind, so the orchestrator may import it while it still resolves every notifier kind through the registry. It turns the loaded `notifications` configuration into a routing table, selects destinations for an event, and delivers. The same table definition serves both processes, and entry selection for the tool uses the same predicate in the sidecar and the main process.
- **The `tracker_comment` destination** is built into the routing component rather than registered as a kind, because a notifier constructor receives only its entry's configuration map and cannot be handed the tracker adapter. It wraps the tracker adapter the process already constructed and posts the event's public rendering on the issue the envelope names through `CommentIssue`: the body, followed by a blank line and the `AgentText` when the event carries one. The public rendering never includes the envelope, and each tracker adapter keeps its own conversion of the text.

The backend packages obey the adapter-family boundary rules: no cross-adapter imports, no importing the orchestrator, normalization to the domain type at the boundary, and generic `notifier_*` vocabulary in the core, never `slack_*`.

**Event catalog.** The orchestrator produces each event at the point where it formerly wrote the matching tracker comment, and the event body, as the renderer builds it from the event's facts, is exactly the text that comment carried; a `session.stopped` event that carries the agent's statement is posted as that body followed by the statement. The stage events had no comment: the orchestrator produces them at the hop decision, with a body of their own that carries no error text, session identifier, or agent kind, so a `tracker_comment` entry may post it.

| Event type | Produced when | Severity |
|---|---|---|
| `session.started` | the dispatch drives issue state, after the in-progress transition and before workspace preparation; a retry that follows a run refused by a provider usage limit publishes none | `info` |
| `session.completed` | a worker exits normally | `info` |
| `session.stopped` | a worker exits on a soft stop (`blocked`, `needs-human-review`, or `no-change-needed`) | `warning`; `info` for `no-change-needed` |
| `session.failed` | a worker exits with an error, or a run whose handoff the evidence verdict withheld; when a provider usage limit refuses an issue's runs one after another, only the first refusal publishes it, and the next refusal after a run the limit did not refuse publishes it again | `warning` |
| `escalation.ci_failure`, `escalation.review_comments`, `escalation.bot_review`, `escalation.merge_conflicts`, `escalation.auto_merge`, `escalation.merge_completion` | the reaction hands its subject to a person, under any `escalation` value | `warning` |
| `auto_merge.merged` | an auto-merge succeeds | `info` |
| `budget.held` | an issue enters the held set under a per-issue ceiling | `warning` |
| `stage.advanced` | a hop decision's result is `advanced` or `partial`: the issue moved to the next stage | `info` |
| `stage.not_advanced` | a hop decision's result is `failed` or `ceiling`: a due move was not made, and `Reason` carries the result | `warning` |
| `agent.message` | the agent calls `notify_operator` | set by the agent |

A cancelled run publishes no session event.

**Routing and delivery.**

- Each `notifications` entry carries an `events` list: the event types that destination receives. An entry of a registered kind that writes no `events` receives `agent.message` only. Routing is a pure function of the loaded configuration and the event type; the agent never influences it, and event names are listed explicitly with no wildcard or filter.
- A destination for orchestrator events is an entry of a registered kind whose `events` hold at least one orchestrator event. The main process constructs it with the same constructor the sidecar uses. An entry that receives only `agent.message` is never constructed in the main process for delivery.
- The `tracker_comment` destination receives the union of the `events` of its single entry and the subscriptions synthesized from the deprecated comment settings and `escalation: comment` (Section 5.3.10). One event is delivered at most once to each destination, however many subscriptions select it.
- A producer routes the event at its decision point, so a detached write is routed by the configuration its decision saw, and then delivers through one call that returns the outcome of every destination. Delivery sends to all destinations concurrently, so a slow notification endpoint never consumes the tracker comment's deadline.
- The producer keeps its own goroutine, wait group, timeout, deduplication, pacing, and metrics. Only `session.started` splits its delivery: the `tracker_comment` target stays on the worker, as the dispatch comment did, and every other destination is delivered detached in the tracker-operations wait group, so an operator endpoint never delays the agent's start.
- A failed delivery to one destination is logged at WARN and does not prevent delivery to the others. Nothing is retried, and no notifier outcome stands in for the outcome of the action a producer uses to confirm its own delivery.
- An orchestrator event never claims a slot in the notification slot directory (Section 9.5.3), and the sidecar delivers `agent.message` only to the entries that receive it.

The `NotifierConstructor` signature is:

```go
type NotifierConstructor func(config map[string]any) (domain.Notifier, error)
```

Backends register via the notifier registry using `init()` functions:

```go
func init() {
    registry.Notifiers.Register("webhook", newNotifier)
}
```

The `config` parameter receives the per-backend fields from the matching `notifications` list entry (Section 5.3.10), with `$VAR` references already resolved and the `kind`, `max_per_session`, and `events` keys removed. A constructor rejects a missing required field or a secret that resolved to the empty string. In the sidecar that surfaces as a fatal startup error rather than a notification posted nowhere; in the main process it fails workflow validation, so startup fails and a reload is rejected with the previous configuration kept (Section 6.2).

### 10.5 Timeouts and Error Mapping

Timeouts:

- `agent.read_timeout_ms`: request/response timeout during startup and sync requests. A wait that can end only after the runtime has heard back from its model backend is never shorter than 60 seconds, the bound below, because provider latency decides its length and a healthy provider can take well over the 5-second default.
- `agent.turn_timeout_ms`: enforced by orchestrator based on wall-clock duration
- `agent.stall_timeout_ms`: enforced by orchestrator based on event inactivity
- `agent.stop_grace_ms`: the graceful-shutdown period a local subprocess launch spends before a force-terminate; see [Section 10.7](#107-local-subprocess-launch-contract)

Error mapping (recommended normalized categories):

- `agent_not_found`
- `invalid_workspace_cwd`
- `response_timeout`
- `turn_timeout`
- `port_exit`
- `response_error`
- `turn_failed`
- `turn_token_limit`
- `turn_request_limit`
- `turn_incomplete`
- `turn_cancelled`
- `turn_input_required`
- `turn_refused`
- `turn_outcome_unknown`
- `credential_unverified`: the credential-verification step (§10.9) did not complete a request with the runtime's credential before the first turn, or the runtime refused its credential on any turn in a structured shape its adapter recognizes. Retryable with exponential backoff.
- `usage_limit`: the provider refused a request because the account's usage allowance is exhausted. The error carries the instant the allowance reopens, when the runtime reported one, and whether the exhausted allowance covers only the model the run used. The adapter converts the runtime's structured fields to that form, and the orchestrator is the only place that validates the instant. Message text is never parsed. A refusal with a future reset instant retries at that instant and holds new sessions of the agent kind until then (see retry and backoff in the scheduling section); a refusal with no usable instant retries on the exponential backoff, as `turn_failed` does. Retryable.
- `port_exit` also covers an SSH connection failure: a remote launch whose ssh client exited with the status OpenSSH reserves for its own connection failures (an unreachable host, a refused SSH login) wraps a shared sentinel into the error, on a working turn and the credential-verification step alike.
- Every registered kind declares whether its adapter recognizes a provider usage-limit refusal: `recognized`, or `not_recognized` with the reason. An undeclared kind fails the registry conformance check, and a reason accompanies `not_recognized` only. A recognizing kind that launches a runtime proves its declaration against a recorded refusal through the shared conformance helper; a recognizing kind that launches nothing proves it through its own scripted outcome. The generic protocol kind declares `recognized` and maps a refusal per runtime: the usage declaration the session selects states which of the runtime's prompt errors and turn notifications is a refusal or a transient failure, and which opt-in, if any, the client sends in its handshake to receive them. A runtime whose declaration states none keeps its refusals in the kind's other error kinds. When the refusal carries no reset instant, the instant comes from the refusal the runtime's own records hold for the turn, or from an exhausted window of the pull the turn already makes, the configured model's own window where each window meters one model, locally or over SSH.
- Two waits end only after the runtime has heard back from its model backend, and each is the larger of `agent.read_timeout_ms` and 60 seconds: the Agent Client Protocol adapter's `initialize` wait, because a runtime may establish its credential with its backend before answering `initialize`, and the first-event deadline the OpenCode CLI adapter sets on the shared fork-per-turn skeleton, because OpenCode prints nothing until the model starts answering. Neither fails a healthy run at the default read timeout.

### 10.6 Agent Runner Contract

The `Agent Runner` wraps workspace + prompt + agent adapter.

Behavior:

1. Create/reuse workspace for issue.
2. Build prompt from workflow template.
3. Start agent session via adapter.
4. Relay agent events to orchestrator.
5. On any error, fail the worker attempt (the orchestrator will retry).

Note:

- Workspaces are intentionally preserved after successful runs.

### 10.7 Local Subprocess Launch Contract

This subsection applies only to adapters that launch a local subprocess (e.g., Claude Code, Copilot CLI, OpenCode CLI, the Codex app-server, and a locally launched Agent Client Protocol runtime). HTTP-based and remote adapters define their own connection semantics.

When `agent.kind` requires a local subprocess:

- Command: the command written for the session's own kind, else that kind's default command. `agent.command` is the default agent kind's command (§5.3.5), and a converted replacement kind launches the converted command (§6.1).
- Invocation:
  - The string form of the command is split on whitespace. The first token is resolved to an executable path using the host's `PATH` lookup rules: a token containing a path separator is used as given, and any other token is searched for on `PATH`, which yields an absolute path. The remaining tokens become a fixed argument prefix inserted before any per-turn arguments (for example, `codex app-server` resolves `codex` and yields `app-server` as a prefix argument).
  - The list form names the executable in element zero and one argument in each later element. Element zero is resolved whole through the same `PATH` lookup, whitespace included, and a failed lookup reports the same not-found error as the string form. The other elements become the fixed argument prefix verbatim: nothing splits, expands, or quotes them. The shared launch resolution is the one reader of the list form, so every adapter, a credential-verification session and an auxiliary launch included, launches it the same way.
  - POSIX and Windows: the adapter execs the resolved binary directly with that argument vector. No shell is involved in local invocation. On Windows the subprocess additionally receives `CREATE_NEW_PROCESS_GROUP` so it can be signaled independently of the orchestrator.
  - When the worker runs remotely over SSH, the string form is instead passed through unsplit and unresolved as the command the remote shell executes; a shell is involved only on that path. The list form is passed as its elements, each single-quoted as one word and joined by one space, so the remote shell neither splits nor expands any element and never reads element zero as an assignment or a reserved word. The whitespace split and the unsplit remote fragment belong to the string form alone. See [Appendix A. SSH Worker Extension (Optional)](27-appendix-a-ssh-worker-extension.md) for the remote execution model.
  - On the Agent Client Protocol kind the command is the whole runtime invocation: the kind types none of a runtime's switches as fields of its own. `sortie validate` refuses each reachable kind that requires a command and has neither a command written for it nor a default command (§6.3), and a launch refuses a whitespace-only command and, locally, a first token that does not resolve to an executable, naming the token. A mistake only the runtime can judge, such as a misspelled switch or an unknown model, is not checked before launch.
  - A runtime that exits before it responds fails with a message stating that it exited, its exit status, and the end of its own standard-error output, bounded in size and masked as §15.3 states. A runtime that answers a startup handshake responds by answering it, so an exit before the answer fails the start. A runtime launched per turn responds by writing a line on standard output that its adapter decodes as its output; a line the decoder rejects is not a response, so an exit before any accepted line fails the turn whatever the exit status, unless the runtime reported the turn's outcome itself (§10.8). An exit Sortie causes, by ending the launch's context or stopping the session, is never reported this way. That failure is never a credential refusal (§10.9).
- Working directory: workspace path, verified immediately before every launch, per Invariant 9; a local launch's `PWD` names that same verified path in its subprocess's environment, replacing whatever `PWD` the orchestrator inherited
- Environment: a local launch carries the orchestrator's environment with `PWD` replaced and without each variable its session withholds; a remote launch neither carries a withheld variable nor lets the remote shell pass it on (Appendix A)
- Stdout/stderr: separate streams

Process group isolation:

- The adapter MUST place the subprocess in its own process group before starting it.
  - POSIX: `Setpgid = true` (new process group at fork time).
  - Windows: `CREATE_NEW_PROCESS_GROUP` creation flag; the subprocess starts suspended, is assigned to a Job Object configured with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so the entire process tree is terminated if the orchestrator crashes, and is resumed only after that assignment, so nothing it starts can run outside the job. A failed assignment is logged and the subprocess runs without the job. A launch whose context is already done when its resume is reached is never resumed: its subprocess is terminated before it executes, and the launch fails as a start refused by a cancellation already in effect. A failed resume terminates the subprocess and fails the launch as a failed start.

Graceful shutdown sequence:

- On context cancellation or `StopSession`, the adapter sends a platform-appropriate graceful shutdown signal to the process group:
  - POSIX: `SIGTERM` to the process group (`kill(-pgid, SIGTERM)`).
  - Windows: `CTRL_BREAK_EVENT` via `GenerateConsoleCtrlEvent` to the process group.
- On a remote launch the local process group holds the ssh client, so the signal ends the connection rather than reaching the runtime. The remote shell then runs the same sequence on the worker host: `SIGTERM` to the session's process group, the same grace period rounded up to whole seconds, then `SIGKILL`. See [Appendix A. SSH Worker Extension (Optional)](27-appendix-a-ssh-worker-extension.md).
- The grace period that follows (`agent.stop_grace_ms`, default 5 seconds) is a ceiling: a shorter caller deadline shortens it. Where an adapter's transport defines a session-close method the runtime advertised, part of that period is spent ahead of the signal, closing the session through the protocol before it is sent. When the grace period or the caller's deadline elapses, whichever comes first, the adapter force-terminates the process tree:
  - POSIX: `SIGKILL` to the process group, resent on a fixed poll interval until the launch's reap takes over or the 2-second group-drain bound elapses.
  - Windows: `TerminateJobObject`, resent the same way until the Job Object reports no running member or the group-drain bound elapses.
- Every termination MUST act on the launch's own process group or Job Object and on nothing else. POSIX addresses the group by number only while the launch's direct child is still unreaped, because an unreaped child keeps its identifier assigned to it; the reap therefore drains the group before it reaps the child, and no group signal follows the end of that drain. Windows reaches the Job Object only through handles the launch owns, and sends the console signal only while the platform's process object still holds its handle to the direct child.
- That force-termination is armed when cancellation starts and fires when the grace ends, so the launch may have ended in between. It MUST NOT act on a process group or Job Object that is no longer the launch's own. On POSIX it stands down once the launch's reap has taken over, because from then on the group identifier may belong to another launch. On Windows it watches the Job Object's membership across the grace and stands down the first time the Job Object reports no running member, and it acts through a handle to the Job Object taken when it was armed and held until it finishes, so a concurrent teardown path closing the launch's own handle cannot redirect it to another launch's job. The reap's own force-kill below takes no such observation and needs none: on POSIX it runs while the unreaped direct child still holds the group identifier, and on Windows it acts only through the launch's own handle.
- On every reap, regardless of whether a graceful signal was ever sent, the reap runs a bounded, resending force-kill against the process group to terminate any process that survived the graceful signal. On POSIX it runs after the direct child has exited and before the child is reaped: the reap waits for the exit without reaping the child, resends `SIGKILL` until no process of the group other than the exited direct child is alive or the group-drain bound elapses, and only then reaps the child. If the reap cannot observe the direct child's exit, it sends nothing to the group and reports the tree unproven. On Windows it runs after the direct child is reaped, through the launch's own Job Object handle, until the Job Object reports no running member or the group-drain bound elapses. A process still alive once the drain bound elapses, or a membership that cannot be read (on Linux, `/proc`; on Windows, a Job Object member whose process cannot be opened or waited on for a reason other than its absence), is reported as a warning, and the reap leaves the tree unproven rather than torn down.
- A launch is cancelled only when a stop Sortie issued began while its direct child was still running; otherwise the child's own exit status decides, whatever ended it and however long the wait for its output ran. A start refused by a cancellation already in effect is a cancellation, any other failed start a failed start.
- A turn on the shared fork-per-turn skeleton ends in the first of these that applies: cancelled, per the rule above; an end of turn the adapter signaled from its output (a session identifier that changes mid-turn, for example), which fails the turn with the kind and message the adapter named; the first-event deadline elapsing before any accepted line; a failed read of the runtime's output; an SSH connection failure on a remote launch; exit status 127; and otherwise the runtime's own exit decides. The deadline, a signaled end of turn, and a failed read each stop the runtime's process group through the graceful shutdown sequence above.
- A turn's usage settles after the runtime is reaped and its standard error drained, on every ending of a started turn except a signaled end of turn, so a cancelled or failed turn reports the figure the runtime had recorded. Settling uses a context that outlives the turn's cancellation, and the adapter bounds its own work. A turn whose launch never started, or failed before it started, settles nothing. On a remote launch Sortie stopped, settling also waits until the remote agent tree has exited, because the local ssh client ends before the runtime does and a host command run against the runtime's state meanwhile would overlap its shutdown; the wait is bounded by the remote supervisor's own teardown bound, one poll interval plus `agent.stop_grace_ms` rounded up to whole seconds.

Standard-output and standard-error ownership:

- Every local-subprocess family (the shared fork-per-turn skeleton behind Claude Code, Copilot CLI, and OpenCode CLI; the Codex app-server; and a locally launched Agent Client Protocol runtime) owns both of its subprocess's pipe read ends for the session or turn's lifetime rather than handing them to the platform's process object. Reaping the subprocess therefore never closes a read end out from under a reader still consuming buffered output on either stream, and the reap runs independently of both readers rather than waiting for either to finish.
- The reap is anchored on the direct child's own exit. Once the reap and the process-group termination have run, every further wait an adapter performs on a reader is bounded: a descendant that inherits an output handle and outlives the direct child can no longer withhold the reap, the process-group termination, or the turn's published outcome. The process-group termination itself resends until no process of the group or Job Object is still alive before it returns, on every reap rather than only for a capture that keeps its own Job Object handle, and reports a warning when a member is still alive once the bound elapses. An adapter that gives up on a reader through an explicit bounded abandon operation emits one WARN record naming the bound; one whose teardown closes its own read end instead reaches the same release without such a record.
- Reaping the process and terminating its group are what release such a reader in the ordinary case, so the turn keeps whatever output the reader had already collected.
- An abandoned standard-output scan is not itself reported as a read failure: the exit code decides the turn, and the drain that precedes the abandonment takes the lines already offered to the consumer. It is bounded, so a producer still writing when it expires loses the tail behind it, and a turn whose terminal evidence was in that tail falls to the exit-based disposition rather than the one that evidence would have given. An abandoned standard-error drain keeps whatever it had already collected, and an abandonment marker is appended after it to signal that later output may be missing.
- An adapter whose runtime answers requests over its standard output reads that stream on a reader that never waits on the session's consumer. When the adapter waits on a request's response directly, the reader hands that response to the waiting request as soon as it reads it. Every other message, including a response the adapter chose to receive in arrival order behind the messages that preceded it, is queued for the consumer in arrival order, without a size limit and without loss. No interval in a session, whether a request awaiting its response, a callback in progress, or the gap between two turns, leaves the runtime's output unread while the runtime is alive. A message that arrives outside a turn, during the handshake or between turns, is processed rather than discarded, and every request the runtime sends receives exactly one reply, in every phase of the session.
- Writing to the runtime is symmetric with reading it: a send returns once its line is queued for delivery, never once the runtime has read it, so no send waits on the runtime's own pace at reading standard input. A request's wait for its own response is bounded by the caller that issued it, not by that pace either.
- An auxiliary launch an adapter runs to read an answer, such as a version canary, a credential or authentication preflight, a usage recovery, or a catalog query, takes the same ownership. Its process group, its Job Object on Windows, is terminated at the reap whatever its exit status, resent until no process of the group or job is still alive or the group-drain bound elapses; this holds on either platform and no longer depends on the launch holding a Job Object handle of its own. A launch running without a Job Object at all, because one could not be created or assigned, is still reaped, but its teardown reaches only the direct process and terminates no descendant it left behind. A termination that fails outright, or that still finds a member alive once the group-drain bound elapses, is reported as a warning naming the launch, and leaves the tree unproven rather than torn down. Every further wait on its output is bounded by the same 5-second drain bound that also bounds a session's standard-error drain, and the direct child's exit status alone decides whether the launch succeeded.

Per-family teardown totals, at default configuration:

- The shared fork-per-turn skeleton (Claude Code, Copilot CLI, OpenCode CLI): the graceful signal wait (`agent.stop_grace_ms`), the standard-error drain bound, and the standard-output drain-after-reap bound, 15 seconds total at defaults. A first-event deadline (OpenCode CLI: the larger of `agent.read_timeout_ms` and 60 seconds, §10.5) bounds the start of a turn rather than any part of teardown.
- The Codex app-server: its graceful wait and its two fixed two-second cleanup waits are unchanged by standard-output ownership. It carries a release, shared with the Agent Client Protocol kind below rather than unique to this kind, bounded by the standard-output drain bound, running from session start through the handshake and every turn rather than as one of `StopSession`'s own waits, that ends a handshake call or a turn otherwise left waiting on a reaped runtime whose reader did not end inside that bound. Here the same release also runs the standard-error collector's bounded wait concurrently with that standard-output wait, both anchored on the subprocess having been reaped and both bounded by the same drain bound, so a handshake or turn failure path that reports the runtime's standard error finds it already resolved rather than paying the bound again. Stopping the session waits for that collector once more, so a stop issued before the release has resolved it, against a runtime whose escaped descendant still holds the standard-error write end, spends one further drain bound inside `StopSession` and carries the total to `agent.stop_grace_ms` plus twice that bound.
- A locally launched Agent Client Protocol runtime: its pinned teardown ceiling is unchanged, `agent.stop_grace_ms` plus three times the standard-error drain bound, 20 seconds at defaults. It reaches that ceiling through its own caller-owned pipes and a final pipe-release step now, rather than through the platform's automatic close of pipes it did not own. It also carries the same release the Codex app-server does, bounded by the standard-error drain bound (`procutil.DefaultDrainGrace`), running from session start rather than as one of `StopSession`'s own waits; unlike the Codex app-server's release, this kind's own drain-and-reap and pipe-release teardown steps already bound and release its standard error, so the release here carries no standard-error arm of its own, and the kind's pinned ceiling stays unchanged.
- An auxiliary launch: its own deadline, plus `agent.stop_grace_ms` when it outlives that deadline, plus at most one group-drain bound, plus at most one drain bound.

Every total above is a floor rather than a ceiling: the process-group or Job Object termination that runs with every reap, on either platform, can itself add up to the group-drain bound on top of the stated total, whenever a leftover member takes more than one resend to clear. The auxiliary launch is the exception, because its own total already counts that bound.

Where a session-close attempt runs, per the graceful shutdown sequence above, it is bounded at half the graceful wait and spent inside it rather than beside it, so it adds nothing to any of these totals.

Recommended additional process settings:

- Max line size: 10 MB (for safe buffering)

### 10.8 Turn Disposition

Every adapter must decide, at the end of each turn, whether the turn completed, failed, or was cancelled, and pair that decision with the returned error. This decision follows one shared rule so that a new adapter inherits it rather than re-deriving it.

The rule is evaluated as an ordered table. The first matching row decides the turn:

1. The runtime reported the turn cancelled (an orchestrator-initiated cancellation): `turn_cancelled`. This row matches on the orchestrator's own cancellation rather than on the word the runtime uses: a runtime report that the turn completed, arriving after the orchestrator cancelled the turn, does not displace this row. A cancellation that begins after the turn's process exited on its own is not this row, however long the wait for its output ran.
2. The adapter recognized a request only a person could answer that the turn cannot continue past: `turn_input_required`. It outranks every failure row below, so such a request is never reported as a generic turn failure, and it ranks under cancellation, so a shutdown already in progress still reports itself.
3. The runtime reported the turn failed, or the adapter itself ended the turn on its own determination (a protocol violation, a timeout waiting for the first response, or a transport failure): `turn_failed`, with the error kind the runtime or the adapter supplied.
4. The runtime reported the turn succeeded: `turn_completed`. A positive report from the runtime is authoritative and is never second-guessed by counting output.
5. The runtime ended the turn without the task-completion report its protocol defines: `turn_failed`.
6. The runtime reported no outcome at all, and the adapter observed no process exit for the turn (a persistent session with no per-turn exit): `turn_failed`.
7. The runtime reported no outcome, a process exit was observed, and the runtime wrote no line on standard output its adapter counts as a response (§10.7) during the turn: `turn_failed`, with the early-exit report (§10.7), whatever the exit status. No adapter family classifies such an exit by its status first.
8. The runtime reported no outcome, a process exit was observed, and the exit code is non-zero: `turn_failed`. The error carries the exit status, so an exit by a signal names the signal.
9. The runtime reported no outcome, the process exited zero, and the adapter looked for its declared work evidence and found none: `turn_failed`. Exit code zero is never by itself a success signal; an adapter with nothing positive to offer reports a failed turn.
10. The runtime reported no outcome, the process exited zero, and the adapter has no work evidence to offer for this kind at all: `turn_failed`, for the same reason as row 9.
11. The runtime reported no outcome, the process exited zero, and the adapter found evidence the model produced something this turn: `turn_completed`.

Extracting the runtime's own report and the per-turn work evidence from a vendor's wire format is adapter-local, and each adapter's protocol document states how it does so. A turn produced work when the adapter observed model-authored content or a tool call the model requested on that turn's own event stream; a token count, a cost figure, a credits figure, and a process exit code are not work evidence. Each adapter declares which of those two facts its runtime exposes, and extracting them from the wire format stays adapter-local. Turning that evidence into a disposition is not adapter-local: every adapter obtains its turn disposition from the one shared rule above, and an adapter whose runtime reports something the rule cannot express extends the rule itself, under review, rather than deciding the case in a private branch.

A provider usage-limit refusal is a failure under row 3 with the error kind `usage_limit`. The adapter reports it in the turn evidence it hands the shared rule: the evidence carries the terminal error kind and, for `usage_limit`, the reset instant and model scope. The rule copies them onto the returned error, so the error of that kind always carries a usage-limit value, empty when the runtime reported no instant, and an error of any other kind carries none.

One adapter sits outside the common shape. Codex reaches no work row at all, because it observes no per-turn process exit.

- Codex's persistent per-session subprocess has no per-turn process exit to observe, so the work rows are unreachable for it and an absent turn-completion report is already a failure on its own terms.

`turn_ended_with_error` remains a documented normalized event type (Section 10.3), reserved for a future adapter whose runtime genuinely distinguishes a transport-class failure from every other failure, even though no built-in adapter emits it today.

### 10.9 Credential Verification

Before the working session's first turn, unless the worker holds a fresh proof of the same agent setup (§10.9.4), the worker runs one shared step, common to every kind that launches a runtime: it starts a separate session through the adapter's own `StartSession`, sends one fixed request with `RunTurn`, stops that session, and judges the outcome with one verdict table. A failure ends the run before any working session, with the `credential_unverified` error kind (§10.5), or, for a runtime that exits before it responds, the early-exit report (§10.7), retried with exponential backoff; a success is followed by the working `StartSession` with the worker's own parameters. The verification session starts from the same parameters and overrides `CredentialVerification`, `ResumeSessionID`, and `MCPConfigPath` for its own session. The attempt's resolved settings block is one of the shared parameters, so the verification session runs with the same `model` and `effort` the working session will. When the run performs the step, its own spend joins the run's usage as an offset applied to every later figure the working session reports (§13.5).

The step is shared across every kind; an adapter contributes only the following:

- **Its declaration of what the verification request still carries** (§10.9.1).
- **Its runtime's switches for a verification session.** No tool server Sortie generates or passes through, no continuation request, no record in any ledger a later session continues from, no stored conversation the runtime offers a way to avoid or delete, and no context component the runtime can drop with a qualifying switch (§10.9.1).
- **Its runtime's structured refusal**, when its protocol or output stream carries one, mapped to the shared `credential_unverified` message on any turn (§10.9.3), rather than left as a generic transport or turn failure.
- **Its runtime's way to drop the verification conversation**, when the runtime offers one, run through the session's own launch target's auxiliary-command composition, so a remote worker's deletion runs on the worker host in the workspace directory.

The verdict never reads runtime prose or standard error. The runtime's own refusal is the structured refusal above. A runtime that exits before it responds, before its startup handshake is answered, or launched per turn without a line its adapter counts as a response (§10.7), is reported with the early-exit report (§10.7) whatever caused the exit, because identity-free code cannot tell a rejected switch from a refused credential that a runtime states only on standard error, and the report carries what it stated. Every other retryable request-time failure is `credential_unverified` whatever caused it, because identity-free code cannot tell a refused credential from a model name the runtime rejects. A start failure other than the runtime's own refusal, a non-retryable kind, a `usage_limit` refusal, and an SSH connection failure (§10.5) keep their own kind and retry class.

Every wait that ends only after the runtime has heard back from its model backend (the Agent Client Protocol adapter's `initialize` wait, the OpenCode CLI adapter's wait for a turn's first event) is never shorter than the same value, sized from the slowest measured handshake across the built-in kinds: 60 seconds.

#### 10.9.1 Verification context contract

The verification request proves that the runtime completes one model request with its credential, so it carries only what that proof needs. The rules below bind every kind that launches a runtime.

- **Keep.** A verification session MUST launch with the working session's resolved command, its workspace as the working directory, its SSH host, its environment and credential files, and its model, effort, provider, endpoint, and permission settings.
- **Drop.** A verification session MUST NOT carry a context component (table below) that its runtime can drop with a qualifying switch. A runtime that can replace its built-in system prompt MUST receive the shared replacement prompt instead. A remote launch of the protocol kind is exempt, because no verification addition claims it.
- **Qualifying switch.** A switch qualifies only when all of the following hold: it applies to that one launch, as an argument, an environment entry, or a protocol parameter of the session; it persists nothing past the session, and a file Sortie creates for it is removed when the session stops; it cannot change the credential, model, provider, or endpoint the runtime resolves; and the runtime documents it in its help text, its official configuration reference, or the protocol schema it publishes. A switch known only from the runtime's binary does not qualify.
- **Settings classes.** In a verification session the adapter MUST NOT pass a kind setting whose only effect is prompt text or tool visibility. It MUST pass every other kind setting unchanged: credential, model, provider, endpoint, effort, permission, and every setting whose effect is not established. The OpenCode CLI adapter is the exception, because its only qualifying tools switch is its permission policy, which a deny-all policy replaces.
- **Declaration.** Every registered kind MUST declare which context components its verification request keeps, and every protocol verification addition MUST declare the same for the launches it claims. A component the applying declaration does not keep MUST be absent from the verification request. A kept tools component MUST meet its bound in the table below. A kind declares at most one of the two tools bounds.
- **Locality.** The component set lives in the registry. The replacement prompt, the refusal message, and the full-context retry live in the shared agent core. The protocol kind's per-runtime switches live in the per-runtime seam. An adapter contributes only its declaration, its runtime's spelling of each switch, and its refusal shape.

| Component | Covers | Dropped | Kept |
|---|---|---|---|
| Tools | Tool definitions of every origin: built-in, skill tool, and tool-server tools, Sortie's and the runtime's own | No tool definition | Some tools: strictly fewer than a working request. All tools: no more than a working request |
| Project instructions | Instruction files read from the workspace (`AGENTS.md`, `CLAUDE.md`, the runtime's equivalents) | Absent | Present |
| User instructions | Instruction files and skill catalogues read from the operator's home | Absent | Present |
| System prompt | The runtime's built-in system prompt | Replaced by the shared replacement prompt | Present |

What each kind's request still carries, because its runtime offers no qualifying switch that drops it:

| Kind | Request still carries |
|---|---|
| `claude-code` | Nothing |
| `copilot-cli` | One read-only tool and the built-in system prompt |
| `codex` | The tools no switch removes and the user instructions |
| `opencode` | Some tools, project instructions, user instructions, and the built-in system prompt, with every tool denied |
| `agent-client-protocol` | Every component for a remote launch and for a command line no addition claims; otherwise what the claiming addition declares |
| `mock` | Nothing, because it launches no runtime |

#### 10.9.2 Protocol verification additions

Session creation in the protocol carries no context member, so every lever is specific to a runtime. Each enters through the per-runtime seam that also carries measurement sources, and the transport asks every registered addition one question that names no runtime.

- At each session start of a reduced verification session on a local launch, the transport offers the launch to every registered addition. An addition claims a launch whose command line names its runtime. When exactly one claims, its switches apply: environment entries added to the launch environment and arguments appended after the operator's command line. Every other claimant is released.
- No addition claims a remote launch, because the command line is not visible there and a file Sortie writes exists only on the launching host.
- A launch that already sets one of the claimant's switches, as an argument in the command line or an environment variable, is left unclaimed, because replacing an operator's own setting could change the model, provider, or credential the runtime resolves.
- An addition's declaration states what the request keeps when its switches apply. An addition whose runtime cannot be driven against a scripted model declares every component kept, so it claims no saving, and its switches still apply.
- A file an addition creates exists only for the session and is removed at teardown, on every path that ends the session.

| Runtime | Switches | Request keeps |
|---|---|---|
| Gemini CLI | A session-private file on the launching host holding the shared replacement prompt, named by the runtime's system-prompt override variable | All tools, project instructions, user instructions |
| Copilot CLI | The read-only tool as the only available tool, and no custom instructions | Some tools, system prompt, project instructions, user instructions |
| OpenCode | An inline configuration document that denies every tool | Some tools, project instructions, user instructions, system prompt |
| Kiro CLI | A selection by name of a session-private agent definition with no tools, held in a directory on the launching host that the runtime's agent-directory variable names | Project instructions, user instructions, system prompt |

The Copilot CLI row passes the instruction switch and still keeps both instruction components, because the runtime loads them over the protocol whatever the switch says.

The Kiro CLI addition claims only a command line that also sets a model. Selecting an agent replaces the operator's default agent, which could carry the model the launch would otherwise use, and the agent definition sets none.

#### 10.9.3 Full-context retry and refusal on any turn

A runtime build can reject, or never honor, a context-dropping switch. When the first verification session never reached the model, its `StartSession` or turn returns the early-exit report (§10.7), or its `StartSession` returns kind `port_exit` or `response_error`, the shared step MUST log a warning carrying that error and start exactly one more session with `VerificationFullContext` set, under a fresh turn bound of the same length. That session's outcome is the step's outcome. The step starts no further session when the run's context is done, when the error wraps an SSH connection failure, or for any other failure, `credential_unverified`, `usage_limit`, and a timeout included. Under `VerificationFullContext` an adapter MUST apply none of its context-dropping additions or omissions. The retry costs one more request, and only on a build that rejects a switch: a runtime that predates a switch saves less and fails nothing.

A runtime's credential refusal in a structured shape its adapter recognizes MUST end the turn with kind `credential_unverified` and the shared refusal message built from the runtime's own text, on a working turn as at session start. Recognized shapes are an authentication-failure marker on the output stream, an unauthorized error class on a turn failure, and the protocol's authentication-required error code. A runtime that states a refusal only on standard error, or only by exiting, keeps the kind it reports; the cache's failure arms (§10.9.4) cover it.

A verification turn that the provider refuses for an exhausted usage allowance returns its `usage_limit` error unchanged. The step neither reports it as `credential_unverified` nor retries it with the full context, so the run takes the usage-limit retry path of the scheduler. Without this, the retry that fires at the reset, which verifies again once the proof has lapsed, would escape the wait when the allowance is still exhausted.

#### 10.9.4 Verification cache

The worker keeps process-lifetime memory of recent proofs, so a run whose agent setup was proven recently starts its working session without the step.

- **Key.** A digest over the agent kind, the form and elements of the resolved command, the SSH host, the sorted names of the environment variables carried to the host, and the encoded settings block. The key leaves out the workspace path, the resume fields, the MCP configuration path, the process environment, and credential files. It MUST NOT appear in a log, an event, an error, or persisted state, because the settings it digests can hold resolved secrets. A settings block that cannot be encoded yields no key: that run verifies and records nothing.
- **Window.** A proof is fresh for 15 minutes after the last verification or successful working turn of its key. A run that finds a fresh proof logs that it skipped the step with the time of the proof, relays no verification event, adds nothing to its request count, and applies a zero usage offset. Because no verification request marks it measured, a skipped run whose resolved usage arrival is `none` is reported unmeasured.
- **Refresh.** A working turn that returns no error and ends as completed records its key. A turn that returns no error with another ending does not.
- **Eviction.** A key is dropped when its window elapses; when a verification of it fails, unless the run was cancelled; when a run ends with an error exit and either none of its working turns succeeded or its error carries kind `credential_unverified`; and when any input of the key changes, which yields a different key. A cancelled run records and evicts nothing. A failure is never cached.
- **Scope.** Concurrent runs that miss the same key each verify. The cache is not persisted, so a restart starts empty. The window has no configuration key.

The workspace is not part of the key. A workspace whose project configuration selects another provider or model therefore skips the step while a proof from another workspace is fresh. Its first turn fails, the failure evicts the key, and the retry verifies there. A credential revoked inside the window behaves the same way: the first working turn fails with `credential_unverified` where the adapter maps the refusal, and otherwise with the kind the runtime reports; either way the retry verifies and names the credential.
