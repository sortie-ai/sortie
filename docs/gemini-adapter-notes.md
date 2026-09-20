# Gemini CLI adapter notes

Working notes for anyone dealing with Gemini CLI through Sortie's generic Agent Client Protocol adapter in `internal/agent/clientprotocol`: the one fact that follows from Gemini having no adapter code of its own, what the protocol surface does and does not carry for token accounting, what stands between a resumed session and a successful load, and what a normal-looking stop reason does not tell you.

Eligibility: not_qualified

Product conformance: unmeasured

The two answers are computed separately and diverge here. The protocol surface is below the richest measured native reference on one load-bearing row, token accounting, so the protocol route would cost an operator something the native route gave them. Product conformance has no answer at all: the human-input case of retry classification was never induced on the protocol surface, and no run crossed a finite token ceiling, so two load-bearing obligations stand unmeasured and whether the effective adapter meets them is unknown rather than settled either way.

## Where to get the volatile facts

Gemini's own version is never pinned in prose here; the adapter records it per session from that session's own `initialize` handshake, and the account's actual model set comes from Google's model-listing endpoint, which is the only authoritative answer because it depends on the key's subscription. Read the flag surface off `gemini --help` on the binary you are targeting. Upstream source lives in `google-gemini/gemini-cli`; when a symptom looks like a protocol bug rather than a Sortie bug, treat the bundled JavaScript that the installed version actually ships as the source of record, because the TypeScript source tree and the shipped bundle can drift apart between releases.

## Entry points

Gemini has no adapter package, no registered kind, no adapter metadata, and no identity branch anywhere in Sortie. The operator reaches it by pointing the generic `agent-client-protocol` kind's `agent.command` at the `gemini` binary with the `--acp` flag; an older `--experimental-acp` spelling still works but is deprecated. Every behavior described below is therefore a property of this one vendor's protocol implementation meeting the generic adapter, not a Gemini-specific code path in Sortie, and there is nowhere in the codebase to special-case a Gemini quirk short of teaching the generic adapter about it.

## Load-bearing capability observations

One row carries the eligibility verdict, token accounting. Product conformance has no verdict because of a different row, retry classification, whose human-input case is unmeasured on the protocol surface. Session continuation carries neither verdict: it works on every surface measured.

Session continuation works on every surface, and the protocol route is no weaker than either native one. The recall turn runs in the seed's own session, the session id comes back unchanged, and the model returns what the seed asked it to remember. What is expensive on this runtime is getting the load to succeed at all, which the protocol-specific section below covers; once it does, the session's content is there.

Retry classification is not measured on the protocol surface rather than failing on it. The asking posture induces a continuable permission request, which is not a request addressed to a person, so no human-input case is induced and the row stands at not observed. That posture is what Sortie's own contract asks a runtime for, so the unmeasured row is not a shortfall in itself. Both native surfaces have no terminal vocabulary for the outcome at all.

Token accounting is a real shortfall on the protocol route. The protocol surface resolves no token-bearing path. The prompt result does carry an extension block, and it carries input and output counts, but it omits cache-read, reasoning, and tool counters and is not admitted to a budget, so it is a presence signal and a lower bound rather than the accounting figure. Sortie's own adapter reaches past the wire for this runtime and does come back with a figure for the turn, which is why the turn reports as measured even though the wire block alone could not raise that flag. Both native surfaces resolve their token paths instead: the JSON surface reports prompt, cached, candidate, thought, and tool counts per model, and the streaming JSON surface reports its own flatter cached, input, output, and total counts. No run on any surface crossed a finite ceiling, so ceiling enforcement itself stays unverified: what is established is that the counts are there to read, not that a ceiling built on them would stop a turn and hold it stopped.

- protocol turn_disposition: Observed: usable
- protocol retry_classification: Not observed: not_observed
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

A stop reason of end_turn does not mean the turn ended cleanly. Of the five stop reasons the protocol defines, this runtime's own code produces four: `end_turn`, `max_turn_requests`, `max_tokens`, and `cancelled`. Loop detection reports as `max_turn_requests`. `max_tokens` is reachable only through the runtime's own pre-emptive context-overflow predictor, which fires before the model's stream is actually exhausted; the stream's own genuine token-limit signal never reaches the protocol layer as `max_tokens`, because the handler that catches an invalid stream folds that signal into `end_turn` alongside the model's safety and recitation blocks. `refusal` is never assigned: no code path in this runtime produces it. So a model declining to answer on safety grounds, and a turn that genuinely ran out of context, both surface as an ordinary, successful-looking `end_turn`, and nothing in the response distinguishes either one from a turn that actually completed as asked.

A passing limit_reached row does not mean the provider's real ceiling got tested. The turn used to induce it carries an oversize prompt, and this runtime's own pre-flight guard rejects that prompt locally before any request reaches the provider. So this row measures the client's own size guard, not where Gemini's context window actually ends.

Session continuation resumes the session with its content. `session/load` succeeds, the recall turn runs under the seed's own session id read back from the runtime, and the model returns the nonce the seed turn asked it to remember, with the process that created the session already torn down. The expense sits in the load, not in the recall.

Three traps sit in front of that, and a load failing is worth attributing to one of them before concluding anything about the session itself.

The first is an upstream ordering defect. The `session/load` response can reach the wire before its replay notifications finish sending, because the runtime does not wait for the replay to complete before responding, even though the protocol expects a response only after the full replay has gone out. Sortie's own continuation logic already tolerates this: it starts watching for replayed chunks before issuing the load call, and after the response arrives it waits a bounded interval for any chunk still in flight before deciding there was no replay to see. That wait is load-bearing, not redundant, and must not be trimmed as dead time.

The second is more expensive and unrelated to the first. A `session/load` issued in the same UTC minute as the `session/new` that created the session fails and permanently destroys that session's resumability, including every later attempt to load it in a following minute. This is an open upstream defect, confirmed live: moving the load into the following UTC minute, from a separate process, produced a successful load with a full history replay in two independent runs, while a same-minute load reliably failed both times it was tried. A `session/load` issued through the adapter by the process that created the session is now spaced out of that minute; the exposure remains for a load issued by any other process or by hand.

The third is the credential path. `session/load` requires an authentication type recorded in the runtime's own settings; supplying the API key only through the environment, with no such setting present, makes the load fail outright. A load failing against a session this same process just created and drove correctly is worth checking here first, before either trap above: the cause is the credential path, not the session or the clock.

This runtime advertises no `sessionCapabilities` object at all in its handshake. Sortie's adapter decides whether to call `session/close` based on that capability being present, so against this runtime there is never a capability to select, and a session here is never closed through the protocol.

## Native headless observations

Session continuation is equivalent between the two native surfaces. Each resumes the seed's own session and returns what the seed asked it to remember, and so does the protocol route, so continuation is not a reason to route around any of the three on this runtime.

Both native surfaces carry the token accounting the protocol surface does not; see the token accounting paragraph above for what each one reports.

Turn disposition and retry classification both read as gaps on both native surfaces. The cases behind that were induced and the surface then reported no outcome for them rather than the condition never arising: cancellation cannot be distinguished at all, because both surfaces are one-shot launches that write their terminal output only at exit, and the human-input outcome has no place in either surface's closed terminal vocabulary. A case the surface stays silent about keeps its obligation and counts against the surface, which is why these rows sit below the protocol surface on turn disposition.

## Workspace trust and process boundary

Tool-server delivery needs a trusted workspace and an authorized call. Tool servers declared in `session/new` are honored: the runtime merges them over whatever servers its own settings file already configures, matching by name with the request winning on a collision, and stdio, sse, and http transports are all supported. Delivery is gated on whether the runtime considers the workspace trusted. `--skip-trust` grants trust for the session rather than only suppressing a prompt: it sets the runtime's own workspace-trust environment variable, which the trust check reads before the folder-trust setting, the editor state, and the trusted-folder list. An untrusted workspace fails closed with no signal: the create call returns success, the declared servers are dropped without a trace, and nothing in the response or in a later notification marks that anything went wrong. The runtime's trust guard raises an error only in its own headless mode and treats protocol mode as interactive, so the guard that would reject an untrusted workspace on the command line never fires here.

A declared server being reachable is not the same as its tools being callable. Protocol mode counting as interactive also decides what happens to a tool call matching no policy rule: the runtime raises a permission request rather than denying or allowing the call outright, and Sortie refuses every such request, so the call never reaches the server while the turn still ends normally. A tool reaches its server when the workspace is trusted and the call is authorized before it is made: a policy rule naming the tool by the qualified name the runtime builds for it (`mcp_` plus the server name plus `_` plus the tool name), or an approval mode that allows a call matching no rule.

The containment boundary itself holds under measurement. Every launch ran in a directory inside the run-scoped root, no project settings applied to any of those directories, and every process-group member observed was the launched command or a descendant of it. Note what that last clause does and does not cover: it is a statement about the group the launch was started in. This runtime's own shell tool starts each command it runs in a fresh process group of its own, outside that one, so a shell-tool child is not a member of the group under observation and a signal aimed at that group does not reach it. That does not always put it out of reach. A reading taken while its parent is still running proves the child belongs to this run by tracing it through that parent, carries that ownership into the readings that follow, records it as a survivor if it outlives the shutdown path, and kills it by its own process id. A child that both starts and loses that parent between two readings is never traced, so a clean cleanup row here states that nothing this run could attribute to itself was left running, not that this runtime left nothing behind.

Our own teardown outruns a graceful exit. For a local launch, Sortie's own teardown sends the process group a catchable termination signal, closes the runtime's standard input so it also sees end-of-input immediately behind that signal, waits a bounded grace period for the process to exit and be reaped on its own, and force-kills the process group only once that wait elapses. Gemini's own history flush completes inside that window: a `session/load` against a session that had ended through this path replayed it, and the recall turn answered from what the seed turn established.

## Excluded capability cases

- retry_classification human_input: induced, and the surface reported no outcome (terminal_vocabulary_closed), so the case keeps its obligation
- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer, so neither the condition nor the surface's account of it was established
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_failure: induced, and the surface reported no outcome (output_channel_silent_on_failure), so the case keeps its obligation
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

- protocol retry_classification human_input: fixture_induction_failed

Windows live qualification is unobserved.
