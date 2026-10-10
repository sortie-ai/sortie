# Kiro adapter notes

Working notes for anyone running Kiro CLI through Sortie's generic Agent Client Protocol kind in `internal/agent/clientprotocol`, which drives `kiro-cli acp`.

Eligibility: qualified

Product conformance: not_qualified

The two verdicts diverge here. Every load-bearing row was measured, and none puts the protocol surface below the richest native reference, so the protocol route can stand in for the native one. Product conformance fails on token accounting: no route supplies a token figure, so that capability does not work whichever route the operator takes.

This file is checked against the tracked measurement under `tools/qualify/probe/testdata/kiro-cli/`. Changing a grade row below fails the staleness gate until a fresh run replaces that measurement.

## Where to get the volatile facts

Read the flags off `kiro-cli chat --help` and `kiro-cli acp --help`. Read the account's model set from the CLI's model-listing command in JSON format; the backend serves that list per subscription. The protocol adapter records the runtime's version per session from the `initialize` handshake. The docs at `kiro.dev` cover the headless path and exit codes, but they are the vendor's claim and the binary is the fact; the two disagree.

This CLI is a distribution of Amazon Q Developer CLI, so `aws/amazon-q-developer-cli` is the source of record for CLI behavior. The public Kiro tracker is for product feature requests. Search upstream for a CLI bug, not the IDE tracker.

The runtime keeps its own log at `kiro-log/kiro-chat.log` under its runtime directory. Some failures below are explained only there.

## Entry points

The generic `agent-client-protocol` kind drives `kiro-cli acp`. It delivers session continuation and, on a local launch and subject to the credential constraint below, Sortie's own tool servers.

The `acp` subcommand is missing from `kiro-cli --help` and listed only under `--help-all`.

One switch, `-a` (`--trust-all-tools`), sets both trust and the asking posture. It approves every tool permission request, so removing it brings back asking for every tool, Sortie's and the runtime's own. `--trust-tools=<names>` narrows it to a list; a declared server's tool is named there as `@<server>/<tool>`, the same form the runtime prints in a tool-call title.

`--model <id>` on `acp` takes effect: the session-creation response reports the requested model. The same flag on `chat` with JSON-Lines output can fail silently instead. It prints a prose warning on stderr, never in the JSON stream, and runs on the default model.

With no stored login and no API key, `acp` exits before the handshake completes and prints a message on stderr naming the login command. No nightly case covers this; a regression still ends at the adapter's bounded handshake wait.

Naming an agent definition, global or in the workspace, makes the session report that agent as its mode and use its model unless a model flag names another. With no agent flag, or an undefined agent, the session reports the default agent and model; an undefined name also gets a vendor notification naming the requested and fallback agents. Both flags apply to the one session a launch starts, which matches the adapter's one session per launch.

The verification session stays in the runtime's store outside the workspace, because the runtime advertises no delete capability. A load shortly after a session's creation waits briefly for the runtime's store to catch up.

## Load-bearing capability observations

No row blocks transport parity. Token accounting blocks product conformance but not parity, because both routes miss it equally.

The credential decides whether Sortie's tools arrive, and this is the most important fact on this page. With `KIRO_API_KEY` the runtime starts sessions, runs turns, and continues sessions correctly, but silently carries none of Sortie's tools. The backend refuses to serve a governance profile for the key, and the runtime disables MCP for the session. Its log says `Failed to get governance config from API - MCP disabled, web tools disabled`, and the wire carries only a vendor-namespaced `governance_disabled` notification. The declared server never starts, the model is never offered the tool, and the turn completes normally while saying it has no such tool. Nothing in Sortie's output marks this as a failure.

No flag turns the profile check off. Whether it fails for every API key or only on some plans is unknown, because only one key was tried. When tools go missing, read the runtime log first.

Every grade on this page was measured under a stored device login, the credential the measurement assumes. Nothing was measured under an API key.

Token accounting has no source on either surface. The runtime reports spend as an abstract credits figure and context use as a percentage, never as tokens. So token-based budgets are inert, and only the turn timeout and cancellation bound a turn. The protocol route reports credits per turn in a vendor extension; the native stream has no token field either. Parity holds on this row because the shortfall is shared; conformance fails because the capability works on neither route. A live run with a one-token `agent.max_tokens` ceiling spent tokens, recorded unaccounted spend, and finished without stopping.

Two cases are declared, not measured, on both surfaces: a turn ending in `runtime_refusal` and a retry classified as `non_retryable_refusal`. This runtime never produces either. A request built to trigger one completes normally, so the case is recorded as `outcome_never_produced`.

On the protocol surface, retry classification excludes the human-input case as not applicable. The consent request the asking-posture launch raised offered a refusing option, the client refused it inside the protocol, and the turn went on. That exclusion does not cover a question addressed to a person. On the native stream the case was induced and the row is a gap: a recognized terminal ended the turn even though the probe's marker file shows the block was reached.

The `unknown_outcome` case has no deterministic inducer on either surface, so it carries no obligation. `limit_reached` stays unmeasured for turn disposition because it was not induced on either surface.

- protocol turn_disposition: Observed: usable
- protocol retry_classification: Observed: usable
- protocol token_ceiling: Observed: gap
- protocol tool_server_delivery: Observed: usable
- protocol session_continuation: Observed: usable
- protocol permission_handling: Observed: usable
- native_stream_json turn_disposition: Observed: gap
- native_stream_json retry_classification: Observed: gap
- native_stream_json token_ceiling: Observed: gap
- native_stream_json session_continuation: Observed: usable

A case that was induced and that the surface reported no outcome for keeps its obligation. It counts against that surface; it is not forgiven.

Permission handling was measured with the `-a` switch removed. The runtime raises the request, Sortie refuses it, the refusal is accepted, and nothing is left pending. So under a posture that asks, a declared tool is delivered and still never called.

## Protocol-specific observations

The handshake advertises `loadSession` true, `mcpCapabilities.http` true with `sse` false, and an empty `sessionCapabilities` object, so Sortie never closes a session here through the protocol. `authMethods` is empty, which proves nothing either way. `agentInfo` carries a name and a version, which the gated suite's identity rule reads.

Session continuation restores the session and its memory: the recall turn runs under the seed's own session identifier and returns what the seed asked it to remember. A successful load alone does not prove that; only the recall answer does.

The runtime sends many notifications under its own vendor method prefix. Sortie records them as unrecognized and moves on. Do not add handlers for them to quiet a log; the standard surface already carries everything the adapter reads.

`limit_reached` was not induced here: the prompt channel cannot carry a request big enough to reach the runtime's limit.

## Native headless observations

`kiro-cli chat --output-format stream-json` emits JSON Lines on stdout, one event per line. It opens with a run-started event, closes with a run-finished event carrying a status, a stop reason, and the final text, and reports a failure as a run-error event. The mode needs the second-generation agent engine and refuses to start on the first.

Session continuation is the richest reference on this surface. The seed launch's run-started event reports the session identifier, the only place this runtime exposes it, and a later launch names it to resume.

Turn disposition is a gap here. The stream stays silent on a failed launch. And because the launch writes its terminal only at exit, a turn cancelled mid-way leaves no terminal, so it looks the same as a turn that has not finished.

The profile treats the plain-JSON surface as absent. The output-format flag accepts only text and JSON Lines and rejects anything else with a non-zero exit. A declared absence is checked by running the ordinary headless invocation and finding no structured terminal; a launch that fails to start would prove nothing.

## Workspace trust and process boundary

`kiro-cli acp` is a launcher, not the worker. It forks a second process that does the work and outlives a single-pid kill until its inherited stdin closes. Sortie puts the launched process at the head of its own group, signals the whole group, closes stdin, waits a bounded grace, and then kills the group. The qualification run asserts the group is gone after teardown, so a survivor is reported as a leak.

The containment boundary held under measurement: every launch ran inside the run-scoped root, no project settings applied, and every process-group member observed was the launched command or its descendant.

Tool servers declared in `session/new` are merged over the runtime's own configuration. A workspace MCP configuration lives at `.kiro/settings/mcp.json`, and agent definitions with their own tool lists live under `.kiro/agents`. A declared server whose entry does not match the shape the runtime expects is dropped silently, with the reason only in the runtime log.

## Excluded capability cases

- retry_classification human_input: not applicable on protocol: the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose
- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer on any runtime, so the case carries no obligation
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_failure: induced, and the surface reported no outcome (output_channel_silent_on_failure), so the case keeps its obligation
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

Windows live qualification is unobserved.

## Verifying a change

The generic adapter's gated suite covers this route when its command coordinate points at `kiro-cli acp`. The live qualification profile is gated separately and spends real credits.
