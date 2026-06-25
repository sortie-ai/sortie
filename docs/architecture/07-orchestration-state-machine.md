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
- Once the worker exits normally, the orchestrator still schedules a short continuation retry
  (about 1 second) so it can re-check whether the issue remains active and needs another worker
  session.

### 7.2 Run Attempt Lifecycle

A run attempt transitions through these phases:

1. `PreparingWorkspace`
2. `BuildingPrompt`
3. `LaunchingAgentProcess`
4. `InitializingSession`
5. `StreamingTurn`
6. `SelfReviewing` — entered only when `self_review.enabled` is true and the coding turn
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
  - Reconcile active runs.
  - Validate config.
  - Fetch candidate issues.
  - Dispatch until slots are exhausted.
  - Dispatched workers perform the optional dispatch-time in-progress transition
    (via `tracker.in_progress_state`) as their first step, before workspace preparation.

- `Worker Exit (normal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Persist completed run attempt to SQLite.
  - Schedule continuation retry (attempt `1`) after the worker exhausts or finishes its in-process
    turn loop.
  - When a CI status provider is configured, the workspace contains SCM metadata
    (`.sortie/scm.json` with a non-empty `branch`), and the issue is still claimed: record a
    pending CI check entry for reconciliation.
  - When an SCM adapter is configured, the workspace contains SCM metadata with
    `pr_number > 0`, non-empty `owner`, and non-empty `repo`, and the issue is still claimed:
    record a pending review comment entry for reconciliation. Only created if no entry already
    exists (preserves in-progress debounce state).
  - When an SCM adapter is configured (`reactions.auto_merge.provider` non-empty), the workspace
    contains SCM metadata with `pr_number > 0`, non-empty `owner`, and non-empty `repo`, and the
    issue is still claimed: record a pending auto-merge entry for reconciliation. Only created if
    no entry already exists.

- `Worker Exit (abnormal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Persist completed run attempt to SQLite.
  - Schedule exponential-backoff retry.

- `Agent Update Event`
  - Update live session fields, token counters, and rate limits.

- `Retry Timer Fired`
  - Re-fetch active candidates and attempt re-dispatch, or release claim if no longer eligible.

- `Reconciliation State Refresh`
  - Stop runs whose issue states are terminal or no longer active.

- `Stall Timeout`
  - Kill worker and schedule retry.

- `CI Status Failing`
  - Persist CI failure run history.
  - Increment CI fix attempt counter.
  - If within `ci_feedback.max_retries` (or `reactions.ci_failure.max_retries`): cancel the
    existing continuation retry, schedule a CI-fix dispatch with failure context injected into
    the prompt.
  - If retries exhausted: escalate (add label or post comment per escalation config),
    cancel retry, release claim.

- `Review Comments Detected`
  - Compute fingerprint from non-outdated review comment IDs.
  - If fingerprint is unchanged and already dispatched: skip.
  - If within debounce window: defer to next tick.
  - If within `reactions.review_comments.max_continuation_turns`: cancel the existing
    continuation retry, schedule a review-fix dispatch with review comment context injected
    into the prompt.
  - If continuation turns exhausted: escalate (add label or post comment per escalation
    config), cancel retry, release claim.

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
- Startup terminal cleanup removes stale workspaces for issues already in terminal states.

#### Startup Recovery Sequence (SQLite)

1. Open or create the SQLite database and run schema migrations.
2. Load persisted retry entries from SQLite.
3. Reconstruct retry timers from persisted `due_at` timestamps.
4. Query tracker for terminal-state issues and clean corresponding workspaces.
5. Construct reaction providers and call `RecoverPendingReactions` to restore eligible CI and
  review pending entries for handoff-stage issues.
6. Query tracker for active issues and reconcile with persisted state.
7. Begin normal polling loop.

