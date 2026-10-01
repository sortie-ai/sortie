---
status: accepted
date: 2026-10-01
decision-makers: Serghei Iakovlev
---

# Route Outbound Messages Through Per-Destination Event Subscriptions

## Context and Problem Statement

Sortie tells people about an issue or a session through two outbound surfaces that shared no code. The orchestrator posted tracker comments from eleven call sites of `TrackerAdapter.CommentIssue`, each with its own text builder and its own gate. The notifier family (`domain.Notifier` with the `slack` and `webhook` backends, configured by the top-level `notifications` list) had exactly one producer, the agent's `notify_operator` tool in the `sortie mcp-server` sidecar, and sent every notification to every configured backend. Nothing the orchestrator observed could reach Slack or a webhook, and nothing could reach the tracker except through a hard-wired call site.

The gates on the tracker surface were of three unrelated kinds. `tracker.comments.on_dispatch`, `on_completion`, and `on_failure` (all `false` by default) gated the dispatch comment, the worker-exit comments (completion, the soft-stop comment for `blocked`, `needs-human-review`, and `no-change-needed`, and failure, which also covered a run whose handoff the evidence verdict withheld) and nothing else. Each reaction's `escalation` setting (`label` or `comment`; `label` by default, except the auto-merge reaction, whose default is `comment`) chose between applying a label and posting a comment when the CI failure, review, bot review, merge conflict, auto-merge, or merge-completion reaction handed its subject to a person. The auto-merge success comment and the budget-hold notice had no configuration gate at all and posted whenever a tracker adapter was present. An operator who wanted "completion goes to the issue, failures also go to Slack" had no way to say it.

The gap now carries a correctness cost. The agent's reason for a `blocked` or `no-change-needed` stop has to appear in the comment Sortie posts, which is the first time agent-authored text enters a message Sortie itself publishes on a tracker issue that people outside the deployment can read. Where that text travels, and how each destination neutralizes it, depends on whether the two surfaces stay separate. This decision settles the shape of the outbound system, the mapping of every existing gate onto it, the migration of operator configuration, and the path the agent's stop statement takes.

## Decision Drivers

1. **One event, many destinations.** "Comment on the issue when a session completes" and "post to Slack when a session completes" are one fact routed to two places. Adding a destination or routing an existing fact to a new place must be configuration, not a new call site.
2. **Sortie owns routing.** The operator's configuration alone decides which destination receives which event. The agent is an executor: it supplies content and never chooses a destination. Agent-originated messages never reach the tracker issue; agent text reaches a tracker comment only as a field of an event Sortie produces.
3. **The operator chooses per destination.** Each destination receives only the event types the operator enabled for it.
4. **Sortie's events never spend the agent's budget.** The `max_per_session` cap bounds the agent's own notifications. An orchestrator event must not consume a slot, and a chatty agent must not suppress an orchestrator event.
5. **The tracker comment is public; operator channels are not.** A tracker comment never carries error text, the agent's session identifier, or the agent kind. The Slack and webhook payloads carry the full envelope, session and dispatch identity included. One event therefore needs a public rendering and an operator rendering.
6. **Agent text is untrusted in every destination.** A statement written by the agent can contain a line that a tracker executes as a command, a mention that pages people, markup that changes meaning, or a secret. Each destination has its own syntax, so the text must reach each destination as a separate field and be neutralized in that destination's terms.
7. **Nothing breaks for existing users.** Every configuration accepted before this decision keeps producing the same tracker comments, labels, and notifications, byte for byte. The old form is deprecated with a warning that suggests the new form, and both forms work at the same time. Duplicate delivery caused by holding both forms at once is a defect.
8. **Producers keep their delivery semantics.** The budget-hold notice is deduplicated by a durable row written before the send and paced to at most ten notices in thirty seconds. The merge-completion missing-SHA escalation records its observation as delivered only after a successful write, so a failed write is retried. Every write except the dispatch comment runs detached in the tracker-operations wait group with a per-write timeout that shutdown drains. A unified path must not flatten these into one semantic.
9. **The adapter boundary holds.** A notifier constructor receives only its entry's configuration map. A package under an adapter family root must not import another family or the orchestrator, and the orchestrator must not import a package that registers a kind; these rules are enforced by a contract test, not only by convention.
10. **No new dependency, no CGo, no rewrite.** Sortie ships as one statically linked binary. The change reuses the domain types, the registry, the backends, the text builders, and the redaction facility that exist.

## Considered Options

- **Keep the surfaces separate**, and extend the soft-stop comment builder to carry the agent's statement.
- **Partial convergence**: one normalized message type and one rendering path for orchestrator comments, with the notifier family left as the agent-only path.
- **Per-destination subscriptions on the existing `notifications` list**, with a built-in `tracker_comment` destination over the configured tracker adapter.
- **Named destinations plus a routing table**, reshaping `notifications` into a map of destinations and a list of rules.
- **Operator-authored templates and conditional triggers** on top of either routing model.

## Decision Outcome

Chosen option: **Per-destination subscriptions on the existing `notifications` list**, because it is the only option that satisfies drivers 1 through 4 without reshaping a section operators already populate (driver 7), and it moves every call site onto one path while leaving the text builders, the reaction fingerprints, and the budget-hold pacing untouched and reusing the existing backends, which change only to render the agent-text field and, for the webhook, to carry the event type on orchestrator events (drivers 8 and 10).

### One event catalog, produced by the orchestrator and the agent

The orchestrator becomes a producer into the notifier family. Each of the eleven tracker-comment call sites publishes a typed event instead of calling `CommentIssue`, and the agent's `notify_operator` call is one more event type. The catalog is closed and lives in the domain package; configuration validation accepts exactly the names it declares, so a valid event name and an emitted event type cannot drift apart. Each type declares its severity for the operator rendering.

| Event type | Produced where the comment was posted before | Severity |
| --- | --- | --- |
| `session.started` | the dispatch comment, under the same precondition that the dispatch drives issue state | `info` |
| `session.completed` | the completion comment on a normal exit | `info` |
| `session.stopped` | the soft-stop comment for `blocked`, `needs-human-review` or `no-change-needed` | `warning` for `blocked` and `needs-human-review`, `info` for `no-change-needed` |
| `session.failed` | the failure comment, including a run whose handoff the evidence verdict withheld | `warning` |
| `escalation.ci_failure`, `escalation.review_comments`, `escalation.bot_review`, `escalation.merge_conflicts`, `escalation.auto_merge`, `escalation.merge_completion` | the reaction escalation, whenever the reaction hands its subject to a person, whatever its `escalation` value | `warning` |
| `auto_merge.merged` | the auto-merge success comment | `info` |
| `budget.held` | the budget-hold notice | `warning` |
| `agent.message` | a `notify_operator` call | set by the agent |

Each event is the existing `domain.Notification`, extended additively: the envelope gains the event type, and the message gains a separate field for agent-supplied text. For an orchestrator event the producer fills the envelope from its own state and sets the body to exactly the text the call site's builder produced, so a comment posted from an unchanged configuration is byte-identical to the comment posted before this decision. A cancelled run produces no event, as it posted no comment before.

The producer publishes through one call that returns the outcome per destination. The producer keeps its own goroutine, wait group, timeout, deduplication, pacing, and metrics. A producer that confirms delivery names the one action whose outcome confirms it, and the outcomes of other destinations never stand in for it. The missing-SHA escalation keeps today's rule: it is delivered when its label is applied under `escalation: label`, or when the `tracker_comment` destination accepts the event under `escalation: comment` or a synthesized subscription. A successful Slack or webhook send therefore never suppresses the retry of a failed tracker action. Under `escalation: none` with no subscriber the event has no destination, so the producer marks it delivered at publish, as it would after a label that nothing retries. For orchestrator events, a failed delivery to one destination is logged and does not prevent delivery to the others. `notify_operator` keeps its current result contract for `agent.message`: a call counts once at least one entry accepts it, and the first failed send ends the call with an error.

### Subscriptions live on each destination

Each `notifications` entry gains an `events` key: the list of event types that destination receives. Routing is a pure function of the loaded configuration and the event's type, held in a package under `internal/notify/` that registers no kind, so the main process and the sidecar compute the same result from the same `WORKFLOW.md`. Event names are listed explicitly; this decision introduces no wildcard and no filter beyond the event type.

The agent never influences routing. The sidecar delivers an `agent.message` only to entries that subscribe to it, and the orchestrator delivers only its own event types. Because the orchestrator never claims a slot under `.sortie/notification_slots/`, and the cap counts only `agent.message` deliveries, Sortie's events and the agent's cap cannot interact. `max_per_session` keeps its meaning: one cap for the agent's calls, the largest value across entries. `notify_operator` is registered only when at least one entry subscribes to `agent.message`, so the agent is never offered a tool whose messages reach no destination.

### The tracker comment is a built-in destination

`tracker_comment` is a reserved `kind` in `notifications`. It is not a registered notifier kind: a notifier constructor receives only its configuration map and cannot be handed the tracker adapter, and building a second tracker adapter from configuration would mean a second set of credentials and a second client beside the one that already drives the issue. The destination lives in the same kind-free package as the router, wraps the tracker adapter that `cmd/sortie` already constructs, and posts on the issue the event's envelope names. The registry refuses a registration under the reserved name. The `slack` and `webhook` destinations are still resolved through `registry.Notifiers` in `cmd/sortie`, and the orchestrator receives them constructed, as it receives every other adapter.

The destination is tracker-agnostic. It hands plain text to `CommentIssue`, and each tracker adapter keeps its own conversion. Validation enforces four rules: at most one `tracker_comment` entry; a `tracker_comment` entry requires a configured tracker; a `tracker_comment` entry requires the `events` key, where an empty list means only the subscriptions synthesized from deprecated keys; and `agent.message` is never a valid subscription for `tracker_comment`. The compatibility default for an entry without `events` applies only to registered notifier kinds, since no `tracker_comment` entry existed before this decision.

### Every existing gate maps onto subscriptions

The gate moves from each call site into the router. The old keys keep working by synthesizing subscriptions for the `tracker_comment` destination when the configuration is loaded, at startup, on reload, and in `sortie validate`; Sortie never rewrites `WORKFLOW.md`.

| Existing mechanism | What it becomes | During deprecation |
| --- | --- | --- |
| `tracker.comments.on_dispatch: true` | `tracker_comment` subscribes to `session.started` | works; deprecated with an advisory |
| `tracker.comments.on_completion: true` | `tracker_comment` subscribes to `session.completed` and `session.stopped` | works; deprecated with an advisory |
| `tracker.comments.on_failure: true` | `tracker_comment` subscribes to `session.failed` | works; deprecated with an advisory |
| `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`, `_ON_COMPLETION`, `_ON_FAILURE` | override the three keys before synthesis, unchanged | works; the same advisory as the key it overrides |
| `reactions.<kind>.escalation: label` | the reaction applies the label; the `escalation.<kind>` event is emitted as well and reaches only destinations subscribed to it | unchanged; not deprecated |
| `reactions.<kind>.escalation: comment` | no label; `tracker_comment` subscribes to `escalation.<kind>` | works; deprecated with an advisory that suggests `escalation: none` plus the subscription |
| `reactions.<kind>.escalation: none` (new value) | no label; the event reaches only destinations subscribed to it | new form |
| the ungated auto-merge success comment | `tracker_comment` subscribes to `auto_merge.merged` | works through an implicit subscription; deprecated with an advisory when the auto-merge reaction is configured |
| the ungated budget-hold notice | `tracker_comment` subscribes to `budget.held` | works through an implicit subscription; deprecated with an advisory when a per-issue ceiling is configured |
| `reactions.auto_merge` with `escalation` omitted | read as `escalation: comment`, its default before this decision | works; the same advisory as an explicit `escalation: comment` |
| a `slack` or `webhook` entry without `events` | the entry receives `agent.message` only, as before | works; deprecated with an advisory that suggests `events: [agent.message]` |
| `notifications[].max_per_session` | unchanged, counting only `agent.message` | not deprecated |

`escalation: none` is part of this decision because the deprecation advisory for `escalation: comment` must name an equivalent in the new form, and the label is an action on issue state rather than a message, so it cannot be expressed as a subscription.

Each deprecated key or implicit behavior that the loaded configuration relies on produces a configuration advisory naming the old form and the new form that replaces it. An advisory reaches the run log once per configuration change, appears in `sortie validate`'s diagnostics without affecting `valid` or the exit status, and appears in the dry run. Nothing is logged per event. This decision sets no removal date and no removal version for the old form; removal is a separate, later decision.

### How old and new forms combine

Both forms work at the same time, and they never compete. The subscriptions of the `tracker_comment` destination are the union of the `events` its explicit entry lists and the subscriptions synthesized from deprecated keys. Each event is delivered at most once to each destination, however many subscriptions select it, and the single permitted `tracker_comment` entry makes the tracker comment one destination. An operator who keeps `tracker.comments.on_completion: true` and also lists `session.completed` on a `tracker_comment` entry gets one comment per completion, not two. The union is monotone: adopting the new form never silently withdraws a comment the operator enabled through an old key.

The implicit subscriptions to `auto_merge.merged` and `budget.held` apply only while no explicit `tracker_comment` entry exists. They come from no key, so there is nothing to union; an explicit entry is the operator's own statement of what the issue receives, and it receives those two events only when the entry lists them. When no tracker is configured, no implicit destination exists, just as no comment was posted without one before this decision.

A front matter fragment written before this decision, with completion and failure comments, a commenting CI reaction, and one Slack channel for the agent:

```yaml
tracker:
  kind: github
  api_key: $SORTIE_GITHUB_TOKEN
  project: acme/billing-api
  comments:
    on_completion: true
    on_failure: true

reactions:
  ci_failure:
    provider: github
    escalation: comment

notifications:
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
```

The same comments and messages in the new form. The two formerly ungated events are listed because an explicit entry ends the implicit subscriptions; neither feature is configured here, so they post nothing until it is:

```yaml
tracker:
  kind: github
  api_key: $SORTIE_GITHUB_TOKEN
  project: acme/billing-api

reactions:
  ci_failure:
    provider: github
    escalation: none                 # no label; the comment comes from the subscription below

notifications:
  - kind: tracker_comment
    events: [session.completed, session.stopped, session.failed, escalation.ci_failure, auto_merge.merged, budget.held]
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
    events: [agent.message]
```

The old form loads and runs unchanged, and draws advisories along these lines (illustrative wording, not a contract):

```text
tracker.comments.on_completion is deprecated; list session.completed and session.stopped in a notifications entry of kind tracker_comment
tracker.comments.on_failure is deprecated; list session.failed in a notifications entry of kind tracker_comment
reactions.ci_failure.escalation: comment is deprecated; set escalation: none and list escalation.ci_failure in a notifications entry of kind tracker_comment
notifications[0] has no events; add events: [agent.message] to keep its current behavior
```

### The agent's stop statement travels as a separate field

The agent attaches a free-text statement to its stop signal by writing it after the first line of `.sortie/status`, within the status file's existing read bound. The first line remains the status token, so an agent that writes one line behaves exactly as before. The statement shares the token's file because it explains that token: one read yields both, the self-review phase consumes both together when it removes the file, and a separate companion file could predate or outlive the token it is meant to explain.

The worker captures the statement from the same read that produced the effective token, both after a coding turn and inside self-review. When self-review retracts an unconfirmed `no-change-needed` declaration, the statement is discarded with it. The statement travels in the worker result under a name distinct from the soft-stop token. At worker exit the producer passes it through the redaction facility, which masks registered secrets and bounds its length, and places it in the agent-text field of the `session.stopped` event. It is never concatenated into the body.

Each destination neutralizes the field in its own syntax:

- **Tracker comment.** The public rendering is the builder's body followed by the statement inside a fenced code block whose fence is longer than any run of backticks in the statement. The block exists so the tracker treats the statement as literal text: no line-leading slash command, mention, or markup in it takes effect. The public rendering never includes the envelope, so the rule that a tracker comment carries no error text, session identifier, or agent kind holds for every event.
- **Slack.** The operator rendering appends the statement after the body with `&`, `<`, and `>` escaped, so the statement cannot form a mention, a channel-wide ping, or a link.
- **Webhook.** The payload of an orchestrator event carries the statement under its own JSON key, and the event type under another; JSON encoding is the neutralization, and the consumer owns presentation. An `agent.message` payload keeps exactly today's field set, with neither key, so an endpoint configured before this decision receives the same JSON as before. The new keys reach only an endpoint whose entry subscribes to an orchestrator event, which no configuration accepted before this decision does.

This changes the soft-stop comment only when an agent writes a statement: a status file with one line posts the same comment as before.

Each destination picks its own events. Here the issue receives the session outcomes, Slack receives the agent's messages and everything that needs a person, and the webhook archives session outcomes; `agent.message` reaches Slack only, because only that entry lists it:

```yaml
notifications:
  - kind: tracker_comment
    events: [session.completed, session.stopped, session.failed]
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
    events: [agent.message, session.stopped, session.failed, escalation.ci_failure, budget.held]
  - kind: webhook
    url: $SORTIE_OPS_WEBHOOK_URL
    events: [session.started, session.completed, session.stopped, session.failed]
```

An agent that stops on `blocked` writes its statement after the token in `.sortie/status`:

```text
blocked
The ticket asks for both soft and hard delete of invoices.
Which one should the API expose?
```

The tracker comment for that stop is the soft-stop text followed by the statement in its fenced block:

````text
Sortie session completed (agent signaled: blocked).
Duration: 12m4s
Turns: 7

```
The ticket asks for both soft and hard delete of invoices.
Which one should the API expose?
```
````

### Considered Options in Detail

**Keep the surfaces separate.** This is the cheapest option: nothing moves, the soft-stop builder gains an argument, and the tracker path keeps its well-tested call sites. It fails driver 1, because routing any orchestrator fact to Slack or a webhook would need a second call site per fact. It fails driver 3, because the three gate kinds stay as they are, with two notices that no operator can turn off. Each future destination for an orchestrator fact would be another hard-wired path.

**Partial convergence.** One normalized message type and one renderer remove the per-site builders' divergence and give the agent's statement a single place to be neutralized, without coupling the tracker to the notifier registry. It still leaves the orchestrator unable to reach any destination but the tracker (driver 1), keeps routing as gates scattered over three configuration areas (driver 3), and builds a second message type next to `domain.Notification`, which already carried a self-contained envelope, a severity, a title, a body, and a category. The later step to full convergence would then have to merge two types.

**Named destinations plus a routing table.** Declaring each destination once and routing through a central rule list gives one place to read "where does event X go", and lets future filters such as issue labels live in rules rather than on every destination. It loses on driver 7: `notifications` would change from a list to a map, every populated configuration would need either a dual-shape parser or a deprecation of the list itself, and operators would carry a second migration on top of the gate migration. It adds two indirections (name, then destination, then rule) to the most common case of one channel. The list shape was chosen precisely so that a new channel is a new entry rather than a reshape, and nothing in this decision's forces requires giving that up: a destination's subscription answers the same question.

**Operator-authored templates and conditional triggers.** Templates would give operators control of wording, language, and mentions with no new dependency, since `text/template` already renders prompts. They lose on driver 5 and driver 7. A template over event data turns every data field into a public contract, so renaming a field breaks configurations. A template on `tracker_comment` could print the session identifier or error text into a public comment. Rendering with `missingkey=error`, as prompt rendering does, turns a template mistake into a delivery failure at run time unless every template is validated against a synthetic event at load. Conditional triggers would need an expression language, which is a dependency or a hand-written interpreter. None of this is needed to route the cataloged events, and fixed renderings keep the public rule enforceable in one place.

**A registered `tracker_comment` kind** was considered as a placement for the destination: widen the notifier constructor to receive a dependency set that includes the tracker adapter. It satisfies the import rules, because the package would depend on the domain interface, but it changes the constructor signature of every notifier kind to serve one that has no integration of its own. A built-in destination in a kind-free package reaches the same result without that change.

## Consequences

### Positive

- An orchestrator fact reaches any destination by configuration, and a new notifier kind receives orchestrator events with no change to the orchestrator.
- The three gate kinds and the two ungated notices become one rule: a destination receives what it subscribes to.
- Every existing configuration posts the same comments, applies the same labels, and sends the same notifications. Holding both forms never produces a duplicate.
- The agent's statement reaches the issue through one path with one producer-side redaction and one neutralization per destination, and agent-originated messages cannot reach the tracker at all.
- Escalations reach Slack or a webhook whether the reaction labels or comments, which `escalation: label` could not offer before.

### Negative

- **The main process gains operator egress.** Slack and webhook destinations are constructed and called from the orchestrator for the first time, so a misconfigured entry now affects the main process as well as the sidecar. Construction failures must follow the reload invariant: a reload that fails keeps the previous configuration and reports the error.
- **Every operator with a tracker sees an advisory.** The implicit subscriptions for the auto-merge success comment and the budget-hold notice are deprecated, so a configuration that enables either feature draws an advisory until it declares a `tracker_comment` entry. The advisory is reported once per configuration change.
- **An explicit `tracker_comment` entry is authoritative for the formerly ungated notices.** An operator who declares the entry and omits `auto_merge.merged` or `budget.held` stops receiving those comments. This is the operator's choice in the new form, and the workflow reference must say so where it introduces the entry.
- **The catalog becomes a public contract.** Event type names appear in operator configuration and in webhook payloads; renaming one later is a deprecation of its own. Adding a type is additive.
- **Producers keep their own protections.** There is no shared rate limit, retry, or delivery journal; system events are as protected as their producers make them, which matches the tracker comments before this decision.
- **Documentation owes updates when this is implemented.** The workflow reference's `tracker.comments` field and its environment overrides, the reaction `escalation` field and its valid values, the `notifications` section, the curated environment variable table, and the configuration error table. The architecture specification's front-matter schema and configuration sections, its tracker-write boundary that lists the orchestrator's comments and the public-comment rule, the notifier family and `notify_operator` description in the agent adapter contract, the reaction contracts that describe `escalation: comment`, the observability section that lists comment and escalation metrics, and the agent-authored workspace files and agent-to-orchestrator protocol descriptions of `.sortie/status`. The changelog gains the new keys and a Deprecated entry for each old form.

## Confirmation

The decision is satisfied when all of the following hold:

1. A golden test over a set of configurations written in the old form shows that the set of tracker comments, labels, and notifications, and the text of every comment, are identical before and after the change.
2. A configuration holding both an old key and the matching new subscription delivers one comment per event.
3. Every deprecated key and implicit behavior in the mapping table produces an advisory naming its replacement, visible in `sortie validate` without changing `valid`.
4. Configuration validation accepts exactly the event types the domain catalog declares, rejects a second `tracker_comment` entry, a `tracker_comment` entry without a tracker, and `agent.message` on `tracker_comment`.
5. An orchestrator event never claims a slot under `.sortie/notification_slots/`, and a sidecar never delivers to a destination that does not subscribe to `agent.message`.
6. A statement containing a line-leading slash command, a mention, markup, a run of backticks, and a registered secret is posted to each supported tracker as inert literal text with the secret masked, reaches Slack escaped, and reaches the webhook under its own key.
7. The contract test passes with the routing package listed as a kind-free package the orchestrator may import, and no orchestrator package imports a notifier kind.
