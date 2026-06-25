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
- Only comments from `CHANGES_REQUESTED` reviews are returned. Approved reviews, comment-only
  reviews, and bot comments (`user.type == "Bot"`) are excluded.

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
  outdated:     bool        # true when the commented code was modified by a subsequent push
```

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
| `scm_conflict_error` | Conflict on a write-class operation (HTTP 405 method-not-allowed or HTTP 409 conflict from the merge endpoint). |

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
   b. Respect `PendingRetryAt` poll throttle: if `now < PendingRetryAt`, re-enqueue and continue.
   c. Check continuation turn cap: if `reaction_attempts[issue_id:review]` >=
      `max_continuation_turns`, escalate (Section 11B.6) and continue.
   d. Call `SCMAdapter.FetchPendingReviews(ctx, pr_number, owner, repo)`.
   e. On fetch error: increment backoff, set `PendingRetryAt`, re-enqueue, continue.
   f. Filter outdated comments. Compute max `submitted_at` timestamp for debounce.
   g. If no actionable comments: re-enqueue with poll interval delay and continue.
   h. Build fingerprint: `sha256(sorted(comment_id_1, comment_id_2, ...))` of non-outdated IDs.
   i. Upsert fingerprint in `reaction_fingerprints` (kind `review`). If stored fingerprint
      matches and is marked dispatched: skip, re-enqueue with poll interval delay.
   j. If `now - LastEventAt < debounce_ms`: set `PendingRetryAt = LastEventAt + debounce_ms`,
      re-enqueue.
   k. Mark dispatched in `reaction_fingerprints` synchronously (prevents duplicate dispatch on
      entry recreation by concurrent worker exit).
   l. Cancel existing retry for the issue.
   m. Schedule review-fix dispatch with `ContinuationContext{"review_comments": [...]}`.
   n. Increment `reaction_attempts[issue_id:review]`.

### 11B.5 Review comment handling

When actionable review comments are detected and debounce has elapsed:

1. Build a template map from actionable comments (Section 12.1).
2. Cancel existing continuation retry for the issue.
3. Schedule a review-fix dispatch carrying the review comment context via `ContinuationContext`.
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

Debounce uses `PendingRetryAt` — the same mechanism as CI pending backoff. When review comments
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

1. `SCMMetadata.pr_number > 0` — only workspaces where the agent created a PR have this field.
   Since `.sortie/scm.json` is written by the agent inside a Sortie-managed workspace, this is
   inherently scoped.
2. Runtime scope: review polling processes entries in `pending_reactions`. Normal worker exit can
  create those entries while the issue is still claimed, and startup recovery can recreate them for
  recent handoff-stage issues after the claim has been released.

Future reaction kinds whose lifecycle outlives active tracker states MUST define restart recovery
and kind-specific `.sortie/scm.json` metadata validation before they can be polled after handoff.

