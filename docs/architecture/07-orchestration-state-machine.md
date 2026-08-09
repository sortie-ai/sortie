## 7. Orchestration State Machine

The orchestrator is the only component that mutates scheduling state. All worker outcomes are
reported back to it and converted into explicit state transitions.

### 7.1 Issue Orchestration States

This is not the same as tracker states (`To Do`, `In Progress`, etc.). This is the service's
internal claim state.

1. `Unclaimed`
   - Issue is not running and has no retry scheduled.

2. `Claimed`
   - Orchestrator has reserved the issue to prevent duplicate dispatch.
   - In practice, claimed issues are either `Running` or `RetryQueued`.

3. `Running`
   - Worker task exists and the issue is tracked in `running` map.

4. `RetryQueued`
   - Worker is not running, but a retry timer exists in `retry_attempts`.

5. `Released`
   - Claim removed because issue is terminal, non-active, missing, or retry path completed without
     re-dispatch.

Important nuance:

- A successful worker exit does not mean the issue is done forever.
- The worker may continue through multiple back-to-back coding-agent turns before it exits.
- After each normal turn completion, the worker re-checks the tracker issue state.
- If the issue is still in an active state, the worker should start another turn on the same live
  coding-agent thread in the same workspace, up to `agent.max_turns`.
- The first turn should use the full rendered task prompt.
- Continuation turns should send only continuation guidance to the existing thread, not resend the
  original task prompt that is already present in thread history.
- Once the worker exits normally with the issue still active and no handoff state configured, the
  orchestrator schedules a short continuation retry (about 1 second) so it can re-check whether
  the issue remains active and needs another worker session. Section 7.3 gives the full set of
  exit dispositions and the order they are evaluated in.

### 7.2 Run Attempt Lifecycle

A run attempt transitions through these phases:

1. `PreparingWorkspace`
2. `BuildingPrompt`
3. `LaunchingAgentProcess`
4. `InitializingSession`
5. `StreamingTurn`
6. `SelfReviewing`, entered only when `self_review.enabled` is true and the coding turn
   loop completed successfully (not on turn failure).
7. `Finishing`
8. `Succeeded`
9. `Failed`
10. `TimedOut`
11. `Stalled`
12. `CanceledByReconciliation`

Distinct terminal reasons are important because retry logic and logs differ.

### 7.3 Transition Triggers

- `Poll Tick`
  - Validate config, which forces a defensive reload, then apply the result to runtime state.
  - Reconcile active runs.
  - Run the periodic workspace sweep when due (Section 8.7).
  - Fetch candidate issues.
  - Dispatch until slots are exhausted. Dispatch is the only step gated on validation success.
  - Dispatched workers perform the optional dispatch-time in-progress transition
    (via `tracker.in_progress_state`) as their first step, before workspace preparation.

- `Worker Exit (normal)`
  - Unconditional steps, taken on every normal exit: remove the running entry, update aggregate
    runtime totals, and persist the completed run attempt to SQLite.
  - The exit then takes exactly one disposition. The conditions are evaluated in the order below
    and the first match wins, so an earlier disposition overrides every later one:

    1. The agent reported itself blocked through a soft stop. Suppress the continuation retry,
       cancel any pending retry, and release the claim. Blocked work has nowhere to continue to.
    2. The freshest tracker observation for the issue reports a terminal state, resolved with
       precedence reconciliation's observation, then the worker's own per-turn observation, then
       the dispatch-time snapshot. Suppress the handoff transition and the continuation retry,
       cancel any pending retry, and release the claim. A terminal state is a decision already
       made about this issue; overwriting it with the handoff state would undo it.
    3. A handoff state is configured, the issue is still in an active state, the exit is not a
       blocked soft stop, and the dispatch drives issue state. Perform the handoff transition
       (Section 11.5). On a successful transition, release the claim, unless the retry slot
       (Section 7.5) is occupied by an incumbent, in which case the incumbent is kept and the
       claim stays so the poll loop cannot clear it. On transition failure, a soft-stop exit
       releases the claim; a non-soft-stop exit schedules the continuation retry instead, unless
       the retry slot is already occupied, in which case the exit defers to the incumbent.
    4. Any other soft stop. Suppress the continuation retry, cancel any pending retry, and release
       the claim. An unrecognized soft-stop reason is logged before taking this path.
    5. The issue is still in an active state and the dispatch drives issue state. Schedule the
       continuation retry (attempt `1`) so the next tick can re-check whether the issue needs
       another worker session, unless the retry slot is already occupied, in which case the exit
       defers to the incumbent instead.
    6. Otherwise the issue is no longer in an active state. Cancel any pending retry and release
       the claim, unless the retry slot is occupied by a foreign incumbent, in which case the
       incumbent is kept and the claim stays to protect it — this is exactly the population
       mid-session-queued retries need protected, since the issue may have left the active states
       while a sibling reaction was still queued for it.
  - Reaction entries are enqueued only when the issue was claimed at the moment of exit, the exit
    either took the handoff disposition or left the issue still claimed, and the exit was not
    suppressed by a terminal observation. Because dispositions 3 and 6 can now retain the claim
    solely to protect a foreign retry-slot incumbent, the predicate is evaluated as if that claim
    had been released whenever it is retained for that reason alone, so protecting an incumbent
    never widens which reaction kinds a later exit seeds. A label-command dispatch still never
    satisfies this predicate: it takes disposition 6, and under this rule its retained claim
    counts as released for the predicate's purposes exactly as an ordinary released claim would.
  - Subject to that predicate, each configured reaction kind records its own pending entry when
    the workspace SCM metadata (§9.5) satisfies that kind's field requirements. The kinds are
    independent: several fire from one exit. Every kind except the CI kind creates its entry only
    when no entry of that kind already exists, which preserves in-progress debounce state; the CI
    entry is rewritten on each exit so it always carries the ref the latest run pushed.

- `Worker Exit (abnormal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Persist completed run attempt to SQLite.
  - Schedule an exponential-backoff retry when the retry slot (Section 7.5) is free; defer to the
    incumbent already occupying it otherwise, keeping the claim.

- `Agent Update Event`
  - Update live session fields, token counters, and rate limits.

- `Retry Timer Fired`
  - Re-fetch active candidates and attempt re-dispatch, or release claim if no longer eligible.

- `Reconciliation State Refresh`
  - Stop runs whose issue states are terminal or no longer active.

- `Stall Timeout`
  - Kill the worker unconditionally, then either schedule an exponential-backoff retry when the
    retry slot (Section 7.5) is free, or defer to the incumbent already occupying it.

- `CI Status Failing`
  - Consult the retry slot (Section 7.5) first. If an incumbent occupies it, defer: re-enqueue the
    pending entry with a refreshed `CreatedAt` and take none of the actions below on this tick —
    no run-history row, no counter increment, no dispatch, and no escalation.
  - On a free slot: persist CI failure run history and increment the CI fix attempt counter.
  - If within `ci_feedback.max_retries` (or `reactions.ci_failure.max_retries`): schedule a CI-fix
    dispatch with failure context injected into the prompt.
  - If retries exhausted: escalate (add label or post comment per escalation config),
    cancel retry, release claim.

- `Review Comments Detected`
  - Compute fingerprint from non-outdated review comment IDs.
  - If fingerprint is unchanged and already dispatched: skip.
  - If within debounce window: defer to next tick.
  - If within `reactions.review_comments.max_continuation_turns` and the retry slot is free:
    schedule a review-fix dispatch with review comment context injected into the prompt. If the
    slot is occupied by an incumbent, defer instead (Section 7.5).
  - If continuation turns exhausted: escalate (add label or post comment per escalation
    config), cancel retry, release claim.

- `Merge Completion Observed`
  - Observed on the reconcile tick for an issue still parked in `tracker.handoff_state` and not
    currently claimed by the orchestrator.
  - Latch idempotency on the merge commit identifier; a commit already latched and dispatched is
    dropped without a transition.
  - Transition the issue to `reactions.merge_completion.target_state`, the last
    orchestrator-driven tracker-state write in an issue's life, after the optional dispatch-time
    in-progress transition and the handoff transition on worker exit (§11G).
  - On failure, route by tracker error kind (§11G.5): a transport or API failure, or any other
    kind, retries with backoff bounded by `reactions.merge_completion.max_retries`, then
    escalates; an auth or payload failure escalates immediately, consuming no retry budget; a
    not-found issue stops, marks the fingerprint dispatched, and logs a warning, with no
    escalation.
  - No orchestration-state transition occurs: the claim was already released when the issue
    entered the handoff state.

### 7.4 Idempotency and Recovery Rules

- The orchestrator serializes state mutations through one authority to avoid duplicate dispatch.
- `claimed` and `running` checks are required before launching any worker.
- Reconciliation runs before dispatch on every tick.
- Restart recovery uses persisted state from SQLite for retry queues and session metadata,
  supplemented by tracker polling for current issue states and filesystem inspection for workspace
  existence.
- Startup pending reaction recovery uses `run_history`, tracker state, and `.sortie/scm.json` to
  reconstruct runtime `pending_reactions` for recent handoff-stage runs before the first poll tick.
  The scan is bounded by `PendingReactionRecoveryLookback` and a fixed candidate cap.
- Startup terminal cleanup maps existing workspace directories to issue identifiers, queries the
  tracker for the states of those specific issues, and removes the ones in terminal states. An
  issue that reaches a terminal state through the merge-completion transition (§11G) becomes
  eligible for this same cleanup without a human relabeling it first.

#### Startup Recovery Sequence (SQLite)

1. Open or create the SQLite database and run schema migrations.
2. Load persisted retry entries from SQLite.
3. Reconstruct retry timers from persisted `due_at` timestamps.
4. Map existing workspace directories to issue identifiers, query the tracker for the states of
   those specific issues, and clean the ones in terminal states.
5. Construct reaction providers and call `RecoverPendingReactions` to restore eligible CI and
  review pending entries for handoff-stage issues.
6. Query tracker for active issues and reconcile with persisted state.
7. Begin normal polling loop.

### 7.5 Retry-Slot Arbitration

`retry_attempts` holds at most one entry per issue: the retry slot. The entry's reaction kind
records the owner, with the empty string meaning the orchestrator's own continuation lane. Every
code path that wants to schedule a retry for an issue (a challenger) checks whether the slot is
occupied before writing. A non-nil occupant (the incumbent) means another unit of work already
owns the issue's next dispatch; the challenger defers instead of overwriting it, leaving the
incumbent untouched and making no other state mutation. The rule keys on occupancy alone: it does
not compare owners, rank reaction kinds, or preempt.

Only two writers are exempt from checking first, because each frees the slot itself immediately
before writing back into it: the retry-timer handler's inner reschedule, which pops and deletes
the entry it is replacing before rescheduling it, and the overdue-retry re-arm pass (below), which
cancels the same entry before re-arming it with a zero delay. Every other writer is admitted only
into a free slot.

Two liveness bounds keep an arbitrated slot from being held for the process lifetime:

- A reaction retry that the issue's own current state does not permit to dispatch is rescheduled
  with backoff, but only for as long as it has been pausing consecutively for that reason. Once
  that dwell reaches 30 minutes, the entry is dropped instead of rescheduled, its persisted row
  deleted, the claim released, and a warning logged naming the kind and how long it had paused.
  The dwell resets whenever the entry is held for any other reason, so an otherwise healthy retry
  is never penalized for an unrelated pause.
- A retry entry whose timer event was dropped (Section 8.4) never fires again on its own. A
  reconcile pass detects any entry whose due time is more than 60 seconds in the past and still
  carries an active timer handle, and re-arms it with a zero delay, changing nothing about the
  entry but its timer. A retry reconstructed at startup and still awaiting its first activation is
  excluded from this pass, so a restart alone never produces the re-arm warning.

