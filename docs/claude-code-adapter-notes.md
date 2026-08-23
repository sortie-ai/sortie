# Claude Code adapter notes

Working notes for anyone changing Sortie's Claude Code adapter in `internal/agent/claude`: why it is built the way it is, where the CLI's model and ours disagree, and what has cost someone a day.

Last updated: 2026-08-23

## Where to get the volatile facts

This file deliberately carries no flag table, no event catalogue, and no payload dump. The CLI ships faster than we can track it and Anthropic documents it better than we ever will. Read the argument surface off `claude --help` on the version you are targeting, read the headless output format, permission modes, session storage, and hook behavior from Anthropic's published Claude Code documentation, and reach for Context7 or the upstream package when the rendered docs are thin. For what Sortie actually sends and reads, the adapter's own `buildArgs` and `parse.go` are the authority.

## Shape of the integration

The adapter registers the `claude-code` kind and drives the CLI in headless print mode. It does not hold a process open: the Claude Code session is disk-persisted state identified by a session ID, while the OS process is short-lived and created once per Sortie turn. `StartSession` therefore spawns nothing. It resolves the workspace and the binary, mints or adopts a session ID, and builds the shared `agentcore.ForkPerTurnSession` that owns the subprocess lifecycle. Everything about process groups, graceful shutdown, the stdout line ceiling, and the exit-code decision table lives in that skeleton and is shared with the other fork-per-turn adapters. Change it there, not here.

Sortie mints the session UUID itself before the first turn and passes it in, rather than reading an ID out of the stream and hoping to catch it. That single decision removes a whole class of failure: a turn that dies before it prints anything still leaves a resumable session, and continuation never depends on parsing. Continuation turns resume that exact ID rather than asking for the most recent conversation in the directory, which is ambiguous the moment more than one session exists there.

The subprocess inherits Sortie's environment verbatim. The adapter manages no credentials and sets no telemetry variables: whatever authenticates the CLI, and whatever exports its traces, has to be in the environment of the Sortie process. That also means an operator variable you did not think about is present in the child.

The CLI can be pointed at a non-Anthropic backend as well, a cloud vendor's hosted models or a gateway in front of them. For this adapter that changes nothing at all: same argv, same stream, same parse, same disposition. The only thing it changes is which variables have to be present in Sortie's environment before the subprocess starts, and those names belong to the vendor's documentation rather than to this file. Do not add a preflight that checks for one provider's variable; there is more than one way for this CLI to be authenticated and the adapter deliberately checks for none of them.

## Turn boundaries and ours

Claude Code has its own idea of a turn, an agentic loop inside one invocation. A Sortie turn is one invocation, and inside it the CLI may run many of its own. The adapter does not bound that inner loop unless the workflow asks for it: letting the loop run to completion keeps a multi-step plan from being cut in half and avoids paying process startup per step. The orchestrator re-reads tracker state between Sortie turns, which is where the real control loop lives.

## Approvals, and requests only a person can answer

An unattended run has nobody to approve anything, so the adapter launches with permission prompting bypassed. An operator can substitute an explicit permission mode, but config validation accepts only the mode that is semantically equivalent to the bypass flag. The direction is deliberately an allowlist: a mode the runtime adds later is refused until someone establishes what it does, rather than sliding through validation and stalling a run in production.

Bypassing prompts does not remove every refusal. Workspace-directory containment is enforced independently of the permission mode, so the runtime still denies a read or write outside the allowed directories, reports the denial on the stream ahead of the tool's own result, and lets the turn continue by another route. Recognize the symptom by that shape: a denial event, a tool result flagged as an error, and a later assistant message picking a different approach.

Classification of any such request goes through `agentcore.DecideHumanRequest`. The adapter supplies three inputs and reads the posture back; it never decides for itself and never constructs a posture. The only adapter-specific knowledge in that path is the tool-name test: one built-in tool carries a genuine question to a person rather than a request for consent to act, and a denial naming it ends the attempt as human-input-required instead of continuing. That test is a single constant in `claude.go`. If you touch it, verify against a real transcript, because a headless session that excludes the tool from its discoverable set makes the case hard to reproduce on demand.

## Usage accounting

This is where the day goes. Three things are true at once and none of them is obvious.

Streamed assistant usage is a snapshot, not a final count. The CLI repeats one message identifier across every event of the same model request, and a later event of that identifier can report a larger usage object than an earlier one as generation continues. Deduplicate by that identifier and keep the componentwise maximum; sum the per-identifier maxima to get the turn's provisional figure. Adding the events up as they arrive multiplies the count.

The terminal event's top-level usage object excludes sub-agent activity, while its per-model breakdown includes it. The adapter prefers the per-model breakdown, summed across models, and falls back to the top-level object only when the breakdown is absent. Reading the top-level object first silently undercounts any turn that spawned a subagent.

The figure that leaves the adapter is run-cumulative and monotone. `agentcore.RunUsage` holds the session total: assistant events set the in-flight turn's provisional contribution, the terminal event settles it, and the snapshot never decreases. The turn result carries that cumulative snapshot, not a per-turn delta. When you write an assertion about usage, be clear about which of the two you are asserting.

The adapter reports no cost figure at all, even though the stream carries one. It reports API timing instead: a clock that starts when the session opens, restarts after every tool-result message because the next API call follows tool execution, and is attached to the next usage event it emits. When no per-request timing was emitted during a turn, the finalize path falls back to the terminal event's own API duration, so the two are never double-counted.

## Deciding how a turn ended

The adapter never decides a disposition. It fills in evidence and hands it to the shared `agentcore.FinalizeTurn`, which owns the mapping from evidence to outcome and error kind. Cancellation, a stdout scan failure, a missing binary, and death by signal are all decided by the skeleton before the adapter's finalize hook runs at all.

One trap sits in that evidence. Work evidence is this turn's own assistant output, never the run-cumulative figure, which is non-zero on every turn after the first. Feed it the cumulative snapshot and the zero-work safety row, the one that turns a process which exited cleanly having produced nothing into a failure rather than a silent success, stops firing for the rest of the run.

The adapter enforces no deadline of its own. The per-turn deadline, the stall threshold, and the teardown budget are all orchestrator-side; the subprocess sees them only through the context the skeleton passes to the command. So a timeout is something the orchestrator reports on the adapter's return, not something the adapter produces.

## Failure modes worth recognizing

A single stdout line can carry a whole file body, because a tool result embeds its output. The skeleton caps a line at 10 MB and a longer one ends the scan and fails the turn. If a turn dies immediately after a large read with a scan error and no terminal event, this is why.

Tool error text arrives wrapped in a runtime-specific envelope and salted with terminal color codes. The adapter unwraps and strips it so log fields stay greppable, then bounds it first-line-plus-tail rather than head-truncating, because the useful part of a failing build is at the end and the exit-code header is at the start.

An uncorrelated tool result reports the tool name as unknown. That means the matching tool-use block never reached the tracker, usually because the result arrived in a message position the scan does not walk. The adapter scans content blocks in both the assistant and the user positions for exactly this reason.

Session transcripts live under the user's home directory, not in the workspace. Removing a workspace does not reclaim them, and a long-running fleet accumulates them.

## Sortie's own tools

The worker generates one MCP configuration file per session, declaring the Sortie tool server and carrying the per-session variables it needs. This CLI accepts exactly one such config path, which is why an operator-supplied config cannot simply be passed alongside ours: the worker merges the two, and a name collision on our reserved server key fails the attempt rather than silently overwriting. Credentials reach the tool server through the inherited environment, never through the file.

Whether Sortie's tools reach an agent at all is a per-adapter property, not a guarantee of the fleet. The mechanism depends on what the CLI accepts, so some adapters wire the generated config through and others cannot. Do not assume, from this adapter, that a tool call is available in another.

## Verifying a change

Unit tests cover argument construction, line parsing, and disposition. The tests that exercise the real binary are env-gated: they run only with `SORTIE_CLAUDE_TEST=1` and skip cleanly without it, and they must keep skipping cleanly rather than failing. `SORTIE_CLAUDE_COMMAND` points at a specific binary and `SORTIE_CLAUDE_MODEL` overrides the model; the suite disables session persistence so repeated runs do not accumulate transcripts in the home directory. The binary still needs working credentials in the environment, so a machine without them will fail the suite rather than skip it.
