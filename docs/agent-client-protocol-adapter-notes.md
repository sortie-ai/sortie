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

This is the one runtime the gated suite was driven against end to end: a session starts, a turn runs to a completed outcome, a `session/request_permission` request arrives mid-turn and the adapter refuses it inside the protocol with the turn continuing past the refusal to completion, and the session stops cleanly afterward.

Session continuation was probed by hand against this runtime, not through the gated suite. `session/load` replays the prior turn's messages, observed as `user_message_chunk` notifications arriving before the load response itself returns, when the workspace directory already carries earlier session history. Against a workspace used for the first time, whose only session was ended by a process-group kill with no graceful phase ahead of it, a reload of that session answers a JSON-RPC error reporting no prior session for the workspace instead of replaying anything; the adapter observes this exactly as it observes any other unconfirmed load, lowering the session continuation entry and falling back to a fresh session without failing the run. That probe predates the graceful phase teardown now runs, and it has not been repeated against it. `session/resume` is not exercised: this runtime advertises no `sessionCapabilities` object at all, so the adapter never selects it.

## Runtimes not observed

Each of the following has an explicit gap rather than a measured result, so an absent probe is never later read as a measured negative.

### `kiro-cli`

The `acp` subcommand exists but appears only under `--help-all`, not the default help output. Its protocol surface is not observed here because this host carries no stored login for it, so the probe fails before the handshake completes.

### `claude`

The Claude Code CLI exposes no protocol entry point in its own help output. There is nothing on this host to probe.

### `codex`

Codex exposes no protocol entry point either. Its `app-server` speaks a different JSON-RPC dialect, the one this project's separate `codex` adapter already drives, and that dialect is not the Agent Client Protocol.

## Verifying a change

Unit tests cover wire parsing, event normalization, permission replies, capability lowering and session continuation, and they need no credential. The tests that drive a real binary are env-gated on `SORTIE_CLIENTPROTOCOL_TEST=1` and skip cleanly without it; keep them skipping cleanly rather than failing. Because this kind names no default runtime, `SORTIE_CLIENTPROTOCOL_COMMAND` supplies the launch command together with whatever flag or subcommand puts that binary into protocol mode, and its absence skips the suite exactly as a missing credential does in the sibling agent suites. The binary needs a working credential in the environment.

A separate, Unix-only qualification profile drives a live model across several surfaces for minutes to decide whether a runtime is eligible for this transport. It is gated on `SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST=1` and skips cleanly without it. Once enabled it reads four coordinates: `SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND` names exactly one executable path with no flags, because the profile appends each surface's own; `SORTIE_CLIENTPROTOCOL_QUALIFICATION_MODEL` names the one model identifier applied to every surface; `SORTIE_CLIENTPROTOCOL_QUALIFICATION_AUTH_ENV_NAMES` lists the authentication environment variable names the profile forwards, names only, never values; and `SORTIE_CLIENTPROTOCOL_QUALIFICATION_DECLARED_GAPS` names one readable path to an operator declaration document, read once at coordinate resolution. An enabled gate with a coordinate missing or invalid fails the run rather than skipping it, except for the declaration coordinate: its absence is a valid, deliberate state that resolves to an empty declaration set, and only an unreadable or rejected file at that path fails the run. Neither the release workflow nor the nightly workflow runs this profile.

The gated suite asserts the shape of what the agent actually sends rather than only that a turn finished. It has to: the client substitutes a default for a field it cannot read instead of refusing the message, so a change in the shape or the meaning of a message it already handles does not fail at runtime, and this suite is the only place that class surfaces. A runtime this suite accepts therefore completes a turn, answers it with the `end_turn` stop reason, streams at least one text `agent_message_chunk`, and reports `agentInfo` on its handshake. A runtime that does none of those is out of scope for the suite rather than a defect to fix in it.

One rule asserts a model choice rather than a protocol shape: that the prompt written to force a tool call produces a `tool_call` update and a terminal `tool_call_update`. It ships because three consecutive live runs each carried the pair, with the tool call's own identifier picked up by the terminal update every time rather than the two counts merely both being non-zero. If it ever goes red, repeat that measurement rather than disabling the rule.
