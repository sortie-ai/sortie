# Gemini CLI adapter notes

Working notes for anyone running Gemini CLI through Sortie's generic Agent Client Protocol adapter in `internal/agent/clientprotocol`.

Eligibility: qualified

Product conformance: qualified

The two verdicts are computed separately, and both turn on token accounting. The protocol wire itself carries no usable token figure, but a measurement source outside the protocol supplies one, which puts the protocol route level with the richest native reference. Product conformance holds too: a live run through Sortie's orchestrator with a one-token `agent.max_tokens` ceiling stopped at the ceiling and dispatched nothing after it.

## Where to get the volatile facts

The adapter records Gemini's version per session from the `initialize` handshake. The account's model set comes from Google's model-listing endpoint, which is the only real answer because it depends on the key's subscription. Read the flags off `gemini --help`.

Upstream source lives in `google-gemini/gemini-cli`. When a symptom looks like a protocol bug, treat the bundled JavaScript that the installed version ships as the source of record; the TypeScript tree and the shipped bundle can drift apart.

## Entry points

Gemini has no adapter package, no registered kind, and no adapter metadata. The operator points the generic `agent-client-protocol` kind's `agent.command` at `gemini --acp`; the older `--experimental-acp` spelling is deprecated. Everything below is how Gemini's protocol implementation meets the generic adapter. There is no place in the generic transport to special-case a Gemini quirk.

The one exception is token accounting, which lives in Gemini's own measurement source. The source recognizes a command line that names the `gemini` executable, a path to it, or the `@google/gemini-cli` package, directly or as a wrapper's argument. A command line that hides its runtime is started a second time once the handshake names it, so a wrapped Gemini CLI still reports token usage.

## Load-bearing capability observations

Token accounting is the one row that carries both verdicts. Session continuation works on every surface. Retry classification blocks neither verdict: its `unknown_outcome` case has no deterministic inducer on any surface, so it carries no obligation.

On the protocol surface, retry classification excludes the human-input case as not applicable. The consent request the asking-posture launch raised offered a refusing option, the client refused it inside the protocol, and the turn went on. That exclusion does not cover a question addressed to a person. The native surfaces have no terminal vocabulary for the human-input outcome at all.

The protocol wire has no token figure Sortie can use. The prompt result's extension block has input and output counts but no cache-read, reasoning, or tool counters, so it is only a presence signal and a lower bound. The figure that reaches the budget comes from the runtime's own telemetry and session journal. Both native surfaces report token counts directly: the JSON surface per model, the streaming JSON surface in a flatter form.

- protocol turn_disposition: Observed: usable
- protocol retry_classification: Observed: usable
- protocol token_ceiling: Observed: gap
- protocol tool_server_delivery: Observed: usable
- protocol session_continuation: Observed: usable
- protocol permission_handling: Observed: usable
- native_json turn_disposition: Observed: gap
- native_json retry_classification: Observed: gap
- native_json token_ceiling: Observed: usable
- native_json session_continuation: Observed: usable
- native_stream_json turn_disposition: Observed: gap
- native_stream_json retry_classification: Observed: gap
- native_stream_json token_ceiling: Observed: usable
- native_stream_json session_continuation: Observed: usable

## Protocol-specific observations

A stop reason of `end_turn` does not mean the turn ended cleanly. This runtime produces `end_turn`, `max_turn_requests`, `max_tokens`, and `cancelled`, and never `refusal`. Loop detection reports as `max_turn_requests`. `max_tokens` comes only from the runtime's own context-overflow predictor, before the stream runs out.

The stream's real token-limit signal, and the model's safety and recitation blocks, are all folded into `end_turn` by the handler for an invalid stream. So a safety refusal and a turn that ran out of context both look like an ordinary successful `end_turn`.

A passing `limit_reached` row does not test the provider's real ceiling. The runtime's own pre-flight guard rejects the oversize prompt locally, so the row measures the client's size guard, not Gemini's context window.

`session/load` works once it succeeds, but three traps sit in front of it. Check a failing load against them before blaming the session.

The first is an upstream ordering defect. The `session/load` response can reach the wire before the replay notifications finish, although the protocol expects it after the full replay. Sortie starts watching for replayed chunks before it sends the load, and after the response it waits a bounded time for chunks still in flight. That wait is load-bearing; do not trim it.

The second is more expensive. A `session/load` sent in the same UTC minute as the `session/new` that created the session fails and permanently destroys the session's resumability, including every later load. This is an open upstream defect, confirmed live: a load in the following minute from a separate process replayed the full history, and a same-minute load failed every time. The adapter spaces a load out of that minute when the same process created the session. A load from any other process, or by hand, is still exposed.

The third is the credential path. `session/load` needs an authentication type recorded in the runtime's own settings. With the API key only in the environment and no such setting, the load fails. If a load fails on a session this same process just created and drove correctly, check this first.

The handshake advertises no `sessionCapabilities` object, so Sortie never closes a session here through the protocol.

## Native headless observations

Turn disposition and retry classification are gaps on both native surfaces. The cases were induced, and the surfaces reported no outcome. Cancellation cannot be told apart, because both surfaces are one-shot launches that write their terminal output only at exit. The human-input outcome has no place in either surface's terminal vocabulary. A case a surface stays silent about keeps its obligation and counts against the surface.

## Workspace trust and process boundary

Tool servers declared in `session/new` are merged over the servers in the runtime's own settings, matched by name, with the request winning. Stdio, SSE, and HTTP transports all work.

Delivery needs a trusted workspace. `--skip-trust` grants trust for the session, not just a quieter prompt: it sets the runtime's workspace-trust environment variable, which the trust check reads before any other trust source. An untrusted workspace fails closed with no signal: `session/new` succeeds, the declared servers are dropped, and nothing reports it. The runtime treats protocol mode as interactive, so the guard that rejects an untrusted workspace on the command line never fires here.

A reachable server does not mean callable tools. Because protocol mode counts as interactive, a tool call that matches no policy rule raises a permission request, and Sortie refuses every such request. The call never reaches the server, while the turn still ends normally. A tool reaches its server only when the workspace is trusted and the call is pre-authorized: by a policy rule naming the tool as `mcp_` plus the server name plus `_` plus the tool name, or by an approval mode that allows unmatched calls.

The containment boundary held under measurement: every launch ran inside the run-scoped root, no project settings applied, and every process-group member observed was the launched command or its descendant.

That last clause covers only the launch's own process group. The runtime's shell tool starts each command in a new process group of its own, so a signal to the launch group does not reach it. Sortie still finds such a child while its parent is running, by tracing it through the parent, and kills it by its own process id if it survives shutdown. A child that starts and loses its parent between two readings is never traced. So a clean cleanup row means nothing this run could attribute to itself was left running, not that the runtime left nothing behind.

Sortie's teardown leaves room for a graceful exit. For a local launch it sends the process group a catchable termination signal, closes the runtime's standard input, waits a bounded grace period, and only then force-kills the group. Gemini's history flush finishes inside that window: a session ended this way loaded, replayed, and answered from its history.

## Excluded capability cases

- retry_classification human_input: induced, and the surface reported no outcome (terminal_vocabulary_closed), so the case keeps its obligation
- retry_classification human_input: not applicable on protocol: the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose
- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer on any runtime, so the case carries no obligation
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_failure: induced, and the surface reported no outcome (output_channel_silent_on_failure), so the case keeps its obligation
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

Windows live qualification is unobserved.
