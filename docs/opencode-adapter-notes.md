# OpenCode adapter notes

Working notes for anyone changing Sortie's OpenCode adapter in `internal/agent/opencode`.

Last updated: November 2026

Eligibility: qualified

Product conformance: qualified

Every load-bearing row was measured, and no measured row puts the protocol surface below the native reference, so the protocol route can stand in for the native one. The effective adapter meets every load-bearing obligation, counting what Sortie's own code supplies outside the protocol.

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

## Session identity and resume

A session ID is server-generated and learned from the first envelope that carries one. The adapter adopts it, and a later envelope carrying a different ID is treated as a fault that fails the turn rather than a thing to reconcile.

`--session <id>` continues the session or, when no session has that ID, creates a new empty one under it. The run exits 0, every envelope carries the requested ID, and nothing in the stream marks the replacement; the different-ID check never fires. The signal comes before the turn: `opencode session export <id>` of a missing session exits 1 with `Session not found: <id>` on stderr, and the adapter already runs that export when a continued session starts. The adapter passes `--session <id>` only after that export confirms the session, so the runtime never creates an empty session under the requested ID. A lost SSH connection during the export fails the start, and the retry resumes the same session. Any other outcome starts a new session without `--session`, and the new ID comes from the first envelope. An export that exits non-zero or names another session logs the verdict `replaced`; a timeout, output above the size limit, or a document that does not decode logs `unknown`. Resuming from a directory other than the one that created the session carries its history. To reproduce, seed a session in an empty directory and take `<session-id>` from the `sessionID` of its output, resume it from a second empty directory, then delete it and export and resume it:

```sh
echo "Remember the word PELICAN. Reply only OK." | opencode run --standalone --format json
cd <second-empty-dir>
echo "What word did I ask you to remember? Reply NONE if you do not know." | opencode run --standalone --format json --session <session-id>
opencode session delete --standalone <session-id>
opencode session export --standalone <session-id>; echo "exit $?"
echo "What word did I ask you to remember? Reply NONE if you do not know." | opencode run --standalone --format json --session <session-id>; echo "exit $?"
```

Sessions are persisted in one global database under the user's home directory, not in a per-project store inside the workspace. Isolation between concurrent issues comes from distinct server-generated IDs, not from separate storage, and removing a workspace reclaims none of it.

## Permissions

OpenCode's permission control is config-driven. Each key resolves to allow, ask, or deny through an ordered rule list; the last matching rule wins. The runtime's own default allows every tool except reads of `.env` files, which ask. A project's saved approvals apply after every configured rule and override any of them except a deny. So when bypass is off and the tool lists do not name `*`, the adapter writes `deny` for `*` as the first rule of its policy: every tool the lists leave undecided is left out of the model's tool list, and an undecided `external_directory` access is refused, whatever approvals the project has saved. The rules the lists produce follow the base rule, so each still decides its own key, and a list that names a key sets its action as written. The inline document applies after the operator's own configuration files, so the base rule overrides the operator's own top-level rules for every key the lists do not name. Rules the operator writes for one agent apply after it, for the agent `opencode.agent` selects and for the default agent alike, and still decide their keys.

The adapter sends its policy in the inline configuration document. OpenCode deep-merges that document into the rules from every other configuration source; it does not replace them. So a value inherited from the operator's shell would leak into the result. The adapter removes every variable it manages from the inherited environment before adding its own. Keep that scrub in step with the managed set: a new managed variable that is not scrubbed is a leak.

The emitted policy is closed. When the workflow names allowed tools, every other permission key the adapter knows is denied explicitly. That known-key set is a policy input, not a validation list. The runtime accepts more keys than its docs list, so a key the workflow names is forwarded as written even when the adapter does not know it. Rejecting unknown keys would break configurations OpenCode accepts.

Permission prompting is bypassed by default, because otherwise the runtime refuses every permissioned tool call. Validation warns when a workflow turns that off, and rejects an overlap between the allowed and denied lists.

A refusal appears on stderr as a warning line styled with escape sequences; stdout stays clean JSON. The adapter strips the escape sequences before matching the prefix. Skip that and the match never hits real output. The notice is read from collected stderr after exit and goes through `agentcore.DecideHumanRequest`.

The hosted free-tier model refuses any request whose tool set lacks its shell tool or its read tool. That refusal arrives as a run-level error, not as the permission notice. When the session's own tool policy denied one of those tools, the adapter appends a clause naming it to the vendor's text. This match depends on the vendor's wording. If upstream rewords it, re-capture the fixtures; do not widen the text match.

## Usage accounting

Step-scoped token counts on the stream are not a running total. Between the tool step and the final text step of one turn the numbers move in both directions, so taking the last one is wrong.

The kind declares two sources and the shared usage component does the arithmetic. The adapter parses reports and states what each source means; it subtracts no baseline, adds no figures, and decides no completeness. The live source is the `tokens` of each `step_finish` part, one report per model request, identified by the part's `id`. The records source is the session export, which holds one message per finished request, keyed by the message identity and named by provider and model. The two derive `turn_end` arrival and `per_model` attribution.

The usage component normalizes the counts, adding cache to input and reasoning to output, and holds the greatest reading per request identity, so a report that repeats counts once. The runtime prints every counter in a step report, zeros included, and an export message carries every count. A report or message with a count that is absent, null, or not an integer is unknown, never zero. An unknown step report counts nothing and keeps the turn from settling on step reports. An unknown export message stops the known records before it, so that turn and every later turn of the session are lower bounds.

Whether the final text step of a turn prints a `step_finish` depends on the release, and the kind handles both without a version check. `opencode run` prints a step's report live while it streams the session, stops reading step events once the session goes idle, and then replays the turn's messages it missed. The last step usually ends after the session goes idle. Current releases replay a `step_finish` for every finished step the live stream missed, under the part identity `prt_<message>_step-finish`, so each step reports exactly once and the final one carries `reason` `stop`. That report ends the turn, so the step reports settle it alone, complete, with live provenance, and the export only splits the figure by served model. Earlier 2.x releases replay no step boundaries, so the final report appears only when the step ended before the session went idle. Without it the sum of the step reports falls short of the turn's spend and the export settles the turn. A turn the runtime did not end itself, by an `error` event, a Sortie stop, a kill, or a cancel, never settles from the step reports.

Records and step reports count the same requests, so the two are never added. When the final report is missing, a figure the export recovers replaces the step readings, and the turn settles exact and complete with the records' provenance, split by served model. When the export fails or holds no new message there, the step sum stands as a lower bound with live provenance and no split, and the turn carries unaccounted spend. A turn with no step report in that case has no figure and its spend is unaccounted. A later turn that reads the export counts from the records again, so the run's total converges on the export's.

The adapter sums per-message figures and ignores the export's session aggregate, for two reasons. The aggregate covers the whole session, including earlier runs, while Sortie reports per run. And its total also includes cache and reasoning tokens, so it is not input plus output.

A finished message's `cost` is read in US dollars as a per-request amount beside its tokens, under the message's `providerID/id` model, and the records source declares it. The `cost` of a `step_finish` report is not read, because its part identity differs from that of the export message that counts the same request, and neither is the export's `info.cost` aggregate. A free model reports `cost` 0 beside non-zero tokens, and a 0 beside tokens is no amount, so the model's amount is a lower bound. A child session's cost is never added to its parent's: the child's tokens count and mark the model's amount a lower bound.

A message counts only once its `finish` field is set. The runtime saves an assistant message with zero tokens before calling the model, so a turn killed mid-step exports a placeholder, not a measurement. A finished message without tokens after the boundary is an aborted request whose spend is recorded nowhere, so it is unknown like any missing count, while one inside the history stays history.

A resumed session reads the export once when it starts. Every message it holds becomes history by identity and never counts as this run's spend, whatever its creation time. A new session starts with no history, and so does a resumed session whose export did not confirm it, because that session starts new.

A turn that hands work to a subagent runs the work in a child session. The stream shows one `tool_use` for the call, with `state.input.agent` set and the child's session id at `state.metadata.metadata.sessionID`, and no event of the child. The parent's export holds none of the child's messages. The adapter exports every named child session once at the end of the turn, and each child message counts once in the turn that ran it. The turn is complete only when every subagent call completed, named its session, and had its export read. Otherwise the turn is a lower bound, and the children that did read still count. A turn short of a child's records leaves no later turn covering the session, because the session's own records hold no child request.

A turn covers the session only when its figure is the complete total of the session's records. A turn settled on step reports never covers it, and neither does any turn after one that left a child session's records uncounted.

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
- The scripted-model case isolates `HOME` and the XDG directories, so a version-manager shim that reads `HOME` fails to start. Name the binary by an absolute path.
- OpenCode 2.x offers MCP tools only inside code mode, and it offers a server's tools only from the request after the tool listing. A scripted endpoint has to script an `execute` call and wait for the listing before it answers.

## Entry points

Two routes drive this runtime. The `opencode` kind, described in the sections above, launches `opencode run` once per turn. The generic `agent-client-protocol` kind drives `opencode acp` and keeps one process open for the session. This page records how the protocol route meets the generic adapter and how it compares with the native stream. The sample workflow for the protocol route is `examples/WORKFLOW.agent-client-protocol.opencode.md`.

The `acp` subcommand is the only protocol entry point, and it takes no model switch; the protocol observations below say where the model comes from. The route carries session continuation and Sortie's own tool server over HTTP, and its spend comes from the runtime's session export.

The native entry point is `opencode run --standalone --format json`, with `--session` to resume. `--auto` approves permission requests, and the asking posture leaves it out. On both surfaces the asking posture also adds an ask-every-tool `permission` rule to the configuration document, because without it the runtime asks about none of the probe's tool calls.

A stored provider login authenticates a run. Every grade on this page was measured under a stored provider credential.

## Route parity

| Axis | `opencode` kind | Protocol route (`agent-client-protocol` running `opencode acp`) | Verdict |
| --- | --- | --- | --- |
| Spend and the token and money ceilings, local launch | Each `step_finish` report's counts as the live figure, settled per served model from the session export at every turn end. The title agent is switched off in every launch's document | The prompt result's usage as the live figure when the runtime reports one, settled from the same session export through the same parser at every turn end. The title switch is merged into the operator's document. A live run stopped at `agent.max_tokens` | Parity |
| Spend and the token and money ceilings, launch over SSH | The session export runs on the worker host through the session's SSH launch, so a remote run is measured as a local one is | The same export runs on the worker host through the same host-command path, with the merged document in the launch preamble. The scripted SSH runs settle the figures the local runs settle | Parity |
| The served model a run is priced at | The session export names the provider and model of each request | The same export, read by the same parser, names the provider and model of each request | Parity |
| Provenance and amounts | US dollars per request from the session export, with the settling source and strategy, through the usage component | The same amounts from the same records, through the same usage component | Parity |
| Provider usage windows | Pulled after a turn with `opencode api --standalone experimental.usage.limits`, on the worker host for a launch over SSH | The same pull through the same limits package, on the worker host for a launch over SSH. On the release the qualification ran on, that call does not exist, so neither route reported a window there | Parity |
| Usage-limit refusal | An error naming `provider.free-limit`, `provider.quota`, `provider.budget-limit` or `provider.model-budget-limit` ends the turn `usage_limit`, with the retry delay the stream carries | A prompt error whose `data.errorName` is one of the same names ends the turn `usage_limit`. The protocol error drops the retry delay, and the turn-end records read returns it from the session export | Parity |
| Tool-server delivery | Sortie's server in the `mcp` member of the launch's configuration document | Sortie's server over HTTP in `session/new` (graded usable), and through a reverse forward on a launch over SSH | Parity |
| Session continuation | `--session` once the session export confirms the session; the recall answer comes back (graded usable) | `session/load` by identifier from a fresh process; the recall answer comes back (graded usable) | Parity |
| Turn outcome, retry classification, permission handling | Gaps on the native stream: a cancellation leaves no outcome, and the stream has no terminal for a question to a person | Graded usable; eligibility qualified, product conformance qualified | The protocol route meets the richest native reference |
| Credential verification before the first session | Shared verification step, through a document that denies every tool; the verification session is deleted afterwards. A refused key ends `credential_unverified` in the kind's gated live suite | Shared verification step, with the denial merged last into the operator's document and the session deleted through `session/delete`. Every grade was measured under a stored provider login; no graded run has used a key from the environment, a refused one, or a host with no stored login | Unmeasured: a refused or absent credential on this route |
| Configured credentials on a remote launch | The kind declares no credential variable, so a provider key reaches an SSH worker only when `worker.ssh_pass_env` names it; the configuration document travels in the launch preamble | The same: `worker.ssh_pass_env` names what reaches the worker, and the `agent-client-protocol.env` entries, the document included, travel in the launch preamble | Parity |
| What `sortie validate` refuses offline | Wrong-typed `opencode` keys, `effort` and `variant` set together, and a tool named in both `allowed_tools` and `denied_tools` | Its block keys' types, `effort` included, and the `permissions` value; a `variant` key draws a warning. The kind's refusals guard settings this route does not have | Parity: no refusal guards a failure the protocol route can reach |
| `model` | `--model <model>`, with `#<level>` appended when a level is set; the configured model scopes the usage-limit hold | The document's `model` member, measured to take effect, with `agent-client-protocol.model` set to the same id for the hold. An id the runtime cannot resolve runs its default model on both routes | Parity at the cost of a configuration edit: the document's `model` member and `agent-client-protocol.model` |
| `agent` | `--agent <name>` | The document's `default_agent` member; the session reported the named agent as current | Parity: a configuration member, measured to select the agent. The agent's own guards were measured on neither route |
| `effort` | `#<level>` on `--model` on every turn, credential verification included. Refused without a plain `model`; a level the model lacks fails the turn before any model request | `agent-client-protocol.effort`, set on the session's `effort` option when each session opens; the `#` suffix of the document's `model` is not read. A level the model does not offer, or a model with no levels, runs at the current level with a warning in Sortie's log | Parity at the cost of a configuration edit for a level the model offers. Where the native kind fails the turn on a level the model lacks, this route warns and runs, an accepted difference because the level never fails a session on this route |
| `variant` | The same `#<level>` slot as `effort` | The same session option, written as `agent-client-protocol.effort`; a `variant` key in the block has no effect and draws `agent-client-protocol.variant.not_read` | Parity at the cost of a configuration edit: the same option written as `agent-client-protocol.effort` |
| `thinking` | `--thinking`; each reasoning part becomes an empty notification | Nothing to set: the runtime streams reasoning as `agent_thought_chunk` whatever any setting says, and the kind turns each chunk into an empty notification | Parity, with nothing to set |
| `dangerously_skip_permissions` | `true` adds `--dangerously-skip-permissions`; `false` writes a `*` deny base rule ahead of the lists' rules | `true`: `agent-client-protocol.permissions: approve-all`. `false`: the same `"*":"deny"` base rule in the document's `permission` member, measured to keep the shell tool from running | Parity at the cost of a configuration edit |
| `disable_autocompact` | `compaction.auto` `false` in the launch's document | The document's `compaction` member with `auto` `false`, which the runtime merges after every configuration file | Parity: the same member of the same document; its effect was measured on neither route |
| `allowed_tools` | A closed `permission` policy: listed keys allow, known keys not listed deny, unknown keys are forwarded | The same `permission` member, written in the document. The deny direction was measured; the allow direction is the runtime's own rule grammar | Parity at the cost of a configuration edit |
| `denied_tools` | `deny` rules in the same policy | `deny` entries in the same `permission` member | Parity at the cost of a configuration edit |
| Default permission posture | `dangerously_skip_permissions` defaults to `true`, so the runtime approves each request that asks, once, and never reaches a deny | `agent-client-protocol.permissions: approve-all` approves each such request once. With `permissions` unset, a request the runtime asks about is refused and the turn fails | Parity at the cost of one configuration line |
| Version check | A version the kind cannot read, and any major other than 2, are refused before launch | The route drives `acp`, never `run`, and a command that does not answer `initialize` fails before the first turn. A session export or limits output that does not parse drops that read, and the turn is unaffected | Parity for a runtime that answers the handshake; a launch with no version refusal, and a read that does not parse leaving usage or limits unreported, are accepted differences |
| Title suppression | The title switch in every launch's document | Merged into the operator's document. An operator's own switch wins and marks the session's figures as lower bounds | Parity, with nothing to set |
| Session sharing | `share` `disabled` in every launch's document | The document's `share` member, which the operator writes; Sortie adds none | Parity at the cost of one configuration line |
| Auto-update | `OPENCODE_DISABLE_AUTOUPDATE=true` in the launch environment | The `agent-client-protocol.env` entry `OPENCODE_DISABLE_AUTOUPDATE: "true"` | Parity at the cost of one configuration line |
| Accepted differences | None | Runs after the move are recorded under the kind `agent-client-protocol`, which every protocol runtime's runs carry, so the `opencode` series in run history and `sortie stats` ends at the move. The by-model table continues, because both kinds record the provider and model the session export names | Accepted differences, not withdrawn capabilities |

The `opencode` kind is deprecated in favor of this route. The withdrawn rows are what an operator gives up by moving, and the operator reference states them. The kind's removal waits until no row withdraws a capability an operator holds and no row is unmeasured.

The key rows rest on the launches in the next section. The eligibility row rests on the tracked capture and the verdict lines above. The spend, refusal and delivery rows rest on the gated suite's scripted runs, locally and over SSH, which run every night and gate every release. The window row rests on the pull code both routes share.

The credential row lacks a measurement for these cases: no graded run on this route has used a provider key from the environment, a refused one, or a host with no stored provider login. The gated suite's credential-verification case, run live in the nightly workflow with a key on a host with no stored login, measures the first two. No run measures the last.

## Where the `opencode` settings go on the protocol route

Every launch behind this table ran the installed OpenCode locally on Linux under the host's stored provider login, in a fresh `git init` workspace, with no global or project OpenCode configuration, on `google/gemini-3.5-flash` whenever a prompt ran. Each went through the `agent-client-protocol` kind with `opencode acp` as the command, OpenCode's configuration document in `agent-client-protocol.env` as `OPENCODE_CONFIG_CONTENT`, and `agent-client-protocol.permissions: approve-all` wherever a converted configuration sets it. Sortie merges its session-title switch into that document and, on the credential check, its tool denial, keeping every member. A test with no build tag launches through the kind with `agent-client-protocol.env` and a stand-in program, never an `env` program, on Linux, macOS and the Windows CI job; it shows the variables arriving at the launched program on each platform, the title switch merged, and the Windows letter-case rule, and no live OpenCode run on Windows backs this table. An SSH worker launch, with the command as a list, received the same variables.

| Setting | `opencode` kind | Protocol route | Converted configuration | Launch and observation |
| --- | --- | --- | --- | --- |
| `model` | `--model <model>`, with `#<level>` appended when a level is set; the usage-limit hold scopes the configured model. | A configuration member, `model` in the document, and a kind setting, `agent-client-protocol.model`, for the hold. | carries it as the document's `model` member, with `agent-client-protocol.model` set to the same id for the hold. | Command `opencode acp`, document `{"model":"<id>"}`, no prompt: the session reported the id as its current model, where a launch with no document reported the runtime's default model. Every prompt launch's session export named only that model. An id the runtime cannot resolve runs its default model on both routes. |
| `agent` | `--agent <name>`. | A configuration member, `default_agent`. | carries it as the document's `default_agent` member. | Command `opencode acp`, document `{"default_agent":"plan"}`, no prompt: the session offered the modes build and plan and reported plan as current, where a launch with no document reported build. A prompt asking the plan agent to write a file was run again after the first miss; the replies were text with no `edit` or `write` call, so the plan agent's write guard was not exercised on this route, and the native kind was not run for this row. |
| `effort` | `#<level>` on `--model`; refused without a plain `model`; a level the model lacks fails the turn before any model request. | A session option, `effort`, that the kind sets from `agent-client-protocol.effort` when each session opens; the `#` suffix of the document's `model` is not read. | carries a level the model offers as `agent-client-protocol.effort`. A level the model does not offer, or a model with no levels, runs at the current level with a warning in Sortie's log, where the native kind fails the turn; an accepted difference, because the level never fails a session on this route. | Document `{"model":"google/gemini-3.5-flash#minimal"}`, no prompt: the session offered the levels minimal, low, medium, high and default and reported default as current; a repeat gave the same result. A document naming a level the model lacks raised no error and the turn replied, so the native refusal before any model request has no carrier either. Gated case through the kind with `agent-client-protocol.effort: high`, one turn: the `session/new` result listed a `select` option with id `effort`, category `thought_level` and `currentValue` `default`; the client sent exactly one `session/set_config_option`, before the first `session/prompt`, with `configId` `effort` and `value` `high`; its result listed the option with `currentValue` `high`; the turn ended with `end_turn`. One `config_option_update` followed the prompt response, and its value was not recorded. |
| `variant` | The same `#<level>` slot as `effort`. | The same session option and the same unread suffix as `effort`. | carries it as the same option, written as `agent-client-protocol.effort`; a `variant` key in the block has no effect and draws `agent-client-protocol.variant.not_read`. | The observation under `effort` is the same slot. The gated case set the option through `agent-client-protocol.effort`; a `variant` key was not run. |
| `thinking` | `--thinking`; each reasoning part becomes an empty notification, `ObserveAssistantOutput` and `MarkModelReached` (`internal/agent/opencode/opencode.go`). | The kind itself: the runtime streams reasoning as `agent_thought_chunk` whatever any setting says, and the kind turns each chunk into an empty `domain.EventOtherMessage` plus `MarkModelReached`. | carries it for `true` and for `false`, with nothing to set. | Deciding symbols, not a launch: the captured event streams hold no raw frame, so none shows a reasoning chunk. Reasoning text reaches the operator on neither route. |
| `dangerously_skip_permissions` | `true` adds `--dangerously-skip-permissions`; `false` writes a `*` deny base rule ahead of the lists' rules (`buildInlineConfig`). | `true`: a kind setting, `agent-client-protocol.permissions`. `false`: a configuration member, `permission`. | carries it. For `true` as `agent-client-protocol.permissions: approve-all`, as the default permission posture row shows. For `false` as the same base rule `"*":"deny"` in the document's `permission` member, with `permissions` left at its default. | Document `{"model":"<id>","permission":{"*":"deny"}}`, `permissions` unset, prompt to run `pwd` and reply with a fixed token: no permission request and no notice. The model's attempts at tool names not offered errored, and the real shell tool never ran. |
| `disable_autocompact` | `compaction.auto` false in the inline document (`marshalInlineConfig`). | A configuration member, `compaction`. | carries it as the document's `compaction` member with `auto` false. | No launch. The runtime loads the document after every configuration file and merges `compaction` per field in document order, so the member wins over a file. |
| `allowed_tools` | The closed `permission` policy (`buildPermissionPolicy`); the credential check denies every tool instead. | A configuration member, `permission`. | carries it as the same `permission` member in the document; the credential check is its own row. | Document with a `*` deny rule, prompt to run `pwd`: the shell tool was not offered and never ran, with no permission request. The deny direction was observed; the allow direction is the runtime's own rule grammar. |
| `denied_tools` | Explicit `deny` rules in the same policy. | A configuration member, `permission`. | carries it as `deny` entries in the same `permission` member. | The observation under `allowed_tools` is the same member. |
| Default permission posture | `dangerously_skip_permissions` defaults to `true`, so the runtime replies `once` to each request that asks and never reaches a deny. | A kind setting, `agent-client-protocol.permissions`. | carries it as `agent-client-protocol.permissions: approve-all`. The kind answers a request that offers exactly one once-only option with its `allow_once`, where the native kind's runtime replies `once`. An ask or deny rule from any configuration the runtime loads behaves alike on both routes. The runtime cancels a question the model puts to a person on both routes, and the model continues. | A read of `.env`, which the runtime asks about by default, completed under `approve-all` with no refusal notice. With `permissions` unset the same read ended refused: the notice said the run is unattended and no one can approve it, the tool result was `The user declined this tool call`, the runtime answered the prompt at once with the `cancelled` stop reason without giving the model another step, and the turn failed as `turn_failed`. A `pwd` call under a `*` ask rule ended the same way with `permissions` unset and completed under `approve-all`. The question cancellation is stated from the runtime's source, since no launch put a question to the model. The plan agent never reached a guarded `edit` or `write` call, so that case is not observed. |
| Version check | `checkRuntimeVersion`: major 2 only, so the kind drives only the `run` surface of that major and refuses another package's `opencode` command. | The kind itself: the route drives `acp`, never `run`, and a command that does not answer `initialize` fails before the first turn. | carries it, with nothing to set. | The handshake named OpenCode and its version. From `opencodeReadRecords`: a session export of another major that does not parse makes the read fail, the failure is logged at debug level, and its records are not used. From `opencodePullWindows`: limits output that does not parse returns the unsupported error, which drops that turn's windows and ends the session's pulls. The turn itself is unaffected in both cases. |
| Credential verification | The deny-all document (`buildVerificationInlineConfig`), carried over SSH too; the session deleted (`deleteVerificationSession`). | A configuration member, `permission`, merged by the kind into the operator's document, and `session/delete`. | carries it, through the merged denial locally and over SSH, and through `session/delete`. | With the credential check's launch params the runtime advertised `delete` and the session was deleted through the protocol; `opencode session delete` afterwards answered `Session not found`. A document `{"model":"<id>","permission":{"bash":"allow"}}` launched as `{"agent":{"title":{"disable":true}},"model":"<id>","permission":{"bash":"allow","*":"deny"}}`, so the denial came last and every member stayed. Its `pwd` prompt ran no shell call where the ask-everything launch ran one, the turn named the document's model, and the export exited 1 with `Session not found`. A test with a stand-in `ssh` shows the same merged denial in the preamble of a remote launch and no `env` value in its arguments. |
| Run history and `sortie stats` | Runs keyed `opencode`. | The kind itself: runs keyed `agent-client-protocol`. | no behavior-preserving carrier: `by_adapter` keys on the kind, so the `opencode` series ends at the move and the `agent-client-protocol` series mixes every runtime the kind drives. Each kind records the reported model from the running entry's `ModelName` (`internal/orchestrator/exit.go`), which `modelOf` in `internal/runstats` prefers over the configured one, and both take the string from the same session export parser, `provider/model`, so the by-model series continues. | No launch. `run_history.agent_adapter`, `ByAdapter` and `modelOf` decide; the protocol route's token usage event named `google/gemini-3.5-flash`, the provider and model of the export. |
| Title suppression | `agent.title.disable` true in every document (`titleAgentDisabled`). | A configuration member, merged by the kind. | carries it through the merge; an operator's own switch wins and marks the session's figures as lower bounds. | A document with no title member launched with `agent.title.disable` true and the session kept the default title. A document defining `agents.title` launched with `agents.title.disabled` true and kept the default title. An operator's own `agent.title.disable` false stayed as written, and the session got a generated title. |
| Session sharing | `share` `disabled` in every document (`marshalInlineConfig`). | A configuration member, `share`. | carries it as the document's `share` member, which the operator writes: Sortie's own additions to the document do not include it. | No launch. The last document defining `share` wins. |
| Auto-update | `OPENCODE_DISABLE_AUTOUPDATE=true` (`buildManagedEnv`). | An `env` entry. | carries it as the `agent-client-protocol.env` entry `OPENCODE_DISABLE_AUTOUPDATE: "true"`. | The untagged launch test shows the configured variables arriving at the launched program on each platform. The preamble of a launch on an SSH worker, with the command as a list, held `OPENCODE_DISABLE_AUTOUPDATE='true'` beside the merged document. |

## Load-bearing capability observations

Session continuation is usable on both surfaces. Permission handling and the runtime failure case are observed on the protocol surface, and the runtime failure case is observed on the native stream. Token accounting is a gap on both surfaces, so it does not separate the routes.

No prompt response in the capture carried a token figure, although Sortie reads one from the wire when it is present. Sortie took the figure from the runtime's session export, the same source the native kind settles from. The ceiling stop worked: a live run through Sortie's orchestrator stopped at the figure ("stopped at 53750 token(s) ... token budget hold"). The grade stays a gap because it describes the wire, the way the Gemini notes record theirs.

The native stream carries no token figure on its terminal either: `opencode run --format json` has no terminal envelope, and a `step_finish` event closes each step. The success turn is recognized, and so is the runtime failure case: the run emits an `error` event, which the recognizer reads as the error terminal. The limit case passes too: the recognized terminal matched the induced case. Turn disposition on this surface is still a gap because cancellation is surface-silent: the runtime writes its terminal only at exit.

Retry classification on the native stream is a gap because the run stream has no human-input terminal. `opencode run` cannot ask a person: it rejects a permission request itself, with feedback telling the model to continue without the action, and prints a notice on standard error. The tool part fails, the turn goes on, and the run exits 0. The case was induced and the surface reported no outcome, so it counts against the surface. On the protocol surface the human-input case is not applicable: the request offered a refusing option and Sortie refused it inside the protocol, so the turn went on and no human-input outcome arose.

Cancellation passed on the protocol surface, where the turn ended in cancellation after its own context was cancelled. On the native stream the run has no cancellation event. An interrupt or termination signal makes it write one `error` event that reports the lost connection to its own server, with type `unknown`, and exit with status 130. Only the exit status marks the cancellation, so the case stays surface-silent: it was induced and the surface reported no outcome. The retryable failure case passed on both surfaces: after one SIGKILL to its group, the launch reported transport loss.

Two cases are declared, not measured, on both surfaces: a turn ending in `runtime_refusal` and a retry classified as `non_retryable_refusal`. A request built to trigger one completed normally, so the case is recorded as `outcome_never_produced`. The runtime failure case is not declared. It is induced through a provider the configuration document declares, pointed at a local stand-in upstream that answers HTTP 400. The protocol turn ends with a protocol-level error response, and the native run emits an `error` event that the recognizer reads as the error terminal; both pass. A refused model id cannot induce the case, because an unresolvable model id is replaced by the default model and the turn completes. The `unknown_outcome` case has no deterministic inducer on either surface, so it carries no obligation. The protocol `limit_reached` case is declared `outcome_folded_into_another`, because the runtime never ends a protocol prompt with a limit stop reason; the next section describes why.

Permission handling is usable on the protocol surface. Under the asking posture the runtime raises a permission request for the declared tool server's tool, which the model calls from inside the runtime's `execute` code tool. Sortie refuses it with the reject option, and the tool does not run: the `execute` result reports `Permission.DeclinedError` and the policy marker stays absent. The refusal reaches the model as that tool's result, so the model carries on and the turn succeeds. The policy precondition passes for the same reason: the refusal was answered and the marker stayed absent. A refused call the model makes directly, not through `execute`, ends the turn instead: the runtime answers the prompt with the `cancelled` stop reason and Sortie records `turn_failed`, as the default permission posture row shows.

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

Gaps on the protocol route outside the capture:

- The documentation site has no page for the protocol route of this runtime. The sample workflow points at the generic reference page.

## Protocol-specific observations

The handshake reports the agent's own name, `OpenCode`, and its version, which the gated suite's identity rule reads. It advertises `loadSession` true, `mcpCapabilities.http` true with `sse` false, and `sessionCapabilities` carrying `close`, `delete`, `fork`, `list`, and `resume`. `authMethods` names one login method, which points at `opencode auth login`.

Tool servers declared in `session/new` reach OpenCode over HTTP.

`opencode acp` takes no model switch. The model comes from OpenCode's configuration, and the qualification passes it through `OPENCODE_CONFIG_CONTENT` as a document with a `model` key holding the model id. An id OpenCode cannot resolve does not fail: the runtime silently selects its default model instead, on this surface and on `run`. That is why a refused model id cannot induce a runtime failure. It also means a mistyped model id runs the turn on a model nobody chose, with nothing in the output marking it.

The title agent makes a billed request that no exported message records. With the model pinned in the environment of the Sortie process, the money run served exactly one model and no request attributed to a title agent. The credential-verification session was deleted through the protocol before it could be re-read, so that check covers the run's session only.

On the release the qualification ran on, the usage-limits pull fails: the `experimental.usage.limits` call does not exist on that release, and the session logs the pull as unsupported. The turn is unaffected. The usage limits section above describes the command on releases that have it.

Session continuation is proven only by the recall answer: a successful load alone does not show that the history came back. On the native stream, resuming through `--session` returned the phrase the seed asked the runtime to remember. On the protocol surface the recall turn runs under the seed's own session identifier and returns the phrase too. A recall prompt that asks for "the phrase from earlier" lets a small model answer with its own earlier reply, `STORED`, which reads as lost history; the profile asks which phrase it was told to remember.

## Output-token limit

When a provider cuts a reply at its output-token limit (finish reason `length`), the runtime's core does not end the turn. It inserts a hidden continue message and runs the step again. If a later reply finishes normally, the turn ends normally. After the third consecutive truncated reply the runtime fails the turn with an `output-limit` error, "Response still truncated after two output token limit continuations". A step limit configured for the agent ends the turn normally. There is no request-count stop. Both surfaces share this behavior.

On the protocol, `opencode acp` therefore never ends a prompt with `max_tokens` or `max_turn_requests`. A truncated reply that the continuation completes ends with `end_turn`, and three truncated replies end with a JSON-RPC internal error that names `output-limit`. The qualification confirms the declaration with a turn whose first reply comes from a local stand-in upstream and stops at the output-token limit; the turn then completes normally.

On the native stream, the run writes a `step_finish` with reason `length` for each truncated reply, then an `error` line whose `error.type` is `output-limit`. That line can precede the last step's lines. The recognizer reads it as the limit terminal: a limit locator keyed on the nested `error.type` value is consulted before the error locator, so the line is not read as a runtime failure. The qualification induces the case offline, through the same declared provider pointed at the local stand-in, which answers with truncated replies.

## Native headless observations

`opencode run --standalone --format json` prints JSON Lines on stdout, one event per line: `step_start`, `tool_use`, `step_finish`, and `text`. Current releases also close the final text step with a `step_finish` whose `reason` is `stop`; earlier 2.x releases usually drop that one. A tool step's `step_finish` carries every token counter, zeros included, and its `messageID` names an export message with identical tokens. A subagent call prints one `tool_use` whose `tool` is `subagent`; the child's events never reach the stream. There is no run-started envelope and no final result envelope. The stream ends with the final `text` event, that step's `step_finish`, or nothing, so the profile's recognizer keys on the first `text` event for a completed turn, on an `error` line whose nested `error.type` is `output-limit` for a limit stop, and on any other `error` event for a failed one. A turn that ends with neither has no terminal.

`--standalone` is required. Without it, `opencode run` attaches to the host's shared background service, which keeps the environment of the client that started it first. It then ignores the per-launch model variable and serves its own default model on every launch.

A cancelled turn leaves an `error` event and exit status 130, never a cancellation terminal. A failed run, such as a provider answering HTTP 400, ends in an `error` event too. The stream has no terminal for a request addressed to a person.

The profile treats the plain-JSON surface as absent. The output-format flag accepts a default text format and JSON Lines, and no structured single-document format. A declared absence is checked by running the ordinary headless invocation and finding no structured terminal; a launch that fails to start would prove nothing.

## Workspace trust and process boundary

The containment boundary held under measurement: every launch ran inside the run-scoped root, no project settings applied, and every process-group member observed was the launched command or its descendant. The profile treats `opencode.json`, `.opencode`, and `AGENTS.md` in a workspace as project settings.

## Excluded capability cases

- retry_classification human_input: induced, and the surface reported no outcome (terminal_vocabulary_closed), so the case keeps its obligation
- retry_classification human_input: not applicable on protocol: the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose
- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer on any runtime, so the case carries no obligation
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: declared outcome_folded_into_another
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

Windows live qualification is unobserved.
