# Kiro adapter notes

Working notes for anyone dealing with Kiro CLI through Sortie. The one route that reaches it is the generic Agent Client Protocol kind in `internal/agent/clientprotocol`, which drives `kiro-cli acp`; a workflow that still names the removed `kiro` kind converts onto it at load. The one thing that decides whether Sortie's own tools reach the agent at all is the credential, which the load-bearing observations below cover.

Eligibility: qualified

Product conformance: not_qualified

The two answers are computed separately and diverge here. Every load-bearing row was measured, and none puts the protocol surface below the richest measured native reference, so the protocol route can stand in for the native one. Product conformance does not hold: the effective adapter does not meet the token-accounting obligation, and nothing outside the protocol supplies it, so that capability does not work for the operator whichever route they take.

This file is validated against the tracked measurement under `tools/qualify/probe/testdata/kiro-cli/`, so an edit to a grade row below reddens the staleness gate until a fresh run replaces that artifact.

## Where to get the volatile facts

Read the flag surface off `kiro-cli chat --help` and `kiro-cli acp --help` on the version you are targeting, and the account's own model set off the CLI's model-listing command in its JSON format, which is the only authoritative answer because the list is served by the backend and depends on the subscription. The runtime's own version is never pinned in prose here; the protocol adapter records it per session from that session's own `initialize` handshake. The rendered docs at `kiro.dev` cover the headless path and the exit codes; treat them as the vendor's claim and the binary as the fact, because the two are known to disagree.

Lineage is worth knowing and does not expire: this CLI is a distribution of Amazon Q Developer CLI, so the upstream `aws/amazon-q-developer-cli` repository is the source of record for CLI behavior, while the public Kiro tracker is where product-level feature requests live. When a symptom looks like a CLI bug, search upstream, not the IDE tracker.

The runtime keeps its own log, and it is the only place some failures are explained at all. Find it under the runtime directory's `kiro-log/kiro-chat.log`. Two of the behaviors below are invisible on the wire and visible only there.

## Entry points

One route reaches this runtime: the generic `agent-client-protocol` kind drives `kiro-cli acp` and speaks the protocol. It delivers session continuation and, subject to the credential constraint below, Sortie's own tool servers. A workflow that names the removed `kiro` kind converts onto this route at load, and `## Route parity` below is the record the removal rests on.

The `acp` subcommand does not appear in `kiro-cli --help`. It is listed under `--help-all` only, which is worth knowing before concluding a build does not have it.

Where other runtimes on this transport spell trust and asking posture as two separate switches, this one spells both with `-a` (`--trust-all-tools`): it auto-approves every tool permission request, so dropping it restores asking for every tool, Sortie's and the runtime's own. `--trust-tools=<names>` narrows that to an explicit set, and a tool offered by a declared server is named there as `@<server>/<tool>`. That qualified form is the runtime's own, and it is also what the runtime prints in a tool-call title.

`--model <id>` on the `acp` entry point does take effect: the session-creation response reports the requested identifier as the session's current model. The same flag on the runtime's own `chat` surface with the JSON-Lines output format can fail silently instead, warning that the model could not be set and running the turn on the default; the warning goes to standard error as prose and never into the JSON stream.

## Route parity

| Axis | Removed `kiro` kind | Protocol route (`agent-client-protocol` running `kiro-cli acp`) | Verdict |
|---|---|---|---|
| Tool-server delivery | Never delivered: the kind declared no delivery, and the runtime's backend disables tool servers under an API key | Delivered on a local launch under a stored device login (graded usable). None under an API key (the same backend gate), and none on a remote launch | The protocol route delivers at least what the kind delivers |
| Session continuation | Continued the workspace's most recent conversation | Loads the session by identifier from a fresh process (graded usable) | Parity |
| Credential verification before the first session | Shared verification step. No login gave `credential_unverified` naming no signed-in account. A refused key failed before the first turn | Shared verification step. No login makes the runtime exit before its handshake: the run records `port_exit`, whose report carries the runtime's own not-logged-in message. A refused key fails before the first turn | Parity under the shipped rule for a runtime that rejects a credential by exiting: both fail before the first turn, both messages name the missing login, and both retry with the same backoff. The recorded failure kind differed |
| Configured credentials on a remote launch | `KIRO_API_KEY` was carried automatically | Carries the names `worker.ssh_pass_env` lists | Parity at the cost of one configuration line |
| What `sortie validate` refuses offline | Refused wrong-typed `kiro` keys, both trust keys set together, and any posture short of full trust | Its one block key's type. A switch the runtime rejects fails before the first turn with the runtime's exit status and error text. A posture short of full trust costs refused tool calls at run time, never an unanswered approval | Parity: no refusal guards a failure the protocol route can reach |
| `kiro` configuration keys | `model`, `agent`, and `trust_all_tools` (full trust was the only valid posture); `trust_tools` was refused. An unknown agent name fell back to the runtime's default agent | `--model` (measured: the session reports the requested model), `--agent` (measured: the session reports the named agent, defined globally or in the workspace, and that agent's own model unless `--model` names another), `-a`; `--trust-tools` is also offered. An unknown agent name falls back to the runtime's default agent | Parity: every key has a switch on the protocol entry point, measured to take effect |
| Token accounting | None. `sortie validate` warned when `agent.max_tokens` or a `token_rates` entry targeted the kind, and each dispatch under a non-zero ceiling logged that the ceiling could not bound the run | None. Under a non-zero ceiling each run logs once, at its end, that the ceiling could not bound it | Shared shortfall; product conformance not qualified on either route. The earlier warning is an accepted difference |
| Turn outcome, retry classification, permission handling | Was read from the text transcript, the exit status and the credits trailer | Graded usable; eligibility qualified | The protocol route meets the richest native reference |
| Accepted differences | None | One verification conversation per worker run stays in kiro-cli's own store, because the runtime offers no session delete. A session load shortly after this process creates it waits briefly for the runtime's own store to catch up. Runs after the move are recorded under the kind `agent-client-protocol`, which every protocol runtime's runs carry, so the `kiro` series in run history and `sortie stats` ends at the move. No warning before spend says a token ceiling cannot bound the run | Accepted differences, not withdrawn capabilities |

Every session in the tracked capture names its own runtime version in that capture's provenance record, and every row above rests on that build. The nightly integration run cited below ran a later build, read from that run's own job log rather than pinned here.

The qualification rows rest on the tracked capture and the eligibility summary rendered from it, described above this section.

The credential and early-exit rows rest on the shared credential-verification and early-exit cases that the protocol adapter's own gated suite runs against this runtime. The protocol route's `port_exit` outcome is the shipped rule: the credential-verification section of `docs/workflow-reference.md` reports that a runtime rejecting a credential only by printing a message and exiting is recorded as `port_exit`, not `credential_unverified`, and the domain error catalog gives both outcomes the same retryable, backoff-bound classification.

Launching the protocol entry point with no stored login and no API key exits before the handshake completes, printing a message on standard error that names the login command and states the account is not signed in. No nightly case exercises this on the protocol route; a regression there still ends at the protocol adapter's own bounded handshake wait, so it cannot hold a run.

Naming an agent definition on the protocol entry point, whether declared globally or inside the workspace, makes the session report that name as its own mode and that definition's own model unless a model flag names another; leaving the flag unset, or naming an undefined agent, reports the runtime's default agent and model instead, and an undefined name also carries a vendor notification naming the requested and the fallback agent. Both flags scope to the one session a launch starts, which matches how the protocol adapter starts or loads exactly one session per launch.

The removed kind's own offline warning was `agent.kind.no_usage_reporting`; the protocol route instead logs its own end-of-run record. Both name a token ceiling the run could not bound, and neither route accounts tokens.

The verification session the protocol route opens stays in the runtime's own store outside the workspace rather than being deleted, because the runtime advertises no delete capability. A session load shortly after its own creation waits briefly for the runtime's own store to catch up. A workflow that moves onto the protocol route keys its run history under the protocol kind rather than the native one from that point on, which is where the native kind's own series in run history and in reported stats ends.

Mechanical mapping. Every row below is a fixed rewrite that needs no judgment, so every `kiro` configuration has a behavior-preserving conversion onto the protocol route, apart from the accepted differences above.

| `kiro` configuration | Protocol-route equivalent | Basis |
|---|---|---|
| `kiro` in `agent.kind`, `dispatch.default.agent` or any `dispatch.rules[*].agent` | `agent-client-protocol` in the same place | The same selectors pick every kind |
| `agent.command` (empty selects `kiro-cli`) | The same executable, `kiro-cli` when it was empty, followed by `acp -a`. The load-time conversion builds this command, so a workflow that still names `kiro` needs no edit; when a string command is converted and an appended argument holds whitespace, a local launch becomes the list form | The kind fell back to `kiro-cli`, which the conversion keeps as its default command (`defaultCommand` in `convertRetired`); the protocol kind has none (`startSession` in `internal/agent/clientprotocol/session.go`). Command governance gives `agent.command` to the default kind alone, so a converted `kiro` route reached through `dispatch` beside a different default kind launches its own converted command and never the default kind's. A hand migration of such a route has no command to write, because the protocol kind reads `agent.command` only as the default kind, so `sortie validate` reports it under the check `agent.command` until the protocol kind is the default kind |
| `kiro.model` | `--model <id>` appended | Measured (configuration-keys row) |
| `kiro.agent` | `--agent <name>` appended; a name holding whitespace is its own element of the list form on a local launch, and single-quoted on a remote one | Measured (`--agent` evidence); the runtime accepts such a name. A `$VAR` reference in the value converts to its resolved value, so a hand migration writes the resolved value, since the flag takes the text literally |
| `kiro.trust_all_tools`, full trust being the only posture the kind accepts | `-a`, already in the command | `validateTrustToolsUntrusted`, which `convertRetired` runs, refuses any posture short of full trust |
| `kiro.mcp_config` | `agent-client-protocol.mcp_config` in a hand migration; the load-time conversion does not carry it, and its warning lists `kiro.mcp_config` as not carried | The kind never delivered it; the protocol route delivers it on a local launch under a stored login, so carrying it would start tool servers that never ran on this route |
| The `kiro:` block | Deleted; a kind reached only through `dispatch` needs `agent-client-protocol: {}` | `dispatch.agent.missing_block` in `ValidateDispatchConfig` |
| A rule's `kiro:` block under a rule that runs `kiro` | Converted with the kind and then deleted. `model` and `agent` reach only the command, so a block that would change the command the converted route launches fails the load whenever the conversion governs that route's sessions; when it governs none, the warning states that the sessions carry none of the `kiro` settings | A rule's settings block cannot set a command, and the keys the conversion does not carry are listed in the warning as `dispatch.rules[i].kiro.<key>` |
| `KIRO_API_KEY` with `worker.ssh_hosts` | `KIRO_API_KEY` added to `worker.ssh_pass_env` in a hand migration; the load-time conversion carries it on a remote launch of the converted route as a declared name, with no entry | The retirement declares it as the kind's credential variable; the protocol kind declares none. `worker.ssh_pass_env` warns when the name is unset and reaches every kind, which the declared name does not |

## Load-bearing capability observations

No row blocks transport parity: session continuation grades usable on both surfaces, so an operator moving from the removed kind to the protocol route loses nothing that was measured. Token accounting blocks product conformance without blocking parity, because both routes miss it equally, and a shortfall both routes share costs nothing in moving between them while still meaning the capability does not work.

The credential decides whether Sortie's tools arrive, and this is the single most important thing on this page. Authenticating with `KIRO_API_KEY` starts sessions, runs turns and continues sessions correctly, and silently carries none of Sortie's tools. The runtime's backend refuses to serve a governance profile for the key, and the runtime responds by disabling MCP entirely for the session: `Failed to get governance config from API - MCP disabled, web tools disabled` in its own log, plus a vendor-namespaced `governance_disabled` notification on the wire. The declared server is never started, the model is never offered the tool, and the turn completes normally while answering that it has no such tool. Nothing on the wire and nothing in Sortie's own output marks this as a failure, which makes it the most expensive way to get this integration wrong: the route is chosen for the tools, and it silently delivers everything except them.

The same server-side profile check also blocked tool servers on the removed kind. It governs both routes, and no flag turns it off.

Whether the check fails for every API key or only for keys on some plans is unestablished; one key was available to try. An operator seeing tools go missing should read the runtime log first, because that line is the only place the cause is stated.

The rows below were measured under a stored device login, which is the credential the measurement procedure assumes and the one under which the runtime's real capability shows. Nothing below was measured under an API key, so read every grade on this page as the device-login answer.

Token accounting has no source here, on either surface. The runtime reports spend as an abstract credits figure and context use as a percentage, never as a token count, so token-based budget enforcement is inert and only the turn timeout and cancellation bound a turn. The protocol route reports credits per turn as a vendor extension on its metadata notifications; the raw native stream carries no token-bearing field either. Both inventories complete with no token-bearing path resolved, and both grade a gap: there is no extension source present for the protocol route to admit, and nothing outside the protocol supplies one. That is the row where the two answers separate, and it is worth reading carefully. Parity is satisfied on it, because the shortfall is shared and an operator changing routes loses nothing they had. Conformance is not, because the capability works on neither route. A live run through Sortie's own orchestrator under a one-token `agent.max_tokens` ceiling on the protocol surface spent tokens and recorded no figure: three turns reported unaccounted spend while the run finished with its own status rather than a stop, so the ceiling could not bound it. The conformance row still stands on the wire gap, because no figure reaches Sortie on either route for a ceiling to be built on.

Two cases are declared rather than measured, on both surfaces: a turn ending in `runtime_refusal` and a retry classified as a `non_retryable_refusal`. This runtime never produces either outcome; a request built to trigger one instead completes the turn normally, so the case is recorded as `outcome_never_produced` rather than graded from an attempt that failed to reach it.

Retry classification does not read the same way on the two surfaces. On the protocol surface the human-input case is excluded as not applicable, and the row grades usable on the cases that remain. The exclusion rests on the consent request the asking-posture launch raised and the client refused inside the protocol: the request offered a refusing option, the refusal was answered there, and the turn went on. It does not cover a question addressed to a person. Permission handling graded usable on the same run, which is what that request actually is. On the raw native stream the case was induced and the row grades a gap, because a recognized terminal ended the turn even though the probe's own marker file, which shows the block was reached, is present.

The `unknown_outcome` case has no deterministic inducer on either surface, so it carries no obligation and retry classification stands answered under product conformance on the cases that remain. `limit_reached` still stays unmeasured for turn disposition, because it was not induced on either surface.

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

Two of the cases excluded below are excluded while keeping their obligation, which is a different thing from being forgiven: a case that was induced and that the surface then reported no outcome for counts against that surface rather than passing for free.

Permission handling was measured under the posture that asks, which for this runtime means the same launch with its single trust-and-posture switch taken back out. The runtime raises the request, Sortie's unattended posture refuses it, the refusal is accepted, and nothing is left pending when the turn ends. The consequence for a real run is the one the transport notes already state: under a posture that asks, a declared tool is delivered and still never called.

## Protocol-specific observations

The handshake advertises `loadSession` true, `mcpCapabilities.http` true with `sse` false, and an empty `sessionCapabilities` object. Sortie decides whether to send `session/close` from that capability being present, so a session here is never closed through the protocol. `authMethods` comes back empty, which is evidence in neither direction. `agentInfo` carries both a name and a version, which is what the gated suite's identity rule reads.

Session continuation restores the session and its memory. The load succeeds, the recall turn runs under the seed's own session identifier read back from the runtime rather than one Sortie minted, and the model returns what the seed turn asked it to remember. A successful load is still not on its own evidence that anything said in the session survived; only the recall turn's own answer settles that. The runtime's own native surface returns it too, so the two agree on this row.

The runtime carries a large vendor-namespaced surface alongside the standard one, announcing available commands, subagent lists, MCP server initialization and per-turn metadata under its own method prefix. Those arrive as notifications rather than requests, so nothing answers them and nothing depends on them; Sortie records them as unrecognized and moves on. Do not add handlers for them to make a log quieter, because the standard surface already carries everything the adapter reads.

A `limit_reached` turn was not induced on this surface. The prompt channel is too small to carry a request large enough to reach whatever ceiling would make the runtime report running out of room, so the case stays unmeasured here rather than graded, and turn disposition keeps its usable grade on the cases that did run.

## Native headless observations

The runtime's own native surface has a structured output mode, and older notes in this file claiming it has none were wrong. `kiro-cli chat --output-format stream-json` emits JSON Lines on standard output, one self-describing event per line, opening with a run-started event that names the protocol as its own payload schema and closing with a run-finished event carrying a status, a stop reason and the final text. A failure arrives in the same envelope under a run-error type. The mode requires the second-generation agent engine and refuses to start on the first.

This run measured the structured surface directly. Session continuation grades usable there and is the richest measured reference on that row: it resumes by naming a session identifier explicitly, a seed launch's own terminal reports the identifier in its run-started event, which is the only place this runtime exposes one at all, and a following launch names it to resume and gets back what the seed turn left. Turn disposition and retry classification both grade a gap there, and token accounting grades the same gap as on the protocol surface.

Two turn-disposition cases were induced here and the surface reported no outcome for either. The structured stream stays silent on a failed launch, and because this is a one-shot launch that writes its terminal only when the process exits, a cancellation signal sent mid-turn leaves no terminal to read: the process just stops, so a cancelled turn and one that has not finished yet look identical. Silence does not excuse either case, and the two of them are why turn disposition grades a gap on this surface. A third case, `limit_reached`, was not induced here either, for the same prompt-channel reason it was not induced on the protocol surface.

The profile treats the plain-JSON surface as absent, which the binary agrees with: the output-format flag accepts only the text and JSON-Lines values, and rejects anything else on a non-zero exit with a plain-text message. Its entry point in the profile is therefore the ordinary headless invocation rather than a flag value the runtime would reject, because a declared absence is corroborated by running the surface and finding no structured terminal in what comes out. A launch that fails to start demonstrates nothing, and the corroboration reads any non-zero exit as a terminal rather than as an absence.

## Workspace trust and process boundary

The protocol entry point is a launcher, not the worker. Launching it forks a second process that does the work and stays alive behind the parent, so a single-pid kill leaves that worker running until its inherited standard input closes. Sortie's teardown sends a catchable signal to the whole process group, closes standard input, waits a bounded grace, and kills the group only once that wait elapses, and the launched process is put at the head of its own group so an inheriting worker is reached. The qualification run asserts the group is gone after teardown, so a survivor is reported as a leak rather than tolerated as a slow exit.

The containment boundary held under measurement: every launch ran in a directory inside the run-scoped root, no project settings applied to any of them, and every process-group member observed was the launched command or a descendant of it.

Tool servers declared in the session-creation request are merged over whatever the runtime's own configuration already holds. A workspace-scoped MCP configuration lives at `.kiro/settings/mcp.json`, and agent definitions with their own tool lists live under `.kiro/agents`; the tracked profile records both. A declared server is dropped silently when its entry does not match the wire shape the runtime expects, with the reason recorded only in the runtime log, so an unexplained absence of tools is worth checking there before anywhere else.

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

The conversion is covered by the unit tests in `internal/agent/kiro` and by the validate and dry-run tests in `cmd/sortie`.

The protocol route is covered by the generic adapter's own gated suite, pointed at this runtime through the client-protocol command coordinate, and by the live qualification profile, which is separately gated and spends real credits.
