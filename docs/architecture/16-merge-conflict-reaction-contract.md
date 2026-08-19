## 11E. Merge Conflict Reaction Contract

The `auto-merge` contract in §11C defers indefinitely when `Mergeability == dirty` (§11C.5); it
does not act on the conflict. This section defines the complementary contract: the `merge-conflict`
reaction kind detects that a managed open PR has merge conflicts, dispatches a single rebase-and-resolve
continuation turn per no-conflict-to-conflict transition, and escalates under an independent episodic
budget. Like §11B and §11D it is a read-only integration; it never calls `MergePR` or modifies the
SCM platform state directly.

The two kinds coexist on the same PR without interference. Each owns a distinct `pending_reactions`
entry, `reaction_fingerprints` row, and `reaction_attempts` counter, because every key is composed
via `ReactionKey(issue_id, kind)` and the fingerprint table primary key is `(issue_id, kind)`.
There is the same YAML-versus-runtime asymmetry §11C.4 documents for auto-merge: the operator sets
`reactions.merge_conflicts` in `WORKFLOW.md`, while the runtime and persisted kind discriminator is
`merge-conflict`.

### 11E.1 SCMAdapter interface

Conflict detection reuses the existing `GetMergeability` read. No new method is added to the
`SCMAdapter` interface. The interface's `PRMergeStatus` return type carries these additive fields:

```go
// PRMergeStatus additions:
BaseBranch string
Closed     bool
```

`BaseBranch` carries the PR target (base) branch name, for example `"main"` or `"develop"`. Every
wired adapter populates it from the same pull-request object `GetMergeability` already fetches, at
no additional request cost: the GitHub and Gitea adapters from that object's base ref, the GitLab
adapter from the merge request's target branch. Other callers of `GetMergeability` (the auto-merge
reconcile pass) ignore the new field. Platform-specific field names do not leave the adapter
package; the orchestrator reads only the domain field.

`Closed` reports whether the provider considers the pull request no longer open, populated from the
same pull-request object at no additional request cost. A provider whose closed state subsumes
merging reports both, so a caller that wants the closed-without-merge condition tests
`Closed && !Merged` rather than `Closed` alone. The CI reaction (§11A.9) is the primary reader.

### 11E.2 Detection rule

A PR is conflicted when `status.Mergeability == MergeabilityDirty`. `MergeabilityUnknown` is a
deferral condition (the provider is still computing), not a conflict. The two values MUST NOT be
conflated: `MergeabilityUnknown` defers at the poll interval without touching the fingerprint or
the attempt counter, exactly as auto-merge handles it (§11C.5).

Not every adapter can satisfy the rule. The Gitea adapter's single `mergeable` boolean cannot
separate a conflict from an in-progress recheck, so it maps a conflicted pull request to
`MergeabilityUnknown` and never to `MergeabilityDirty` (§11C.5). This reaction therefore never arms
on that provider: its entry takes the U1 deferral on every tick until the TTL backstop drops it,
which escalates nothing and leaves no tracker-visible signal. Configuration shape is still valid, so
neither the offline validator nor construction rejects the pairing.

### 11E.3 Reconcile loop integration

The merge-conflict reconcile pass runs after `reconcileBotReviewComments` and before
`reconcileAutoMerge` in each poll tick. It is skipped entirely when no `SCMAdapter` is constructed
or when merge-conflict is not configured.

The state machine mirrors `reconcileCIStatus` (§11A), not the bot-review or auto-merge loops. The
defining property: there is no retry-budget check before the `GetMergeability` read. The read runs
on every due tick so the not-dirty branch (N1, which resets the per-episode attempt counter) is
always reachable. The budget check and the attempt increment live inside the dirty branch (D1), after
the precondition guards and the head-SHA dedup. The comparison is strict over-limit
(`attempts > MaxRetries`), matching `handleCIFailure`.

Loop body per `ReactionKindMergeConflict` entry in `state.PendingReactions`:

1. Delete the entry from the map (prevents reprocessing within the same tick).
2. Type-assert `KindData` to `*MergeConflictReactionData`; on mismatch, log and skip.
3. Drop the entry when its age exceeds `MergeConflictPendingTTL` (TTL backstop).
4. Respect the `PendingRetryAt` poll throttle: if `now < PendingRetryAt`, re-enqueue and continue.
5. Call `GetMergeability`. On error, increment the pending-backoff counter, set `PendingRetryAt`,
   re-enqueue, count `sortie_merge_conflict_checks_total{result="error"}`, and continue.
6. Switch on `status.Mergeability`:
   - `MergeabilityUnknown` (U1): re-enqueue at `now + poll_interval`; count `"unknown"`; do not
     touch the fingerprint or the attempt counter.
   - `MergeabilityDirty` (D1): apply the dirty-branch logic described in §11E.4 and §11E.5.
   - All other values (`clean`, `unstable`, `blocked`) (N1): delete the fingerprint row; delete the
     per-episode attempt counter (`delete(state.ReactionAttempts, rkey)`); re-enqueue at
     `now + poll_interval`; count `"clear"`. This closes the episode.

### 11E.4 Fingerprint and deduplication

Before any dispatch, the dirty branch applies three precondition guards, in order. All three run
before any counter increment, so a deferral never burns an attempt:

- **Retry-slot guard**: consult the retry slot (Section 7.5) first, ahead of every other guard. A
  non-nil incumbent means the branch defers as a whole, re-enqueuing the pending entry unchanged
  except for a refreshed `CreatedAt` and returning without touching the fingerprint, the counter,
  or `sortie_merge_conflict_checks_total`.
- **D1a**: if `status.HeadSHA == ""`, defer at poll interval (no rebase anchor).
- **D1b**: if `status.BaseBranch == ""`, defer at poll interval (no rebase target). This is
  defense-in-depth: every wired adapter always populates `BaseBranch` (§11E.1), so D1b is a safety
  net rather than a normal-operation path on any of them.

After all three guards pass, the fingerprint is computed as `sha256(headSHA)` and upserted into
`reaction_fingerprints` with `kind = "merge-conflict"`. The reaction reuses the existing table;
no migration is required. The fingerprint key `(issue_id, "merge-conflict")` is independent of
the `"merge"` fingerprint the auto-merge reaction maintains.

When the stored fingerprint matches the computed value and is marked dispatched, the entry is
re-enqueued at `now + poll_interval` with no increment and no dispatch. This dedup prevents a
persistent same-head observation from firing multiple continuations while the rebase is in flight.

The fingerprint churns on each new push: a new head SHA produces a new fingerprint with
`dispatched = 0`, so a conflict that persists across a rebase re-arms a new attempt. The N1 branch
deletes the row entirely when the PR is not dirty, so the first subsequent dirty observation always
finds `dispatched = 0` and dispatches.

### 11E.5 Escalation and episode exits

When the fingerprint dedup passes, the per-episode counter increments and the strict over-limit cap
is checked:

- `state.ReactionAttempts[rkey]++`; `attempts = state.ReactionAttempts[rkey]`
- If `attempts > MaxRetries` (default 1): invoke `escalateMergeConflictFailure`.
- Otherwise: invoke `dispatchMergeConflictContinuation`.

`escalateMergeConflictFailure` applies the configured escalation action (label or comment) in a
detached `TrackerOpsWg` goroutine with a 30-second timeout, then performs the episode-exit cleanup:

- `escalation: label` (default): add `escalation_label` (default `needs-human`) to the tracker
  issue via `TrackerAdapter.AddLabel`.
- `escalation: comment`: post a plain-text comment naming the PR number and the number of attempts,
  via `TrackerAdapter.CommentIssue`.

Episode-exit cleanup is identical for both escalation postures. After the action:

```
delete(state.PendingReactions,    ReactionKey(issueID, "merge-conflict"))
DeleteReactionFingerprint(issueID, "merge-conflict")
delete(state.ReactionAttempts,    ReactionKey(issueID, "merge-conflict"))
```

The counter reset is scoped to this kind's own slot, not an issue-wide clear. This makes both
episode exits symmetric: the N1 branch and the escalation exit each leave the counter deleted, so a
later independent conflict always opens a fresh episode at `attempts = 1` regardless of which exit
closed the prior episode.

Cross-kind isolation: `escalateMergeConflictFailure` MUST scope all deletions to the `merge-conflict`
kind only. It MUST NOT call `CancelRetry` or `DeleteRetryEntry`, MUST NOT clear any sibling kind's
slot, and MUST NOT `delete state.Claimed[issue_id]`. A failed merge-conflict resolution MUST NOT
invalidate parallel `ci`, `review`, `bot-review`, or `merge` reactions on the same issue. Escalation
tracker-call failures are logged and counted
(`sortie_merge_conflict_escalations_total{action="error"}`) but do not block the slot-scoped cleanup.

### 11E.6 Continuation data

The retry-slot guard in Section 11E.4 is unaffected by anything in this section: a deferral
returns before `dispatchMergeConflictContinuation` is ever called, so the `MarkReactionDispatched`
timing this section describes only ever runs on a tick that actually dispatches.

`dispatchMergeConflictContinuation` schedules a continuation turn via `ScheduleRetry` with the
prompt key `merge_conflict`, admitted into the slot the guard above already confirmed is free. The
continuation map contains:

| Key | Value |
|-----|-------|
| `pr_number` | PR number from `MergeConflictReactionData` |
| `branch` | PR head branch (the branch the agent rebases) |
| `head_sha` | `status.HeadSHA` at dispatch time |
| `base` | `status.BaseBranch`, the PR's real target branch, read live from `GetMergeability` on this tick |

`base` is always the PR object's actual base ref, not an assumed default branch. The orchestrator
already holds the PR object and cannot diverge from the platform's view; the agent does not
re-derive the base independently. `MarkReactionDispatched` is called on the same tick the
continuation is scheduled; this is the marker the D1 dedup branch reads in subsequent ticks.

The `merge_conflict` key MUST be registered in `prompt.continuationKeys` and seeded to nil in the
template data map so `Option("missingkey=error")` does not reject templates that reference the
field when no conflict continuation is active.

### 11E.7 Seeding and recovery

Worker-exit seeding applies the same metadata predicate `review` and `bot-review` use: an entry is
created only when `SCMMetadata` reports `pr_number > 0` and non-empty `owner`, `repo`, and
`branch`. The seeding block runs in the `WorkerExitNormal` branch of `HandleWorkerExit`, after the
existing auto-merge enqueue block, guarded on `params.SCMAdapter != nil &&
params.MergeConflictReactionConfigured`. A "create only if absent" guard preserves an in-progress
pending entry across re-exits, matching the review, bot-review, and auto-merge kinds.

Startup recovery re-seeds a `merge-conflict` pending entry for recent handoff-stage issues when
`SCMMetadata` passes the same predicate. The recovered count is surfaced in the startup recovery
log. Recovery is gated on the configured flag, so a configured-but-providerless setup recovers no
entries, matching the §11D.6 pattern for bot-review.

### 11E.8 Config and observability

Activation requires `reactions.merge_conflicts.provider` to be non-empty in `WORKFLOW.md`,
mirroring the `reactions.auto_merge.provider` activation pattern documented in §11C.4. The operator
omits the block to disable the reaction entirely.

Configuration fields:

| Field | Default | Constraint |
|-------|---------|------------|
| `max_retries` | `1` | non-negative; `0` means escalate on first detection |
| `escalation` | `label` | `label` or `comment` |
| `escalation_label` | `needs-human` | none |
| `poll_interval_ms` | `60000` | `>= 30000` |

The `max_retries` default of 1 is lower than other reaction kinds (which default to 2) because
merge-conflict resolution by a coding agent is less likely to succeed on a second attempt.

Two metrics counters:

- `sortie_merge_conflict_checks_total{result}`: incremented once per counted reconcile-loop outcome
  for a due entry. Label values: `dispatched`, `error`, `unknown`, `clear`. Dirty-branch deferrals
  (empty head SHA, empty base branch, or the same-head dedup early-return) and the escalation path do
  not increment this counter.
- `sortie_merge_conflict_escalations_total{action}`: incremented inside the escalation goroutine.
  Label values: `label`, `comment`, `error`.

### 11E.9 State machine

Per-issue `merge-conflict` reaction lifecycle (the `issue_id:merge-conflict` slot):

| From | Event | To | Action |
|------|-------|----|--------|
| (none) | Worker exits normally, SCM adapter and merge-conflict configured, PR metadata present | pending | Seed the entry if absent. |
| pending | Reconcile tick, `now < PendingRetryAt` | pending | Re-enqueue, no API call. |
| pending | Reconcile tick, fetch error | pending | Increment backoff, set `PendingRetryAt`, re-enqueue, count error. |
| pending | Reconcile tick, `Mergeability == unknown` | pending | Re-enqueue at `now + poll_interval`; do not touch fingerprint or counter. |
| pending | Reconcile tick, `Mergeability != dirty` | pending | Delete fingerprint; delete per-episode counter; re-enqueue. Episode closes. |
| pending | Reconcile tick, `Mergeability == dirty`, precondition guard fails (empty HeadSHA or BaseBranch) | pending | Re-enqueue at `now + poll_interval`; do not increment counter. |
| pending | Reconcile tick, `Mergeability == dirty`, fingerprint dispatched for this head | pending | Re-enqueue at `now + poll_interval`; do not increment counter. |
| pending | Reconcile tick, `Mergeability == dirty`, new head this reaction has not dispatched for, attribution `not_orchestrator` | pending | Delete the per-episode counter before incrementing, so the reported attempt count is `1` rather than continuing to climb across successive conflicting heads; record `HeadRecordedAt`. |
| pending | Reconcile tick, `Mergeability == dirty`, new head this reaction has not dispatched for, attribution `unknown` | pending | Leave the per-episode counter at its prior value; record `HeadRecordedAt`. |
| pending | Reconcile tick, `Mergeability == dirty`, new head, `attempts <= MaxRetries` | dispatched | Increment per-episode counter; schedule continuation; mark dispatched; count dispatched. |
| pending | Reconcile tick, `Mergeability == dirty`, new head, `attempts > MaxRetries` | escalated | Increment per-episode counter; apply escalation; delete slot, fingerprint, and per-episode counter. MUST NOT release the claim or clear sibling slots (§11E.5). |
| pending | Issue reaches terminal state (tracker reconcile) | (none) | Drop the `merge-conflict` pending entry and its per-episode counter, cancel and delete the issue's retry, and release the claim; `reaction_fingerprints` is left intact. |

