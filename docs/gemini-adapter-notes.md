# Gemini CLI adapter notes

Working notes for anyone dealing with Gemini CLI through Sortie's generic Agent Client Protocol adapter in `internal/agent/clientprotocol`: the one fact that follows from Gemini having no adapter code of its own, the two shapes in which token accounting under-reports spend, and what a normal-looking stop reason does not tell you.

## Where to get the volatile facts

Gemini's own version is never pinned in prose here; the adapter records it per session from that session's own `initialize` handshake, and the account's actual model set comes from Google's model-listing endpoint, which is the only authoritative answer because it depends on the key's subscription. Read the flag surface off `gemini --help` on the binary you are targeting. Upstream source lives in `google-gemini/gemini-cli`; when a symptom looks like a protocol bug rather than a Sortie bug, treat the bundled JavaScript that the installed version actually ships as the source of record, because the TypeScript source tree and the shipped bundle can drift apart between releases.

## The fact everything else follows from

Gemini has no adapter package, no registered kind, no adapter metadata, and no identity branch anywhere in Sortie. The operator reaches it by pointing the generic `agent-client-protocol` kind's `agent.command` at the `gemini` binary with the `--acp` flag; an older `--experimental-acp` spelling still works but is deprecated. Every behavior described below is therefore a property of this one vendor's protocol implementation meeting the generic adapter, not a Gemini-specific code path in Sortie, and there is nowhere in the codebase to special-case a Gemini quirk short of teaching the generic adapter about it.

## Token accounting misses spend in two shapes

This runtime never sends the protocol's standard usage notification. Token counts instead ride a vendor extension, `_meta.quota`, attached to the result of a completed turn, which by itself would just mean parsing a vendor-shaped field rather than a standard one. Two gaps sit underneath that. A cancelled turn's result carries no `_meta` at all, so a turn Sortie cancels reports zero tokens spent even though the model was billed for them; Sortie cancels turns on both stall detection and budget decisions, so this undercount is reachable in ordinary operation rather than only in an edge case. And even where `_meta.quota` is present, it carries only input and output token counts: cached and thought tokens never appear in it, so a running total built from this path understates the real cost.

## A stop reason of end_turn does not mean the turn ended cleanly

Of the five stop reasons the protocol defines, this runtime's own code produces three: `end_turn`, `max_turn_requests`, and `cancelled`. Loop detection reports as `max_turn_requests`. `max_tokens` is reachable only through the runtime's own pre-emptive context-overflow predictor, which fires before the model's stream is actually exhausted; the stream's own genuine token-limit signal never reaches the protocol layer as `max_tokens`, because the handler that catches an invalid stream folds that signal into `end_turn` alongside the model's safety and recitation blocks. `refusal` is never assigned: no code path in this runtime produces it. So a model declining to answer on safety grounds, and a turn that genuinely ran out of context, both surface as an ordinary, successful-looking `end_turn`, and nothing in the response distinguishes either one from a turn that actually completed as asked.

## Session continuation replays history, with two traps

Continuing a session is implemented and does work: `session/load` rebuilds the prior conversation and streams it back as genuine `session/update` notifications, one per historical turn. Two things about that replay need care.

The first is an upstream ordering defect. The `session/load` response can reach the wire before its replay notifications finish sending, because the runtime does not wait for the replay to complete before responding, even though the protocol expects a response only after the full replay has gone out. Sortie's own continuation logic already tolerates this: it starts watching for replayed chunks before issuing the load call, and after the response arrives it waits a bounded interval for any chunk still in flight before deciding there was no replay to see. That wait is load-bearing, not redundant, and must not be trimmed as dead time.

The second trap is more expensive and unrelated to the first. A `session/load` issued in the same UTC minute as the `session/new` that created the session fails and permanently destroys that session's resumability, including every later attempt to load it in a following minute. This is an open upstream defect, confirmed live: moving the load into the following UTC minute, from a separate process, produced a successful load with a full history replay in two independent runs, while a same-minute load reliably failed both times it was tried. Anything that seeds a session and reloads it again shortly afterward, including a routine redispatch, can land inside the same minute and lose that session for good.

## Tool-server delivery depends on workspace trust, and fails silently

Tool servers declared in `session/new` are honored: the runtime merges them over whatever servers its own settings file already configures, matching by name with the request winning on a collision, and stdio, sse, and http transports are all supported. Delivery is gated on whether the runtime considers the workspace trusted, though, and an untrusted workspace fails closed with no signal anywhere: `session/new` still returns success, the declared servers are dropped without a trace, and nothing in the response or in a later notification marks that anything went wrong. The cause is that the runtime's trust check only raises an error in its own headless mode, and it treats protocol mode as interactive rather than headless, so the guard that would reject an untrusted workspace on the command line never fires here.

## There is nothing to call session/close on

This runtime advertises no `sessionCapabilities` object at all in its handshake. Sortie's adapter decides whether to call `session/close` based on that capability being present, so against this runtime there is never a capability to select, and a session here is never closed through the protocol.

## Our own teardown outruns a graceful exit

Sortie ends a client-protocol session by killing its process group, with no graceful-exit step and nothing that waits for one first. A runtime that would otherwise flush its conversation history to disk on a clean exit never gets the chance to under this teardown. Gemini is exactly such a runtime: it persists session history to disk as the conversation happens, but a process killed mid-lifecycle, rather than allowed to exit on its own, never finishes that write, and the next `session/load` against a workspace whose only session ended that way reports no prior session to resume rather than replaying one.
