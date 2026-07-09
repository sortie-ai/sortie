---
status: accepted
date: 2026-07-09
decision-makers: Serghei Iakovlev
---

# Detect PR Label Commands by Polling the Label-Event Journal

## Context and Problem Statement

Operators need an in-band way to command work on a Sortie-managed pull
request from the code-review surface itself: request an automated
review of the PR, or request a fix session that addresses accumulated
review feedback. The gesture is applying a label to the PR. Labels are
visible in every PR list view, filterable, and applicable from the PR
page without any new tool — and closing the loop
issue → agent PR → human review → fix → review is the intended
workflow.

Everything Sortie observed before this decision was durable state. The
reconcile loop re-reads CI conclusions, review comments, review
decisions, and mergeability on every tick and deduplicates against
fingerprints persisted in `reaction_fingerprints`; a missed tick loses
nothing because the observed facts persist and remain re-readable. A
label command broke that assumption: it is a discrete human gesture,
not a durable condition. A snapshot of current labels could not see a
label applied and removed between two ticks, could not distinguish
removed-and-reapplied from unchanged, and could not attribute the
gesture to an actor. No component observed gestures.

The delivery constraint is fixed by the deployment posture. Sortie is a
single statically-linked binary whose embedded HTTP server binds to
`127.0.0.1` by default and is not required for orchestrator
correctness. No inbound reachability from the SCM platform can be
assumed. The polling loop is the sole delivery mechanism and must
remain correct on its own; push-based delivery, if ever added, is
defined as an advisory trigger for an immediate reconcile pass, never
as an authoritative event source.

The problem, precisely: convert each labeling gesture into at most one
agent dispatch — across process restarts, honoring retraction, and
permitting deliberate repetition — without adding an inbound network
surface.

## Decision Drivers

1. **Polling primacy.** The poll-and-reconcile loop must remain the
   sole mechanism required for correctness. Any detection path that
   only works with push delivery violates the platform's coexistence
   contract for webhooks.
2. **Zero inbound surface.** Operators run Sortie on workstations and
   CI hosts behind NAT. A publicly reachable endpoint, inbound TLS,
   and webhook secret rotation are operational costs the deployment
   model deliberately avoids.
3. **Gesture fidelity.** Applied-then-removed and
   removed-then-reapplied must be distinguishable from steady state. A
   re-application after a completed run is a legitimate new command:
   review → fix → review again is the point of the feature.
4. **At-most-once dispatch across restarts.** Deduplication state must
   survive the process, like every other reaction kind. Process memory
   is not a dedup store.
5. **Attribution.** The command starts an agent session; the fix
   variant pushes code to the PR branch. Knowing *who* commanded is a
   prerequisite for auditing and for any future authorization policy.
6. **Team label sovereignty.** On GitHub, applying a label requires
   the triage role, while creating one requires write access. Label
   vocabularies are team property and often follow naming policies.
   Command label names must therefore be configurable, ship as
   namespaced defaults only, be individually disableable, and never be
   created by Sortie itself.
7. **Reaction-family consistency.** Default-off activation via a
   `provider` key, one pending entry and one fingerprint row per
   reaction kind, cross-kind isolation, and provider specifics
   normalized at the adapter boundary — the invariants every existing
   reaction kind obeys.

## Considered Options

- **Option A.** Snapshot polling: list the PR's current labels each
  tick and diff against the previous observation.
- **Option B.** Journal polling: read the PR's label-event journal
  each tick and process entries past a persisted high-water mark.
- **Option C.** Webhook receiver: an HTTP endpoint receiving the
  platform's label events.
- **Option D.** Comment commands instead of labels as the human
  channel.

## Decision Outcome

Chosen option: **Option B (journal polling)**, because the label-event
journal turns the transient gesture into exactly the kind of fact the
poll loop is built to consume: durable, uniquely identified, timestamped,
and attributed. Each labeling action becomes a journal record the
orchestrator can re-read at any time, so polling loses nothing, the
record's identity is a natural deduplication key, and the actor comes
for free. The mechanism stays inside the existing tick, the existing
`pending_reactions` model, and the existing `reaction_fingerprints`
table — no new network surface, no new persistence concept.

On GitHub the journal is the per-issue events API (pull requests are
issues for this API family), whose `labeled`/`unlabeled` entries carry
a unique id, the label name, the acting user, and a timestamp. The
abstraction is portable: GitLab exposes the same concept as resource
label events on merge requests, with the same id/user/action/timestamp
shape.

### Command contract

The unit of command is **one `labeled` journal event** whose label
name matches a configured command label on a Sortie-managed PR.
Label-name comparison uses the adapter's normalized (lowercased) label
names.

Per tick and per managed PR, the reconcile pass reads the journal and
considers events that sort after the persisted high-water mark by
(timestamp, id). The mark always advances to the newest examined event
— including `unlabeled` entries and foreign labels — because it tracks
journal position, not command history. Among the new events, all
matches for one command kind collapse into at most one command
(burst collapse): applying a label twice between ticks is one intent,
not two.

Commands do not queue beyond depth one per kind. A gesture arriving
while a command of the same kind is already queued or its session is
running collapses into the outstanding command. The follow-up gesture
is not lost in practice: a session dispatched after the collapse sees
all feedback accumulated up to its start.

The guarantee is **at-most-once per gesture batch**, matching the
family's posture: the fingerprint row is marked dispatched before the
dispatch is scheduled, so a crash in the narrow window between marking
and scheduling loses the command rather than duplicating it. The
operator re-applies the label. A crash at any other point neither
replays nor duplicates, because the mark and the dispatched flag live
in SQLite.

### Retraction

A command is confirmed only if its label is still present on the PR at
detection time. When the journal shows new matching `labeled` events
but the label is absent at the tick, the gesture was retracted: the
mark advances and nothing dispatches. This gives the operator a
cancellation window bounded by the poll interval — the fat-finger
escape hatch. Once a dispatch is scheduled, removing the label does
not cancel the running session.

### Acknowledgment

After scheduling a dispatch, the orchestrator removes the command
label from the PR. The label thereby reads as a one-shot button:
present means "command queued, not yet picked up"; disappearance means
"accepted" (or "retracted by you"); re-applying is a single gesture to
issue the next command.

This is the deliberate contrast between command labels and
standing-instruction labels. A standing instruction (Kodiak's
`automerge` label is the canonical example) stays on the PR and
describes desired steady state; a command is consumed. Sortie's two
labels are commands.

Correctness never depends on the removal. Deduplication rests entirely
on the journal mark; a failed removal (missing write scope, transport
error) logs a warning and leaves a stale label behind. The degradation
is cosmetic: the next command then requires removing the stale label
manually before re-applying. The removal itself produces an
`unlabeled` journal entry, which is never a command; `labeled` events
authored by the orchestrator's own identity are likewise ignored
defensively, although Sortie never applies command labels.

### Deduplication storage

The existing `reaction_fingerprints` table is reused with two new kind
discriminators: `label-review` and `label-fix`. The primary key
`(issue_id, kind)` gives each command kind an independent high-water
mark and dispatched flag, preserving cross-kind isolation. No
migration is required; `kind` is TEXT.

One documented deviation: for these two kinds the `fingerprint` column
stores the high-water mark (newest processed journal event's timestamp
and id), not a hash of observed state. The column is opaque TEXT and
the table's upsert-and-flag semantics are unchanged, but readers of
the schema should not assume every fingerprint is a digest.

### Configuration surface

One block in the `reactions` family of `WORKFLOW.md` front matter:

```yaml
reactions:
  label_commands:
    provider: github          # required to activate; absent = feature off
    review_label: "sortie:review"   # default; "" disables the review command
    fix_label: "sortie:fix"         # default; "" disables the fix command
    poll_interval_ms: 60000         # default; minimum 30000
```

Activation follows the family invariant: the block absent or
`provider` empty means the feature is off and no journal read ever
happens. Setting `provider` while both labels are empty is a
configuration validation error — a loud misconfiguration, not a
silently inert block.

The defaults are namespaced (`sortie:` prefix) to keep out of the way
of team vocabularies, and they are only defaults: teams whose label
sets follow a policy configure their own names. Sortie never creates
labels; the operator creates the two labels (write access on GitHub)
or points the configuration at existing ones. This mirrors the GitHub
tracker adapter's existing label-hygiene stance for state labels.

The runtime kind discriminators (`label-review`, `label-fix`) differ
from the YAML key (`label_commands`), the same YAML-versus-runtime
asymmetry the other reaction kinds document.

### Scope and lifecycle

Detection covers Sortie-managed PRs only: those recorded in the run
metadata of issues in active states. Pending entries are seeded on
normal worker exit when PR metadata is present and the feature is
configured, and re-seeded by startup recovery — the same
seeding-and-recovery pattern the sibling reaction kinds use. When the
linked issue reaches a terminal state, the reaction state is cleared
with the rest of the issue's reactions; commands on such PRs are
ignored thereafter.

Journal-read transport errors back off per entry, as sibling kinds do.
There is no escalation posture and no retry budget for detection
itself: a human is in the loop by construction and re-gestures when
nothing visibly happens, and failures of the *dispatched session*
already flow through the normal agent retry and budget machinery.
The per-issue session and token ceilings also bound the blast radius
of gratuitous labeling: a PR cannot be commanded into unbounded spend.

The detection latency contract is one `poll_interval_ms` (default one
minute, floor thirty seconds). Against a dispatched session that runs
for minutes, the added latency is immaterial; if it ever matters, a
webhook accelerator can be added later under the platform's
advisory-trigger rule without changing this contract.

### Authorization posture

The platform's label permission is the gate: on GitHub, applying a
label requires the triage role or higher. That is a real gate —
comments, by contrast, are open to anyone who can read a public
repository. The residual risk is named openly: a triage-level user,
who cannot push code, can command an agent session that pushes to the
PR branch. The risk is bounded because the branch still lands only
through the ordinary review and merge gates, and because the per-issue
budgets cap the compute. The journal supplies the actor, which is
recorded in logs and the dispatch context. An actor allowlist is a
compatible future extension; the data for it is already flowing.

### Adapter surface

The `SCMAdapter` interface grows by one read and one write, additive
per the adapter pattern of
[ADR-0003](0003-adapter-based-integration.md) and following the
read-write widening precedent of
[ADR-0012](0012-auto-merge-reaction.md):

```go
type LabelEvent struct {
    ID    string    // provider journal entry id, opaque, unique per PR
    Label string    // normalized label name
    Actor string    // login of the acting user
    Added bool      // true = labeled, false = unlabeled
    At    time.Time // journal timestamp
}

ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]LabelEvent, error)
RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error
```

The GitHub adapter implements `ListLabelEvents` over the per-issue
events endpoint and `RemoveLabel` over the issue-labels endpoint,
normalizing at the boundary; platform field names do not leave the
adapter package. The GitHub journal endpoint offers no server-side
"since" filter, so the adapter pages; Sortie-managed PRs are
short-lived and low-traffic, keeping the journal small, and a page
cache is an available optimization if profiling ever demands one.

Both dispatch flavors introduce continuation prompt keys, which must
be registered and seeded to nil in the template data map, per the
`missingkey=error` rendering rule.

Token scopes follow the family's best-effort preflight pattern. The
review command needs the PR write scope it already needs to post
review comments; the fix command needs the content write scope it
already needs to push. Label removal rides the same write class; a
scope gap degrades acknowledgment only, never dedup correctness.

### Prior art

The design was checked against how established automation commands are
given on pull requests, rather than invented fresh:

- **Human-applied trigger labels are an established pattern.** Kodiak
  gates auto-merge on a label whose name is configurable
  (`merge.automerge_label`, default `automerge`, arrays allowed).
  The backport-action triggers backports from labels matched by a
  configurable regex (`label_pattern`, default `^backport ([^ ]+)$`,
  disableable). GitHub itself treats `labeled` as a first-class
  workflow trigger (`pull_request: types: [labeled]`).
- **No mature tool hardcodes its label names.** Every surveyed
  label-triggered tool ships a default and a configuration knob. This
  ADR follows that norm.
- **The other established channel is comment commands.** Kubernetes
  Prow implements `/foo`-style chat-ops (`/lgtm` typed by a human
  causes the bot to apply the `lgtm` label; merge automation then
  acts on labels); Dependabot is commanded via `@dependabot ...`
  comments. In those systems labels are machine-managed state and
  comments are the human channel. Sortie deliberately puts the human
  on the label because the platform grants labels a permission gate
  that comments lack, and because labels are already Sortie's
  operational vocabulary — the GitHub tracker adapter expresses issue
  states as labels.

### Considered Options in Detail

**Option A (snapshot polling).** Mirrors the episode model the
merge-conflict reaction uses and costs no new API surface — current
labels even ride the PR object other reactions already fetch. Rejected
on gesture fidelity and attribution: a label applied and removed
between ticks vanishes (defensible as retraction), but a label removed
and re-applied between ticks reads as *unchanged*, silently swallowing
a legitimate repeat command; and a snapshot names no actor, so
auditing and any future authorization are impossible. Snapshots also
force dedup to be inferred from observed absence between episodes,
which is racy at exactly the poll granularity the feature lives on.

**Option C (webhook receiver).** Push delivery of label events is
real (`pull_request` webhooks carry a `labeled` action), and latency
would drop from a minute to seconds. Rejected for the first version on
three grounds. First, the deployment posture: the embedded server
binds to localhost by default and is optional; a webhook receiver
must be reachable *by the platform*, which drags in public exposure,
TLS termination, and secret management for every operator. Second,
delivery reliability: the platform does not automatically redeliver
failed deliveries — a missed delivery is silent until something
reconciles, so a polling fallback must exist anyway. Third, the
platform's own webhook coexistence rule already binds: push events
may only accelerate an immediate reconcile pass; polling must remain
correct without them. A webhook is therefore always *in addition to*
the chosen mechanism, never *instead of* it — pure added surface for
v1, and an admissible accelerator later.

**Option D (comment commands).** The dominant human→bot channel in the
ecosystem, with strong precedent. Rejected for v1, not on fashion but
on fit: comments carry no platform permission gate (any reader of a
public repository can comment, so an allowlist becomes mandatory
security machinery on day one), while labels inherit the triage gate;
and Sortie's GitHub vocabulary is already label-based, so operators
manage one concept, not two. Comment commands remain a compatible
future channel: a second detection source could feed the same command
contract defined here without changing its semantics.

## Consequences

### Positive

- **No new network surface.** The feature ships inside the existing
  tick, preserving the single-binary, outbound-only posture.
- **Restart-safe at-most-once semantics.** The high-water mark and the
  dispatched flag live in `reaction_fingerprints`; crashes neither
  replay nor double-dispatch.
- **Full gesture fidelity.** Retraction, repetition, and bursts each
  have defined, explainable outcomes; nothing depends on the accident
  of poll timing except the bounded latency and cancellation windows.
- **Attribution for free.** Every command has an actor in the journal
  record, enabling audit today and an allowlist tomorrow.
- **Team-owned vocabulary.** Names are configurable, defaults are
  namespaced, either command is individually disableable, and Sortie
  never creates labels.
- **Portable contract.** The normalized `LabelEvent` shape is
  satisfiable by GitLab's resource label events; a future adapter fits
  without contract change.

### Negative

- **Latency is the poll interval.** Up to a minute (floor thirty
  seconds) between gesture and dispatch. Accepted against
  minutes-long sessions; a webhook accelerator remains possible.
- **One more read per managed PR per tick.** The journal endpoint has
  no since-filter on GitHub, so the adapter pages through small
  journals; cost grows with PR activity, not with repository size.
- **Acknowledgment needs a write.** Removing the label after dispatch
  is an SCM write; where the scope is missing, labels linger and the
  one-shot-button reading degrades until removed manually. Correctness
  is unaffected, but the WARN must be visible.
- **Triage-level users gain compute.** A user who cannot push code can
  start an agent session that pushes to the PR branch. Bounded by
  review/merge gates and per-issue budgets, but real, and stated here
  rather than discovered later.
- **Fingerprint semantics fork.** For two kinds the `fingerprint`
  column holds a position mark, not a digest. Documented, but a reader
  of the schema must now know both meanings.
- **Labels must pre-exist.** Operator setup gains one step (create or
  choose two labels); reference documentation must say so.

## Confirmation

The decision is validated when all of the following hold, using the
mock SCM adapter for integration coverage:

1. Applying a configured command label to a managed PR produces
   exactly one dispatch within one poll interval, with the actor
   recorded in the dispatch context.
2. Applying and removing the label within one interval produces zero
   dispatches, and the high-water mark still advances.
3. Removing and re-applying the label after a completed command
   produces a second dispatch.
4. Multiple label applications between two ticks produce one dispatch.
5. A process restart between ticks produces no duplicate dispatch; the
   mark and dispatched flag are read back from SQLite.
6. After a dispatch is scheduled, the command label is removed from
   the PR; a removal failure logs a warning and does not affect
   subsequent deduplication.
7. Gestures arriving while a command of the same kind is queued or
   running collapse into it; no second entry per kind exists.
8. A command label applied to a PR whose linked issue is terminal, or
   to a PR Sortie does not manage, produces no reaction.
9. With the `label_commands` block absent, no journal read is issued;
   with `provider` set and both labels empty, configuration validation
   fails.
