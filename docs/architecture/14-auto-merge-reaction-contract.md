## 11C. Auto-merge Reaction Contract

The auto-merge reaction is default-off; enablement requires `reactions.auto_merge.provider` to be
non-empty in `WORKFLOW.md`. The merge operation is irreversible: the orchestrator does NOT roll back
on partial-failure tail operations (branch delete, tracker comment). The reaction runs after the CI,
review-comment, bot-review-comment, and merge-conflict reconcile passes in the tick, so the
precondition reads observe the most current per-kind state; it runs before the label-review and
label-fix passes, whose relative ordering does not affect correctness because each of those two is
fully cross-kind isolated.

### 11C.1 SCMAdapter write surface

The `SCMAdapter` interface, introduced in §11B.1 for review-comment fetching, is widened by the
read-write surface defined by the interface-surface section of the auto-merge ADR. The full
interface now exposes six methods:

```text
SCMAdapter:
  FetchPendingReviews(ctx, prNumber, owner, repo) ([]ReviewComment, error)
  GetReviewDecision(ctx, prNumber, owner, repo)   (ReviewDecision, error)
  GetCIStatus(ctx, prNumber, owner, repo)         (string, error)
  GetMergeability(ctx, prNumber, owner, repo)     (PRMergeStatus, error)
  MergePR(ctx, prNumber, owner, repo, strategy, commitTitle, commitMessage, expectedHeadSHA) (MergeResult, error)
  DeleteBranch(ctx, owner, repo, branch)          error
```

`FetchPendingReviews` is the original read-only method documented in §11B.1. The five new methods
provide the write surface needed by the auto-merge reconcile loop. All implementations MUST be safe
for concurrent use.

### 11C.2 Domain types

The auto-merge reaction introduces five domain types:

- `MergeStrategy`: merge strategy for `MergePR`. One of `merge`, `squash`, `rebase`.
- `ReviewDecision`: normalized review status. One of `APPROVED`, `CHANGES_REQUESTED`,
  `REVIEW_REQUIRED`, `NOT_REQUIRED`.
- `MergeabilityState`: PR mergeability classification. One of `clean`, `unstable`, `blocked`,
  `dirty`, `unknown` (lowercase string values).
- `PRMergeStatus`: struct carrying the merge precondition state. Fields: `ReviewDecision`,
  `CIConclusion`, `Draft` (bool, separate from `Mergeability`), `Mergeability`
  (`MergeabilityState`), `HeadSHA`, `BranchName`. `ReviewDecision` and `CIConclusion` are unset
  by `GetMergeability`; callers obtain those from dedicated reads.
- `MergeResult`: struct carrying `SHA` (merge commit SHA), `Merged` (bool reporting whether the
  merge completed), and `Message` (provider-supplied status text). The already-merged case is
  NOT signaled on this struct; it is signaled via `*SCMError` with kind `ErrSCMConflict` and the
  substring `already merged` (case-insensitive) in the error message. Callers disambiguate that
  subcase as documented in §11C.3.

### 11C.3 Error kind

`ErrSCMConflict` is the sixth `SCMErrorKind` value. Its HTTP semantics (405 method-not-allowed
and 409 conflict from the merge endpoint) are documented in the §11B.3 table.

The auto-merge reconcile loop imposes merge-specific dispositions on this kind. A 409 response
with an "already merged" body is treated as idempotent success. A 405 or a 409 with a head-SHA
mismatch or branch-protection refusal is re-enqueued with the poll interval; the next tick
observes the new SHA via a refreshed fingerprint. The full disposition table is in §11C.5 and
§11C.6.

### 11C.4 Reconcile loop integration

The auto-merge reconcile loop runs as Part G of active run reconciliation (see §8.5 Part G for
the algorithm). The loop processes entries from `pending_reactions` whose kind discriminator is
`"merge"`.

There is an intentional asymmetry between the YAML configuration key and the runtime kind value.
The YAML key the operator sets in `WORKFLOW.md` is `reactions.auto_merge`. The runtime and
persisted kind value used in `pending_reactions` map keys and in the `reaction_fingerprints`
table's `kind` column is `"merge"`, not `"auto_merge"`. A reader who only sees the YAML key
would expect the runtime kind to be `auto_merge`; it is `merge`.

### 11C.5 Merge precondition state machine

The auto-merge reconcile loop evaluates the merge preconditions reported by `GetMergeability`
(via `PRMergeStatus`) alongside the review-decision and CI-conclusion reads before calling
`MergePR`. The transition table below summarizes the loop's decisions. `Draft` is the
`PRMergeStatus.Draft` boolean (not a `MergeabilityState` value); `Mergeability` is the
`PRMergeStatus.Mergeability` field:

| From state | Observation | To state | Action |
|------------|-------------|----------|--------|
| Pending | `Draft == true` | Pending | Re-enqueue with poll interval. |
| Pending | `Mergeability == dirty` | Pending | Re-enqueue with poll interval. |
| Pending | `Mergeability == blocked` | Pending | Re-enqueue with poll interval. |
| Pending | `Mergeability == unknown` | Pending | Re-enqueue with poll interval (transient state). |
| Pending | `Mergeability` not in (`clean`, `unstable`) | Pending | Re-enqueue with poll interval. |
| Pending | `ReviewDecision != APPROVED` and `!= NOT_REQUIRED` | Pending | Re-enqueue with poll interval. |
| Pending | `require_ci == true` and `CI != success` | Pending | Re-enqueue with poll interval. |
| Pending | All preconditions satisfied (`Mergeability` in (`clean`, `unstable`); `!Draft`; review and CI satisfied) | Merging | Call `SCMAdapter.MergePR`. |
| Merging | Merge succeeded | Done | Post tracker comment; delete branch when `delete_branch == true`; clear fingerprint; increment `sortie_reactions_auto_merge_total{result="merged"}`. |
| Merging | `ErrSCMConflict` ("already merged") | Done | Treat as idempotent success; same actions as merge succeeded. |
| Merging | `ErrSCMConflict` (head SHA mismatch) | Pending | Re-enqueue with poll interval; next tick refreshes fingerprint. |
| Merging | `ErrSCMAuth` | Escalated | Escalate immediately; do not re-enqueue. |
| Merging | `ErrSCMPayload` | Escalated | Escalate immediately; do not re-enqueue. |
| Merging | Other transient error | Pending | Re-enqueue with backoff; escalate on a later tick when `MaxRetries > 0` and the attempt count reaches it (§11C.6). |

**Gitea auto-merge reads.** The Gitea adapter has no aggregate review-decision field and no
`mergeable_state` string, so it composes both preconditions itself. `GetReviewDecision` folds the
per-review list, taking each reviewer's latest approving or changes-requested review, normalizing
Gitea's native `REQUEST_CHANGES` review state to this contract's `CHANGES_REQUESTED`, and combining
the result with the PR's requested-reviewers signal into one decision. `GetMergeability` maps Gitea's single
`mergeable` boolean to `clean` (mergeable and not a draft), `blocked` (draft), or `unknown`
(anything else), and never yields `dirty` or `unstable`. Gitea cannot distinguish a merge conflict
from an in-progress recheck, so both present as `unknown`, which this table re-enqueues on the poll
interval as a transient state.

### 11C.6 Escalation behavior

The count-based escalation check guards the comparison with `MaxRetries > 0`: the orchestrator
escalates the issue when `MaxRetries > 0` and `reaction_attempts[issue_id:merge] >= MaxRetries`,
or immediately when `ErrSCMAuth` or `ErrSCMPayload` is returned on `MergePR` (both paths invoke
the escalation directly, bypassing the `MaxRetries` check). `MaxRetries` defaults to `2`.

A configured `MaxRetries` of `0` disables count-based escalation rather than triggering it on the
first attempt: the `> 0` guard keeps the comparison false no matter how large
`reaction_attempts[issue_id:merge]` grows, so a `merge`-kind entry with no retry budget keeps
retrying transient failures instead of escalating on them. This is the opposite of the sibling
merge-conflict reaction, whose comparison (`attempts > MaxRetries`, §11E.5) carries no such guard,
so a merge-conflict `MaxRetries` of `0` escalates on the first conflict detection (§11E.8). The
same `0` literal therefore carries two incompatible meanings across these two adjacent
`WORKFLOW.md` configuration blocks. An `ErrSCMAuth` or `ErrSCMPayload` failure still escalates
immediately regardless of the configured `MaxRetries`, including when it is `0`.

Two escalation postures are available, set by the operator in `WORKFLOW.md`:

- `label`: applies the `needs-human` label to the tracker issue via `TrackerAdapter.AddLabel`.
  This is the default posture.
- `comment`: posts a plain-text message to the tracker issue identifying the PR number, the
  retry count, and that manual merge is required.

Both postures record the escalation in the reaction attempt state and stop further auto-merge
retries for that issue.

Cross-kind isolation: escalation cleanup MUST scope deletions to the `merge` kind only. The
escalation path MUST NOT mutate `ci` or `review` reaction state. The full normative invariant
is in §11C.10.

### 11C.7 Fingerprint

Before calling `MergePR`, the reconcile loop computes a merge fingerprint as SHA-256 over the
head SHA, a newline byte, and the review-decision string (for example:
`sha256(headSHA + "\n" + reviewDecision)`). The fingerprint is upserted into the
`reaction_fingerprints` table with `kind = "merge"`.

The fingerprint's purpose is to deduplicate merge attempts: if the same head SHA and review
decision are observed across multiple ticks before a successful merge result is persisted, the
loop skips the re-attempt. When the merge succeeds, the fingerprint entry is cleared.

### 11C.8 Adapter registration

Activation is via `reactions.auto_merge.provider` being non-empty in `WORKFLOW.md`, mirroring
the `reactions.review_comments.provider` activation pattern documented in §11B.8. The
`SCMAdapterConstructor` type and registry registration code are defined in §11B.8; the
auto-merge path reuses the same constructor and registration mechanism without change.

### 11C.9 Token scope and preflight

The startup token-scope preflight is documented in full in §6.3. The auto-merge reaction
requires two token scopes:

- `pull_requests:write`: always required for `MergePR`.
- `contents:write`: required when `reactions.auto_merge.delete_branch != false` (for
  `DeleteBranch`).

A classic `repo` scope satisfies both. Fine-grained PAT permission names are accepted.

Scope verification is best-effort: the preflight relies on `X-OAuth-Scopes` returned by
`GET /rate_limit`. Classic personal access tokens populate that header; fine-grained PATs and
GitHub App installation tokens do not. When the verifier returns no scope information (and no
error), and when the adapter does not implement the scope-verifier interface, the preflight
fails open: it logs WARN and proceeds. Genuine scope gaps for those token classes surface at
runtime as `ErrSCMAuth` on the first `MergePR` attempt, which the orchestrator dispatches to
the auth-class escalation path. See §6.3 for the full fail-open semantics.

Three failure postures apply:

- **Auth-class sticky**: a missing-scope failure (the verifier returns one or more missing
  scope names) sets `state.AutoMergePreflightFailed = true` permanently for the process
  lifetime. Every subsequent `reconcileAutoMerge` tick drops `merge`-kind pending entries with
  a WARN log.
- **Transport-class bounded retry**: a transport failure on the preflight call schedules one
  retry via `state.AutoMergePreflightRetryDueAt`. After one retry attempt, any further
  transport failure leaves the sticky flag set.
- **Fail-open (no scope information)**: the verifier returned no scopes and no missing entries
  (fine-grained PAT or GitHub App token), or the adapter does not implement scope verification.
  The orchestrator proceeds with auto-merge enabled and surfaces any real scope gap at the
  first `MergePR` call.

**Gitea caveat.** The Gitea adapter cannot reproduce this scope check: Gitea exposes no
`/rate_limit` endpoint and returns no `X-OAuth-Scopes` header, so no startup request reveals a
token's own scope. Its preflight substitutes a user-role gate for scope verification: at startup
it reads the repository's `permissions.push` field for the configured token and fails the
preflight when the token's user lacks repository write access. This is a check on the token
user's role, not a verification of the token's own scope: a token whose user has write access
still passes even when the token itself is scoped read-only. That gap surfaces only at runtime,
when a `MergePR` or `DeleteBranch` call returns a 403 that the adapter enriches to name the
missing `write:repository` scope explicitly. An operator satisfies both checks by granting the
Gitea token's user repository write access and the token the `write:repository` scope.

For the full preflight algorithm, see §6.3.

### 11C.10 Cross-kind isolation

`postAutoMergeSuccess` and `escalateAutoMergeFailure` MUST NOT call any function that modifies
state owned by a different reaction kind. Specifically, these paths MUST NOT call:

- `CancelRetry`
- `DeleteRetryEntry`
- `ClearReactionsForIssue`
- `delete state.Claimed[issue_id]`

A failed auto-merge does NOT invalidate parallel `ci` or `review` reaction continuations on
the same issue, so the auto-merge cleanup path is scoped to the `merge` kind only.

