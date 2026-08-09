## 8. Polling, Scheduling, and Reconciliation

### 8.1 Poll Loop

At startup, Sortie validates config, performs startup cleanup, schedules an immediate tick, and
then repeats every `polling.interval_ms`.

The effective poll interval should be updated when workflow config changes are re-applied.

Tick sequence:

1. Run dispatch preflight validation, which triggers a defensive config reload so every later step
   in the tick sees the same fresh configuration snapshot.
2. Apply the reloaded configuration to runtime state.
3. Reconcile running issues.
4. Run the periodic workspace sweep when its tick counter is due (Section 8.7).
5. Fetch candidate issues from tracker using active states.
6. Sort issues by dispatch priority.
7. Dispatch eligible issues while slots remain.
8. Notify observability/status consumers of state changes.

Preflight runs first so the reload it forces is visible to reconciliation and to the sweep, not
only to dispatch. If validation fails, dispatch is skipped for that tick, but configuration is
still applied, reconciliation still runs, and the sweep still runs when due: those steps keep
orchestrator state aligned with the tracker using the last known good configuration, which remains
valid for that purpose. Dispatch is the only step gated on preflight success.

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

- The caller checks the retry slot first (Section 7.5): a challenger consults the slot and
  schedules only when it is free. The cancellation the primitive performs internally is a
  backstop for a call site that skipped the check, not the primitive's own contract.
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
6. A reaction retry the issue's own current state does not permit to dispatch is rescheduled with
   backoff, but only for as long as it has been pausing consecutively for that reason; past 30
   minutes of consecutive pausing it is dropped instead, its persisted row deleted and its claim
   released, with a warning naming the kind and the dwell (Section 7.5).
7. A retry entry whose timer event was never delivered is re-armed with a zero delay once its due
   time is more than 60 seconds in the past, so a dropped timer event self-heals within a few
   ticks instead of holding its slot for the process lifetime (Section 7.5).

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

- Terminal-state workspace cleanup is handled by startup cleanup, active-run reconciliation
  (including terminal transitions for currently running issues), and the periodic workspace
  sweep (Section 8.7).
- Retry handling mainly operates on active candidates and releases claims when the issue is absent,
  rather than performing terminal cleanup itself.
- Within one periodic sweep pass, the terminal check always runs before the age bound (see
  `workspace.retention_days` in the configuration specification), and a key the terminal check
  removes is never re-evaluated by the age bound on that same pass. The age bound needs no answer
  from the tracker: it is evaluated from the workspace listing and the persistence layer alone, so
  it runs whether or not the tracker state read for that pass succeeded, and it still evaluates and
  can remove eligible workspaces on a pass where that read failed. Section 8.7 defines the full
  pass.

### 8.5 Active Run Reconciliation

Reconciliation runs every tick, in the order below.

Part A: Stall detection

- For each running issue, compute `elapsed_ms` since:
  - `last_agent_timestamp` if any event has been seen, else
  - `started_at`
- If `elapsed_ms > agent.stall_timeout_ms`, terminate the worker unconditionally, then queue a
  retry when the retry slot is free or defer to the incumbent already occupying it, re-emitting
  the cancellation warning on every stalled tick either way (Section 7.5).
- If `stall_timeout_ms <= 0`, skip stall detection entirely.

Part B: Tracker state refresh

- Fetch current issue states for the deduplicated union of all running issue IDs and every issue
  ID holding a pending reaction entry; skip the call entirely when that union is empty or no
  tracker adapter is configured.
- For each running issue:
  - If tracker state is terminal: terminate worker and clean workspace.
  - If tracker state is still active: update the in-memory issue snapshot.
  - If tracker state is neither active nor terminal: terminate the worker without workspace
    cleanup, then cancel the pending retry and delete its persisted row, unless the retry slot is
    occupied by an incumbent, in which case both are skipped together so the in-memory entry and
    the persisted row stay in agreement and the incumbent survives (Section 7.5). This population
    is exactly the mid-session-queued retries the design depends on protecting.
- For an issue reported terminal, whether or not it has a running worker: release the issue's
  pending reaction entries, reaction attempt counters, pending retry, and dispatch claim, leaving
  `reaction_fingerprints` intact. An issue with no running worker and no terminal state is left
  untouched.
- If state refresh fails, keep workers running and try again on the next tick, and release
  nothing.

Part C: CI status reconciliation (when `ci_feedback.kind` or `reactions.ci_failure` is configured)

- For each entry in `pending_reactions` with kind `ci`:
  - Deduplicate before fetching: a stored fingerprint equal to the current ref and already
    marked dispatched drops the entry for this pass (§11A.5).
  - Call `CIStatusProvider.FetchCIStatus` with the SCM ref (SHA preferred, branch as fallback).
  - If the call fails: log a warning, re-enqueue with an exponential backoff delay derived from
    the poll interval and the pending attempt count, and continue to the next entry.
  - If status is `passing`: clear reaction attempts for the issue and kind, and delete the
    fingerprint row.
  - If status is `pending`: re-enqueue with the same exponential backoff as the fetch-error path.
  - If status is `failing`: consult the retry slot before handling as a CI failure; a non-nil
    incumbent defers instead of dispatching (Section 7.5, Section 7.3 "CI Status Failing").

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
  - Check `reaction_fingerprints` table: if the fingerprint matches and is marked dispatched,
    re-enqueue with the poll interval delay and continue.
  - If within debounce window (`now - LastEventAt < debounce_ms`): defer and re-enqueue.
  - Otherwise: consult the retry slot (Section 7.5). A non-nil incumbent means the pass defers,
    re-enqueuing the entry unchanged rather than dispatching. A free slot schedules a review-fix
    dispatch with review comment context and increments `reaction_attempts`. The fingerprint is
    marked dispatched later, when the scheduled retry fires and dispatch succeeds, not during
    this pass.

Part E: Bot review comment reconciliation (when `reactions.bot_review` is configured)

- Skip entirely when no SCM adapter is configured.
- Mirrors Part D's reconcile loop for the `bot-review` kind, but with no debounce gate and no
  `LastEventAt` tracking, and it calls `SCMAdapter.FetchBotReviewComments` (allowlisted bot
  authors) instead of `FetchPendingReviews`.
- Dispatches immediately on a confirmed comment set, with no debounce window, because bot
  comments arrive in bulk on push rather than trickling in from a human reviewer.
- Consults the retry slot before dispatching, exactly as Part D: a non-nil incumbent defers
  instead (Section 7.5).
- See Section 11D for the full contract.

Part F: Merge conflict detection (when `reactions.merge_conflicts` is configured)

- Skip entirely when no SCM adapter is configured.
- For each `pending_reactions` entry with kind `merge-conflict`, call
  `SCMAdapter.GetMergeability` on every due tick (no retry-budget check gates the read). A
  dirty result runs the escalation-bearing conflict-resolution path; any other result closes
  the episode and clears its fingerprint and attempt counter.
- The dirty branch consults the retry slot before its other guards (Section 7.5); a non-nil
  incumbent defers the whole branch, re-enqueuing the entry unchanged.
- See Section 11E for the full contract.

Part G: Auto-merge reconciliation (when `reactions.auto_merge` is configured)

Activation gate: `reactions.auto_merge.provider` non-empty AND
`state.AutoMergePreflightFailed == false`.

If `state.AutoMergePreflightRetryDueAt` is non-zero and `now >= AutoMergePreflightRetryDueAt`,
run the scope-verification preflight first and clear or re-arm the flags before processing
entries.

For each entry in `pending_reactions` with kind `merge`:

1. Remove the entry from the `pending_reactions` map.
2. Drop with a WARN log when `state.AutoMergePreflightFailed == true`.
3. Drop when the entry has exceeded the TTL backstop; the TTL is a fixed internal constant
   (30 minutes), not operator-configurable.
4. Re-enqueue when `now < pending.PendingRetryAt`.
5. Escalate when `MaxRetries > 0` and `reaction_attempts[issue_id:merge] >= MaxRetries`; a
   configured `MaxRetries` of `0` disables count-based escalation instead of firing it
   immediately (Section 11C.6).
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
- On `ErrSCMPayload` on the merge call: escalate immediately (do not re-enqueue), the same
  disposition as `ErrSCMAuth`.
- On other transient errors: re-enqueue with backoff; escalate per the guard in item 5.

Cross-kind isolation: the success and escalation paths MUST scope cleanup to `kind = "merge"`
only. They MUST NOT mutate `ci` or `review` reaction state (full invariant in §11C.10).

Part G runs after Parts C through F (CI, review, bot review, and merge conflict) so its
precondition reads observe the most current per-kind state. It runs before the label-command
passes (Parts H and I); that relative order does not affect correctness because those passes
are fully cross-kind isolated (Section 11F.4).

Part H: Label review command detection (when `reactions.label_commands.review_label` is
configured)

- Skip entirely when no SCM adapter is configured or the `label_commands` block is absent.
- For each Sortie-managed PR, read the label-event journal past its stored high-water mark via
  `SCMAdapter.ListLabelEvents`. A confirmed `review_label` command is decided in this order: a
  foreign-kind incumbent occupying the retry slot defers (Section 7.5), leaving the label and the
  high-water mark untouched; a same-kind incumbent or a running worker of this kind collapses,
  advancing the mark without a second dispatch; otherwise the pass dispatches a read-only,
  no-clone review session and removes the label.
- Ordering relative to the other parts does not affect correctness: the pass is fully
  cross-kind isolated. See Section 11F for the full contract.

Part I: Label fix command detection (when `reactions.label_commands.fix_label` is configured)

- Structurally identical to Part H, scoped to the `fix_label` command and the `label-fix`
  reaction kind, including the same three-way retry-slot decision (Section 7.5).
- A confirmed command dispatches a read-write, clone-and-checkout fix session instead of the
  read-only review session.
- Ordering relative to the other parts does not affect correctness: the pass is fully
  cross-kind isolated. See Section 11F for the full contract.

Part J: Merge completion reconciliation (when `reactions.merge_completion` is configured)

- Skip entirely when no SCM adapter or tracker adapter is configured.
- Before examining any entry, check once whether the configured `target_state` is still a
  member of the runtime terminal-state list; log one warning per onset of drift, suppressed
  while the condition persists.
- For each due entry in `pending_reactions` with kind `merge-completion`, fetch tracker state
  for the linked issues in one batched read.
- Drop an entry whose issue is missing from the response or already terminal. Defer an entry
  whose issue is currently claimed. Stop an entry whose issue has left the configured handoff
  state.
- Fetch mergeability via `SCMAdapter.GetMergeability`; re-enqueue with backoff on a transport
  error, and re-enqueue on the poll interval while the pull request is not yet reported merged
  or reports no merge commit identifier.
- Upsert the merge commit identifier into `reaction_fingerprints` under kind
  `merge-completion`; skip an entry already latched to that identifier.
- Call `TrackerAdapter.TransitionIssue` to the configured `target_state`. Route a failure by
  the tracker error taxonomy: retry with backoff up to `max_retries` then escalate for
  transport and API failures, escalate immediately for auth and payload failures, and stop
  without escalating for a not-found issue.
- Unlike the CI, review, bot-review, merge-conflict, and auto-merge kinds in this list, a pending
  entry of this kind carries no expiry, the same posture as the label-review and label-fix kinds
  (Parts H and I): a merge can wait on human review for days, so the entry is bounded instead by
  the issue leaving the handoff state and by the pending-reaction recovery lookback.
- Placed last so a merge the orchestrator performed earlier in the same tick, in Part G, is
  observed on this same pass.
- See §11G for the full contract.

### 8.6 Startup Terminal Workspace Cleanup

When Sortie starts:

1. Enumerate workspace directories on disk.
2. Map directory names back to issue identifiers.
3. Query the tracker for the states of those specific issues.
4. For each issue in a terminal state, remove the corresponding workspace directory.
5. If the terminal-issues fetch fails, log a warning and continue startup.

This approach scopes the query to workspaces that actually exist on disk, avoiding expensive
full-project terminal issue sweeps for large trackers.

### 8.7 Periodic Workspace Sweep

Startup cleanup and active-run reconciliation between them cover an issue that is terminal when the
process starts and an issue that turns terminal while a worker is running. Neither covers an issue
whose worker has already exited: it holds no running entry, so nothing observes it turning terminal
and nothing observes its workspace aging on disk. The periodic sweep closes that gap.

The sweep runs on the poll tick, throttled to one pass every sixty ticks rather than every tick,
because its cost scales with the number of leftover directories rather than with the number of
running agents. Its work is housekeeping: a workspace left behind wastes disk, but it never
produces a wrong scheduling decision, so bounded tracker load is worth more than immediate removal.

One pass proceeds as follows:

```text
sweep_workspaces(state, config):
  keys = list_workspace_keys(config.workspace.root)
  if listing failed:
    log_warning()
    return                              # no summary record for this pass

  exclusions = {}                       # workspace key -> reason, first writer wins
  for entry in state.running:
    set_if_absent(exclusions, key_of(entry), "running")
  for entry in state.retry_attempts:
    set_if_absent(exclusions, key_of(entry), "retry")
  for entry in state.pending_reactions:
    if reaction_kind_pins_workspace(entry.kind):
      set_if_absent(exclusions, key_of(entry), "reaction")

  remaining = keys - exclusions.keys
  states = tracker.fetch_issue_states_by_identifiers(remaining)
  if fetch failed:
    states = {}                         # every key falls through to the age bound

  terminal_keys = [k for k in remaining if states[k] is known and terminal]
  age_keys      = remaining - terminal_keys

  remove_workspaces(terminal_keys)      # the terminal gate
  run_age_bound(age_keys, config.workspace.retention_days)
  emit_sweep_summary()
```

Rules that govern the pass:

- The candidate set for a pass is every workspace key the listing returned. A key is excluded from
  evaluation before any tracker call when it belongs to a running entry, a scheduled retry, or a
  pending reaction entry whose kind pins its workspace. Exclusion precedence is running, then
  retry, then reaction, so a key present in more than one source is attributed to exactly one
  reason and the pass's outcome counters partition the candidate set (Section 13.1).
- A pending reaction entry pins its workspace only when its kind carries an expiry. A kind that
  polls an external fact indefinitely (§11F and §11G) does not pin, because pinning it would hold a
  workspace on disk for as long as that unbounded poll runs. A kind the orchestrator does not
  recognize pins, the non-destructive default.
- The terminal gate removes every evaluated key the tracker reports in a terminal state. A key
  whose state the tracker does not report on is not removed by this gate.
- The age bound then evaluates whatever the terminal gate left, under the rules in Section 9.1. A
  failed tracker read leaves every evaluated key unclassified, so all of them reach the age bound
  rather than being skipped.
- Each pass that produced a candidate set emits exactly one summary record, including a pass over
  zero keys and a pass that removed nothing (Section 13.1). A pass that could not list the
  workspace root emits no summary and removes nothing.

