# Agent Client Protocol adapter notes

Working notes for anyone changing Sortie's Agent Client Protocol adapter in `internal/agent/clientprotocol`. Unlike the other agent adapters, this kind names no default runtime: the operator's `agent.command` picks the binary and the flag or subcommand that puts it into protocol mode, so these notes cover several candidate runtimes rather than one vendor. An agent's own version is never pinned in prose here; the adapter records it per session from that session's own `initialize` handshake.

## Pinned schema artifact

The generated wire types in `internal/agent/clientprotocol/wire_gen.go` come from the stable schema artifact published under tag `schema-v1.21.0`, resolving to commit `272bf799f35a258c6a4107a0410ed361e83683d3`. That tag and commit are the provenance for every wire shape the adapter package assumes; a change to either is a pin move, not a routine edit.

## Runtimes observed

Each of these runtimes speaks the protocol and answered an `initialize` handshake driven from this host.

### `copilot`

The entry point is the `--acp` flag. The handshake advertises `loadSession` true, both `mcpCapabilities.http` and `mcpCapabilities.sse` true, and a `sessionCapabilities` object carrying `close` and `list` but not `resume`. `promptCapabilities.image` is true. Only the handshake was driven against this runtime; no full turn was exercised.

### `opencode`

The entry point is the `acp` subcommand. The handshake advertises `loadSession` true, both `mcpCapabilities.http` and `mcpCapabilities.sse` true, and a `sessionCapabilities` object carrying `close`, `fork`, `list`, and `resume`. Only the handshake was driven against this runtime; no full turn was exercised.

### `gemini`

The entry point is the `--acp` flag; a `--experimental-acp` spelling also exists and is deprecated. The handshake advertises `loadSession` true and both `mcpCapabilities.http` and `mcpCapabilities.sse` true, and it advertises no `sessionCapabilities` object at all.

This is the one runtime the gated suite was driven against end to end: a session starts, a turn runs to a completed outcome, a `session/request_permission` request arrives mid-turn and the adapter refuses it inside the protocol with the turn continuing past the refusal to completion, and the session stops cleanly afterward. `What a delivered tool server needs to be callable` below states what that refusal means for the tool servers this session delivered.

Session continuation was probed by hand against this runtime, not through the gated suite. `session/load` replays the prior turn's messages, observed as `user_message_chunk` notifications arriving before the load response itself returns, when the workspace directory already carries earlier session history. Against a workspace used for the first time, whose only session was ended by a process-group kill with no graceful phase ahead of it, a reload of that session answers a JSON-RPC error reporting no prior session for the workspace instead of replaying anything; the adapter observes this exactly as it observes any other unconfirmed load, lowering the session continuation entry and falling back to a fresh session without failing the run. That probe predates the graceful phase teardown now runs, and it has not been repeated against it. `session/resume` is not exercised: this runtime advertises no `sessionCapabilities` object at all, so the adapter never selects it.

### `kiro-cli`

The entry point is the `acp` subcommand, which appears only under `--help-all` and not in the default help output. The handshake advertises `loadSession` true, `mcpCapabilities.http` true with `sse` false, and an empty `sessionCapabilities` object, so `session/close` is never selected against it. `agentInfo` carries both a name and a version.

This is the second runtime the gated suite is driven against end to end, and the second with a tracked qualification profile. Session continuation was confirmed from a second agent process rather than by hand: a session is created, given a turn that leaves history, stopped, and then loaded by identifier from a fresh subprocess in a following UTC minute, which replays the history and answers from it.

Its tool servers carry a credential constraint no other runtime here has. The runtime asks its own backend for a governance profile before enabling MCP at all, and that request fails for an API-key credential, so a session authenticated that way starts and runs normally while silently carrying no declared tool server. Under a stored device login the same request, model and posture deliver and call the tool. `docs/kiro-adapter-notes.md` records the log line that distinguishes the two; there is no signal for it on the wire beyond a vendor-namespaced notification.

The runtime also carries a large vendor-namespaced surface under its own method prefix, announcing available commands, subagent lists, MCP server initialization and per-turn metadata. Those arrive as notifications rather than requests, so this client answers none of them and depends on none of them.

`gemini`'s tracked tool-server and continuation rows predate the wiring that grades those rows from an induction rather than from a fixture constant, and they are re-measured the next time that runtime is run rather than being carried forward as though this change had verified them.

A locally launched runtime reaches this transport through three command-line slots: one that puts it on the protocol wire, one that grants the workspace the trust a declared tool server's delivery depends on, and one that puts it in a posture that does not ask before running a tool. Two runtimes have been measured, and they do not spell the slots the same way: `kiro-cli` fills the trust and posture slots with a single switch, so removing it restores asking and re-imposes the workspace's default trust together, and there is no way to take back one without the other. Whether a third runtime spells three slots, fewer, or none is unobserved until it is measured. What is fixed on our side is that no shared code branches on which runtime is naming them.

| Slot | What it is for | `gemini`'s value | `kiro-cli`'s value |
|---|---|---|---|
| Protocol switch | Puts the runtime on the protocol wire | `--acp` | `acp` subcommand |
| Trust switch | Makes the workspace trusted, so declared tool servers are not dropped | `--skip-trust` | `-a` |
| Posture switch | Puts the runtime in a mode that does not ask before running a tool | `--approval-mode yolo` | `-a`, the same switch |

## Runtimes not observed

Each of the following has an explicit gap rather than a measured result, so an absent probe is never later read as a measured negative.

### `claude`

The Claude Code CLI exposes no protocol entry point in its own help output. There is nothing on this host to probe.

### `codex`

Codex exposes no protocol entry point either. Its `app-server` speaks a different JSON-RPC dialect, the one this project's separate `codex` adapter already drives, and that dialect is not the Agent Client Protocol.

## Usage reporting

The registered kind declares `none` arrival and `none` attribution. `normalize.go`'s `updateUsage` folds a `usage_update` session update into the debug-log-only arm of the normalization table; no `token_usage` event is ever emitted for it. `capability.go`'s `newCapabilityRecord` initializes `tokenCounts` to `capabilityGap` unconditionally, for both a local and a remote launch, so the declaration holds for every session regardless of launch mode.

## Session close

Teardown sends `session/close` when the handshake advertises `sessionCapabilities.close`. Of the runtimes recorded above, `copilot` and `opencode` advertise it; `gemini` does not. This path carries fixture coverage only, because both advertising runtimes above were probed at handshake alone.

## Session load spacing

The adapter holds a `session/load` call until the clock leaves the UTC minute in which this process created the session being loaded. The wait is spent on the open connection, after the handshake and the negative control, and is bounded at one minute.

The wait exists because one runtime's `session/load` issued inside that minute fails and permanently destroys the session's resumability, including every later attempt to load it. `docs/gemini-adapter-notes.md` records the measured evidence for that defect; these notes cover only the spacing built to answer it.

The session continuation capability record cannot be the mechanism here, because the call that would observe the defect is the same call that causes it: a `session/load` sent to measure whether continuation works is the load that destroys the session if it lands in the wrong minute. A per-process ledger of when this adapter created each session, and a deferral computed from that instant, is the only way to keep the observing call out of the window it cannot survive.

This wait is load-bearing. It must not be trimmed as dead time by a later change: removing it restores the defect silently, because nothing else in the adapter or the orchestrator notices a lost session until the run comes back with no history.

Three limits apply. A session created by a previous process is not covered, because the ledger lives in process memory and starts empty on every restart. A wall-clock step backward larger than the deferral can still leave the load inside the session's creation minute, because the deferral clamps at one minute rather than re-reading the clock until the boundary passes. On a launch through the SSH worker, the wait is measured on the orchestrator host's clock, not on the clock of the host actually running the runtime, so a skew between the two hosts larger than the deferral can still land the load inside the runtime host's own creation minute.

## What a delivered tool server needs to be callable

Delivery and discovery are not the question: the session-creation request carries the declaration, and the runtime launches the server and reads its tool list. A runtime that asks the client for consent before invoking a tool gets a refusal, and the tool is not invoked. The protocol's stdio server declaration carries no trust or approval field, so a declared server cannot be marked pre-authorized on the wire. The only lever is the runtime's own configuration, reached through the operator's `agent.command` and whatever configuration that runtime reads: an approval mode that does not ask, or a rule pre-authorizing the tools of the server Sortie declares.

This was measured on `gemini` and on `kiro-cli`, the two runtimes above the gated suite drives end to end, and both behave this way: under a posture that asks, the declared server is delivered and its tool is still never called. The other runtimes in these notes are unmeasured on this point, and an absent probe is never read as a measured negative.

`kiro-cli` adds a second lever that has nothing to do with posture. Its tool servers are enabled only when the runtime's backend serves it a governance profile, and that request fails for an API-key credential, so a session authenticated that way drops every declared server before any posture question arises. A posture switch cannot recover it and no configuration file can either. `docs/kiro-adapter-notes.md` carries the detail.

When the situation arises, the adapter reports it once per session on two surfaces. The notification reaches the orchestrator's generic event handling, which records the event type at `Debug` and carries the message itself nowhere but the running entry's last agent message, where the next message-carrying event overwrites it. The `Warn` record is the only surface that outlives the run, for as long as the run's log is kept. That record is raised by a refused permission request, so it never fires under a posture switch that puts the runtime in a mode that does not ask: an untrusted workspace silently dropping the declared servers then leaves no signal of its own on this path.

## Verifying a change

Unit tests cover wire parsing, event normalization, permission replies, capability lowering and session continuation, and they need no credential. The tests that drive a real binary are env-gated on `SORTIE_CLIENTPROTOCOL_TEST=1` and skip cleanly without it; keep them skipping cleanly rather than failing. Because this kind names no default runtime, `SORTIE_CLIENTPROTOCOL_COMMAND` supplies the launch command together with whatever flag or subcommand puts that binary into protocol mode, and its absence skips the suite exactly as a missing credential does in the sibling agent suites. The binary needs a working credential in the environment.

A separate, Unix-only qualification profile drives a live model across several surfaces for minutes to decide whether a runtime is eligible for this transport. It is gated on `SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST=1` and skips cleanly without it. Once enabled it reads four coordinates: `SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND` names exactly one executable path with no flags, because the profile appends each surface's own; `SORTIE_CLIENTPROTOCOL_QUALIFICATION_MODEL` names the one model identifier applied to every surface; `SORTIE_CLIENTPROTOCOL_QUALIFICATION_AUTH_ENV_NAMES` lists the authentication environment variable names the profile forwards, names only, never values, and may be set to an empty list for a runtime that authenticates from a stored login instead of from the environment, which is a declaration rather than an omission: the coordinate must still be set, so forgetting it fails the run exactly as before; and `SORTIE_CLIENTPROTOCOL_QUALIFICATION_PROFILE` names one readable runtime profile document carrying the runtime's entry points, native recognizers, and declared gaps together, renamed from the coordinate this replaces. It is now REQUIRED: its absence fails the run rather than resolving to an empty declaration set, because the run's entry points live there. A profile may declare a capability gap, a surface the runtime offers no entry point for, or both; declaring a surface absent is corroborated by its own launch before any probe spends anything else, so a false declaration stops the run rather than being trusted. An enabled gate with any coordinate missing or invalid fails the run rather than skipping it.

This live profile sits behind two triggers rather than one. An offline staleness gate runs on every push and pull request, at no model cost: editing a tracked profile, its measurement artifact, its notes document, or a published route sample keeps that gate red until a fresh, digest-bound measurement replaces the stale one. Separately, the nightly conformance job compares the tracked profile's `capability_gap_labels` against the live capability-gap notice the same turn already collects, through `SORTIE_CLIENTPROTOCOL_PROFILE`, so a capability that regresses or improves after qualification is caught on a clock rather than tolerated until someone edits a file.

The two structured native surfaces' `limit_reached` inducer is bounded by the native transport: the largest context-overflow estimate it can produce is 2,129,920 tokens (`floor((8,388,608 + 2 + 131,071) / 4)`, using the whole of standard input, the two-character separator the runtime inserts between it and the prompt, and the whole of a single `execve` argument). Raising the filler constant buys nothing past that ceiling, because the constant already equals the runtime's own standard-input truncation limit; a future runtime whose context limit exceeds about 2.13 million tokens needs a different native inducer rather than a larger number. The protocol surface carries no such ceiling.

The gated suite asserts the shape of what the agent actually sends rather than only that a turn finished. It has to: the client substitutes a default for a field it cannot read instead of refusing the message, so a change in the shape or the meaning of a message it already handles does not fail at runtime, and this suite is the only place that class surfaces. A runtime this suite accepts therefore completes a turn, answers it with the `end_turn` stop reason, streams at least one text `agent_message_chunk`, and reports `agentInfo` on its handshake. A runtime that does none of those is out of scope for the suite rather than a defect to fix in it.

One rule asserts a model choice rather than a protocol shape: that the prompt written to force a tool call produces a `tool_call` update and a terminal `tool_call_update`. It ships because three consecutive live runs each carried the pair, with the tool call's own identifier picked up by the terminal update every time rather than the two counts merely both being non-zero. If it ever goes red, repeat that measurement rather than disabling the rule.
