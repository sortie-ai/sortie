# OpenCode adapter notes

Working notes for anyone changing Sortie's OpenCode adapter in `internal/agent/opencode`: why this one does not use the shared subprocess skeleton, where OpenCode's session and permission model collide with ours, and the failures that look like something else.

Last updated: 2026-08-23

## Where to get the volatile facts

Nothing here pins a flag list, an event vocabulary, a permission key set, or an exit code. Read the argument surface off `opencode run --help` on the version you are targeting, take the permission actions, provider configuration, and server surface from the rendered docs at `opencode.ai`, and go to the upstream repository when the docs and the shipped binary disagree, which for this CLI they routinely do. The adapter's `command.go` and `parse.go` define what Sortie actually sends and reads.

## The surface we drive, and what it is not

`opencode run` is a thin client over the same HTTP and event APIs the server mode exposes. Without an attach target it boots a server in-process and talks to it through an internal fetch. That is the single most useful thing to know about this integration, because it explains every gap in the stdout stream: the JSON that `run` prints is a CLI-specific projection of selected server events, not the canonical event bus. It carries no session-started envelope, no status or idle transition, no permission event, and no final result envelope. Process exit is the only explicit end-of-turn signal on this surface.

If a future change needs anything the projection hides, retry and backoff state above all, the answer is the server surface with its documented schemas, not a richer parse of `run` output.

One trap to carry into that work if it ever happens: nothing in this adapter wires a port today, but the underlying server mode treats a port value of zero as a sentinel rather than as "pick any free port", and reading that wrong in either direction either attaches to a server this process did not start or misses the server the CLI actually opened. Confirm the current sentinel semantics against `opencode run --help` and the server-mode docs at `opencode.ai` before trusting a zero default, and set the port explicitly on both sides regardless of what you find there.

## Why this adapter owns its own loop

Every other fork-per-turn adapter delegates the subprocess lifecycle to `agentcore.ForkPerTurnSession`. This one does not. It starts the process itself, runs its own stdout reader goroutine, its own wait goroutine, and its own select loop, because it needs two things the skeleton does not offer: a deadline on the first JSON event rather than on the turn, and post-exit subprocess queries against the same binary to recover usage and to reconstruct a masked diagnostic.

That independence is a maintenance hazard. Improvements to the shared skeleton do not reach this adapter, so when the two are meant to behave alike, they have to be kept alike by hand. The terminal decision itself is still shared: evidence is assembled here and handed to `agentcore.FinalizeTurn`, so disposition and error kinds stay consistent across the fleet.

The first-JSON deadline defaults to thirty seconds and is overridden by the workflow's read timeout. It stops once the first envelope arrives, so it guards a subprocess that never starts talking, not a mid-turn stall. Stalls are the orchestrator's business.

There is one ordering constraint in the wait path that is easy to break and hard to see: the wait goroutine waits for both the stdout reader and the stderr collector to finish before reaping the process, because reaping closes the pipe read ends and races a scanner still draining buffered output. On stderr that race silently drops the permission-refusal warning that the finalize path depends on. The wait on the stdout reader stays unbounded; the wait on the stderr collector is bounded, so a descendant that inherits only the stderr handle and outlives the direct child cannot withhold the reap. Firing that bound costs the permission-refusal warning on the residual path where reaping the process does not also release the collector.

## Session identity and the dir-scoped resume

A session ID is server-generated and learned from the first envelope that carries one. The adapter adopts it, and a later envelope carrying a different ID is treated as a fault that fails the turn rather than a thing to reconcile.

Resume is scoped to the directory the session was created in. Resuming a valid session ID from a different project directory does not error: the process exits zero and prints nothing. Downstream that becomes a zero-work failure, which reads like the agent did nothing rather than like a resume that never matched. The adapter satisfies the constraint by construction, passing the issue workspace as both the process working directory and the directory flag on every turn, so the two cannot diverge. The trap is in tests: a resume test that runs the second turn in a fresh temporary directory passes through the code path without exercising resume at all. Share the workspace across both turns or the test proves nothing.

Sessions are persisted in one global database under the user's home directory, not in a per-project store inside the workspace. Isolation between concurrent issues comes from distinct server-generated IDs, not from separate storage, and removing a workspace reclaims none of it.

## Permissions

OpenCode's permission control is config-driven rather than flag-driven: each key resolves to allow, ask, or deny. The adapter injects its policy through an environment variable, and the critical detail is that OpenCode deep-merges that value into the permission block it already resolved from configuration rather than replacing it. An inherited value from the operator's shell would therefore bleed into the merged result. The adapter scrubs every variable it manages out of the inherited environment before appending its own, so an operator-side value never survives into the subprocess. Keep that scrub in step with the managed set: a new managed variable that is not also scrubbed is a leak.

The policy the adapter emits is closed rather than open. When the workflow names allowed tools, every other permission key the adapter knows about is denied explicitly, which is why that set exists at all. It is a policy input, not a validation whitelist: the runtime accepts more keys than its documentation lists, including a catchall for keys nobody has enumerated, so a key the workflow names is forwarded verbatim whether or not the adapter recognizes it, with unknown keys logged at debug level. Rejecting an unrecognized key would break configurations OpenCode itself accepts.

Separately from the policy, the adapter runs with permission prompting bypassed by default, because the alternative is the runtime auto-rejecting every permissioned tool call. Validation warns when a workflow turns that off, and treats an overlap between the allowed and denied lists as an error, since the two would contradict each other.

A refusal surfaces on stderr as a human-readable warning line while stdout stays JSON-clean. The adapter recognizes it from the collected stderr after the process exits and routes it through `agentcore.DecideHumanRequest` like every other adapter. Recognizing it at turn end rather than as it happens means the notice arrives late and does not reset the first-JSON timer, neither of which matters while the warning never reaches stdout.

## Usage accounting

Step-scoped token counts on the stream are not a running total. Between the tool step and the final text step of one turn the numbers move in both directions, so summing them across steps is wrong and taking the last one is wrong too.

The adapter recovers authoritative usage by running a sanitized session export after the subprocess exits, and it computes its own totals from per-message figures rather than reading the export's session aggregate. Two reasons, both durable. The aggregate spans the entire session including turns from an earlier run, while the figure Sortie reports is run-scoped; and the aggregate's own total field is not the sum of its input and output components, because it also folds in cache and reasoning tokens. So the adapter selects assistant messages by session ID and, on a resumed session, by creation time at or after the run's start, then sums them. The export is sanitized deliberately, so tool output bodies never reach a log.

A failed or empty export must never lower a figure the run has already reported. The finalize path skips the update entirely rather than replacing a settled value with zero.

## Failure detection

An error envelope on stdout is the authoritative failure signal. The process exit code is not load-bearing and has changed meaning across releases; do not key anything on it.

One failure can arrive as two error envelopes: the actionable diagnostic the session publishes, and a generic placeholder the run command reports when the underlying fault was not in its API error schema. Their order on the stream is not guaranteed, so the adapter keeps whichever envelope carries detail rather than whichever arrives last. When only the placeholder was seen, the finalize path consults the binary's own model catalog and reconstructs the unknown-model diagnostic, which is by far the most common cause. Any other masked cause still reaches the operator as the placeholder, so a report of the generic message is a signal to reproduce, not a bug in the parse.

Work evidence for the turn is this turn's own parsed assistant parts, text, reasoning, or tool use, never the export figure, which is non-zero on any turn after the first. An exit-zero run that parsed envelopes but produced no assistant output takes the shared zero-work row and is reported as a failure rather than a silent success. That row is also what a mismatched-directory resume ends up in.

## Sortie's own tools

`StartSession` parses the worker-generated MCP configuration and translates it into OpenCode's own configuration document, keyed under `mcp`. That document is delivered on every turn's subprocess through the runtime's inline configuration environment variable, additive to whatever an operator's own project or global configuration already declares, never as the generated file's path handed over verbatim; live probing found the runtime rejects that standard `mcpServers` key outright. The variable is set only in the turn's own environment build and never in the shared managed-environment builder, so the `export` and `models` auxiliary invocations that builder also serves never carry it and never spawn a tool sidecar of their own.

Delivery happens on a local launch only. An SSH session gets no document: this adapter already renders its managed environment as `KEY=<value>` onto the remote command string, and doing the same with the generated servers would publish credential values on the local `ssh` process's own argument list, so the adapter delivers nothing there and the session runs exactly as it does today, without tools.

The run projection this adapter reads carries no MCP startup-status signal: see "The surface we drive, and what it is not" above for the event types it omits. A server that fails to start is visible only indirectly, as the agent's own tool calls failing, not as a distinct diagnostic on this surface.

## Verifying a change

Unit tests cover argument and environment construction, the permission policy, event parsing, export parsing, and disposition. The tests that drive the real binary are env-gated on `SORTIE_OPENCODE_TEST=1` and skip cleanly without it; keep them skipping cleanly rather than failing. `SORTIE_OPENCODE_COMMAND` points at a specific binary and `SORTIE_OPENCODE_MODEL` overrides the model. Give the suite a generous read timeout: a first launch on a clean machine runs database migrations that can take well over the default before the first event appears. And any test that resumes a session must run both turns in the same workspace.
