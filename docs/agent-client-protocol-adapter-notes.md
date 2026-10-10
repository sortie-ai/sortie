# Agent Client Protocol adapter notes

Working notes for anyone changing Sortie's Agent Client Protocol adapter in `internal/agent/clientprotocol`.

Last updated: 2026-10-10

This kind names no default runtime. The operator's `agent.command` picks the binary and the flag or subcommand that puts it into protocol mode, so these notes cover several runtimes. A runtime's version is never pinned here; the adapter records it per session from the `initialize` handshake. Runtime-specific traps live in `docs/gemini-adapter-notes.md` and `docs/kiro-adapter-notes.md`.

## Pinned schema artifact

The generated wire types in `wire_gen.go` come from a pinned release of the protocol's stable schema. The tag and commit live in `schemagen/generate.go` and in `PROVENANCE.txt` beside the schema copy under `testdata/`. Changing them is a pin move, not a routine edit.

The adapter deliberately ignores some members of the pinned schema. It reports the tool kind instead of the optional tool `name` on a tool call. It does not advertise the client capabilities for notice and compaction session updates, and does not read those updates.

A scheduled check reports when the publisher's stable schema releases differ from the pin. It never moves the pin, and it closes its own report on the first run that finds no difference.

## Runtimes

What each runtime showed in its `initialize` handshake:

| Runtime | Protocol entry point | What was observed |
|---|---|---|
| `gemini` | `--acp` (`--experimental-acp` is deprecated) | Driven end to end by the gated suite. Advertises no `sessionCapabilities` object. |
| `kiro-cli` | `acp` subcommand, listed only under `--help-all` | Driven end to end. Empty `sessionCapabilities`; `mcpCapabilities.sse` is false. |
| `copilot` | `--acp` | Handshake only. `sessionCapabilities` has `close` and `list`, not `resume`. |
| `opencode` | `acp` subcommand | Handshake only. `sessionCapabilities` has `close`, `fork`, `list`, and `resume`. |
| `claude` | none | The CLI exposes no protocol entry point. |
| `codex` | none | `app-server` speaks a different JSON-RPC dialect, the one the `codex` adapter drives. |

A local launch needs up to three command-line switches: one that puts the runtime on the protocol, one that trusts the workspace so declared tool servers are not dropped, and one for a posture that does not ask before running a tool. Runtimes spell them differently, and no shared code branches on which runtime it is.

| Switch | `gemini` | `kiro-cli` |
|---|---|---|
| Protocol | `--acp` | `acp` subcommand |
| Trust | `--skip-trust` | `-a` |
| Posture | `--approval-mode yolo` | `-a`, the same switch |

`kiro-cli` uses one switch for trust and posture, so you cannot take back one without the other.

Some runtimes send many notifications under their own vendor method prefix. They are notifications, not requests, so the client answers none of them and depends on none of them.

## Usage reporting

The kind declares `turn_end` arrival and `per_model` attribution; a remote launch resolves to `none`/`none`. The figure does not come from the wire. A `usage_update` session update is only logged at debug level. The `_meta.quota` block on a prompt result is a presence signal and a lower bound, never the accounting figure.

The figure comes from a measurement source outside the protocol. Each source registers itself into `usagesource` from an `init` function, so adding one is additive. Before the first start, every registered source sees the launch target, and a source that arms something is carried only when the command line names its runtime. The handshake confirms at most one carried source, the first whose `Recognize` accepts the runtime's reported name, and sets `tokenCounts` to `ours`.

If the start carried a source the handshake does not confirm, or lacked the one that recognizes the runtime, the runtime is torn down and started again with the right source before the session is created. This happens at most twice, and each relaunch is logged at Info.

A source accepts or refuses a record by whether it has every field the source's mapping needs, never by the runtime's version. The first drain that yields no record lowers `tokenCounts` to `gap` for the rest of the session. A session with no applicable source, which includes every remote launch, is unmeasured, and the token ceiling is inactive.

The source for `gemini-cli`, under `usagesource`, reads the runtime's OpenTelemetry output first and its session journal as a backstop. It maps `input_token_count` to input (the cache read is already inside it), `output_token_count` plus `thoughts_token_count` to output, and `cached_content_token_count` to the cache-read subset. It discards the runtime's own total and `tool_token_count`.

The wait for a record is bounded and ends early once the record reaches the wire's lower bound. The telemetry is batched behind a five-second tick that no configuration shortens. A turn that reached the model but ended without its own prompt result still gets one bounded wait, with no lower bound to end it early, and what it recovers does not change the turn's disposition.

A turn that made a model request no figure accounts for is recorded as "spend occurred, amount partly unknown". The issue's summed total then becomes a lower bound, which is different from a measured zero. Every turn of a remote launch that reports spend on the wire ends in this state, because nothing reads the far host's files.

## Session close and delete

Teardown sends `session/close` when the handshake advertises `sessionCapabilities.close`. This path has fixture coverage only, because the runtimes that advertise it were probed at the handshake only.

When a handshake advertises `sessionCapabilities.delete`, a verification session's teardown sends `session/delete` instead, so the verification conversation does not stay in the runtime's store. No measured runtime advertises it.

## Credential verification

The `initialize` wait during credential verification is the larger of `agent.read_timeout_ms` and `agentcore.CredentialExchangeBound`, because a runtime may talk to its own backend before it answers. `kiro-cli` with an API key takes far longer than the 5-second default read timeout to answer `initialize`; with a stored device login it answers in well under a second. With the longer wait, a refused key fails its first prompt with a `-32603` error whose `data` names the invalid bearer token.

A `-32000` response to `initialize` or `session/new` means the runtime refused its credential, on a verification session and a working one alike. A runtime that exits before it answers any startup request gets the shared early-exit report, not a credential diagnosis. A connection lost while the runtime stays alive past the observation grace keeps `port_exit`'s transport message. A failed `session/load` or `session/resume` still falls back to a fresh session and is never read as a credential refusal.

## Session load spacing

The adapter holds a `session/load` call until the clock leaves the UTC minute in which this process created the session. The wait runs on the open connection, after the handshake and the negative control, and is bounded at one minute.

It exists because one runtime's `session/load` inside that minute fails and permanently destroys the session's resumability. `docs/gemini-adapter-notes.md` has the evidence.

The session continuation capability record cannot do this job, because the call that would observe the defect is the call that causes it. A per-process record of when this adapter created each session is the only way to keep the load out of that minute.

This wait is load-bearing. Do not trim it as dead time: removing it brings the defect back silently, and nothing notices until a run comes back with no history.

Limits:

- A session created by an earlier process is not covered, because the record lives in memory.
- A wall-clock step backward larger than the deferral can still land the load inside the creation minute, because the deferral is clamped at one minute instead of re-reading the clock.
- On an SSH launch the wait uses the orchestrator host's clock, so clock skew between the hosts larger than the deferral can still land the load inside the runtime host's creation minute.

## What a delivered tool server needs to be callable

Delivery is not the problem: `session/new` carries the declaration, and the runtime launches the server and lists its tools. But a runtime that asks the client for consent before a tool call gets a refusal, and the tool is not called. The protocol's stdio server declaration has no trust or approval field, so Sortie cannot mark a server as pre-approved on the wire. The only lever is the runtime's own configuration, reached through `agent.command`: an approval mode that does not ask, or a rule that pre-approves the tools of Sortie's server.

Both `gemini` and `kiro-cli` behave this way: under a posture that asks, the declared server is delivered and its tool is never called. The other runtimes are unmeasured on this point.

`kiro-cli` adds a second condition: with an API-key credential it drops every declared server before any posture question. See `docs/kiro-adapter-notes.md`.

The adapter reports a refused permission request once per session, on two surfaces. The notification reaches the orchestrator's generic event handling, which logs the event type at `Debug` and keeps the message only as the running entry's last agent message, where the next message overwrites it. The `Warn` log record is the only surface that outlives the run.

That record comes from a refused permission request. Under a posture that never asks it never fires, so an untrusted workspace that silently drops the declared servers leaves no signal on this path.

## Reasoning level

The kind forwards no reasoning level. It reads no `effort` key and never sets a session configuration option for one, because the option ids differ per runtime and some runtimes offer none. The operator writes the runtime's own reasoning option in `agent.command`. The registration declares `EffortNotForwarded`, so a workflow that sets `effort` in this kind's block gets `agent.effort.not_forwarded` instead of a silent drop. A completeness test fails when a non-test file of this package reads the key.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_CLIENTPROTOCOL_TEST=1`. Because the kind has no default runtime, `SORTIE_CLIENTPROTOCOL_COMMAND` supplies the launch command, and the suite skips without it. The other coordinates are described in the test file and in `docs/agent-adapter-howto.md`. Traps:

- The scripted tool-server case needs the runtime's trust and posture switches in the command. Without the trust switch `gemini` drops the declared server; without the posture switch it asks, gets refused, and the call never reaches the server.
- `SORTIE_CLIENTPROTOCOL_ASKING_COMMAND`, used by the scripted refusal case, must leave out the posture switch. A runtime that never asks runs the guarded command and the case fails.
- The scripted cases isolate `HOME` and the XDG directories, so a version-manager shim that reads `HOME` fails to start. Name the runtime by an absolute path.
- The client fills in a default for a field it cannot read instead of refusing the message. A change in the shape of a message it already handles therefore does not fail at runtime. The gated suite's shape assertions are the only place that shows up.
- Whether the model calls a tool when asked is the model's choice, not a protocol obligation. The live suite logs a missing tool call as a `model-dependent observation` and does not fail on it. It still fails when a tool call arrives without a normalized tool result. The scripted cases drive the tool call deterministically and do fail without it.

The qualification profile in `tools/qualify` drives a live model for minutes and spends real credits. It has its own gate; `docs/agent-adapter-howto.md` describes its coordinates and the `make qualify-*` targets. Things that are not obvious:

- A named authentication variable with no value is a clean skip, because the operator mints a temporary credential for each run. Every other missing or malformed coordinate fails the run, so a typo cannot pass green.
- An offline staleness gate on every push fails when a tracked profile changes without a fresh measurement, or when a runtime's notes disagree with its tracked measurement.
- The nightly compares the tracked profile's `capability_gap_labels` with the live gap labels through `SORTIE_CLIENTPROTOCOL_PROFILE`, so a capability that changes after qualification is caught. A session that goes unmeasured after its first turn counts `token counts` as a gap even though no notice names it.
