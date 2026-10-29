# Copilot CLI adapter notes

Working notes for anyone changing Sortie's GitHub Copilot CLI adapter in `internal/agent/copilot`.

Eligibility: unmeasured

Product conformance: unmeasured

Neither verdict is a failure. Both are unmeasured because the cases that need the runtime to refuse a turn never occurred: a request built to trigger a refusal completed normally on both surfaces. No measured row puts the protocol surface below the native one.

This file is checked against the tracked measurement under `tools/qualify/probe/testdata/copilot-cli/`. Changing a grade row below fails the staleness gate until a fresh run replaces that measurement.

## Where to get the volatile facts

Nothing here pins a flag list, an event vocabulary, or a payload shape. Read the flags off `copilot --help` on the version you target, and the output format, permission flags, hooks, and telemetry from GitHub's Copilot CLI reference. Check the upstream issue tracker when the docs and the binary disagree. `buildArgs` and `parse.go` define what Sortie actually sends and parses.

## Shape of the integration

The adapter registers the `copilot-cli` kind and drives the CLI in headless prompt mode with JSON lines on stdout. It holds no process open: the CLI session is disk-persisted state identified by a server-assigned ID, and the OS process is created once per Sortie turn. Process groups, graceful shutdown, the stdout line ceiling, and the exit-code decision table all live in the shared `agentcore.ForkPerTurnSession` skeleton, not here.

Two flags are load-bearing and unconditional. Without the autonomous-continuation flag the CLI answers once and exits, so a non-trivial headless task is impossible; the adapter passes it on every invocation along with a step cap, defaulting to a bounded value when the workflow does not set one. And the flag that disables the CLI's ask-the-user tool is passed on every invocation as well, which closes the human-question path completely: no turn of this adapter can end needing human input, because the runtime has no way to ask.

## Session identity, and why turn one is different

`StartSession` mints a v4 UUID for every new session before any turn runs and returns it as `domain.Session.ID`. The first turn passes `--session-id <uuid>`, and every later turn passes `--resume <uuid>`. A session handed back after a restart uses `--resume <uuid>` from its first turn.

No launch ever passes `--continue`. That flag resumes the most recent session in the home directory, whichever directory created it, so it could continue another issue's conversation or a credential-verification session.

The adapter opens the runtime's session-state tree only by a known ID, and it validates that ID against a path-segment character set before joining it into a path. Both properties are security boundaries, not stylistic choices. The journal's location, the ID rule, and the read size and line caps live in `internal/agent/copilot/sessionstate`, which the protocol kind imports too.

## Preflight, and the fake-binary trap

`StartSession` runs a version canary before it will hand back a session, local-mode only: the shared runtime version query (`internal/agent/agentcore`), which fails when the binary does not answer `--version` with a zero exit, as under a Node.js older than 22. It checks nothing else. The credential itself is proven separately: before the working session's first turn, unless a recent proof of the same setup stands, a shared step (`internal/agent/agentcore`) starts a verification session and sends one fixed request. This CLI reports a refused or absent credential only on stderr with a failing exit, so the step reports it as the shared early-exit report carrying that message. A stored `gh` login passes the step.

This is the thing that ruins an afternoon when writing tests. A stand-in binary that does not answer `--version` with a zero exit never reaches a turn, because the version canary rejects it first, and the failure surfaces as the shared early-exit report at session start, which looks nothing like a parse problem. In SSH mode the canary is skipped, so a broken remote binary instead fails later, at the credential-verification step or the first turn, with a different error kind. Know which mode your test is in.

## Token accounting lives off the event stream

The adapter reads no token figure from the event stream. The session-state journal is its only source, an on-disk record the adapter reads after the subprocess exits.

That journal is session-cumulative across every process that resumes the session, not per-invocation. A second turn's record includes the first turn's spend. Recovering one run's own contribution means subtracting a baseline held in an `agentcore.ResumeBaseline`. `StartSession` seeds it with zero for a session the run creates and with the resume usage handed over for a resumed one. With no hand-over, a local launch whose id passes the path-segment check reads the journal's newest record once: a journal without a record, or a missing file, seeds zero, and any other failure leaves the baseline unknown. No outcome fails `StartSession`. The read at the end of every started turn (cancelled ones included) compares the newest record with the baseline's latest reading. Equal totals mean the turn wrote no record, so it reports no figure and emits no `token_usage` event; otherwise the baseline turns the record into the run figure. An unknown baseline takes the record as its starting point, so that turn reports no figure and `SpendUnaccounted`. A skipped or failed read reports no figure and nothing is abandoned: the next turn reads again. Journal reads are also skipped in SSH mode, on an ID that fails the path-segment check, and on a file that breaches the size or line caps. A turn that reports no figure after the CLI served output also reports `SpendUnaccounted`, so every SSH-mode turn that streamed output does.

The practical rule: a usage figure from this adapter is run-cumulative and never decreases, a failed journal read never lowers a figure already reported, and the "measured" flag distinguishes an unknown spend from a genuine zero. Assertions that ignore that distinction pass for the wrong reason.

The registered kind attaches the `result` event's `usage.premiumRequests` as its live source, a vendor unit that enters no token figure and is logged in the `copilot turn completed` record, and the session-state journal as its records source. It derives `turn_end` arrival and `per_model` attribution: the figure and the model both come from the shutdown record, reported through the shared turn-end report in `agentcore`. The journal is read only on a local launch, so a run on an SSH worker reports no token usage: `agent.max_tokens` and the money ceilings cannot bound it, preflight warns, and window spend reads as a lower bound.

Besides the shutdown record, the journal carries one `session.usage_record` line per model request. The runtime writes it before the reply, with the request's token counts, the model that served it, and an opaque request identity under `data.usage`. The `agent-client-protocol` kind reads these lines, and this kind does not. The checkpoint and shutdown totals cover the same tokens as those lines, so a reader takes one or the other for a span of tokens, never both.

The model name is the shutdown record's `modelMetrics` key whose token count grew the most since the previous record, breaking a tie by whichever trimmed name sorts first. A record naming no model, or one where no key grew, leaves the field blank rather than falling back to the operator-configured `copilot-cli.model` passthrough value, which is the model requested rather than the model the runtime reported using. The usage event also carries the newest shutdown record's `modelMetrics` per model, under the shared resume baseline seeded from the source of the combined seed.

## Quota windows are pulled, not streamed

Neither the JSON stream nor the session-state journal carries a quota figure. The runtime's quota snapshot event is ephemeral and never reaches the journal, so the adapter asks for the quotas after a turn that reached the runtime has ended. A cancelled turn and a credential-verification session do not ask. A pull the runtime cannot answer ends the session's pulls; any other failure leaves the next turn to try again.

There are two routes, tried in one pull. Both yield one reading per quota that is not unlimited. Test the unlimited flag, not the entitlement: the SDK documents `-1` for an unlimited entitlement, while a live answer for an unlimited quota carries `0`.

The endpoint route is `GET https://api.github.com/copilot_internal/user` with `Authorization: token <T>`. `T` is the first non-empty of `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`, read from the turn's environment. The fields read, trimmed:

```json
{
  "quota_reset_date_utc": "2026-11-01T00:00:00.000Z",
  "quota_snapshots": {
    "premium_interactions": {
      "unlimited": false,
      "percent_remaining": 0.0,
      "entitlement": 20000,
      "timestamp_utc": "2026-10-28T00:34:20.549Z"
    },
    "chat": { "unlimited": true, "percent_remaining": 100.0 }
  }
}
```

`percent_remaining` is the share left, so the used share is its complement. `timestamp_utc` is when the snapshot was taken, and `quota_reset_date_utc` is the instant the quotas reset. The answer is fresh on every call. The sibling `quota_reset_date` is a bare date and is not read. A 401, 403, or 404 falls through to the headless route. Any other HTTP error is reported as its status code alone. The token goes in that one header and nowhere else, and the response body is never logged.

The headless route runs when no token variable is set, or when the endpoint refuses the token. It starts `copilot --headless --no-auto-update --stdio` on the runtime's host, locally or over the session's SSH launch, and speaks JSON-RPC with `Content-Length` framing on the standard streams. The exchange is `connect` first, with the parameters `@github/copilot-sdk` sends, then `account.getQuota` with `{}`, then stdin closes. The answer, trimmed:

```json
{
  "quotaSnapshots": {
    "premium_interactions": {
      "isUnlimitedEntitlement": false,
      "entitlementRequests": 20000,
      "usedRequests": 20000,
      "remainingPercentage": 0,
      "resetDate": "2026-10-28T00:15:43.579Z"
    }
  }
}
```

Two traps live in this answer. First, the runtime serves it from a cached copy of the endpoint's answer, kept in its user cache file outside `COPILOT_HOME`, with no network round trip. Authenticated headless runs did not refresh that cache, so the figure can trail the endpoint. Second, `resetDate` is the time the cached snapshot was taken, not a reset: it equals the cache's `timestamp_utc` and differs from the endpoint's `quota_reset_date_utc`. The adapter reads it as the observation time and reports no reset on this route. An error answer, such as `Not authenticated`, means the runtime holds no GitHub sign-in of its own, and it ends the session's pulls.

The `agent-client-protocol` declaration for Copilot CLI runs the same pull, with the same token variables read from the session's launch environment. Its headless command is the configured command cut after the `copilot` word: `--acp` and every later word are dropped, a launcher, environment assignments, or a path before the word stay, and the headless arguments follow. The protocol's live channel carries no quota: `initialize` acknowledges no quota opt-in, a `usage_update` carries only `used` and `size`, and the ACP connection answers `account.getQuota` with method-not-found.

A custom provider reports no quota. With `COPILOT_PROVIDER_BASE_URL` set in the runtime's environment the turn spends no Copilot quota, so the pull returns unsupported before any request on either route.

The endpoint request is made from Sortie's host, also for a remote session. A token that only the remote host sets is invisible there, so such a session reaches the quotas through the headless route on the remote host.

## Approvals

By default the adapter grants everything, because an unattended run has nobody to prompt. Only an approval allow-list (`allowed_tools`) displaces that blanket grant, and only because the grant is a superset of it. A deny rule outranks the grant, and the two visibility filters decide what the model can see rather than what it may do, so both keep their effect beside the grant instead of displacing it. The grant is a bundle covering tool approval, path verification, and URL access, so replacing it narrows all three at once and restores the runtime's default rule confining file access to the working directory. Config validation reports the allow-list case as a warning rather than an error, because the configuration is honored exactly as written, at the cost that a turn whose calls were all denied can still report success.

A denial is recognized on the tool-completion event by its error code, distinct from an ordinary tool failure. The adapter routes it through `agentcore.DecideHumanRequest` like every other adapter and emits the shared operator-facing notice. It never constructs a posture itself, and it never treats a denial as terminal, because the run continues.

## Deciding how a turn ended

The runtime's task-completion report decides the turn, not the exit code alone. A non-zero exit is a failure. On a zero exit with a terminal event but no task-completion report, the turn hit the autopilot continuation ceiling; it did not succeed. Without a terminal event, the decision falls to the process exit and work evidence.

The CLI publishes no stop reason, and its journal writes the same shutdown word for a finished session and for one cut off at the ceiling. The task-completion report is the only signal that separates them. `agentcore.FinalizeTurn` maps the evidence to a disposition. On a non-zero exit the failure reason is the turn's last `session.error` message, or `non-zero exit in result event` when none arrived.

Work evidence comes from the shared per-turn observer, never a token count: it records assistant-authored content (`assistant.message`'s non-empty `data.content`, any `assistant.message_delta`) and tool-call activity (`assistant.message`'s non-empty `data.toolRequests`, a parsed `tool.execution_start` or `tool.execution_complete`). Get that wrong and the safety row that turns "exited cleanly, produced nothing" into a failure stops firing for the rest of the run. This CLI has more than one way to exit zero having done nothing.

A zero exit that wrote no decodable event line, for example after a configuration file it could not parse, gets the early-exit report. The zero-work row applies only when at least one event line arrived and no terminal event followed.

The adapter enforces no read deadline and no turn deadline of its own; both are orchestrator-side. Combined with the disabled ask-the-user tool, that makes the response-timeout and input-required outcomes unreachable here by construction.

## Sortie's own tools

The worker generates one MCP configuration file per session and the adapter passes it by file reference. On a local launch the file is derived from the generated one: it names an operator header value by the environment variable that carries it instead of holding the value, and it keeps the env block described below. The flag on this CLI is additive rather than replacing, but the adapter still routes an operator-supplied config through the same merge into one file, and a collision on our reserved server key fails the attempt rather than silently overwriting. When an operator value is passed through on its own, the adapter distinguishes inline JSON from a file path by prefix, because the flag itself does: a bare path is read as inline JSON and fails to parse. The runtime starts the tool server with the environment it inherited plus the configuration file's `env` block, and a block entry wins over an inherited variable of the same name. The block carries every `SORTIE_`-prefixed variable, credentials and the dispatch identity included, so the tool server sees them regardless of what the runtime itself inherited. On an SSH launch no file is passed: the runtime receives an inline declaration of Sortie's tool server, by the address of the launch's reverse forward with the bearer carried by environment variable name, beside the operator's HTTP servers. Operator stdio servers are left out, and the tool server's env block never leaves the orchestrator host.

Whether Sortie's tools reach an agent at all is a per-adapter property. The mechanism depends on what the CLI accepts, so what works here does not carry to the rest of the fleet.

## Failure modes worth recognizing

A process that ends having written nothing on standard output, whether from a configuration file the CLI cannot parse, a rejected switch, or a refused credential, fails with the early-exit report, whose standard-error tail holds the diagnosis, a wrong one included: a rejected switch surfaces as an unquoted-prompt complaint rather than naming the switch. The adapter also logs the same stderr lines at warning level on any failing turn. Look there first when a turn fails with no events at all.

Stderr can also be missing outright. When it carries a single marker line saying it was not collected, a descendant process held the handle open past the direct child, the diagnostics were never read, and the wait for them was cut short so the turn could still be reaped and reported. That marker is the finding, not a truncated log.

Two failures worth recognizing come from outside our code entirely, and both are worth checking before you go looking for a bug in the adapter. A resume that comes back with no conversation history, on a session whose journal is present on disk, points at the journal itself rather than at our resume logic: the journal is line-delimited JSON and a raw character sequence that breaks a line-oriented parse takes the whole history with it. And a subprocess that starts, prints nothing, and never exits has been seen under environment managers that wrap the shell and rearrange its file descriptors; there is nothing on our side to fix, and stall reconciliation is what ends such a turn. If either reproduces, confirm it against the current release before treating it as known.

Session state accumulates under the user's home directory, outside the workspace, so removing a workspace does not reclaim it. The credential-verification step's own conversation does not linger there: `StopSession` deletes it by sending a `Content-Length`-framed JSON-RPC `session.delete` request, carrying the session's UUID, to a one-shot `copilot --server --stdio` process, keeping standard input open until the response arrives; anything but a prompt `success: true` is logged as a warning rather than failing the run.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_COPILOT_TEST=1` and skip cleanly without it. The other `SORTIE_COPILOT_*` variables they read are described in that file. Traps:

- The working case needs a real credential: one of the token variables the CLI accepts, or an authenticated `gh`.
- A stand-in binary is rejected by the version canary before any turn, as described above.
- The scripted-model case uses the CLI's own-provider settings, with `COPILOT_OFFLINE` turning off GitHub authentication, telemetry, and update traffic. Its script must end with a `task_complete` call: in autopilot the runtime keeps prompting until the model calls it, and a plain text finish draws a request the script does not hold.
- The scripted-model case isolates `HOME` and the XDG directories and puts `COPILOT_HOME` inside them, so a version-manager shim that reads `HOME` fails to start. Name the binary by an absolute path.

## Entry points

Two routes drive this runtime. The `copilot-cli` kind, described in the sections above, runs the CLI in headless prompt mode once per turn. The generic `agent-client-protocol` kind drives `copilot --acp` and keeps one process open for the session. This page records how the protocol route meets the generic adapter and how it compares with the native one. The sample workflow for the protocol route is `examples/WORKFLOW.agent-client-protocol.copilot.md`.

`--acp` is the only protocol entry point. The route delivers session continuation, spend through the shared usage component, and Sortie's own tool server over HTTP.

`--allow-all-tools` sets the posture. It approves every tool permission request, so removing it brings back asking for every tool, Sortie's and the runtime's own. No other switch is needed for a declared tool server to arrive; the trust section below explains why.

`--model <id>` pins the model on both routes. Without it the account default applies.

A stored login authenticates a run. Every grade on this page was measured under a stored login with no token variable passed to the runtime.

## Load-bearing capability observations

No row blocks transport parity. Turn disposition and retry classification are not observed on either surface, because the refusal cases never occurred, and that is what holds both verdicts at unmeasured. Token accounting is a gap on both surfaces, so it does not separate the routes.

The protocol wire carries no token figure. Sortie takes the figure from the runtime's own records, the session-state journal that the native kind reads too. The effective adapter returned a measured figure from that source, and the ceiling stop worked: a live run through Sortie's orchestrator stopped at the figure ("stopped at ... token(s), the issue entered the token budget hold") and dispatched nothing in the quiet window after it. The grade stays a gap because it describes the wire, the way the Gemini notes record theirs.

The native stream carries no token figure either. Its terminal event reports premium requests, a vendor unit, and no token counts. That row is a known gap with an owner, not a finding about this runtime.

Other gaps on the protocol route, each with an owner:

- The run record does not carry the provenance of a spend figure or whether it is complete.
- Amounts in the runtime's own units, premium requests and AI credits, are not recorded on the run.
- The documentation site has no page for the protocol route of this runtime. The sample workflow points at the generic reference page.

Two cases are not observed on either surface: a turn ending in `runtime_refusal` and a retry classified as `non_retryable_refusal`. A request built to trigger one completed normally. They are recorded as unobserved, not declared never produced, which is why both verdicts are unmeasured. `limit_reached` was induced on the protocol surface the same way: the oversize turn completed normally. On the native surface it was not induced, because the prompt channel cannot carry a request big enough.

On the protocol surface, retry classification excludes the human-input case as not applicable. The consent request the asking-posture launch raised offered a refusing option, the client refused it inside the protocol, and the turn went on. That exclusion does not cover a question addressed to a person. The native stream has no terminal vocabulary for the human-input outcome, so the case is not inducible there.

An endpoint failure is a declared outcome on the protocol surface and not inducible on the native one; the next section gives the frames. Cancellation passed on the protocol surface, where the turn ended in cancellation after its own context was cancelled. The native stream writes its terminal only at exit, so cancellation cannot be induced there. The retryable failure case passed on both surfaces: after one SIGKILL to its group, the launch reported transport loss. The `unknown_outcome` case has no deterministic inducer on either surface, so it carries no obligation.

- protocol turn_disposition: Not observed: not_observed
- protocol retry_classification: Not observed: not_observed
- protocol token_ceiling: Observed: gap
- protocol tool_server_delivery: Observed: usable
- protocol session_continuation: Observed: usable
- protocol permission_handling: Observed: usable
- native_stream_json turn_disposition: Not observed: not_observed
- native_stream_json retry_classification: Not observed: not_observed
- native_stream_json token_ceiling: Observed: gap
- native_stream_json session_continuation: Observed: usable

A case that was induced and that the surface reported no outcome for keeps its obligation. It counts against that surface; it is not forgiven.

## Protocol-specific observations

The handshake reports the agent's own name, `Copilot`, and its version, which the gated suite's identity rule reads. It advertises `mcpCapabilities.http`, and `sessionCapabilities` carries `close` and `list` but not `resume`.

Session continuation restores the session and its memory: the recall turn runs under the seed's own session identifier and returns what the seed asked it to remember. A successful load alone does not prove that; only the recall answer does.

Copilot loads a tool server declared in `session/new` only over HTTP. A stdio declaration is ignored: the model is not offered the tool, the turn completes, and nothing marks the failure. Sortie's own tool server arrives over HTTP, so only an operator's stdio server is affected. A tool call is titled `<server>-<tool>`, on this route and on the native one.

Without `--allow-all-tools`, a tool call raises `session/request_permission`. When Sortie answers `reject_once`, Copilot fails the call with "The user rejected this tool call.", prints "Info: Operation cancelled by user", and ends the turn without a follow-up model request. Nothing is left pending. So under a posture that asks, a declared tool is delivered and never called.

An invalid `GITHUB_TOKEN` beside a stored login did not stop a run. The stored login authenticated it, and the sample workflow says the same.

An endpoint failure is folded into a completed turn. When the scripted model endpoint answers HTTP 400 after its script is exhausted, Copilot renders the error as agent text and ends the turn normally. The frames of the script-exhaustion run:

- frame 6: `agent_message_chunk`, "Warning: No token count multiplier ..."
- frame 7: `usage_update`
- frames 8 and 9: `tool_call` and `tool_call_update`
- frame 10: `usage_update`
- frame 11: `agent_message_chunk`, "Error: 400 scripted model: request 2 ... arrived after the script was exhausted"
- frame 12: `{"stopReason":"end_turn",...}`

Nothing on the wire marks the failure, so the profile declares it for the protocol surface only, as an outcome folded into another case. The native route differs: `copilot -p` exits with status 1, prints the error on stderr, and writes no result event. That is why the native surface carries the `output_channel_silent_on_failure` entry for the same case.

Every model request sends a `usage_update` before its model text. The Copilot usage declaration rests on that order, because the runtime's own text chunks carry no message identifier and no metadata, so frame order is the only signal on the wire. It held on the bring-your-own-key route, in the frames above, where a `usage_update` (frame 7) comes ahead of the first tool call and another (frame 10) ahead of the next request. It held on the hosted provider too: a text-only request runs `usage_update` at frame 6 and the first model text at frame 7, and a tool-forcing request runs `usage_update` (6), `tool_call` (7), `tool_call_update` (9), a second `usage_update` (10), then the model's text (11).

Runtime text of Copilot's own can precede the first `usage_update`, so the rule reads model text, not any chunk. Two such lines were seen. The notice "Warning: No token count multiplier is known ..." appears for a model id the multiplier table does not know, such as `scripted-model`. On the hosted provider, the credential verification session starts with `Info: Disabled tools: ...` at frame 6, then `usage_update` at frame 7, and the model's text at frame 8 (`"SORTIE_CRE"`). Neither line counts as the model being reached.

The notice depends on the model id. `claude-sonnet-4.5` is known to the installed Copilot CLI and prints no notice; `scripted-model` does. The default-declaration row of the gated suite names the known id, so that its interrupt scenarios measure the default path and not a property only the Copilot declaration describes.

## Native headless observations

`copilot -p <prompt> --output-format json` emits JSON Lines on stdout. The terminal is a `result` event that carries `exitCode` and `sessionId`. A later launch resumes the session with `--resume <session id>`, and the recall turn ran in the same session as the seed.

The CLI offers no single-document JSON output, so the profile declares the plain-JSON surface absent. The run corroborated the declaration.

A refused model or an endpoint failure ends a `-p` launch with exit status 1 and an error on stderr, and no result event. The stream is silent on failure, so that case is not inducible on this surface. The result event is written at exit, so cancellation is not inducible either.

Token accounting is covered above: the stream holds no token figure, and the terminal reports premium requests.

## Workspace trust and process boundary

The containment boundary held under measurement: every launch ran inside the run-scoped root, no project settings applied, and every process-group member observed was the launched command or its descendant.

A tool server declared in `session/new` needs no trust switch. It loaded over HTTP in a workspace the runtime did not trust, and the capture's tool-server row passed without one. There is no trust switch to add on this route: `--trust-folder` is a prompt-mode switch, and the CLI refuses it beside `--acp`.

A workspace `.mcp.json` is a separate path. In prompt mode it loaded only with `--trust-folder`; without the switch the file was ignored.

## Excluded capability cases

- retry_classification human_input: induced, and the surface reported no outcome (terminal_vocabulary_closed), so the case keeps its obligation
- retry_classification human_input: not applicable on protocol: the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose
- retry_classification unknown_outcome: no deterministic inducer on any runtime, so the case carries no obligation
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_failure: declared outcome_folded_into_another
- turn_disposition runtime_failure: induced, and the surface reported no outcome (output_channel_silent_on_failure), so the case keeps its obligation

## Unobserved surfaces

These cases were induced and the runtime did not produce them:

- native_stream_json retry_classification non_retryable_refusal: fixture_induction_failed
- native_stream_json turn_disposition runtime_refusal: fixture_induction_failed
- protocol retry_classification non_retryable_refusal: fixture_induction_failed
- protocol turn_disposition limit_reached: fixture_induction_failed
- protocol turn_disposition runtime_refusal: fixture_induction_failed

Windows live qualification is unobserved.
