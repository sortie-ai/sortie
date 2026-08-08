## 11D. Bot Review Comment Feedback Contract

The `review` contract in §11B routes human `CHANGES_REQUESTED` review comments and deliberately
discards bot-authored comments. This section defines the complementary contract: the `bot-review`
reaction kind surfaces the half §11B drops. It detects automated review-bot PR comments, dispatches
them as agent continuation turns, and escalates under an independent budget. Like §11B it is a
read-only integration; it never resolves, replies to, or dismisses comments on the platform.

The two kinds run on the same PR without interference. Each owns a distinct `pending_reactions`
entry, `reaction_fingerprints` row, and `reaction_attempts` counter, because every key is composed
via `ReactionKey(issue_id, kind)` and the fingerprint table primary key is `(issue_id, kind)`.
There is the same YAML-versus-runtime asymmetry §11C.4 documents for auto-merge: the operator sets
`reactions.bot_review` in `WORKFLOW.md`, while the runtime and persisted kind discriminator is
`bot-review`.

### 11D.1 SCMAdapter interface

Bot classification is an SCM-adapter concern. `FetchPendingReviews` (§11B.1) returns the human
half; the `SCMAdapter` interface is widened with a sibling method that returns the bot half:

```go
FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]ReviewComment, error)
```

- `prNumber`, `owner`, and `repo` are sourced from `SCMMetadata` (`.sortie/scm.json`), never from
  the tracker project configuration. `botUsernames` is the operator allowlist and MAY be empty.
- Returns a non-nil (possibly empty) `[]ReviewComment` on success or a `*SCMError` (§11B.3) on
  failure.
- Implementations MUST be safe for concurrent use.

The result reuses the `ReviewComment` shape from §11B.2 unchanged; the bot's login is carried in the
`reviewer` field. No author-type field is added to `ReviewComment`, because classification is
resolved inside the adapter before the struct is constructed. No platform field name (`user.type`,
`position`) leaves the adapter package.

### 11D.2 Classification predicate

A review or comment is bot-authored when the platform reports `user.type == "Bot"` OR the author
login matches a `botUsernames` entry under case-insensitive comparison. The two conditions are a
union: either qualifies. The allowlist is the operator's escape hatch for bots that do not set the
`Bot` user type. Classification reads only `user.type` and `user.login`; no comment-body content
heuristic is used.

This is the mirror image of the `FetchPendingReviews` selection, with one further difference: the
bot path MUST NOT require a `CHANGES_REQUESTED` review state. Automated review tools commonly post
`COMMENTED` reviews, so any review or comment passing the bot-author predicate is selected
regardless of review state. Comments the platform marks outdated are returned with `outdated == true`
for the caller to filter.

### 11D.3 Reconcile loop integration

Bot-review reconciliation runs after `review` reconciliation and before the auto-merge pass in the
poll tick. It mirrors the `review` loop (§11B.4) with two omissions and one substitution: there is
no debounce gate and no `LastEventAt` tracking, and it calls `FetchBotReviewComments` with the
allowlist instead of `FetchPendingReviews`. The flow:

1. Skip entirely when no `SCMAdapter` is constructed or when bot-review is not configured.
2. For each `pending_reactions` entry with kind `bot-review`:
   a. Remove the entry from the map (prevents reprocessing within the same tick).
   b. Type-assert the kind data; on mismatch, log and skip without panicking.
   c. Drop the entry when its age exceeds the pending TTL backstop.
   d. Respect the `PendingRetryAt` poll throttle: if `now < PendingRetryAt`, re-enqueue and continue.
   e. Check the continuation-turn cap: if `reaction_attempts[issue_id:bot-review]` reaches
      `max_continuation_turns`, escalate (§11D.5) and continue.
   f. Call `FetchBotReviewComments`. On error, increment backoff, set `PendingRetryAt`, re-enqueue,
      count `sortie_bot_review_checks_total{result="error"}`, and continue.
   g. Filter outdated comments. When none remain, re-enqueue with the poll-interval delay.
   h. Compute the fingerprint (§11D.4) and upsert it. When the stored fingerprint matches and is
      marked dispatched, re-enqueue with the poll-interval delay.
   i. Dispatch immediately, with no debounce window: count
      `sortie_bot_review_checks_total{result="dispatched"}`, cancel any in-flight first-turn retry
      for the issue, schedule a continuation dispatch carrying the bot comments under the prompt key
      `bot_review_comments`, and increment `reaction_attempts[issue_id:bot-review]`.

Immediate dispatch is the defining runtime difference from `review`: bot comments arrive in bulk on
push, so there is no reviewer-pacing window to wait out.

### 11D.4 Fingerprint and independent tuning

The fingerprint is the same deterministic hash §11B.7 defines, `sha256(sorted(comment_id, ...))`
over the non-outdated comment IDs, stored in `reaction_fingerprints` with kind `bot-review`. Because
each push from a deterministic bot re-emits a fresh comment-ID set, the bot-review fingerprint churns
where the human-review fingerprint is sticky: a new push changes the set, the upsert resets the
dispatched flag to 0, and the next tick dispatches again. The sibling `review` fingerprint is never
touched.

The kind carries its own validated configuration, independent of `review`:

| Field | Default | Constraint |
|-------|---------|------------|
| `escalation` | `label` | `label` or `comment` |
| `escalation_label` | `needs-human` | none |
| `poll_interval_ms` | `60000` | `>= 30000` |
| `max_continuation_turns` | `5` | positive |
| `bot_usernames` | empty | list of strings |

The poll-interval default is tighter than `review`'s `120000` and the retry budget larger than
`review`'s `3`, because bot findings arrive in bulk and their fixes are mechanical. There is no
`debounce_ms` field. A `bot_usernames` value that is not a list, or a list with a non-string
element, is a startup configuration error.

### 11D.5 Escalation and ownership

When `reaction_attempts[issue_id:bot-review]` reaches `max_continuation_turns`, the orchestrator
escalates. Escalation follows the auto-merge cross-kind isolation contract in §11C.10, not the
`review` escalation path:

- `escalation: label` (default): add `escalation_label` (default `needs-human`) to the tracker issue
  via `TrackerAdapter.AddLabel`, in a detached goroutine with a 30-second timeout.
- `escalation: comment`: post a plain-text comment naming the PR number and the number of
  continuation turns attempted, under the same goroutine pattern.

Cleanup is scoped to the `bot-review` slot only: delete `pending_reactions[issue_id:bot-review]` and
the `bot-review` fingerprint row. The escalation path MUST NOT call `ClearReactionsForIssue`,
`CancelRetry`, or `DeleteRetryEntry`, MUST NOT delete `state.Claimed[issue_id]`, and MUST NOT delete
the residual `reaction_attempts[issue_id:bot-review]` counter. The issue claim and that residual
counter are owned by whichever kind currently holds the claim (the first-turn dispatch, `review`, or
`merge`); releasing them from the bot-review path would corrupt sibling-kind ownership. Because the
pending entry is deleted, the kind stops polling, but the residual counter and the claim are not
released by any path in the current implementation. `ClearReactionsForIssue` clears an issue's
residual `reaction_attempts` counters and `pending_reactions` entries, but it does not touch
`state.Claimed`, and no path, including the tracker-reconcile terminal-state path, calls it in
production. The residual counter and the claim persist for the life of the orchestrator process.
This mirrors how `escalateAutoMergeFailure` leaves `reaction_attempts[issue_id:merge]` and
`state.Claimed` uncleaned after a merge-kind escalation.

Escalation tracker-call failures are logged and counted
(`sortie_bot_review_escalations_total{action="error"}`) but do not block the slot-scoped cleanup.

### 11D.6 Seeding, recovery, and metadata validation

The `bot-review` lifecycle can outlive active tracker states: a pending entry is seeded on normal
worker exit while the issue is still claimed, and startup recovery recreates it for recent
handoff-stage issues after the claim has been released. §11B.9 requires that any such kind define
both restart recovery and kind-specific `.sortie/scm.json` metadata validation. Bot-review
discharges both halves.

Worker-exit seeding and startup recovery apply the same metadata predicate `review` uses: an entry
is created only when `SCMMetadata` reports `pr_number > 0` and non-empty `owner`, `repo`, and
`branch`. Bot-review introduces no metadata field beyond what `review` already validates. Recovery
is gated on a configured-flag distinct from adapter presence, so a configured-but-providerless setup
recovers no bot-review entries; the recovered count is surfaced in the startup recovery log.

### 11D.7 State machine

Per-issue `bot-review` reaction lifecycle (the `issue_id:bot-review` slot):

| From | Event | To | Action |
|------|-------|----|--------|
| (none) | Worker exits normally, SCM adapter and bot-review configured, PR metadata present | pending | Seed the entry if absent. |
| pending | Reconcile tick, `now < PendingRetryAt` | pending | Re-enqueue, no API call. |
| pending | Reconcile tick, fetch error | pending | Increment backoff, set `PendingRetryAt`, re-enqueue, count error. |
| pending | Reconcile tick, no actionable comments | pending | Re-enqueue at `now + poll_interval`. |
| pending | Reconcile tick, fingerprint unchanged and dispatched | pending | Re-enqueue at `now + poll_interval`. |
| pending | Reconcile tick, new actionable comments, attempts < cap | dispatched | Schedule continuation, increment attempts, count dispatched. |
| pending | Reconcile tick, attempts reach cap | escalated | Apply escalation; clear ONLY the `bot-review` slot. MUST NOT release the claim or clear sibling slots (§11D.5). |
| pending | Issue reaches terminal state (tracker reconcile) | pending | No effect: the tracker-reconcile terminal-state path does not call `ClearReactionsForIssue`. The slot keeps re-enqueuing until the TTL backstop drops it; the residual counter and the claim are not released by any path. |

