# Codex adapter notes

Working notes for anyone changing Sortie's Codex adapter in `internal/agent/codex`: why this one holds a process open when the rest do not, where its trust and approval models collide with ours, and what will cost you a day.

Last updated: 2026-08-23

## Where to get the volatile facts

The binary describes its own protocol. `codex app-server generate-json-schema --out <dir>` writes the JSON Schema set for the wire protocol and `codex app-server generate-ts --out <dir>` writes the equivalent TypeScript declarations, both generated from the types the running binary uses. That makes them the version-exact answer to any wire-format question, and the first thing to consult before you believe a payload shape from anywhere else, this file included. They are easy to miss because top-level help marks their parent command experimental, and both require an output directory.

Two cautions about that schema, and both are durable.

The schema is authoritative for what the app-server *declares* and unreliable for what it *enforces*, and the two come apart field by field rather than surface by surface. One request rejects every value a field does not declare; another field is marked required and is not enforced at all. Read enforcement off a probe of the specific field you care about, never off the schema.

Two different schemas cover this ground and they govern different surfaces: the vendor's published configuration schema describes what an operator may write in the agent's own configuration file, while the protocol schema each release generates describes what may travel on a request. They agree almost everywhere. Where they disagree, the surface the value crosses governs, and this bites in practice because our pass-through configuration reaches wire fields and not the agent's configuration file. A value an operator has watched work in their own config file can be rejected outright when Sortie sends the same string on the wire, because the configuration type accepts aliases the wire type does not.

One more before you probe anything: resolve what the name on the path actually is. A version manager puts a shell-script shim where the binary appears to be, so `file`, `strings`, and argument-parser probes describe the launcher rather than the agent, and both a clean exit and a failure may be the shim's rather than the binary's.

## Shape of the integration

This is the one adapter in the fleet that does not fork per turn. `StartSession` launches the app-server once, performs the initialization handshake, authenticates if the runtime has no credentials of its own, and starts or resumes a thread; every turn afterwards is a request on that one thread over JSON-RPC on stdio. The session's identity is the thread ID. The default command is two tokens, and only the first is resolved on the path.

Everything downstream follows from the process outliving the turn. There is no per-turn exit code to classify, so the adapter reports work as unobservable and the shared decision's zero-work row can never fire for it. The failure signal in its place is the channel itself: stdout closing before the turn reports completion, a read error on stdout, a failed write of the turn request. Teardown belongs to the session, not the turn, which is why the graceful-then-forced shutdown sequence lives in `StopSession` and not on the cancellation path.

The handshake requests the experimental capability, which is what unlocks client-side tool registration on thread start. If you change the handshake, expect tool registration to be the first thing that stops working.

Resume is a soft path. When resuming a thread fails, the adapter logs a warning and starts a fresh thread rather than failing the session. That keeps a run alive at the cost of a real trap: a session that looks resumed may be a new thread with no history at all, and nothing downstream distinguishes the two. Check the log before concluding that the model forgot something.

## Turns, cancellation, and what an interrupt means

A Sortie turn is one turn request and the events that follow it until the runtime reports completion. On cancellation the adapter writes a single best-effort interrupt straight to the runtime's stdin, deliberately not through the cancelled context, and then keeps reading only until the runtime's own completion arrives, stdout closes, or the read timeout elapses. Past that bound it returns cancelled rather than waiting forever for an acknowledgement that may never come. The runtime acknowledges no client response at all, which is why that bound exists and why it is also the backstop for every refusal described below.

One distinction is deliberate and worth preserving. A turn the runtime reports as interrupted when *we* cancelled it is a cancellation. The same status when our context was still live is a failure, because from the orchestrator's side an interruption nobody asked for must not release the claim in place of a retry.

## Approvals and requests only a person can answer

The thread-level approval policy defaults to never-ask, and config validation refuses any other value: an unattended run has nobody to answer, so a policy that lets the agent stop and ask is rejected before the run starts rather than discovered at 3am.

Requests still arrive, and the shape of a refusal is dictated by the response schema of the specific request, not by our preference. Some responses can express a decline, and the agent proceeds by another route inside the same turn. Others declare no value that means no: their schema requires a grant object or an answer, so the only way to refuse is a JSON-RPC error, and those requests end the attempt as human-input-required because refusing them leaves the agent nothing to continue with. When you add a new recognized request, decide which of those two it is by reading its response schema, then let `agentcore.DecideHumanRequest` classify it. The adapter never constructs a posture itself.

The error code and message used for the refusals that cannot be expressed as a result are pinned rather than derived. The schema enumerates no values for either, and the runtime acknowledges nothing, so an unaccepted code produces silence rather than an error. The bounded wait above is what keeps that silence from hanging a turn.

An orchestrator-initiated cancellation outranks any of this: a recognized request that arrives after the context is already done finalizes as cancelled, not as human input required.

## The trusted-project trap

On thread start the runtime may record the working directory as a trusted project in the user-level configuration, on its own, with no prompt on this surface (the interactive UI asks first; the app-server does not). It does that when the request carries a working directory, the path has no trust level yet, and the requested sandbox permits writing there, which is exactly the shape of a normal Sortie run.

The decision then outlives everything you would expect to reset it. It is recorded once, against the resolved repository root or the working directory, and it persists in the user's own configuration rather than in the workspace, so deleting the workspace does not undo it. A path already marked trusted keeps loading that project's own configuration, and starting the MCP servers that configuration declares, on later runs whatever sandbox is requested, because the sandbox is consulted only when deciding whether to record trust for a path that has none yet. Since Sortie derives the workspace path from the issue identifier and reuses it, one earlier run arms every later run at that path.

Two consequences worth holding onto. Anything that arrives with the checkout under the agent's own configuration directory is live in the same run that grants the trust. And servers started from that layer run as children of the app-server and are not confined by the sandbox value we send: that value governs the commands the agent executes, not the transports it connects to.

## Tools the prompt advertises and the session cannot call

A Codex agent is told about tools it has no way to invoke. The prompt carries Sortie's tool advertisement, the thread carries no tool declarations, and nothing reconciles the two. Each half has its own cause, and both are worth knowing before you go debugging either one.

The session half. The adapter reads its tool registry from a key in the raw configuration map it is constructed with, and no shipped code path puts a registry there: only tests do. An operator cannot supply one from the workflow file either, because the value is type-asserted to the registry type and anything parsed out of YAML fails that assertion. The registry is therefore nil in a real run, the thread-start request carries no client-side tool declarations at all, since those are attached only when the tool list is non-empty, and a Codex session can call none of Sortie's tools.

The prompt half. The worker appends the tool advertisement on the first turn of a session, and nothing in that path is conditioned on the agent kind. What gates it is the per-session tool registry the worker builds for that turn coming back non-empty, and one tool registers on nothing more than the workspace path being set, which the worker always has by then. An ordinary Codex run therefore carries the advertisement. The section is dropped only when building that registry fails outright, which logs a warning; the registry built once at startup is a fallback the orchestrator does not use.

The dispatch path itself is complete and covered by tests, so what is missing is the wiring, not the mechanism.

The generated MCP configuration path is discarded: the adapter neither stores it nor reads it. This adapter writes no MCP configuration and passes no MCP argument.

Whether Sortie's tools reach an agent at all is a per-adapter property, and this is the adapter where the answer is currently no.

## Usage accounting

Usage does not ride on the turn's completion payload; it arrives on its own notification, and if you go looking for a usage member on the completion event you will not find one.

The totals on that notification are thread-cumulative, spanning every turn of the thread including turns from an earlier run that resumed it. The adapter recovers this run's own contribution by capturing a baseline at the first notification belonging to the current turn and subtracting it thereafter. A notification belonging to a different turn does not emit anything; it raises the baseline instead.

One subtlety to preserve: a payload that carries no usage object at all is distinguishable from one reporting zeroes, because the field is a pointer. The absent case emits nothing and leaves the measurement flag alone, which is what keeps "we do not know" different from "it cost nothing". Flatten that to a value type and the distinction dies silently.

## Failure modes worth recognizing

Stdout closing before a turn completes is reported as a port failure, and it is the most common way a broken session presents: the handshake succeeded, a turn started, and then nothing.

The stdio reader caps a single line, and that cap is ours rather than a documented server limit. A single very large notification would fail the read rather than being split. If a turn dies mid-way on a read error after a large diff or tool output, suspect the cap first.

A handshake or authentication failure tears the subprocess down and returns before any session exists, so those never present as turn failures.

## Verifying a change

Unit tests cover argument and request construction, the handshake, event parsing, tool dispatch, and disposition, and they are unusually thorough about the protocol because there is no cheap way to re-derive it. The tests that drive the real binary are env-gated on `SORTIE_CODEX_TEST=1` and skip cleanly without it; keep them skipping cleanly rather than failing. `SORTIE_CODEX_COMMAND` overrides the command and `SORTIE_CODEX_MODEL` the model, and the binary needs a working credential in the environment.
