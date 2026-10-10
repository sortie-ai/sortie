# OpenCode adapter notes

Working notes for anyone changing Sortie's OpenCode adapter in `internal/agent/opencode`.

Last updated: 2026-10-10

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

## Why this adapter owns its own loop

Every other fork-per-turn adapter uses `agentcore.ForkPerTurnSession`. This one starts the process itself and runs its own reader, wait goroutine, and select loop. It needs two things the skeleton does not offer: a deadline on the first JSON event, and a query against the same binary after exit to recover usage.

This is a maintenance hazard. Fixes to the shared skeleton do not reach this adapter, so where the two should behave alike, keep them alike by hand. The terminal decision is still shared through `agentcore.FinalizeTurn`.

The first-JSON deadline is the larger of the read timeout and `agentcore.CredentialExchangeBound`. OpenCode prints nothing until the provider starts answering, and the free hosted model can take much longer than the 5-second default read timeout. The deadline stops at the first envelope, so it catches a process that never talks, not a stall in the middle of a turn. Stalls belong to the orchestrator.

The adapter owns both pipe read ends, so the reap does not depend on the stdout reader. The wait goroutine reaps first and only then waits, bounded, for stderr. A descendant that holds stderr open cannot delay the reap. It can delay the published turn result, and if the bound fires, the permission-refusal warning is lost. For that reason nothing that must react to process exit reads the result channel; the stdout reader's own bound starts from the reap. The early-return paths also bound their drain of the stdout reader, so a descendant holding only stdout cannot block them.

## Session identity and the dir-scoped resume

A session ID is server-generated and learned from the first envelope that carries one. The adapter adopts it, and a later envelope carrying a different ID is treated as a fault that fails the turn rather than a thing to reconcile.

Resume is scoped to the directory the session was created in. Resuming a valid session ID from another directory does not error: the process exits zero and prints nothing. Sortie then reports an early exit with status 0 and empty stderr, which looks like a runtime that never answered, not like a resume that did not match.

The adapter cannot hit this, because every turn runs in the issue workspace. Tests can: a resume test whose second turn runs in a fresh temporary directory never exercises resume. Run both turns in the same workspace.

Sessions are persisted in one global database under the user's home directory, not in a per-project store inside the workspace. Isolation between concurrent issues comes from distinct server-generated IDs, not from separate storage, and removing a workspace reclaims none of it.

## Permissions

OpenCode's permission control is config-driven. Each key resolves to allow, ask, or deny through an ordered rule list; the last matching rule wins, and the default is ask.

The adapter sends its policy in the inline configuration document. OpenCode deep-merges that document into the rules from every other configuration source; it does not replace them. So a value inherited from the operator's shell would leak into the result. The adapter removes every variable it manages from the inherited environment before adding its own. Keep that scrub in step with the managed set: a new managed variable that is not scrubbed is a leak.

The emitted policy is closed. When the workflow names allowed tools, every other permission key the adapter knows is denied explicitly. That known-key set is a policy input, not a validation list. The runtime accepts more keys than its docs list, so a key the workflow names is forwarded as written even when the adapter does not know it. Rejecting unknown keys would break configurations OpenCode accepts.

Permission prompting is bypassed by default, because otherwise the runtime refuses every permissioned tool call. Validation warns when a workflow turns that off, and rejects an overlap between the allowed and denied lists.

A refusal appears on stderr as a warning line styled with escape sequences; stdout stays clean JSON. The adapter strips the escape sequences before matching the prefix. Skip that and the match never hits real output. The notice is read from collected stderr after exit and goes through `agentcore.DecideHumanRequest`.

The hosted free-tier model refuses any request whose tool set lacks its shell tool or its read tool. That refusal arrives as a run-level error, not as the permission notice. When the session's own tool policy denied one of those tools, the adapter appends a clause naming it to the vendor's text. This match depends on the vendor's wording. If upstream rewords it, re-capture the fixtures; do not widen the text match.

## Usage accounting

Step-scoped token counts on the stream are not a running total. Between the tool step and the final text step of one turn the numbers move in both directions, so summing them across steps is wrong and taking the last one is wrong too.

The kind declares `turn_end` arrival and `per_model` attribution. After the subprocess exits, `queryExportUsage` runs a sanitized session export and reports the figure through the shared turn-end report, with the model of the last kept message. The export also runs over SSH, so remote launches are measured too, unlike `copilot-cli`. The export is sanitized so tool output never reaches a log.

The adapter sums per-message figures and ignores the export's session aggregate, for two reasons. The aggregate covers the whole session, including earlier runs, while Sortie reports per run. And its total also includes cache and reasoning tokens, so it is not input plus output. The adapter selects assistant messages by session ID and, on a resumed session, by creation time at or after the run start.

A message counts only once its `finish` field is set. The runtime saves an assistant message with zero tokens before calling the model, so a turn killed mid-step exports a placeholder, not a measurement.

The configuration document disables the runtime's title agent. Its request is billed but recorded in no message, so the sum would miss it.

A failed or empty export must never lower a figure the run has already reported. The finalize path skips the update entirely rather than replacing a settled value with zero.

## Failure detection

An error envelope on stdout is the authoritative failure signal. The process exit code is not load-bearing and has changed meaning across releases; do not key anything on it.

The error envelope carries `type`, `message`, and a numeric `status` when present as flat members.

One failure can arrive as two error envelopes: the actionable diagnostic the session publishes, and a generic placeholder the run command reports when the underlying fault was not in its API error schema. Their order on the stream is not guaranteed, so the adapter keeps whichever envelope carries detail rather than whichever arrives last. When only the placeholder was seen, it reaches the operator as it is, so a report of the generic message is a signal to reproduce, not a bug in the parse.

Work evidence for the turn is this turn's own parsed assistant parts, text, reasoning, or tool use, never the export figure, which is non-zero on any turn after the first. An exit-zero run that parsed envelopes but produced no assistant output takes the shared zero-work row and is reported as a failure rather than a silent success.

## Sortie's own tools

`StartSession` translates the worker-generated MCP configuration into OpenCode's own server entries under the `mcp` key of the inline configuration document. The runtime rejects the standard `mcpServers` key, so the generated file cannot be handed over as is. The entries add to whatever the operator's own configuration declares.

The document is set only in the turn's own environment (local) or on the SSH carrier (remote), never in the shared managed-environment builder. So the export and delete helper invocations never carry it and never start a tool server of their own.

Tool servers reach a local launch only. An SSH turn carries the document without the `mcp` member, so a remote session has no tools.

The `run` output has no MCP startup status. A server that fails to start shows up only as failing tool calls. The runtime starts each server in the background and does not wait for it before the turn begins.

Every tool-server call is reported under one runtime-chosen name, not the tool's own name, because code mode is the only route to a tool server.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_OPENCODE_TEST=1` and skip cleanly without it. The other `SORTIE_OPENCODE_*` variables they read are described in that file. Traps:

- A first launch on a clean machine runs database migrations before the first event, and the free hosted model can be slow to answer. Give the suite a generous read timeout.
- A test that resumes a session must run both turns in the same workspace.
- The scripted-model case isolates `HOME` and the XDG directories, so a version-manager shim that reads `HOME` fails to start. Name the binary by an absolute path.
- OpenCode 2.x offers MCP tools only inside code mode, and it offers a server's tools only from the request after the tool listing. A scripted endpoint has to script an `execute` call and wait for the listing before it answers.
