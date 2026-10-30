# Claude Code adapter notes

Working notes for anyone changing Sortie's Claude Code adapter in `internal/agent/claude`.

Last updated: 2026-10-29

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

The registered kind converts nothing itself. It feeds `usage.Ledger` from two sources it declares: the stream is the live source, counted per turn with spend visible while the turn runs, and Claude Code's session journal is the records source. `incremental` arrival and `per_model` attribution derive from those declarations. `ParseLine` emits one `token_usage` event per first-seen assistant message id, carrying the model that served the request.

Streamed assistant usage is a snapshot, not a final count. The CLI repeats one message identifier across every event of the same model request, and a later event of that identifier can report a larger usage object than an earlier one as generation continues. The ledger keeps the largest reading per identifier, and the sum of those is the turn's provisional figure. Adding the events up as they arrive multiplies the count. A message whose usage lacks a count, and the runtime's own synthetic message, yield no reading.

A turn settles from the `model_usage_since_last_result` member of its `result` events. The member is a per-model delta over the requests the process made since its previous `result`, sub-agents included. Each turn runs as one process, and a process can emit more than one `result`: with a background sub-agent, a later one carries an empty member. The adapter keeps every member of the turn, so the turn's figure is their entries together and an empty later member never makes the turn unknown. The turn settles once, after the process exits, split by model. The top-level `usage` excludes sub-agent activity, and `modelUsage` and `total_cost_usd` span every process of the session, so none of the three is read. A member that is absent or not an object, or an entry missing a count, leaves the turn to the journal.

A resumed process replays no earlier message, and its member counts only its own requests. Resume therefore needs no version check. `StartSession` of a continued session reads the journal, the sub-agent journals the session journal names included, and the ledger notes which requests it holds as history so no later read counts them. When that read fails or finds no session journal, the boundary stays unknown, no history is counted meanwhile, and the next turn end reads the journal again until a completed read fixes it.

The journal lives under the runtime's home on the host that ran it: `$CLAUDE_CONFIG_DIR` when set and non-empty, else `~/.claude`; `XDG_CONFIG_HOME` has no effect. The session journal is `projects/<dir>/<session>.jsonl` and a sub-agent's is `projects/<dir>/<session>/subagents/agent-<id>.jsonl`. `<dir>` is the runtime's physical working directory with every UTF-16 code unit outside `[A-Za-z0-9]` replaced by `-`, case kept, nothing collapsed, so a letter outside ASCII becomes one `-` and a character outside the Basic Multilingual Plane two. Past 200 units the name is the first 200, a `-`, and the base-36 absolute value of the Java `String.hashCode` of the raw path. The adapter takes the directory from the `cwd` of the latest `system/init` event and, before the first one, from the workspace path, because the runtime reports its resolved working directory on its own host, including a symlink only a remote host has.

A sub-agent journal is named by the `task_id` of a `task_started` stream event or by the session journal: the `user` line holding the result of the tool that started the sub-agent carries `toolUseResult.agentId`. A `toolUseResult` that is a string names none. The reader never lists a directory to find one. Claude Code can resume an earlier run's sub-agent by its id and appends to that sub-agent's earlier journal in place, so every read, not only the start-up read, names the session journal's sub-agents.

A journal line is one content block, so a request with several blocks has several lines under one `message.id` and `requestId`, each carrying the request's final counts; the first stands, and the request identity is those two joined. The runtime's synthetic message yields nothing. A line that does not decode, or an assistant line without `message.id`, is an unknown reading under a line identity of file name and ordinal, and counting stops at it, so a turn is never settled as zero. A resume appends to the same file and copies nothing, which is why history is excluded by identity. The reader merges the session and sub-agent files by timestamp so the requests sit in the order the runtime made them.

A line is written when its request completes. A killed process keeps its completed requests and nothing of the one in flight, and a kill right after a request completes can lose that request's line, so a turn settled from the journal after a kill is never complete and reports `SpendUnaccounted`. The run figure then holds per class the larger of the journal's completed requests and the streamed provisional counts, because a reported snapshot never falls.

The journal is read at `StartSession` of a continued session and at the end of a turn the stream left unsettled or whose boundary is unknown. A completed turn of a session with a known boundary reads nothing. Every read goes through the shared remote read: a local session opens the file under the resolved home, and a remote session runs one process per file over its own SSH launch, where the remote shell resolves the home. Each read is bounded by the agent read timeout. A missing session journal fails the read, because the runtime resumes a session from that file and its absence never means an empty history; a missing sub-agent journal adds nothing. A failed read leaves the turn as if the records held nothing and logs `session journal read failed` at debug.

The figure that leaves the adapter is run-cumulative and monotone. `agentcore.RunUsage` holds the snapshot: each streamed request sets the in-flight turn's provisional contribution, the turn's settlement replaces it with the run figure, and the snapshot never decreases. Settlement emits no event, because the orchestrator counts every `token_usage` event as an API request. The terminal event carries the snapshot instead. The turn result carries that cumulative snapshot, not a per-turn delta. When you write an assertion about usage, be clear about which of the two you are asserting.

The adapter reports no cost figure at all, even though the stream carries one. It reports API timing instead: a clock that starts when the session opens, restarts after every tool-result message because the next API call follows tool execution, and is attached to the next usage event it emits. When no per-request timing was emitted during a turn, the finalize path falls back to the terminal event's own API duration, so the two are never double-counted.

## Deciding how a turn ended

The adapter never decides a disposition. It fills in evidence and hands it to the shared `agentcore.FinalizeTurn`. Cancellation and a stdout scan failure are decided by the skeleton before the adapter's finalize hook runs.

A runtime that exits on its own before writing any line the decoder reads as an event gets the shared early-exit report, whatever its exit status. A death by a signal Sortie did not send reaches the finalize hook as a non-zero exit whose error names the signal.

One trap sits in that evidence. Work evidence comes from the shared per-turn observer, never the run-cumulative usage figure, which is non-zero on every turn after the first: a `text` content block with non-empty text is assistant output, and a `tool_use` or `tool_result` block is tool activity. Feed it the cumulative snapshot and the zero-work safety row, the one that turns a process which exited cleanly having produced nothing into a failure rather than a silent success, stops firing for the rest of the run.

The adapter enforces no deadline of its own. The per-turn deadline, the stall threshold, and the teardown budget are all orchestrator-side; the subprocess sees them only through the context the skeleton passes to the command. So a timeout is something the orchestrator reports on the adapter's return, not something the adapter produces.

## Failure modes worth recognizing

A single stdout line can carry a whole file body, because a tool result embeds its output. The skeleton caps a line at 10 MB and a longer one ends the scan and fails the turn. If a turn dies immediately after a large read with a scan error and no terminal event, this is why.

Tool error text arrives wrapped in a runtime-specific envelope and salted with terminal color codes. The adapter unwraps and strips it so log fields stay greppable, then bounds it first-line-plus-tail rather than head-truncating, because the useful part of a failing build is at the end and the exit-code header is at the start.

An uncorrelated tool result reports no tool name. That means the matching tool-use block never reached the tracker, usually because the result arrived in a message position the scan does not walk. The adapter scans content blocks in both the assistant and the user positions for exactly this reason.

Session transcripts are the journal the adapter reads as records. They live under the runtime's home directory on the host that ran the session, not in the workspace, so they outlive it: removing a workspace does not reclaim them, and a long-running fleet accumulates them.

A subscription usage limit arrives as a `rate_limit_event` whose `rate_limit_info` reports `status` `rejected`, `resetsAt` in Unix seconds, `rateLimitType`, and `overageStatus`, followed by a `result` with `is_error` true and `api_error_status` 429 and a `usage` object of zeros. The `subtype` of that `result` reads `success`, so the refusal is read from `is_error` and `api_error_status`, never from `subtype`. The adapter keeps the turn's latest `rate_limit_info` and ends the turn `usage_limit` when the `result` is a 429 and the info is `rejected` with an `overageStatus` that is empty or `rejected`; an authentication refusal keeps precedence. `resetsAt` is the instant, and `rateLimitType` of `seven_day_opus` or `seven_day_sonnet` marks an allowance that covers the run's model only. The CLI sends the request once and does not retry the 429. An API-key 429 carries no rejected `rate_limit_event` and stays `turn_failed`. Every `rate_limit_event` also reports a usage window: `rateLimitType`, when present, names it, `status` says how near the limit it is (`allowed`, `allowed_warning`, `rejected`), and `utilization` is the used share as a fraction that may exceed 1. The captured `allowed` reports carry no `utilization`, so such a window has no share until the runtime warns. API-key sessions send no `rate_limit_event`. The fields the runtime marks `@internal` (`api_error`, `api_error_params`, `unifiedWindows`) are not read.

`claude-code.session_persistence: false` cannot hold in a Sortie workflow: preflight refuses the configuration before the run starts. The adapter continues a session by passing `--resume <session_id>` on every turn after the first, and that flag reads the session file that `session_persistence: false` prevents Claude Code from writing. A maintainer who sees the refusal named `agent.kind.session_resume` should look for `session_persistence: false` in the `claude-code` block.

## Sortie's own tools

The worker generates one MCP configuration file per session, declaring the Sortie tool server and carrying the per-session variables it needs. This CLI accepts exactly one such config path, which is why an operator-supplied config cannot simply be passed alongside ours: the worker merges the two, and a name collision on our reserved server key fails the attempt rather than silently overwriting. That merged file is also where credentials for the tool server come from: the worker copies every `SORTIE_`-prefixed variable out of its own process environment into the config's env block, which is how a workflow's `$SORTIE_*` credential indirection resolves inside the tool server. Treat that file as carrying secrets, not just plumbing. On a local launch the runtime receives a file derived from it: the derived file names an operator header value by the environment variable that carries it instead of holding the value, and it keeps the env block, so it carries the same secrets. On an SSH launch no file is passed: the runtime receives an inline declaration of Sortie's tool server, by the address of the launch's reverse forward with the bearer carried by environment variable name, marked to load upfront, beside the operator's HTTP servers. Operator stdio servers are left out, and the env block never leaves the orchestrator host.

Whether Sortie's tools reach an agent at all is a per-adapter property, not a guarantee of the fleet. The mechanism depends on what the CLI accepts, so some adapters wire the generated config through and others cannot. Do not assume, from this adapter, that a tool call is available in another.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_CLAUDE_TEST=1` and skip cleanly without it. The other `SORTIE_CLAUDE_*` variables they read are described in that file. Traps:

- The working-credential case needs real credentials in the environment. Without them it fails; it does not skip.
- The scripted-model case points the runtime at a loopback endpoint with `ANTHROPIC_BASE_URL`. Unless a proxy variable is set, the runtime first sends a `HEAD` request to that address, which the endpoint does not expect. The case sets an https proxy to a dead address to stop it; the plain-http loopback requests stay direct.
- The scripted-model case isolates `HOME` and the XDG directories, so a version-manager shim that reads `HOME` fails to start. Name the binary by an absolute path.
- The scripted-model case also runs the kill, cancellation, and continuation scenarios, locally and in the SSH lab. Those scenarios are what exercise the journal read, so a change to usage accounting is not verified until they pass in both.
