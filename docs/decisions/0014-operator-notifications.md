---
status: accepted
date: 2026-06-09
decision-makers: Serghei Iakovlev
---

# Use an adapter family for operator notifications

## Context and Problem Statement

While an agent runs, it cannot reach a human in real time. Its existing outbound surfaces
both travel to the orchestrator, not to a person: it can write a line to a `.sortie/status`
file that the orchestrator reads after a turn, and it can emit a normalized `notification`
event that the orchestrator logs. Both surface only when someone inspects the logs or state.
An agent that hits a decision it should not make alone, or wants to flag progress or a
blocker, has no way to raise a hand while the work is happening. This decision adds that
missing surface: a real-time channel from the agent to a human operator, exposed as a
`notify_operator` tool.

The narrow way to build it is to give the tool a Slack call, and perhaps a generic HTTP
call. That framing asks "which backends do we support," and it is a trap: a fixed set baked
into the tool means every later channel (email, Discord, PagerDuty, SMS) reopens the tool and
reopens this decision. Sortie already integrates every external dimension (issue trackers,
coding agents, CI providers, source-control backends) as an adapter family: a domain
interface, implementations in their own packages that register themselves under a `kind`
string, and a registry the core resolves through. Adding a tracker or an agent is a new
package, never a core change. Notifications take the same shape, so the real decision is the
contract for a notification adapter family, not a list of channels.

A second fact constrains the design. Agent tools do not run inside the orchestrator. They run
in a separate `sortie mcp-server` process that the agent runtime launches for the session and
that exits when its input closes. That process receives exactly two things from the
orchestrator: the path to the workflow file, and the orchestrator's `SORTIE_`-prefixed
environment variables. Any configuration the tool needs must arrive through the workflow file,
and any secret must arrive through a `SORTIE_`-prefixed variable. A separate configuration
file has nowhere to land.

## Decision Drivers

1. **The agent has no real-time channel to a human.** The status file and the notification
   event reach the orchestrator and surface only on inspection, so an agent cannot escalate a
   decision or raise a blocker mid-session.
2. **New channels must be additive.** Adding email, Discord, PagerDuty, or SMS later must be a
   new package behind an existing interface, not a rewrite of the tool and not a fresh
   architecture decision. This is the extensibility the tracker and agent families already
   provide.
3. **The tool runs in a separate, per-session process.** It receives only the workflow file
   path and the orchestrator's `SORTIE_`-prefixed variables, then dies when the session ends.
   Configuration and secrets must travel by those two channels.
4. **This decision introduces Sortie's first arbitrary outbound HTTP.** Posting to an
   operator-supplied URL carrying a secret is a new egress and secret-handling surface, and
   Sortie has no facility for redacting secrets from its logs.
5. **A notification must be correlatable.** A bare severity-title-body message cannot tell an
   operator which issue or session it concerns, nor can a machine consumer route it. The
   payload must carry session context the agent cannot forge.
6. **An adjacent output surface already exists.** The orchestrator already posts lifecycle
   comments to a tracker issue. The notification design must not collide with it and must
   leave a future merge additive.

## Considered Options

- **Option A.** Model notifications as an adapter family: a `Notifier` domain interface,
  backends in `internal/notify/<kind>/` packages that self-register into a
  `registry.Notifiers`, and a thin Tier 2 `notify_operator` tool that resolves the configured
  backends and calls `Send`. v1 ships a `webhook` backend and a `slack` backend.
- **Option B.** Hardcode the backends (Slack, generic HTTP) inside the `notify_operator` tool.
- **Option C.** Load notification backends as dynamic plugins.

## Decision Outcome

Chosen option: **Option A**, because it makes every future channel an additive package behind
a stable interface, reuses the registry and conditional-registration patterns the tracker and
agent families already prove, and keeps the agent-facing tool ignorant of any specific
backend. Options B and C are rejected below.

### Notifications are an adapter family

A domain interface `Notifier` exposes one method, `Send(ctx, Notification) error`. Each
backend is a package under `internal/notify/<kind>/` that implements `Notifier` and registers
itself in `init()` into a process-global `registry.Notifiers` under its `kind` string, exactly
as the tracker, agent, CI, and source-control families register. These packages obey the same
boundary rules as the other families: no cross-adapter imports, no orchestrator imports,
normalization to the domain type at the boundary, and generic vocabulary in the core
(`notifier_*`, never `slack_*`).

The `notify_operator` tool is a thin Tier 2 wrapper. It resolves the configured backends from
the registry by `kind` and calls `Send`; it knows nothing about Slack or HTTP. Fixing this
contract once, the interface, the registry, the normalized `Notification`, the tool, and the
configuration shape, makes a new channel a new package plus its `init()` registration, with no
change to the tool, the core, or this decision.

### Configuration: a `notifications` list in the workflow file

Backends are configured by a top-level `notifications` section in the workflow file, a list of
entries, each carrying a `kind` discriminator and that backend's own fields. The workflow file
is the only configuration channel that reaches the sidecar, so the section lives there.

The section is a list, not a single object, so a second channel is a second list entry rather
than a change to the section's shape. Reshaping a populated configuration section is the
breaking rewrite this design exists to avoid; the existing `dispatch.rules` list is the
precedent. Per-backend fields pass through the same adapter-passthrough mechanism the tracker
and agent sections already use, rather than being enumerated in the core configuration schema,
so a new backend's fields (an SMTP host, a routing key) need no core-schema change.

A backend secret is given as a reference to a `SORTIE_`-prefixed environment variable, the same
`$VAR` resolution the tracker API key uses. The prefix is mandatory, not stylistic: the sidecar
re-resolves the workflow file in its own process, and only `SORTIE_`-prefixed variables reach
it, so a reference without the prefix resolves to an empty string there and posts nowhere.

### v1 backends: `webhook` and `slack`

v1 ships two members of the family. `webhook` posts the notification as JSON to a configured
URL. `slack` posts the same content shaped for a Slack incoming webhook. A Slack incoming
webhook is itself an HTTP POST of JSON, so the two share almost all of their code and differ
only in how they render the body; shipping both is nearly free and immediately covers Discord,
Teams, and custom endpoints through `webhook`. Each backend builds on the shared HTTP client
with its configured endpoint as the base URL, classifies its own transport and API errors, and
applies a mandatory per-call timeout, which a Tier 2 tool requires.

Email and other transports are deliberately out of v1. Email is a different transport (SMTP,
TLS, MIME) with no precedent in the code, and the adapter family makes it a later additive
package rather than a question this decision must answer.

### Notification payload: an envelope plus a message

The normalized `Notification` has two layers, and every backend consumes the same shape.

The envelope is filled by the tool from the session context; the agent neither provides it nor
can override it. It carries the issue identifier and internal id, the session id, the attempt
number, the agent kind, a generated unique id, an ISO-8601 UTC timestamp, and a configurable
`source` identifying the Sortie instance, defaulting to the hostname. The message is supplied
by the agent: a `severity` (`info`, `warning`, or `critical`), a `title`, a `body`, and an
optional `category` (`decision_needed`, `progress`, `blocked`, `completed`, or `other`).

A severity-title-body message alone is not enough. A "blocked, need a decision" notification
with no issue identifier or session id is nearly useless to an operator and uncorrelatable to a
machine consumer. The session context already lives in the sidecar's environment, so the tool
fills the envelope from it rather than trusting the agent to repeat it; a system-owned envelope
also stops an agent from attributing a notification to another issue. Carrying the whole known
session context from the start, rather than a flat message that later needs correlation bolted
on, keeps foreseeable additions (an issue URL, severity-based routing) additive fields rather
than a schema change.

### Rate limiting: a per-session in-memory cap

The tool enforces a per-session ceiling on the number of notifications, a configurable
`max_per_session` with a sane default. Exceeding it is a domain error in the JSON result
(`rate_limited`), not a transport failure. The counter lives in memory in the tool instance:
the sidecar exists for exactly one session, so an in-memory counter needs neither persistence
nor cross-process coordination. A value of `0` selects the default, not unlimited, so the spam
guard cannot be disabled by omission.

### No configured backend, no tool

If the `notifications` section is empty or absent, `notify_operator` is not registered, the
same gate the tracker tool uses when no project is configured. An unregistered tool never
appears in the agent's tool listing or its prompt, so the agent is never offered a tool it
cannot use. The registration is derived from the workflow file so that the sidecar's tool set
matches what the main process advertises.

### Boundary with orchestrator lifecycle comments

Sortie already has an outbound, opt-in surface: the orchestrator posts plain-text comments to a
tracker issue on dispatch, completion, and failure, and during CI, review, and auto-merge
reconciliation, gated by `tracker.comments.*`. It shares a genus with notifications, an
outbound, opt-in, stakeholder-addressed message about a session, but differs on every
operational axis. The orchestrator initiates it, not the agent; it runs in the main process,
not the sidecar; deterministic lifecycle events trigger it, not agent discretion; and it
targets a tracker already behind an adapter.

v1 does not merge the two. Tracker comments are working code outside this decision's scope,
their surface is broader than notifications, and folding a tracker that is already an adapter
into a notifier now is coupling without payoff. Premature unification, before the second
mechanism even exists, is itself the anti-pattern.

The design does keep the merge additive. `Notifier.Send` takes a self-contained `Notification`:
the whole context rides in the envelope, with no dependency on state available only in the
sidecar. The producer fills the envelope. In the sidecar the tool fills it from the
environment; a future orchestrator producer would fill it from its own state. Because the
registry is process-global, both processes resolve backends identically. The target picture,
left to a later decision, is notifiers as a single egress with two producers, the agent through
`notify_operator` and the orchestrator through lifecycle events, at which point a tracker
comment becomes one more notifier. Binding `Send` to sidecar-only context now would foreclose
that, so the interface is defined to forbid it. This convergence is an explicit non-goal for
v1.

### Considered Options in Detail

**Option B (hardcode backends in the tool).** Embedding Slack and HTTP inside `notify_operator`
works for v1, but every later channel reopens the tool, and, since this decision sets the
precedent for notification backends, reopens the architecture decision too. That is precisely
the rewrite-per-channel cost the adapter family removes. The literal "which backends" framing
produces this option; treating backends as a family dissolves the question.

**Option C (dynamic plugins).** Loading backends as plugins keeps the core untouched but
reintroduces the fragility the adapter families were chosen to avoid: a brittle binary
interface, platform limits, and a break in the single statically-linked binary Sortie ships.
Compile-time adapters give the same extensibility without the operational cost.

**A flat message with no envelope** was rejected as the payload: it drops correlation, leaving
notifications useless to operators and uncorrelatable to machines, and pushes formatting onto
the agent. **A single configuration object** instead of a list was rejected because a second
channel would force a breaking reshape. **A separate configuration file** was rejected because
nothing delivers it to the sidecar.

## Consequences

### Positive

- A new channel is a new package plus an `init()` registration. The contract is fixed once,
  and neither the tool nor the core changes.
- The tool reuses proven patterns: the adapter registry, conditional registration only when
  configured, and the Tier 2 error contract that encodes domain failures in the result.
- Notifications are correlatable: the system-owned envelope carries issue, session, and attempt
  context the agent cannot forge.
- Convergence with the orchestrator's lifecycle comments stays additive, because `Notifier.Send`
  consumes a self-contained `Notification` that either process can produce.

### Negative

- **Secrets must be `SORTIE_`-prefixed.** A backend secret named without the prefix resolves to
  an empty string in the sidecar and posts nowhere. This is a framework constraint, not a
  preference, and belongs in the workflow reference beside the `notifications` section.
- **First arbitrary outbound HTTP.** The tool posts to an operator-supplied URL carrying a
  secret. Sortie has no secret-redaction facility in its logging, so the backends MUST NOT log
  webhook URLs, request bodies, or responses, and each call MUST carry a timeout. The operator
  URL is trusted, but the egress is real: the sidecar reaches the network with the agent's
  access.
- **The agent kind needs one additive propagation.** The sidecar's per-session environment
  carries the issue id and identifier, the session id, the workspace, the database path, and
  the attempt, but not the agent kind. The envelope's agent kind requires adding the session's
  agent to that environment, and the value MUST be the agent frozen at dispatch (the one
  actually running the session), not the workflow's default, because a routing rule can select
  a different agent.
- **The rate-limit counter is per-process.** If the agent runtime restarts the sidecar
  mid-session, the in-memory counter resets. This is a rare edge and an accepted limit of a
  per-session in-memory guard.
- **A naming clash to keep apart.** "Webhook" already names the inbound tracker webhooks that
  trigger reconciliation. The `webhook` notification backend is an outbound POST and is a
  distinct concept; documentation must not conflate them.
- **Documentation owes updates on acceptance.** The workflow reference gains the `notifications`
  section and the `SORTIE_`-prefixed secret rule; the architecture specification gains one
  section describing the notifier family (interface, registry, normalized type, tool); and the
  tool catalog gains `notify_operator`.
- **No new metric in v1.** Tool calls, notification sends included, are already counted by the
  existing per-tool counter with a success-or-error result label, so a delivery failure already
  shows up. A dedicated counter, if wanted later, should be added with its dashboard and
  coverage in one change.

## Confirmation

The decision is satisfied when all of the following hold:

1. A `Notifier` interface and a normalized `Notification` type live in the domain package, and
   `registry.Notifiers` registers backends by `kind`, mirroring the other adapter families.
2. `internal/notify/webhook` and `internal/notify/slack` each implement `Notifier`,
   self-register in `init()`, build on the shared HTTP client with the configured endpoint as
   base URL, classify their own errors, and apply a per-call timeout.
3. A top-level `notifications` list parses from the workflow file with a required `kind` per
   entry and per-backend fields carried through adapter passthrough rather than the core schema.
4. `notify_operator` validates its input against a schema that rejects unknown fields, fills the
   envelope from session context the agent cannot set, validates `severity` and `category`
   against their enums, requires a non-empty `title` and `body`, and encodes domain failures
   (`invalid_input`, `rate_limited`, `send_failed`, `backend_unavailable`) in the JSON result
   rather than as a Go error.
5. `notify_operator` is registered only when at least one valid backend is configured, and the
   registration is derived from the workflow file so the sidecar and the main process agree on
   the tool set.
6. A secret referenced without the `SORTIE_` prefix is documented as unsupported, and no backend
   logs URLs, request bodies, or responses.
7. The per-session notification count is capped by `max_per_session`, where `0` selects the
   default rather than unlimited.
8. `Notifier.Send` takes a self-contained `Notification` whose envelope is filled by the
   producer, so a future orchestrator producer can reuse the family without an interface change.
