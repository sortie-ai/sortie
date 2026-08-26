## 11B. PR Review Comment Feedback Contract

This section defines the SCM adapter interface for PR review comment fetching and the
orchestrator's review comment feedback loop. Review comment routing is a read-only integration: it
queries human review comments on Sortie-created PRs and injects structured context into agent
continuation prompts. It does not create PRs, approve reviews, or resolve comments.

### Naming convention

SCM adapters use the `*Adapter` suffix rather than `*Provider`. The distinction matches the
tracker and agent naming: an adapter manages a broader integration surface with multiple
operations and may carry per-instance state (HTTP client, auth token). The adapter today covers both read-only review-comment fetching (this section) and the
merge-write surface used by auto-merge (see §11C).

### 11B.1 SCMAdapter interface

```go
type SCMAdapter interface {
    FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]ReviewComment, error)
}
```

- `prNumber` is the pull request number. `owner` and `repo` identify the repository. These
  values are sourced from `SCMMetadata` (written by the agent to `.sortie/scm.json`), never
  from the tracker project configuration.
- Returns a non-nil (possibly empty) `[]ReviewComment` on success or a `*SCMError` on failure.
- Implementations MUST be safe for concurrent use.
- Only comments from `CHANGES_REQUESTED` reviews are returned. Approved reviews and comment-only
  reviews are excluded here. Bot exclusion at this layer is platform-intrinsic only: an
  implementation excludes an author its platform reports as an automated bot account
  (`user.type == "Bot"` on GitHub); it does not consult the operator's `bot_usernames` allowlist.
  The reaction layer applies that second arm on the returned set, before debounce, fingerprint,
  and dispatch (§11B.4 step g, §11D.2).

**Gitea adapter.** A Gitea SCM adapter registers under kind `gitea`, at parity with the GitHub
adapter. It normalizes Gitea's native `REQUEST_CHANGES` review state to the `CHANGES_REQUESTED`
decision this contract uses, skips dismissed reviews, and deduplicates comments by identifier.
Gitea users carry no `type: Bot` marker, so `FetchPendingReviews` on Gitea cannot exclude a
bot-authored changes-requested review the way the GitHub adapter's platform-marker check does;
on Gitea, the reaction-layer username allowlist (§11D) is the only classification signal that
separates automated-reviewer feedback from human feedback, and it now applies on every provider
rather than only where the platform exposes no bot marker. The platform reports the two diff sides
as separate line fields rather than one
signed offset, so a comment carries the line of the side it is anchored to, and `outdated` stays
false because the route exposes no invalidation signal.

**GitLab adapter.** A GitLab SCM adapter registers under kind `gitlab`. GitLab has no review
object bundling a verdict with a comment set, so `FetchPendingReviews` composes the selection from
two reads: the merge request's dedicated reviewers route supplies each reviewer's review state, and
the notes route supplies the comments, joined on the author login. Only a reviewer whose review
state is `requested_changes` is kept, and because the platform attaches no comment to a verdict,
the returned set is every comment that reviewer wrote on the merge request rather than the comments
of one review round. No embedded user object in the API carries a platform bot marker, so a
reviewer is classified through a separate per-user lookup cached for the adapter's lifetime; a
lookup that fails for any reason other than a deleted account treats the reviewer as not a bot and
the read continues. `end_line` is always zero, because a GitLab comment position describes one
line, and `outdated` has no platform field: the adapter derives it by comparing the recorded head
SHA against the merge request's current head, so a comment carrying no diff position is never
outdated.

### 11B.2 ReviewComment structure

```text
ReviewComment:
  id:           string      # SCM-platform comment identifier
  file_path:    string      # file the comment is attached to; empty for PR-level comments
  start_line:   int         # first line of commented range; 0 for non-inline comments
  end_line:     int         # last line of commented range; 0 for single-line or non-inline
  reviewer:     string      # username of the comment author
  body:         string      # comment text
  submitted_at: time.Time   # UTC timestamp from the platform
  outdated:     bool        # true when the platform reports the commented code modified by a push
```

`outdated` is a positive signal only: `true` means the platform reported the commented code
modified by a subsequent push, and `false` means no such report was made; it does not assert that
the code is unchanged. A provider with no platform signal for anchor invalidation leaves the field
false for every comment it returns.

`submitted_at` feeds the debounce window and nothing else, and a platform value that is absent or
is not a valid RFC 3339 value normalizes to the zero time and does not fail the read. A zero value
cannot raise the debounce window's upper bound, so the affected comment set can dispatch up to one
debounce interval earlier than it otherwise would, and deduplication is unaffected because the
fingerprint (§11B.7) is built from comment identifiers.

### 11B.3 SCMError type

```text
SCMError:
  kind:    SCMErrorKind    # normalized error category
  message: string          # operator-friendly description
  err:     error           # underlying error (may be nil)
```

Error categories:

| Kind | Meaning |
|------|---------|
| `scm_transport_error` | Network or transport failure. |
| `scm_auth_error` | Authentication or authorization failure. |
| `scm_api_error` | Non-success HTTP status or API-level error. |
| `scm_not_found` | PR or repository does not exist. |
| `scm_payload_error` | Malformed or unexpected response structure. |
| `scm_conflict_error` | Conflict on the merge write path (HTTP 405 method-not-allowed or HTTP 409 conflict from the merge endpoint). No other operation returns this kind. |

`SCMError` implements `Error()` and `Unwrap()` for use with `errors.Is`/`errors.As`.

Orchestrator behavior on SCM errors:

- Log a warning with the PR number and error category.
- Increment backoff counter and set `PendingRetryAt` with exponential backoff.
- Re-enqueue the pending review entry.
- Increment `sortie_review_checks_total{result="error"}`.

### 11B.4 Reconcile loop integration

Review comment reconciliation runs as Part D of active run reconciliation (Section 8.5), after
CI status reconciliation. The flow is:

1. Skip entirely when `reactions.review_comments` is not configured (no `SCMAdapter`
   constructed).
2. For each entry in `pending_reactions` with kind `review`:
   a. Remove the entry from the map (prevents reprocessing within the same tick).
   b. Check the configured watch window: if `reactions.review_comments.watch_window_ms` is
      positive and the entry's age, measured from its creation, exceeds it, delete the entry's
      `reaction_attempts` counter, log a WARN record, and drop the entry (no re-enqueue). Default
      `1800000` (thirty minutes); `0` removes the bound.
   c. Respect `PendingRetryAt` poll throttle: if `now < PendingRetryAt`, re-enqueue and continue.
   d. Check continuation turn cap: if `reaction_attempts[issue_id:review]` >=
      `max_continuation_turns`, escalate (Section 11B.6) and continue.
   e. Call `SCMAdapter.FetchPendingReviews(ctx, pr_number, owner, repo)`.
   f. On fetch error: increment backoff, set `PendingRetryAt`, re-enqueue, continue.
   g. Filter outdated comments, then drop any surviving comment whose author matches
      `reactions.bot_review.bot_usernames` (§11D.2's allowlist arm). Compute max `submitted_at`
      timestamp over the surviving set for debounce; an excluded comment does not raise
      `LastEventAt`.
   h. If no actionable comments: re-enqueue with poll interval delay and continue.
   i. Build fingerprint: `sha256(sorted(comment_id_1, comment_id_2, ...))` of the surviving IDs.
   j. Upsert fingerprint in `reaction_fingerprints` (kind `review`). If stored fingerprint
      matches and is marked dispatched: skip, re-enqueue with poll interval delay.
   k. If `now - LastEventAt < debounce_ms`: set `PendingRetryAt = LastEventAt + debounce_ms`,
      re-enqueue.
   l. Consult the retry slot (Section 7.5). A non-nil incumbent means the pass defers,
      re-enqueuing the entry unchanged rather than dispatching.
   m. On a free slot: schedule review-fix dispatch with
      `ContinuationContext{"review_comments": [...]}`.
   n. Increment `reaction_attempts[issue_id:review]`.
   o. The fingerprint is marked dispatched in `reaction_fingerprints` later, in
      `HandleRetryTimer`, after the scheduled retry fires and dispatch succeeds.

### 11B.5 Review comment handling

When actionable review comments are detected and debounce has elapsed:

1. Build a template map from actionable comments (Section 12.1).
2. Consult the retry slot (Section 7.5). A non-nil incumbent means the pass defers instead of
   dispatching, leaving the incumbent untouched.
3. On a free slot: schedule a review-fix dispatch carrying the review comment context via
   `ContinuationContext`.
4. The worker injects the context into the prompt on turn 1 via `prompt.WithContinuationContext`.

Review-fix dispatches count toward the regular retry machinery but use a fixed delay rather than
exponential backoff.

### 11B.6 Escalation behavior

When `reaction_attempts[issue_id:review]` reaches `max_continuation_turns`:

- `escalation: label` (default): add `escalation_label` (default `needs-human`) to the tracker
  issue via `TrackerAdapter.AddLabel`. The label call runs in a detached goroutine with a 30-second
  timeout.
- `escalation: comment`: post a plain-text comment:
  ```
  Review fix continuation turns exhausted for PR #{pr_number} on branch {branch}.
  {turn_count} continuation turns attempted. Remaining review comments require human attention.
  ```

After escalation:

- Cancel any pending retry for the issue.
- Delete the persisted retry entry from SQLite.
- Release the claim (`delete claimed[issue_id]`).
- Clear all `reaction_attempts` and `pending_reactions` entries for the issue.

Escalation failures are logged and counted (`sortie_review_escalations_total{action="error"}`)
but do not block claim release.

### 11B.7 Fingerprint and debounce

The fingerprint is a deterministic hash of the current set of actionable review comments:

```text
fingerprint = sha256(sorted(comment_id_1, comment_id_2, ...))
```

Only non-outdated comment IDs are included. This means:

- New comments → fingerprint changes → dispatch triggered.
- Comment resolved/outdated → fingerprint changes → dispatch triggered with remaining comments.
- Same comments, no changes → fingerprint unchanged → skip.

The fingerprint is stored in `reaction_fingerprints` (Section 19.2) with kind `review`.

Debounce uses `PendingRetryAt`: the same mechanism as CI pending backoff. When review comments
are detected but the newest comment timestamp is within the debounce window
(`reactions.review_comments.debounce_ms`):

1. Set `LastEventAt` to the maximum `submitted_at` among fetched comments.
2. Set `PendingRetryAt = LastEventAt + debounce_ms`.
3. Re-enqueue the entry. The next reconcile tick re-checks after the debounce window expires.

### 11B.8 Adapter registration

SCM adapters register via the SCM adapter registry using `init()` functions:

```go
func init() {
    registry.SCMAdapters.Register("github", NewGitHubSCMAdapter)
}
```

The `SCMAdapterConstructor` signature is:

```go
type SCMAdapterConstructor func(adapterConfig map[string]any) (domain.SCMAdapter, error)
```

The `adapterConfig` parameter receives the merged config: `reactions.review_comments.Extra`
plus tracker credentials (API key, endpoint) when `tracker.kind` and the review comments
provider match.

### 11B.9 Scope filtering

Review comment reconciliation only processes PRs created by Sortie:

1. An entry is created only when `SCMMetadata` reports `pr_number > 0` and non-empty `owner`,
   `repo`, and `branch`. Since `.sortie/scm.json` is written by the agent inside a Sortie-managed
   workspace, this is inherently scoped.
2. Runtime scope: review polling processes entries in `pending_reactions`. Normal worker exit can
  create those entries while the issue is still claimed, and startup recovery can recreate them for
  recent handoff-stage issues after the claim has been released.

Future reaction kinds whose lifecycle outlives active tracker states MUST define restart recovery
and kind-specific `.sortie/scm.json` metadata validation before they can be polled after handoff.

