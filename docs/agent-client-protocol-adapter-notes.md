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

Session continuation was probed by hand against this runtime, not through the gated suite. `session/load` replays the prior turn's messages, observed as `user_message_chunk` notifications arriving before the load response itself returns, when the workspace directory already carries earlier session history. Against a workspace used for the first time, whose only session was ended through this adapter's own teardown, a bounded graceful phase ahead of the process-group kill, a reload of that session answers a JSON-RPC error reporting no prior session for the workspace instead of replaying anything; the adapter observes this exactly as it observes any other unconfirmed load, lowering the session continuation entry and falling back to a fresh session without failing the run. `session/resume` is not exercised: this runtime advertises no `sessionCapabilities` object at all, so the adapter never selects it.

## Runtimes not observed

Each of the following has an explicit gap rather than a measured result, so an absent probe is never later read as a measured negative.

### `kiro-cli`

The `acp` subcommand exists but appears only under `--help-all`, not the default help output. Its protocol surface is not observed here because this host carries no stored login for it, so the probe fails before the handshake completes.

### `claude`

The Claude Code CLI exposes no protocol entry point in its own help output. There is nothing on this host to probe.

### `codex`

Codex exposes no protocol entry point either. Its `app-server` speaks a different JSON-RPC dialect, the one this project's separate `codex` adapter already drives, and that dialect is not the Agent Client Protocol.
