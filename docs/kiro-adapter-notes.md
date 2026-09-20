# Kiro adapter notes

Working notes for anyone dealing with Kiro CLI through Sortie, on either of the two routes that reach it: the native `kiro` kind in `internal/agent/kiro`, and the generic Agent Client Protocol kind in `internal/agent/clientprotocol`. The one thing that decides whether Sortie's own tools reach the agent at all, the two shapes a credential problem takes, and what an exit code does not tell you here.

Eligibility: unmeasured

Product conformance: not_qualified

The two answers are computed separately and diverge here. No load-bearing row puts the protocol surface below the richest measured native reference, and retry classification is unmeasured on that surface, so whether the protocol route can stand in for the native one has no answer yet. Product conformance does have one: the effective adapter does not meet the token-accounting obligation, and nothing outside the protocol supplies it, so that capability does not work for the operator whichever route they take.

This file is validated against the tracked measurement under `internal/qualification/probe/testdata/kiro-cli/`, so an edit to a grade row below reddens the staleness gate until a fresh run replaces that artifact.

## Where to get the volatile facts

Read the flag surface off `kiro-cli chat --help` and `kiro-cli acp --help` on the version you are targeting, and the account's own model set off the CLI's model-listing command in its JSON format, which is the only authoritative answer because the list is served by the backend and depends on the subscription. The runtime's own version is never pinned in prose here; the protocol adapter records it per session from that session's own `initialize` handshake. The rendered docs at `kiro.dev` cover the headless path and the exit codes; treat them as the vendor's claim and the binary as the fact, because the two are known to disagree.

Lineage is worth knowing and does not expire: this CLI is a distribution of Amazon Q Developer CLI, so the upstream `aws/amazon-q-developer-cli` repository is the source of record for CLI behavior, while the public Kiro tracker is where product-level feature requests live. When a symptom looks like a CLI bug, search upstream, not the IDE tracker.

The runtime keeps its own log, and it is the only place some failures are explained at all. Find it under the runtime directory's `kiro-log/kiro-chat.log`. Two of the behaviors below are invisible on the wire and visible only there.

## Entry points

Two routes reach this runtime, and they are not equivalent.

The native `kiro` kind drives `kiro-cli chat` and parses its output. The generic `agent-client-protocol` kind drives `kiro-cli acp` and speaks the protocol. The protocol route is the one that delivers session continuation and, subject to the credential constraint below, Sortie's own tool servers; the native route delivers neither. Neither kind is retired by the other: pick per deployment.

The `acp` subcommand does not appear in `kiro-cli --help`. It is listed under `--help-all` only, which is worth knowing before concluding a build does not have it.

Where other runtimes on this transport spell trust and asking posture as two separate switches, this one spells both with `-a` (`--trust-all-tools`): it auto-approves every tool permission request, so dropping it restores asking for every tool, Sortie's and the runtime's own. `--trust-tools=<names>` narrows that to an explicit set, and a tool offered by a declared server is named there as `@<server>/<tool>`. That qualified form is the runtime's own, and it is also what the runtime prints in a tool-call title.

`--model <id>` on the `acp` entry point does take effect: the session-creation response reports the requested identifier as the session's current model. The same flag on the native `chat` path with the JSON-Lines output format can fail silently instead, warning that the model could not be set and running the turn on the default; the warning goes to standard error as prose and never into the JSON stream.

## Load-bearing capability observations

No row blocks transport parity: session continuation grades usable on both routes, so an operator moving from native to protocol loses nothing that was measured. Token accounting blocks product conformance without blocking parity, because both routes miss it equally, and a shortfall both routes share costs nothing in moving between them while still meaning the capability does not work. Retry classification is unmeasured on the protocol surface, and that is what leaves transport parity without an answer.

The credential decides whether Sortie's tools arrive, and this is the single most important thing on this page. Authenticating with `KIRO_API_KEY` starts sessions, runs turns and continues sessions correctly, and silently carries none of Sortie's tools. The runtime's backend refuses to serve a governance profile for the key, and the runtime responds by disabling MCP entirely for the session: `Failed to get governance config from API - MCP disabled, web tools disabled` in its own log, plus a vendor-namespaced `governance_disabled` notification on the wire. The declared server is never started, the model is never offered the tool, and the turn completes normally while answering that it has no such tool. Nothing on the wire and nothing in Sortie's own output marks this as a failure, which makes it the most expensive way to get this integration wrong: the route is chosen for the tools, and it silently delivers everything except them.

This is the same server-side profile check that already blocks MCP on the native route. It governs both, and no flag turns it off.

Whether the check fails for every API key or only for keys on some plans is unestablished; one key was available to try. An operator seeing tools go missing should read the runtime log first, because that line is the only place the cause is stated.

The rows below were measured under a stored device login, which is the credential the measurement procedure assumes and the one under which the runtime's real capability shows. Nothing below was measured under an API key, so read every grade on this page as the device-login answer.

Token accounting has no source here, on either surface. The runtime reports spend as an abstract credits figure and context use as a percentage, never as a token count, so token-based budget enforcement is inert and only the turn timeout and cancellation bound a turn. The protocol route reports credits per turn as a vendor extension on its metadata notifications; the raw native stream carries no token-bearing field either. Both inventories complete with no token-bearing path resolved, and both grade a gap: there is no extension source present for the protocol route to admit, and nothing outside the protocol supplies one. That is the row where the two answers separate, and it is worth reading carefully. Parity is satisfied on it, because the shortfall is shared and an operator changing routes loses nothing they had. Conformance is not, because the capability works on neither route. The registered `kiro` kind declares `none` arrival and `none` attribution, matching this absence directly.

Two cases are declared rather than measured, on both surfaces: a turn ending in `runtime_refusal` and a retry classified as a `non_retryable_refusal`. This runtime never produces either outcome; a request built to trigger one instead completes the turn normally, so the case is recorded as `outcome_never_produced` rather than graded from an attempt that failed to reach it.

Retry classification does not read the same way on the two surfaces. On the protocol surface it is not observed rather than failing: the posture that asks induces a continuable permission request, which is not a request addressed to a person, so no human-input case was induced at all. Permission handling graded usable on the same run, which is what that request actually is. On the raw native stream the case was induced and the row grades a gap, because a recognized terminal ended the turn even though the probe's own marker file, which shows the block was reached, is present.

- protocol turn_disposition: Observed: usable
- protocol retry_classification: Not observed: not_observed
- protocol token_ceiling: Observed: gap
- protocol tool_server_delivery: Observed: usable
- protocol session_continuation: Observed: usable
- protocol permission_handling: Observed: usable
- native_stream_json turn_disposition: Observed: gap
- native_stream_json retry_classification: Observed: gap
- native_stream_json token_ceiling: Observed: gap
- native_stream_json session_continuation: Observed: usable

One row above is not graded from an observation: protocol retry classification stands at not observed, because the case that would decide it was never induced. Two of the six cases excluded below are excluded while keeping their obligation, which is a different thing from being forgiven: a case that was induced and that the surface then reported no outcome for counts against that surface rather than passing for free.

Permission handling was measured under the posture that asks, which for this runtime means the same launch with its single trust-and-posture switch taken back out. The runtime raises the request, Sortie's unattended posture refuses it, the refusal is accepted, and nothing is left pending when the turn ends. The consequence for a real run is the one the transport notes already state: under a posture that asks, a declared tool is delivered and still never called.

## Protocol-specific observations

The handshake advertises `loadSession` true, `mcpCapabilities.http` true with `sse` false, and an empty `sessionCapabilities` object. Sortie decides whether to send `session/close` from that capability being present, so a session here is never closed through the protocol. `authMethods` comes back empty, which is evidence in neither direction. `agentInfo` carries both a name and a version, which is what the gated suite's identity rule reads.

Session continuation restores the session and its memory. The load succeeds, the recall turn runs under the seed's own session identifier read back from the runtime rather than one Sortie minted, and the model returns what the seed turn asked it to remember. A successful load is still not on its own evidence that anything said in the session survived; only the recall turn's own answer settles that. The native route returns it too, so the two routes agree on this row.

The runtime carries a large vendor-namespaced surface alongside the standard one, announcing available commands, subagent lists, MCP server initialization and per-turn metadata under its own method prefix. Those arrive as notifications rather than requests, so nothing answers them and nothing depends on them; Sortie records them as unrecognized and moves on. Do not add handlers for them to make a log quieter, because the standard surface already carries everything the adapter reads.

A `limit_reached` turn was not induced on this surface. The prompt channel is too small to carry a request large enough to reach whatever ceiling would make the runtime report running out of room, so the case stays unmeasured here rather than graded, and turn disposition keeps its usable grade on the cases that did run.

## Native headless observations

The native route has a structured output mode, and older notes in this file claiming it has none were wrong. `kiro-cli chat --output-format stream-json` emits JSON Lines on standard output, one self-describing event per line, opening with a run-started event that names the protocol as its own payload schema and closing with a run-finished event carrying a status, a stop reason and the final text. A failure arrives in the same envelope under a run-error type. The mode requires the second-generation agent engine and refuses to start on the first.

Sortie's native `kiro` adapter does not use it. That adapter parses the human transcript, which is what the default text output still produces: an ANSI-styled stdout with the closing cost trailer on standard error. Everything awkward about the native adapter follows from that choice rather than from the runtime: it emits no tool-result events, leaves the model field empty on its events, and reports no usage.

This run measured the structured surface directly, independent of Sortie's adapter. Session continuation grades usable there and is the richest measured reference on that row: it resumes by naming a session identifier explicitly, a seed launch's own terminal reports the identifier in its run-started event, which is the only place this runtime exposes one at all, and a following launch names it to resume and gets back what the seed turn left. Turn disposition and retry classification both grade a gap there, and token accounting grades the same gap as on the protocol surface.

Two turn-disposition cases were induced here and the surface reported no outcome for either. The structured stream stays silent on a failed launch, and because this is a one-shot launch that writes its terminal only when the process exits, a cancellation signal sent mid-turn leaves no terminal to read: the process just stops, so a cancelled turn and one that has not finished yet look identical. Silence does not excuse either case, and the two of them are why turn disposition grades a gap on this surface. A third case, `limit_reached`, was not induced here either, for the same prompt-channel reason it was not induced on the protocol surface.

The profile treats the plain-JSON surface as absent, which the binary agrees with: the output-format flag accepts only the text and JSON-Lines values, and rejects anything else on a non-zero exit with a plain-text message. Its entry point in the profile is therefore the ordinary headless invocation rather than a flag value the runtime would reject, because a declared absence is corroborated by running the surface and finding no structured terminal in what comes out. A launch that fails to start demonstrates nothing, and the corroboration reads any non-zero exit as a terminal rather than as an absence.

Exit zero does not mean the turn succeeded on the text path by itself. The cost trailer on standard error and a non-blank line on standard output are the two positive success signals; either one reports the turn completed. A turn that never ran because the credential was rejected also exits zero, with a blank stdout and an authentication line on stderr and no trailer, and is reported failed. A turn whose standard error could not be collected has no trailer to read either, and is reported as a failure for the same reason when its stdout is also blank.

So the native adapter never maps a bare zero exit to success by itself. It reports success when the trailer is present or the stdout transcript carries a non-blank line, reports a specific authentication failure when a zero exit arrives with the auth marker and no non-blank stdout line, and otherwise lets the shared decision treat a zero exit carrying neither signal as a turn that produced nothing. Both markers are matched by substring containment and never by the numbers that follow them, which is what keeps the classification stable while the values vary. Non-zero exits carry no category worth reading: the vendor documents one meaning for a code that the binary also uses for something else, so the adapter reads nothing into the value.

The credential trap has two shapes, and they need different defenses. With no credential at all, a headless chat does not fail: it enters an interactive device-authorization flow, prints a code and a login URL, and blocks forever waiting for a person. The non-interactive flag does not suppress it. With an invalid credential it fails fast instead, exiting zero with empty stdout and an authentication line on stderr. The native adapter defends against both before a turn runs: it rejects an empty credential variable outright and then runs a bounded identity canary, rejecting anything that does not clearly report an authenticated key, and classifying a canary that times out or exits non-zero as a retryable credential problem rather than a missing agent. The preflight is local-mode only.

One more reason not to lean on the CLI for input validation: once a device registration is cached from an earlier login attempt, an unauthenticated invocation resumes that pending authorization and blocks on it rather than reporting a bad flag, so the same malformed invocation that fails cleanly on a pristine machine hangs on a used one. Kiro is no guard against a bad argument. The preflight and the external cancellation bound carry that weight.

Conversations are persisted per working directory on the native route, and the conversation identifier is not obtainable at run time: the CLI's session listing comes back empty for headless conversations, and the turn output prints no identifier. The native adapter therefore continues by position rather than by identity, arming the resume flag once a turn has actually succeeded and passing it on every later turn in the same workspace, which is safe only because Sortie runs one session per workspace per issue. The identity that session carries is Sortie's own resume value, reported back on the turn result and used for logging, never passed to the CLI.

## Workspace trust and process boundary

The protocol entry point is a launcher, not the worker. Launching it forks a second process that does the work and stays alive behind the parent, so a single-pid kill leaves that worker running until its inherited standard input closes. Sortie's teardown sends a catchable signal to the whole process group, closes standard input, waits a bounded grace, and kills the group only once that wait elapses, and the launched process is put at the head of its own group so an inheriting worker is reached. The qualification run asserts the group is gone after teardown, so a survivor is reported as a leak rather than tolerated as a slow exit.

The containment boundary held under measurement: every launch ran in a directory inside the run-scoped root, no project settings applied to any of them, and every process-group member observed was the launched command or a descendant of it.

Tool servers declared in the session-creation request are merged over whatever the runtime's own configuration already holds. A workspace-scoped MCP configuration lives at `.kiro/settings/mcp.json`, and agent definitions with their own tool lists live under `.kiro/agents`; the tracked profile records both. A declared server is dropped silently when its entry does not match the wire shape the runtime expects, with the reason recorded only in the runtime log, so an unexplained absence of tools is worth checking there before anywhere else.

## Excluded capability cases

- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer, so neither the condition nor the surface's account of it was established
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_failure: induced, and the surface reported no outcome (output_channel_silent_on_failure), so the case keeps its obligation
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

- protocol retry_classification human_input: fixture_induction_failed

Windows live qualification is unobserved.

## Verifying a change

Unit tests cover argument construction, the trust posture, stderr classification, and disposition for the native adapter. The tests that drive the real binary are gated twice: on the native adapter's own test variable and again on the credential variable being set. The second guard is not redundant, it is the defense against the device-login hang, so a credential-less machine skips rather than blocking a test run forever. Keep both guards, and keep them skipping cleanly rather than failing.

The protocol route is covered by the generic adapter's own gated suite, pointed at this runtime through the client-protocol command coordinate, and by the live qualification profile, which is separately gated and spends real credits.
