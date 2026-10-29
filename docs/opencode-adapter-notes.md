# OpenCode adapter notes

Working notes for anyone changing Sortie's OpenCode adapter in `internal/agent/opencode`.

Last updated: 2026-10-29

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
- `SettleUsage` runs the session export and hands the figure to the turn-end report. The skeleton calls it on every ending of a started turn except the mismatch, so a cancelled turn, a read failure, and a missed deadline recover usage like a clean exit does.
- `OnFinalize` raises the permission notices from standard error, settles the spend on a clean exit, and builds the turn evidence, including the free-tier refusal clause.
- `GetUsage` and `GetSessionID` read the session state. `EmitSessionStartID` is nil because `ParseLine` emits `session_started` itself on the first envelope that carries an ID.

`StopSession` calls the skeleton's stop, then deletes the verification session when the session is a credential verification.

The verification request carries a deny-all permission policy, and on 2.2.0 it still declares the `skill` tool: the runtime's browser plugin appends a `skill` rule after every configuration source, so no configuration removes the definition. The deny-all rules leave the tool unusable, and the adapter declares that the verification request keeps some tools.

The first-event deadline is the larger of the read timeout and `agentcore.CredentialExchangeBound`. OpenCode prints nothing until the provider starts answering, and the free hosted model can take much longer than the 5-second default read timeout. The deadline stops at the first envelope, so it catches a process that never talks, not a stall in the middle of a turn. Stalls belong to the orchestrator.

The export runs after the reap, on a context that outlives the turn's cancellation, and bounds itself with `agentcore.AuxiliaryTimeout`. A slow export delays the turn's result, never the stop.

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

The kind attaches each `step_finish` part's `tokens` as its live source and the session export as its records source, and derives `turn_end` arrival and `per_model` attribution from them. After the subprocess exits, `queryExportUsage` runs a sanitized session export and reports the figure through the shared turn-end report, with the model of the last kept message. The export also runs over SSH, so remote launches are measured too, unlike `copilot-cli`. The export is sanitized so tool output never reaches a log. The figure also carries a per-model split, each kept message counting under its provider and model.

The export's invocation and parsing live in `internal/agent/opencode/sessionexport`, shared with the OpenCode declaration on the `agent-client-protocol` kind, so both routes read an export the same way and refuse the same ones: another session's document, and one above 64 MiB. The usage component normalizes the counts, adding cache to input and reasoning to output. A step report omits a counter that is zero, so a missing `reasoning`, `cache.read`, or `cache.write` in a step report reads as zero, while a step report missing its `input` or `output` count adds nothing. An export message carries every count, so a kept message missing any count is unknown, never zero, and makes the export recover nothing. So does a finished message without tokens inside the run, an aborted request whose spend is recorded nowhere, while one from before the run stays history like any other.

The adapter sums the step reports of a turn as they arrive, and the export settles every turn. A figure the export recovers replaces the step counts, and the two are never added. A turn whose export recovers nothing keeps the counts its step reports carried, as a lower bound, and carries unaccounted spend. The final text step of a turn emits no `step_finish` tokens, so that sum falls short of the turn's spend; step reports that add no input and no output tokens leave it with no figure.

The adapter sums per-message figures and ignores the export's session aggregate, for two reasons. The aggregate covers the whole session, including earlier runs, while Sortie reports per run. And its total also includes cache and reasoning tokens, so it is not input plus output. The adapter selects assistant messages by session ID and, on a resumed session, by creation time at or after the run start.

A message counts only once its `finish` field is set. The runtime saves an assistant message with zero tokens before calling the model, so a turn killed mid-step exports a placeholder, not a measurement.

The configuration document disables the runtime's title agent. Its request is billed but recorded in no message, so the sum would miss it.

A failed or empty export must never lower a figure the run has already reported. When the export recovers nothing, the settle step reports the turn's step counts instead of a zero, and a turn without step counts reports nothing.

## Usage limits

The run stream carries no subscription window. Two other places do.

The pull runs `opencode api experimental.usage.limits` once a turn that reached the runtime has ended, through the same auxiliary launch the export uses, locally or over the session's SSH launch. It runs on empty stdin. A cancelled turn and a credential-verification session do not pull. The command exists only on releases that ship the usage-limits subsystem. On an older build it fails or hangs until the bound, and a command that fails to start, exits non-zero, outlives the bound, or prints no object ends the session's pulls. Do not gate the pull on a version check.

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

The command goes through the runtime's background service, not through an in-process server. That is the service every launch avoids with `--standalone`, so this is the one invocation that uses it, and it starts the service when none is running. Two consequences follow. The service keeps the environment of whichever client started it first and outlives Sortie, so a service an operator already runs answers for its own state. And a service from another release already holding the default port makes the command collide with it until the port is changed with `opencode service set port`. A plain `opencode serve` route returns 401 without the service credential that `api` supplies, so do not call the HTTP route directly.

The free hosted model reports its limit in the stream instead. When the limit is hit the run prints an error envelope with `type` set to `provider.free-limit`, status 429, and `retryAfterMs`:

```json
{"type":"error","error":{"type":"provider.free-limit","status":429,"retryAfterMs":3600000}}
```

`retryAfterMs` is milliseconds, relative to the moment of the failure. It derives from the response's `retry-after-ms` or `retry-after` header, which is why a header of 3600 seconds arrives as 3600000. The adapter turns it into one exhausted window that resets that long after the observation. The envelope is also the free-tier refusal described under failure detection, and reading the delay changes nothing about how that failure is reported. Other quota errors, such as `provider.budget-limit` and `provider.quota`, carry no usable window. The same run over the protocol surface drops the delay, which is why only the native kind reads it.

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
