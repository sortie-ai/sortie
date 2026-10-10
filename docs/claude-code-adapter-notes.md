# Claude Code adapter notes

Working notes for anyone changing Sortie's Claude Code adapter in `internal/agent/claude`.

Last updated: 2026-10-10

## Where to get the volatile facts

This file carries no flag table, no event catalogue, and no payload dump. Read the flags off `claude --help` on the version you target. Read the headless output format, permission modes, session storage, and hooks from Anthropic's Claude Code documentation. For what Sortie actually sends and reads, `buildArgs` and `parse.go` are the authority.

## Shape of the integration

The adapter registers the `claude-code` kind and drives the CLI in headless print mode. It does not hold a process open: the Claude Code session is disk-persisted state identified by a session ID, while the OS process is short-lived and created once per Sortie turn. `StartSession` therefore spawns nothing. It resolves the workspace and the binary, mints or adopts a session ID, and builds the shared `agentcore.ForkPerTurnSession` that owns the subprocess lifecycle. Everything about process groups, graceful shutdown, the stdout line ceiling, and the exit-code decision table lives in that skeleton and is shared with the other fork-per-turn adapters. Change it there, not here.

Sortie mints the session UUID itself before the first turn and passes it in, rather than reading an ID out of the stream and hoping to catch it. That single decision removes a whole class of failure: a turn that dies before it prints anything still leaves a resumable session, and continuation never depends on parsing. Continuation turns resume that exact ID rather than asking for the most recent conversation in the directory, which is ambiguous the moment more than one session exists there.

The subprocess inherits Sortie's environment, with one exception. The adapter manages no credentials and sets no telemetry variables, so whatever authenticates the CLI or exports its traces must be in Sortie's own environment. Any operator variable you did not think about also reaches the child.

The exception is `CLAUDE_CODE_EFFORT_LEVEL`. When the block sets `effort`, the adapter withholds that variable on every launch, local and SSH, because Claude Code lets it silently outrank `--effort`. When `effort` is unset, the variable passes through.

An unrecognized `--effort` value shows up as a stderr line starting `Warning: Unknown --effort value`. It is the one stderr line that a successful turn raises to a warning in Sortie's log, once per session.

The CLI can also run against a cloud vendor's hosted models or a gateway. For this adapter nothing changes except which variables must be in Sortie's environment. Do not add a preflight that checks for one provider's variable; the CLI has several ways to authenticate and the adapter checks none of them.

## Turn boundaries and ours

Claude Code has its own idea of a turn, an agentic loop inside one invocation. A Sortie turn is one invocation, and inside it the CLI may run many of its own. The adapter does not bound that inner loop unless the workflow asks for it: letting the loop run to completion keeps a multi-step plan from being cut in half and avoids paying process startup per step. The orchestrator re-reads tracker state between Sortie turns, which is where the real control loop lives.

## Approvals, and requests only a person can answer

An unattended run has nobody to approve anything, so the adapter launches with permission prompting bypassed. An operator can substitute an explicit permission mode, but config validation accepts only the mode that is semantically equivalent to the bypass flag. The direction is deliberately an allowlist: a mode the runtime adds later is refused until someone establishes what it does, rather than sliding through validation and stalling a run in production.

Bypassing prompts does not remove every refusal. Workspace-directory containment is enforced independently of the permission mode, so the runtime still denies a read or write outside the allowed directories, reports the denial on the stream ahead of the tool's own result, and lets the turn continue by another route. Recognize the symptom by that shape: a denial event, a tool result flagged as an error, and a later assistant message picking a different approach.

Classification of any such request goes through `agentcore.DecideHumanRequest`. The adapter supplies three inputs and reads the posture back; it never decides for itself and never constructs a posture. The only adapter-specific knowledge in that path is the tool-name test: one built-in tool carries a genuine question to a person rather than a request for consent to act, and a denial naming it ends the attempt as human-input-required instead of continuing. That test is a single constant in `claude.go`. If you touch it, verify against a real transcript, because a headless session that excludes the tool from its discoverable set makes the case hard to reproduce on demand.

## Usage accounting

This is where the day goes.

The registered kind declares `incremental` arrival and `per_model` attribution: `ParseLine` emits one `token_usage` event per first-seen assistant message id, each carrying `Model: state.lastModel`.

Streamed assistant usage is a snapshot, not a final count. The CLI repeats one message identifier across every event of the same model request, and a later event of that identifier can report a larger usage object than an earlier one as generation continues. Deduplicate by that identifier and keep the componentwise maximum; sum the per-identifier maxima to get the turn's provisional figure. Adding the events up as they arrive multiplies the count.

The terminal event's top-level usage object excludes sub-agent activity, while its per-model breakdown includes it. The adapter prefers the per-model breakdown, summed across models, and falls back to the top-level object only when the breakdown is absent. Reading the top-level object first silently undercounts any turn that spawned a subagent.

The figure that leaves the adapter is run-cumulative and monotone. `agentcore.RunUsage` holds the session total: assistant events set the in-flight turn's provisional contribution, the terminal event settles it, and the snapshot never decreases. The turn result carries that cumulative snapshot, not a per-turn delta. When you write an assertion about usage, be clear about which of the two you are asserting.

The adapter reports no cost figure at all, even though the stream carries one. It reports API timing instead: a clock that starts when the session opens, restarts after every tool-result message because the next API call follows tool execution, and is attached to the next usage event it emits. When no per-request timing was emitted during a turn, the finalize path falls back to the terminal event's own API duration, so the two are never double-counted.

## Deciding how a turn ended

The adapter never decides a disposition. It fills in evidence and hands it to the shared `agentcore.FinalizeTurn`. Cancellation and a stdout scan failure are decided by the skeleton before the adapter's finalize hook runs.

A runtime that exits on its own before writing any line the decoder reads as an event gets the shared early-exit report, whatever its exit status. A death by a signal Sortie did not send reaches the finalize hook as a non-zero exit whose error names the signal.

One trap sits in that evidence. Work evidence comes from the shared per-turn observer, never the run-cumulative usage figure, which is non-zero on every turn after the first: a `text` content block with non-empty text is assistant output, and a `tool_use` or `tool_result` block is tool activity. Feed it the cumulative snapshot and the zero-work safety row, the one that turns a process which exited cleanly having produced nothing into a failure rather than a silent success, stops firing for the rest of the run.

The adapter enforces no deadline of its own. The per-turn deadline, the stall threshold, and the teardown budget are all orchestrator-side; the subprocess sees them only through the context the skeleton passes to the command. So a timeout is something the orchestrator reports on the adapter's return, not something the adapter produces.

## Failure modes worth recognizing

A single stdout line can carry a whole file body, because a tool result embeds its output. The skeleton caps a line at 10 MB and a longer one ends the scan and fails the turn. If a turn dies immediately after a large read with a scan error and no terminal event, this is why.

Tool error text arrives wrapped in a runtime-specific envelope and salted with terminal color codes. The adapter unwraps and strips it so log fields stay greppable, then bounds it first-line-plus-tail rather than head-truncating, because the useful part of a failing build is at the end and the exit-code header is at the start.

An uncorrelated tool result reports the tool name as unknown. That means the matching tool-use block never reached the tracker, usually because the result arrived in a message position the scan does not walk. The adapter scans content blocks in both the assistant and the user positions for exactly this reason.

Session transcripts live under the user's home directory, not in the workspace. Removing a workspace does not reclaim them, and a long-running fleet accumulates them.

`claude-code.session_persistence: false` cannot hold in a Sortie workflow: preflight refuses the configuration before the run starts. The adapter continues a session by passing `--resume <session_id>` on every turn after the first, and that flag reads the session file that `session_persistence: false` prevents Claude Code from writing. A maintainer who sees the refusal named `agent.kind.session_resume` should look for `session_persistence: false` in the `claude-code` block.

## Sortie's own tools

The worker generates one MCP configuration file per session, declaring the Sortie tool server and carrying the per-session variables it needs. This CLI accepts exactly one such config path, which is why an operator-supplied config cannot simply be passed alongside ours: the worker merges the two, and a name collision on our reserved server key fails the attempt rather than silently overwriting. That merged file is also where credentials for the tool server come from: the worker copies every `SORTIE_`-prefixed variable out of its own process environment into the config's env block, which is how a workflow's `$SORTIE_*` credential indirection resolves inside the tool server. Treat that file as carrying secrets, not just plumbing.

Whether Sortie's tools reach an agent at all is a per-adapter property, not a guarantee of the fleet. The mechanism depends on what the CLI accepts, so some adapters wire the generated config through and others cannot. Do not assume, from this adapter, that a tool call is available in another.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_CLAUDE_TEST=1` and skip cleanly without it. The other `SORTIE_CLAUDE_*` variables they read are described in that file. Traps:

- The working-credential case needs real credentials in the environment. Without them it fails; it does not skip.
- The scripted-model case points the runtime at a loopback endpoint with `ANTHROPIC_BASE_URL`. Unless a proxy variable is set, the runtime first sends a `HEAD` request to that address, which the endpoint does not expect. The case sets an https proxy to a dead address to stop it; the plain-http loopback requests stay direct.
- The scripted-model case isolates `HOME` and the XDG directories, so a version-manager shim that reads `HOME` fails to start. Name the binary by an absolute path.
