## 11G. Merge Completion Reaction Contract

The auto-merge contract in §11C and the merge-conflict contract in §11E both observe pull
request merge state to decide whether to act on the pull request itself. This section defines a
third observer of the same state: the `merge-completion` reaction kind, which transitions the
linked tracker issue to a terminal state once its managed pull request merges, independently of
who or what performed the merge. Unlike its two siblings it never calls an SCM write method and
never dispatches an agent continuation; its only side effect is the tracker transition. See
ADR-0017 for the decision record.

### 11G.1 Trigger and seeding

A pending `merge-completion` entry is created on normal worker exit when the SCM adapter is
configured, the reaction is configured, the exiting worker produced a non-empty workspace path,
and the workspace's `SCMMetadata` (§9.5) reports `pr_number > 0` and non-empty `owner` and `repo`.
Seeding additionally requires that the issue was claimed by the orchestrator at the moment of
exit, and that either the exit followed the handoff-transition path or the issue remains claimed
afterward. Unlike the checkout-bearing sibling kinds, no `branch` field is required: the reaction
performs no checkout and reads no branch. A "create only if absent" guard preserves an in-progress
pending entry across a re-exit on the same issue.

Startup recovery re-seeds a `merge-completion` pending entry from persisted run history for an
issue that satisfies the same metadata predicate, is currently sitting in the configured handoff
state, and whose most recent recorded activity falls inside the shared thirty-day recovery
lookback. Recovery is gated on the reaction being configured, matching the pattern the auto-merge,
bot-review, merge-conflict, label-review, and label-fix kinds already follow; the review kind is
the one exception, gated only on the SCM adapter being present rather than on a dedicated
configured flag. Recovery also caps the number of candidate issues considered per startup pass at
two hundred; an issue beyond that cap is silently skipped and its pending entry is not rebuilt
until a subsequent worker exit or a later startup recovers it.

### 11G.2 Reconcile loop integration

The merge-completion reconcile pass runs as Part J of active run reconciliation, last in the tick
order, after the label-command passes (§8.5 Part J summarizes where this pass sits in the tick;
this section is the full contract). Placing it last means a merge the orchestrator performed
earlier in the same tick, in Part G, is observed on this same pass rather than deferred to the
next one. The pass is skipped entirely when no SCM adapter or no tracker adapter is configured, or
when the reaction itself is not configured.

The pass fetches tracker state for every due entry's issue in one batched call per tick, not one
call per issue. A due entry that is not yet ready to defer, stop, or transition proceeds to a
`GetMergeability` read; a not-found result on that read drops the entry (the pull request is
gone), while any other read failure re-enqueues with backoff. The poll interval defaults to sixty
seconds and configuration rejects any value below thirty seconds. The exponential backoff applied
on a tracker state-read failure, a fingerprint upsert or read failure, or a mergeability-read
failure floors at that poll interval, not at a fixed constant.

Unlike five of its seven sibling reaction kinds, a `merge-completion` pending entry carries no
expiry. CI, review, bot-review, auto-merge, and merge-conflict each observe something transient
and bound their pending entries with a fixed TTL so a dropped observation does not poll forever.
Merge-completion carries no such TTL, the same posture as the label-review and label-fix kinds: a
merge instead waits on human review for an unbounded time, so this kind has no equivalent TTL
constant. The entry is bounded another way: it stops being re-enqueued once the issue leaves the
configured handoff state. It is rebuilt through two paths: a fresh pending entry seeded on a
qualifying normal worker exit (§11G.1), and a startup recovery re-seed for an issue still in the
handoff state within the shared thirty-day recovery lookback (§14.3).

Before any entry is examined, the pass checks once per tick whether the `target_state` captured
at construction is still a member of the runtime `tracker.terminal_states` list, which is read
fresh on every tick unlike the reaction configuration itself. A drift (the target state falling
out of a reloaded terminal-state list) logs one warning at the onset and suppresses repeats while
the condition persists. The check never changes an entry's disposition; a running orchestrator
keeps transitioning issues to the frozen target until it restarts, at which point the same drifted
configuration is rejected offline (§11G.3).

### 11G.3 Terminal-target selection rule

The reaction transitions the linked issue to the single state named by its own `target_state`
field. The field has no default and the target is never inferred from `tracker.terminal_states`:
deriving it by list position or by matching a well-known name would depend on ordering and
vocabulary that carry no guaranteed meaning, since a tracker's terminal-state list routinely mixes
a completion state with one or more abandonment states.

`target_state` is validated offline, with no network access, against the tracker configuration as
it stood when the check ran:

- Required whenever `reactions.merge_completion.provider` is non-empty.
- MUST NOT equal `tracker.handoff_state` (case-insensitive comparison).
- MUST NOT be a member of `tracker.active_states` (case-insensitive), falling back to the tracker
  adapter's default active-state list only when `tracker.active_states` itself is empty.
- MUST be a member of `tracker.terminal_states` exactly as written in configuration
  (case-insensitive), with no fallback to the adapter's default terminal-state list.

`tracker.handoff_state` and `tracker.terminal_states` are themselves required whenever the
reaction is configured, each reported as a distinct configuration error when absent.

Reaction configuration, including `target_state`, is captured once at orchestrator construction
and is not rebuilt when `WORKFLOW.md` is reloaded. A change to `target_state`, or to either
tracker prerequisite, takes effect only on the next restart. The offline validator runs the same
construction path, so its verdict cannot diverge from what a restart would build.

### 11G.4 Idempotency latch

The idempotency key is the `reaction_fingerprints` row identified by the issue and the kind
`merge-completion`, whose fingerprint value is the merge commit identifier
(`PRMergeStatus.MergeCommitSHA`, §11C.2) reported by `GetMergeability`, not the pull request
number. A merge reported with `Merged == true` but no commit identifier is treated as no
observation: the entry re-enqueues at the poll interval rather than latching on an empty value.

Before transitioning, the pass upserts the observed commit identifier into the fingerprint row. If
the stored value already equals the observed one and is marked dispatched, the transition is
skipped; this dedups the same merge across both repeated poll ticks and a process restart in
between. A different commit identifier (the issue produces a second managed merge) resets the
dispatched flag through the same upsert, re-arming the latch for exactly one further transition.

The row's lifecycle diverges from the sibling reaction kinds in three ways. First, on a successful
transition the row is marked dispatched and retained, never deleted: deleting it would let the
next poll tick observe the same merge as new. Second, when a transition attempt escalates, whether
immediately on an authentication or payload failure or after the retry bound is exhausted, only
the pending entry and the reaction-attempt counter are cleared; the fingerprint row is left
undispatched. This is deliberate residue rather than an oversight: a fingerprint left undispatched
lets a later reconcile of the same commit identifier, driven by a fresh pending entry from a
worker re-exit, retry the transition instead of the escalated attempt being treated as final.

Third, when `TransitionIssue` succeeds but the subsequent `MarkReactionDispatched` call itself
fails, the pending entry and the reaction-attempt counter are still cleared and a warning is
logged, but the fingerprint row is left undispatched even though the tracker transition already
completed. This residue is accepted rather than treated as a bug: the issue is terminal by the
time this happens, so on any later re-seed of the pending entry, the terminal-state drop in
§11G.7 removes it before a second `TransitionIssue` call is ever attempted.

A not-found transition failure (§11G.5) is the one exception that marks the row dispatched despite
never completing the transition, because there is no issue left for a later attempt to reach.

Cross-kind isolation: the stop and escalation paths clear only this kind's own pending entry and
reaction-attempt counter. Neither path deletes `state.Claimed[issue_id]` or any pending entry,
fingerprint row, or attempt counter belonging to another reaction kind on the same issue.

### 11G.5 Failure matrix by tracker error kind

A transition failure is routed by the tracker error taxonomy defined in §11:

| Error kind | Posture |
|------------|---------|
| `ErrTrackerTransport` | Retry with backoff, bounded by `max_retries`, then escalate. |
| `ErrTrackerAPI` | Retry with backoff, bounded by `max_retries`, then escalate. |
| Unclassified, or any kind not listed above | Routed with the retryable kinds above, the non-destructive default. |
| `ErrTrackerAuth` | Escalate immediately; consumes no retry budget. |
| `ErrTrackerPayload` | Escalate immediately; consumes no retry budget. |
| `ErrTrackerNotFound` | Stop: mark the fingerprint dispatched, drop the entry, log at warning level, no escalation. |

`max_retries` defaults to `2` and bounds only the transport and API dispositions; the comparison
is strict over-limit (`attempts > max_retries`) against a per-issue counter scoped to this kind.
The counter is incremented after every `TransitionIssue` call regardless of outcome, including on
an authentication or payload failure; that disposition consumes no retry budget only in the sense
that it skips the over-limit comparison against the counter, not that the counter is left
untouched. The counter's value at the point of escalation is still surfaced to the operator, in
the `comment` escalation posture's attempt count (§11G.6).

Not every orchestrator-driven tracker write in the failure model (§14) gets a later re-attempt (a
dispatch-time in-progress transition failure is only logged, and a handoff transition failure on a
soft-stop worker exit releases the claim without one), but this transition is the terminal event in
an issue's lifecycle under this reaction with no continuation of its own to fall back on: it fires
once, after the pull request has already merged. Within a single process run, a failure that is
only logged here is a failure an operator would otherwise never see. Across a restart or a
subsequent worker exit, the accepted residue described in §11G.4 gives the transition that later
attempt instead. This is why every disposition above ends in either a bounded retry followed by
escalation, an immediate escalation, or an explicit logged stop, and never a silent drop.

### 11G.6 Escalation behavior

Escalation applies one of two operator-configured postures, matching the mechanism the sibling
reaction kinds already use:

- `label` (default): add `escalation_label` (default `needs-human`) to the tracker issue via
  `TrackerAdapter.AddLabel`.
- `comment`: post a plain-text comment via `TrackerAdapter.CommentIssue` naming the pull request
  number, the repository, the configured target state, and the number of transition attempts made.

Both actions run in a detached `TrackerOpsWg` goroutine with a thirty-second timeout, so a slow or
failing tracker call does not block the reconcile tick. An escalation failure (the label or
comment write itself errors) is logged at warning level and does not reopen the pending entry: the
entry was already dropped from `state.PendingReactions` before the escalation ran, and it stays
dropped regardless of whether the escalation call succeeds.

### 11G.7 State machine

Per-issue `merge-completion` reaction lifecycle (the `issue_id:merge-completion` slot):

| From | Event | To | Action |
|------|-------|----|--------|
| (none) | Worker exits normally, SCM adapter and merge-completion configured, PR metadata present | pending | Seed the entry if absent. |
| pending | Reconcile tick, `now < PendingRetryAt` | pending | Re-enqueue, no API call. |
| pending | Reconcile tick, batched tracker state read fails | pending | Back off every due entry; re-enqueue; no forge call. |
| pending | Reconcile tick, issue missing from the state response | (none) | Drop the entry. |
| pending | Reconcile tick, issue already terminal | (none) | Drop the entry. |
| pending | Reconcile tick, issue currently claimed by the orchestrator | pending | Re-enqueue at the poll interval; no forge call. |
| pending | Reconcile tick, issue has left the configured handoff state | (none) | Stop; drop the entry. |
| pending | Reconcile tick, `GetMergeability` returns not-found | (none) | Drop the entry. |
| pending | Reconcile tick, `GetMergeability` fails (other error) | pending | Back off; re-enqueue. |
| pending | Reconcile tick, PR not yet merged | pending | Re-enqueue at the poll interval. |
| pending | Reconcile tick, PR merged with no reported commit identifier | pending | Re-enqueue at the poll interval; log a warning. |
| pending | Reconcile tick, fingerprint upsert fails | pending | Back off; re-enqueue; log a warning. |
| pending | Reconcile tick, fingerprint read fails (after a successful upsert) | pending | Back off; re-enqueue; log a warning. |
| pending | Reconcile tick, merge commit already latched and dispatched | (none) | Drop the entry without transitioning. |
| pending | Reconcile tick, new or unlatched merge commit, `TransitionIssue` succeeds | dispatched | Mark the fingerprint dispatched; clear the attempt counter; drop the entry. |
| pending | Reconcile tick, new or unlatched merge commit, `TransitionIssue` succeeds, `MarkReactionDispatched` fails | (none) | Clear the attempt counter; drop the entry; log a warning; fingerprint stays undispatched (accepted residue, §11G.4). |
| pending | `TransitionIssue` fails with `ErrTrackerNotFound` | dispatched | Mark the fingerprint dispatched anyway; clear the attempt counter; drop the entry; no escalation. |
| pending | `TransitionIssue` fails with `ErrTrackerAuth` or `ErrTrackerPayload` | escalated | Escalate immediately; clear the attempt counter; drop the entry; fingerprint stays undispatched. |
| pending | `TransitionIssue` fails (transport, API, or unclassified), `attempts <= max_retries` | pending | Back off; re-enqueue; fingerprint stays undispatched. |
| pending | `TransitionIssue` fails (transport, API, or unclassified), `attempts > max_retries` | escalated | Escalate; clear the attempt counter; drop the entry; fingerprint stays undispatched. |
