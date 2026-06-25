## 8. Polling, Scheduling, and Reconciliation

### 8.1 Poll Loop

At startup, Sortie validates config, performs startup cleanup, schedules an immediate tick, and
then repeats every `polling.interval_ms`.

The effective poll interval should be updated when workflow config changes are re-applied.

Tick sequence:

1. Reconcile running issues.
2. Run dispatch preflight validation.
3. Fetch candidate issues from tracker using active states.
4. Sort issues by dispatch priority.
5. Dispatch eligible issues while slots remain.
6. Notify observability/status consumers of state changes.

If per-tick validation fails, dispatch is skipped for that tick, but reconciliation still happens
first.

### 8.2 Candidate Selection Rules

An issue is dispatch-eligible only if all are true:

- It has `id`, `identifier`, `title`, and `state`.
- Its state is in `active_states` and not in `terminal_states`.
- It is not already in `running`.
- It is not already in `claimed`.
- Global concurrency slots are available.
- Per-state concurrency slots are available.
- Blocker rule passes: for issues in any non-running active state, do not dispatch when any blocker
  is non-terminal. The blocker-gating states are the configured active states, not a hardcoded
  state name.

Sorting order (stable intent):

1. `priority` ascending (1..4 are preferred; null/unknown sorts last)
2. `created_at` oldest first
3. `identifier` lexicographic tie-breaker

### 8.3 Concurrency Control

Global limit:

- `available_slots = max(max_concurrent_agents - running_count, 0)`

Per-state limit:

- `max_concurrent_agents_by_state[state]` if present (state key normalized)
- otherwise fallback to global limit

The runtime counts issues by their current tracked state in the `running` map.

Optional SSH host limit:

- When `worker.max_concurrent_agents_per_host` is set, each configured SSH host may run at most
  that many concurrent agents at once.
- Hosts at that cap are skipped for new dispatch until capacity frees up.

### 8.4 Retry and Backoff

Retry entry creation:

- Cancel any existing retry timer for the same issue.
- Store `attempt`, `identifier`, `error`, `due_at_ms`, `session_id` (continuation retries propagate the session ID from the exiting worker; error and reaction retries leave it null), and new timer handle.

Backoff formula:

- Normal continuation retries after a clean worker exit use a short fixed delay of `1000` ms.
- Failure-driven retries use `delay = min(10000 * 2^(attempt - 1), agent.max_retry_backoff_ms)`.
- Power is capped by the configured max retry backoff (default `300000` / 5m).

Retry handling behavior:

1. Fetch active candidate issues (not all issues).
2. Find the specific issue by `issue_id`.
3. If not found, release claim.
4. If found and still candidate-eligible:
   - Dispatch if slots are available.
   - Otherwise requeue with error `no available orchestrator slots`.
5. If found but no longer active, release claim.

Per-issue effort budget (defense-in-depth):

- When `agent.max_sessions > 0`, the retry handler counts completed sessions for the issue
  from `run_history` before fetching candidates.
- If the count reaches `max_sessions`, the claim is released and a warning is logged instead
  of re-dispatching.
- If the count query fails, the budget check is skipped (fail-open) and dispatch proceeds
  normally.
- `max_sessions = 0` (default) disables the budget entirely.

Per-issue token budget (cost ceiling):

- When `agent.max_tokens > 0`, the retry handler sums `total_tokens` across the issue's
  `run_history` entries on the same pre-dispatch path, after the session check.
- If the sum reaches `max_tokens`, the claim is released and a warning is logged instead of
  re-dispatching, identical in mechanism to the session ceiling.
- The session and token ceilings are independent hard ceilings. A re-dispatch is blocked when
  either is reached, so whichever fills first across polling cycles is the one that fires. When
  a single evaluation finds both exhausted, the reported and logged reason names the token
  budget; the block itself is identical regardless of which ceiling triggered it.
- The per-tick rebuild of the exhausted-issue set accounts for both ceilings: it runs the
  session-count batch query when `max_sessions > 0` and the token-sum batch query when
  `max_tokens > 0`, and unions the results. Each blocked issue carries a machine-readable
  reason (`token_budget` or `session_budget`, token taking precedence) surfaced in the runtime
  snapshot beside the exhausted set.
- If the token query fails, the check fails open and dispatch proceeds, matching the session
  check. A token sum recorded before the token columns were added reads as zero.
- `max_tokens = 0` (default) disables the budget entirely.

Note:

- Terminal-state workspace cleanup is handled by startup cleanup and active-run reconciliation
  (including terminal transitions for currently running issues).
- Retry handling mainly operates on active candidates and releases claims when the issue is absent,
  rather than performing terminal cleanup itself.

### 8.5 Active Run Reconciliation

Reconciliation runs every tick and has four parts.

Part A: Stall detection

- For each running issue, compute `elapsed_ms` since:
  - `last_agent_timestamp` if any event has been seen, else
  - `started_at`
- If `elapsed_ms > agent.stall_timeout_ms`, terminate the worker and queue a retry.
- If `stall_timeout_ms <= 0`, skip stall detection entirely.

Part B: Tracker state refresh

- Fetch current issue states for all running issue IDs.
- For each running issue:
  - If tracker state is terminal: terminate worker and clean workspace.
  - If tracker state is still active: update the in-memory issue snapshot.
  - If tracker state is neither active nor terminal: terminate worker without workspace cleanup.
- If state refresh fails, keep workers running and try again on the next tick.

Part C: CI status reconciliation (when `ci_feedback.kind` or `reactions.ci_failure` is configured)

- For each entry in `pending_reactions` with kind `ci`:
  - Call `CIStatusProvider.FetchCIStatus` with the SCM ref (SHA preferred, branch as fallback).
  - If the call fails: log a warning, re-enqueue the entry, and continue to the next entry.
  - If status is `passing`: clear reaction attempts for the issue and kind.
  - If status is `pending`: re-enqueue the entry for the next tick.
  - If status is `failing`: handle as a CI failure (see Section 7.3, "CI Status Failing").

Part D: Review comment reconciliation (when `reactions.review_comments` is configured)

- Skip entirely when no SCM adapter is configured (no `reactions.review_comments.provider`).
- For each entry in `pending_reactions` with kind `review`:
  - Remove entry from the map (prevents reprocessing within the same tick).
  - Respect `PendingRetryAt` poll throttle: if not yet due, re-enqueue and continue.
  - Check continuation turn cap (`reactions.review_comments.max_continuation_turns`): if
    exceeded, escalate (Section 11B.4) and continue.
  - Call `SCMAdapter.FetchPendingReviews` with the PR number, owner, and repo from
    `ReviewReactionData`.
  - If the call fails: increment backoff counter, set `PendingRetryAt` with exponential
    backoff, re-enqueue, and continue.
  - Filter out outdated comments. Compute max timestamp for debounce gating.
  - If no actionable comments: re-enqueue with poll interval delay.
  - Build fingerprint from sorted non-outdated comment IDs (SHA-256 hash).
  - Check `reaction_fingerprints` table: if fingerprint matches and is marked dispatched, skip.
  - If within debounce window (`now - LastEventAt < debounce_ms`): defer and re-enqueue.
  - Otherwise: mark dispatched in `reaction_fingerprints`, cancel existing retry, schedule a
    review-fix dispatch with review comment context, increment `reaction_attempts`.

Part E: Auto-merge reconciliation (when `reactions.auto_merge` is configured)

Activation gate: `reactions.auto_merge.provider` non-empty AND
`state.AutoMergePreflightFailed == false`.

If `state.AutoMergePreflightRetryDueAt` is non-zero and `now >= AutoMergePreflightRetryDueAt`,
run the scope-verification preflight first and clear or re-arm the flags before processing
entries.

For each entry in `pending_reactions` with kind `merge`:

1. Remove the entry from the `pending_reactions` map.
2. Drop with a WARN log when `state.AutoMergePreflightFailed == true`.
3. Drop when the entry has exceeded the configured TTL.
4. Re-enqueue when `now < pending.PendingRetryAt`.
5. Escalate when `reaction_attempts[issue_id:merge] >= MaxRetries`.
6. Fetch mergeability via `SCMAdapter.GetMergeability`; re-enqueue on transport error or
   `MergeabilityUnknown`; re-enqueue with poll interval on `Draft`, `Dirty`, or `Blocked`.
7. Fetch review decision via `SCMAdapter.GetReviewDecision`; re-enqueue when not `APPROVED`
   or `NOT_REQUIRED`.
8. When `require_ci == true`, fetch CI status via `SCMAdapter.GetCIStatus`; re-enqueue when
   not `success`.
9. Compute the fingerprint from `(head_sha, review_decision)`; upsert into
   `reaction_fingerprints` with `kind = "merge"`. Skip when the stored fingerprint matches
   and is marked dispatched.
10. Call `SCMAdapter.MergePR` with the configured strategy and the observed head SHA;
    increment `reaction_attempts[issue_id:merge]`.
11. On success: post a tracker comment; delete the branch when `delete_branch == true`; clear
    the fingerprint; remove the pending entry; increment
    `sortie_reactions_auto_merge_total{result="merged"}`.

Error dispositions:

- On `ErrSCMConflict` ("already merged"): treat as idempotent success; same actions as
  merge succeeded, including incrementing `sortie_reactions_auto_merge_total{result="merged"}`.
- On `ErrSCMConflict` (head SHA mismatch or branch-protection refusal): re-enqueue with
  poll interval; the next tick observes the new SHA via a refreshed fingerprint.
- On `ErrSCMAuth` on the merge call: escalate immediately (do not re-enqueue).
- On other transient errors: re-enqueue with backoff; escalate after `MaxRetries`.

Cross-kind isolation: the success and escalation paths MUST scope cleanup to `kind = "merge"`
only. They MUST NOT mutate `ci` or `review` reaction state (full invariant in §11C.10).

Part E runs after Part D so the precondition reads observe the most current per-kind state.

### 8.6 Startup Terminal Workspace Cleanup

When Sortie starts:

1. Enumerate workspace directories on disk.
2. Map directory names back to issue identifiers.
3. Query the tracker for the states of those specific issues.
4. For each issue in a terminal state, remove the corresponding workspace directory.
5. If the terminal-issues fetch fails, log a warning and continue startup.

This approach scopes the query to workspaces that actually exist on disk, avoiding expensive
full-project terminal issue sweeps for large trackers.

