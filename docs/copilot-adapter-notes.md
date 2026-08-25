# Copilot CLI adapter notes

Working notes for anyone changing Sortie's GitHub Copilot CLI adapter in `internal/agent/copilot`: the decisions behind it, where its session and cost model collide with ours, and the failures that are hard to diagnose from a log.

Last updated: 2026-08-23

## Where to get the volatile facts

Nothing here pins a flag list, an event vocabulary, or a payload shape. Read the argument surface off `copilot --help` on the version you are targeting, and take the headless output format, permission flags, hooks, and telemetry from GitHub's published Copilot CLI reference. Context7 and the upstream issue tracker are the places to check when the rendered docs and the shipped binary disagree, which they periodically do. What Sortie actually sends and parses is defined by `buildArgs` and `parse.go`, and those are authoritative over any prose.

## Shape of the integration

The adapter registers the `copilot-cli` kind and drives the CLI in headless prompt mode with JSON lines on stdout. It holds no process open: the CLI session is disk-persisted state identified by a server-assigned ID, and the OS process is created once per Sortie turn. Process groups, graceful shutdown, the stdout line ceiling, and the exit-code decision table all live in the shared `agentcore.ForkPerTurnSession` skeleton, not here.

Two flags are load-bearing and unconditional. Without the autonomous-continuation flag the CLI answers once and exits, so a non-trivial headless task is impossible; the adapter passes it on every invocation along with a step cap, defaulting to a bounded value when the workflow does not set one. And the flag that disables the CLI's ask-the-user tool is passed on every invocation as well, which closes the human-question path completely: no turn of this adapter can end needing human input, because the runtime has no way to ask.

## Session identity, and why turn one is different

Unlike some agent CLIs, this one does not accept a caller-assigned session ID. The ID exists only after the runtime creates it and is learned from the terminal event of the turn that created it. So the first turn of a session this run created carries no session flag at all, the finalize hook captures the ID from the terminal event, and later turns resume it explicitly. A session the orchestrator hands back after a restart is different: its ID is known before the first turn, and that turn resumes immediately.

When a turn produces no terminal event carrying an ID and none is known from an earlier turn, the adapter falls back to asking the CLI to resume the most recent conversation in the working directory. That is only safe because Sortie isolates one workspace per issue. Do not extend that idea into session discovery: with more than one agent running concurrently, the most recently written session directory is not necessarily this session's. The adapter opens the runtime's session-state tree only by a known ID, and it validates that ID against a path-segment character set before joining it into a path. Both properties are security boundaries, not stylistic choices.

## Preflight, and the fake-binary trap

`StartSession` runs a version canary and a credential preflight before it will hand back a session, and both are local-mode only. The credential check accepts any of the token environment variables the CLI understands and otherwise falls back to a working `gh` login; with no source at all it refuses the session before any turn runs.

This is the thing that ruins an afternoon when writing tests. A stand-in binary that exits zero and prints something unparseable never reaches a turn, because the preflight rejected it first, and the failure surfaces as an agent-not-found error that looks nothing like a parse problem. In SSH mode both checks are skipped, so a broken remote binary instead fails later, at the first turn, with a different error kind. Know which mode your test is in.

## Token accounting lives off the event stream

The stdout stream carries no input token count anywhere. The only token-shaped field on it is a per-message output count, which the adapter sums as an in-turn estimate and nothing more. The runtime's real accounting lives in an on-disk session journal, which the adapter reads after the subprocess exits.

That journal is session-cumulative across every process that resumes the session, not per-invocation. A second turn's record includes the first turn's spend. Recovering one run's own contribution means subtracting a baseline: the record that predates the run. The adapter resolves that baseline once, at its first read attempt, and the consequence is unforgiving. Miss the first attempt on a session someone else started and the boundary record is gone for good; the run then marks recovery unavailable and lets the output-only estimate stand for the rest of its life rather than reporting a figure inflated by a previous run's spend. Journal reads are also skipped in SSH mode, on an ID that fails the path-segment check, and on a file that breaches the size or line caps.

The practical rule: a usage figure from this adapter is run-cumulative and never decreases, a failed journal read never lowers a figure already reported, and the "measured" flag distinguishes an unknown spend from a genuine zero. Assertions that ignore that distinction pass for the wrong reason.

There is a third source worth knowing about even though the adapter does not read it. This CLI can export OpenTelemetry, and its spans carry the per-request input and output token counts that the JSON stream does not. An operator who exports the relevant variables into Sortie's environment gets them in the subprocess too, since it inherits that environment, and ends up with a token breakdown the adapter itself cannot see. That is the answer to give when someone needs per-request accounting today rather than a change here.

## Approvals

By default the adapter grants everything, because an unattended run has nobody to prompt. Any tool scoping the workflow configures displaces that blanket grant, since the blanket grant would override it. Config validation reports scoping as a warning rather than an error, and the reason matters: with scoping in place the CLI denies an out-of-scope call under its own policy and the session continues, so the configuration narrows what the agent may do without defeating the non-interactive posture.

A denial is recognized on the tool-completion event by its error code, distinct from an ordinary tool failure. The adapter routes it through `agentcore.DecideHumanRequest` like every other adapter and emits the shared operator-facing notice. It never constructs a posture itself, and it never treats a denial as terminal, because the run continues.

## Deciding how a turn ended

The terminal event is authoritative when present: its own exit code decides success or failure regardless of what the process did. With no terminal event the decision falls to the process exit and to work evidence, and the shared `agentcore.FinalizeTurn` owns the mapping.

Work evidence is this turn's own output tokens, never the run-cumulative figure, which is non-zero on every turn after the first. Get that wrong and the safety row that turns "exited cleanly, produced nothing" into a failure stops firing for the rest of the run. That row exists because this CLI has more than one way to exit zero having done nothing at all, including a configuration file it could not parse.

The adapter enforces no read deadline and no turn deadline of its own; both are orchestrator-side. Combined with the disabled ask-the-user tool, that makes the response-timeout and input-required outcomes unreachable here by construction.

## Sortie's own tools

The worker generates one MCP configuration file per session and the adapter passes it by file reference. The flag on this CLI is additive rather than replacing, but the adapter still routes an operator-supplied config through the same merge into one file, and a collision on our reserved server key fails the attempt rather than silently overwriting. When an operator value is passed through on its own, the adapter distinguishes inline JSON from a file path by prefix, because the flag itself does: a bare path is read as inline JSON and fails to parse. Credentials reach the tool server through the inherited environment, never through the file.

Whether Sortie's tools reach an agent at all is a per-adapter property. The mechanism depends on what the CLI accepts, so what works here does not carry to the rest of the fleet.

## Failure modes worth recognizing

A configuration the CLI cannot parse can end the process with a zero exit and an empty event stream. The turn is reported as a failure by the zero-work row, and the diagnosis is in stderr, which the adapter logs at warning level on any failing turn. Look there first when a turn fails with no events at all.

An authentication problem can also produce a process that fails with nothing on stdout. The symptom is identical to the case above; the distinguishing evidence is again stderr.

Two failures worth recognizing come from outside our code entirely, and both are worth checking before you go looking for a bug in the adapter. A resume that comes back with no conversation history, on a session whose journal is present on disk, points at the journal itself rather than at our resume logic: the journal is line-delimited JSON and a raw character sequence that breaks a line-oriented parse takes the whole history with it. And a subprocess that starts, prints nothing, and never exits has been seen under environment managers that wrap the shell and rearrange its file descriptors; there is nothing on our side to fix, and stall reconciliation is what ends such a turn. If either reproduces, confirm it against the current release before treating it as known.

Session state accumulates under the user's home directory, outside the workspace, so removing a workspace does not reclaim it.

## Verifying a change

Unit tests cover argument construction, event parsing, journal reading, and disposition. The tests that drive the real binary are env-gated on `SORTIE_COPILOT_TEST=1` and skip cleanly without it; keep them skipping cleanly rather than failing. `SORTIE_COPILOT_COMMAND` points at a specific binary and `SORTIE_COPILOT_MODEL` overrides the model. Because of the preflight, the suite also needs a real credential in the environment: one of the token variables the CLI accepts, or an authenticated `gh`.
