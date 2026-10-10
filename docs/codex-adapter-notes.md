# Codex adapter notes

Working notes for anyone changing Sortie's Codex adapter in `internal/agent/codex`.

Last updated: 2026-10-10

## Where to get the volatile facts

The binary describes its own protocol. `codex app-server generate-json-schema --out <dir>` writes the JSON Schema set for the wire protocol and `codex app-server generate-ts --out <dir>` writes the equivalent TypeScript declarations, both generated from the types the running binary uses. That makes them the version-exact answer to any wire-format question, and the first thing to consult before you believe a payload shape from anywhere else, this file included. They are easy to miss because top-level help marks their parent command experimental, and both require an output directory.

Two cautions about that schema, and both are durable.

The schema is authoritative for what the app-server *declares* and unreliable for what it *enforces*, and the two come apart field by field rather than surface by surface. One request rejects every value a field does not declare; another field is marked required and is not enforced at all. Read enforcement off a probe of the specific field you care about, never off the schema.

Two different schemas cover this ground and they govern different surfaces: the vendor's published configuration schema describes what an operator may write in the agent's own configuration file, while the protocol schema each release generates describes what may travel on a request. They agree almost everywhere. Where they disagree, the surface the value crosses governs, and this bites in practice because our pass-through configuration reaches wire fields and not the agent's configuration file. A value an operator has watched work in their own config file can be rejected outright when Sortie sends the same string on the wire, because the configuration type accepts aliases the wire type does not.

One more before you probe anything: resolve what the name on the path actually is. A version manager puts a shell-script shim where the binary appears to be, so `file`, `strings`, and argument-parser probes describe the launcher rather than the agent, and both a clean exit and a failure may be the shim's rather than the binary's.

## Shape of the integration

This is the one adapter in the fleet that does not fork per turn. `StartSession` launches the app-server once, performs the initialization handshake, authenticates if the runtime has no credentials of its own, and starts or resumes a thread; every turn afterwards is a request on that one thread over JSON-RPC on stdio. The session's identity is the thread ID. The default command is two tokens, and only the first is resolved on the path.

Everything downstream follows from the process outliving the turn. There is no per-turn exit code to classify, so the shared decision's work rows are structurally unreachable for this adapter: it observes no per-turn process exit at all. The failure signal in its place is the channel itself: stdout closing before the turn reports completion, a read error on stdout, a failed write of the turn request. Teardown belongs to the session, not the turn, which is why the graceful-then-forced shutdown sequence lives in `StopSession` and not on the cancellation path.

The handshake requests the experimental capability, but the adapter registers no client-side tools on thread start. Sortie's tools reach the agent through the MCP sidecar described below.

Resume is a soft path. When resuming a thread fails, the adapter logs a warning and starts a fresh thread rather than failing the session. That keeps a run alive at the cost of a real trap: a session that looks resumed may be a new thread with no history at all, and nothing downstream distinguishes the two. Check the log before concluding that the model forgot something.

## Turns, cancellation, and what an interrupt means

A Sortie turn is one turn request and the events that follow it until the runtime reports completion. On cancellation the adapter writes a single best-effort interrupt straight to the runtime's stdin, deliberately not through the cancelled context, and then keeps reading only until the runtime's own completion arrives, stdout closes, or the read timeout elapses. Past that bound it returns cancelled rather than waiting forever for an acknowledgement that may never come. The runtime acknowledges no client response at all, which is why that bound exists and why it is also the backstop for every refusal described below.

One distinction is deliberate and worth preserving. Any status the runtime reports in a `turn/completed` notification after *we* already cancelled the turn is reported as a cancellation, whatever word the runtime used. The same `interrupted` status when our context was still live stays a failure, because from the orchestrator's side an interruption nobody asked for must not release the claim in place of a retry.

## Approvals and requests only a person can answer

The thread-level approval policy defaults to never-ask, and config validation refuses any other value: an unattended run has nobody to answer, so a policy that lets the agent stop and ask is rejected before the run starts rather than discovered at 3am.

Requests still arrive, and the shape of a refusal is dictated by the response schema of the specific request, not by our preference. Some responses can express a decline, and the agent proceeds by another route inside the same turn. Others declare no value that means no: their schema requires a grant object or an answer, so the only way to refuse is a JSON-RPC error, and those requests end the attempt as human-input-required because refusing them leaves the agent nothing to continue with. When you add a new recognized request, decide which of those two it is by reading its response schema, then let `agentcore.DecideHumanRequest` classify it. The adapter never constructs a posture itself.

The error code and message used for the refusals that cannot be expressed as a result are pinned rather than derived. The schema enumerates no values for either, and the runtime acknowledges nothing, so an unaccepted code produces silence rather than an error. The bounded wait above is what keeps that silence from hanging a turn.

An orchestrator-initiated cancellation outranks any of this: a recognized request that arrives after the context is already done finalizes as cancelled, not as human input required.

## The trusted-project trap

On thread start the runtime may record the working directory as a trusted project in the user-level configuration, on its own, with no prompt on this surface (the interactive UI asks first; the app-server does not). It does that when the request carries a working directory, the path has no trust level yet, and the requested sandbox permits writing there, which is exactly the shape of a normal Sortie run.

The decision then outlives everything you would expect to reset it. It is recorded once, against the resolved repository root or the working directory, and it persists in the user's own configuration rather than in the workspace, so deleting the workspace does not undo it. A path already marked trusted keeps loading that project's own configuration, and starting the MCP servers that configuration declares, on later runs whatever sandbox is requested, because the sandbox is consulted only when deciding whether to record trust for a path that has none yet. Since Sortie derives the workspace path from the issue identifier and reuses it, one earlier run arms every later run at that path.

Two consequences worth holding onto. Anything that arrives with the checkout under the agent's own configuration directory is live in the same run that grants the trust. And servers started from that layer run as children of the app-server and are not confined by the sandbox value we send: that value governs the commands the agent executes, not the transports it connects to.

## How the tool sidecar reaches this adapter

`StartSession` parses the worker-generated MCP configuration and re-expresses each declared server as a `-c mcp_servers.<name>=<inline table>` override on the app-server command line, one override per server, so the operator's own `[mcp_servers]` entries in `config.toml` are kept, not replaced. The runtime spawns the declared server itself, over stdio, which is the same sidecar every other kind uses; nothing changes about how the sidecar answers a tool call.

Every override also carries `default_tools_approval_mode="approve"`. The runtime asks for approval before calling a tool that declares no annotations, Sortie's own tools declare none, and under the never-ask thread policy that ask becomes a refusal handed back to the model: the call never reaches the server, and the turn still ends as a success. The key applies to every server the adapter renders, the operator's `mcp_config` servers included, which matches the blanket grants the Claude Code and Copilot adapters launch with; an operator who declares a server for an unattended run has consented to its use. Entries in the operator's own `config.toml` are left as they are. A tool item that completes as `failed` or `declined` reaches the orchestrator as a tool error, so a refused call never reads as a successful one.

Delivery happens on a local launch only. An SSH session gets no override: `LaunchTarget.Args` carries nothing across an SSH launch, and routing the override through the remote command string instead would either drop it silently or publish credential values on the local `ssh` process's own argument list, so the adapter delivers nothing there and a remote session runs without tools.

Credentials travel by name, not by value. For a stdio server's environment entry, when the adapter's own process already holds that name under the same value, the override carries the name in `env_vars` and the runtime resolves it from the process environment it hands the spawned server; every other entry is rendered as a literal `env` value. The runtime gives a spawned MCP server only a small fixed set of its own environment variables, so a passthrough name is both how a credential is delivered and how it stays off the command line that the local process table shows.

An HTTP-transport entry (an operator's own `mcp_config`, never Sortie's own server) renders as `url` plus a header table that carries variable names rather than header values, on the same reasoning as the stdio branch: a value on the override would sit in the app-server's argument list. Each header is matched to an environment variable holding its value and delivered as that name; a header whose value is in no variable fails the session, naming the header but never its value, since the alternative is to publish a credential to every local user. A bearer-token header has a second form on the runtime side. `codex mcp add --help` and `codex mcp get` on the installed binary give the exact keys.

The runtime reports a declared server's startup outcome on `mcpServer/startupStatus/updated`. A failure status is logged at warn, naming the server and the reported reason; it never fails the turn or the session, so a session that lost its tools this way still completes and the log is the only place that says so.

## Usage accounting

Usage does not ride on the turn's completion payload; it arrives on its own notification, and if you go looking for a usage member on the completion event you will not find one.

The registered kind declares `incremental` arrival and `per_model` attribution: the app-server sends one `thread/tokenUsage/updated` notification per model API request, each carrying `Model: state.model`.

The totals on that notification are thread-cumulative, spanning every turn of the thread including turns from an earlier run that resumed it. The adapter recovers this run's own contribution by capturing a baseline at the first notification belonging to the current turn and subtracting it thereafter. A notification belonging to a different turn does not emit anything; it raises the baseline instead.

One subtlety to preserve: a payload that carries no usage object at all is distinguishable from one reporting zeroes, because the field is a pointer. The absent case emits nothing and leaves the measurement flag alone, which is what keeps "we do not know" different from "it cost nothing". Flatten that to a value type and the distinction dies silently.

## Model reporting

The effective model rides on the response of whichever thread operation opens the session: `thread/start`'s `result.model`, a sibling of the `thread` object rather than a member of it, or `thread/resume`'s `result.model` on the same shape. Both are session-scoped, read once and stored rather than re-read per turn: the passthrough model sent on `thread/start` and `turn/start` requests is never read back as the effective one, so the stored value is the runtime's own report, not a mirror of what Sortie asked for.

A live turn can later be rerouted to a different model. `model/rerouted`'s `toModel` replaces the stored model when it is non-empty; an empty `toModel` leaves the stored model unchanged, and either way a notification event still fires so stall detection keeps seeing activity.

Only the `thread/start` shape was captured from a real binary. `testdata/thread_start_response.jsonl` is that capture with the machine-specific `result` members removed; the `thread` object is untouched. The `thread/resume` and `model/rerouted` shapes come from the generated schema, because neither can be provoked on demand, so `testdata/model_rerouted.jsonl` is derived, not captured. Treat both shapes as unverified.

## Failure modes worth recognizing

Stdout closing before a turn completes is reported as a port failure, and it is the most common way a broken session presents: the handshake succeeded, a turn started, and then nothing.

The stdio reader caps a single line, and that cap is ours rather than a documented server limit. A single very large notification would fail the read rather than being split. If a turn dies mid-way on a read error after a large diff or tool output, suspect the cap first.

A handshake or authentication failure tears the subprocess down and returns before any session exists, so those never present as turn failures.

## Verifying a change

Unit tests are thorough about the protocol on purpose, because there is no cheap way to re-derive it. The live tests in `integration_test.go` are gated on `SORTIE_CODEX_TEST=1` and skip cleanly without it. The other `SORTIE_CODEX_*` variables they read are described in that file. Traps:

- The runtime ignores the `OPENAI_BASE_URL` variable, and a project-local config file ignores provider keys. The scripted-model case points the runtime at its loopback endpoint with `-c` overrides that define a custom provider.
- The scripted-model case sets `CODEX_HOME` inside an isolated `HOME`, which also confines the automatic trusted-project record. A version-manager shim that reads `HOME` then fails to start, so name the binary by an absolute path.
- The Linux sandbox needs user namespaces. The nightly runner enables them as [the official action](https://github.com/openai/codex-action/blob/main/action.yml) does, and runs `codex sandbox -- /usr/bin/true` first, so a broken setup fails as a setup error.
- On a hosted runner the workspace sandbox cannot bring up loopback, so every command fails before it runs. The scripted turn case therefore runs under `dangerFullAccess`.
- Under `dangerFullAccess` the runtime calls an MCP tool without asking for approval, so that sandbox cannot prove the adapter's approval grant. The tool-server case runs under `workspaceWrite` with the `never` approval policy, where the call reaches the server only if the grant works.
- A catalog model reaches MCP tools through tool search or a code-mode cell. The scripted case uses an unknown model slug, which turns both off, so the tools appear directly in the request. All routes end as the same `mcpToolCall` item under the same grant.
