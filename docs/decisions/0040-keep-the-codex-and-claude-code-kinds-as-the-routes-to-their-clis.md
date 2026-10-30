---
status: accepted
date: 2026-10-30
decision-makers: Serghei Iakovlev
---

# Keep the Codex and Claude Code Kinds as the Routes to Their CLIs

## Context and Problem Statement

Sortie reaches Codex CLI through the hand-written `codex` kind, which drives `codex app-server` over its own JSON-RPC dialect, and reaches Claude Code through the hand-written `claude-code` kind, which drives the installed `claude` executable over its stream-json protocol. The project's direction is that runtimes arrive on the `agent-client-protocol` kind and hand-written kinds retire once that route reaches parity, which leaves both runtimes an open question: work toward moving them, or keep their kinds.

Neither runtime speaks the Agent Client Protocol of its own. Codex's publisher answered a request for native support by pointing to an external bridge, and a later request has drawn no maintainer reply. A request for native support in Claude Code was closed automatically with no reply from its publisher. A move would therefore run through a bridge, maintained elsewhere or written here.

## Decision Drivers

1. **One binary, no runtime dependency.** Sortie ships as a single statically-linked binary, and an SSH worker host needs only a shell, a workspace, the agent executable and its credentials.
2. **The operator keeps what they hold.** A route change must not withdraw a figure or a check an operator relies on today.
3. **Maintenance follows the coupling.** A route that still depends on the runtime's own wire format keeps that cost wherever the code sits.
4. **Measured cost, not expected cost.** The choice rests on what each kind has actually cost to keep.

## Considered Options

- Keep the `codex` and `claude-code` kinds, exclude them from retirement, and deepen them
- Move each runtime onto `agent-client-protocol` through an external bridge
- Move each runtime onto `agent-client-protocol` through a bridge Sortie writes and ships

## Decision Outcome

Chosen option: **keep the `codex` and `claude-code` kinds**, because for each runtime it is the only option that keeps every capability an operator holds without adding a runtime dependency, and the measured upkeep of both kinds is low.

The `codex` kind stays Sortie's route to Codex CLI and the `claude-code` kind stays its route to Claude Code. Neither is a candidate for deprecation or retirement, and no move onto `agent-client-protocol` is pursued for a runtime while it has no protocol mode of its own. Work on either runtime goes into its kind, toward a fuller native integration.

The upkeep was measured over the six months before this record. The `codex` kind uses about 30 app-server methods, all present in Codex 0.166.0, and one production fix in that window was caused by a Codex release; the nightly live run caught it, and it was fixed two days after it was filed. The `claude-code` kind is about 1,500 production lines, and two production defects in that window were caused by Claude Code releases, each fixed within one to two days. The nightly run installs the latest release of both runtimes unpinned.

### Considered Options in Detail

**An external bridge.** Rejected on drivers 1, 2 and 3, for each runtime.

For Codex, the only maintained bridge is a TypeScript package distributed through npm, so every host that runs Codex would need Node. Each bridge release bundles its own Codex pinned to one minor version; it adopted a new minor zero to three days after release when it did, and skipped minors for up to five weeks. Through it the operator loses what the `codex` kind delivers today: structured usage windows, which the bridge renders only as Markdown text; spend split by model; the usage-limit refusal with its reset time; the effective model; `effort` forwarding; the offline `codex.approval_policy.interactive` check; and tools over SSH through a reverse forward. Clients that drive Codex through the bridge and need those figures were observed opening a direct app-server connection beside it, so the coupling to Codex remains.

For Claude Code, the only maintained bridge runs it through the Claude Agent SDK, which spawns Claude Code over the same stream-json protocol the `claude-code` kind reads, so the bridge removes no coupling. It is an npm package that needs Node 22 or later and a bundled Claude Code binary of about 300 MB on every host, and by default it runs that bundled binary rather than the operator's installed one. Over the six months it trailed the SDK by a median of three days and at worst by 18. Through it the operator loses the token split and cost by model, thinking tokens, a typed usage-limit refusal with its reset time, which arrives only as text, and the rate-limit report on a turn that starts already over the limit. Usage windows and a running cost total do arrive, under vendor `_meta` keys. The SDK's documentation states that third-party products built on it may not offer claude.ai login without the publisher's approval; the `claude-code` kind drives the operator's installed CLI and does not use the SDK.

**A bridge Sortie writes.** Rejected on drivers 1 and 3. It keeps the same coupling to the runtime's wire format that the kind has, and adds an Agent Client Protocol server side Sortie must keep conformant. Run as a separate process it would put the Sortie binary on every SSH worker host for the first time. It adds code and gives the operator nothing the kind lacks.

## Consequences

### Positive

- No host needs Node, a bundled agent binary or a second agent process to run Codex or Claude Code, locally or over SSH.
- Operators keep exact per-model spend, usage windows, the usage-limit reset time, the effective model and `effort` forwarding on both runtimes, and the offline approval check on Codex.
- Releases of both runtimes keep reaching Sortie's tests on the day they ship, through the unpinned nightly rows, rather than after a third party adopts them.

### Negative

- A fleet-wide change lands in each kind separately. Most of each kind's commits in the six months before this record were cross-adapter changes of this kind: about three quarters for `codex` and 58 of 67 for `claude-code`.
- Each kind depends on an interface its publisher does not promise to keep: Codex labels the app-server experimental and versions it without a compatibility promise, and the `claude-code` kind reads stream-json fields and session journals that Claude Code does not document. A release can break either kind without notice. The nightly live rows are the safeguard, not a guarantee.
- The project carries more than one agent transport for as long as these runtimes have no protocol mode.

## Confirmation

1. Neither the `codex` nor the `claude-code` kind declares a deprecation or has a retired-kind entry.
2. The nightly run keeps a scripted and a live row for each of `codex` and `claude-code` that install the latest release unpinned.
3. A proposal to move either runtime onto `agent-client-protocol` reopens this decision for that runtime only after it ships a protocol mode of its own.
