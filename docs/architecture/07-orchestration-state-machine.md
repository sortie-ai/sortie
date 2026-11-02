## 7. Orchestration State Machine

The orchestrator is the only component that mutates scheduling state. All worker outcomes are reported back to it and converted into explicit state transitions.

### 7.1 Issue Orchestration States

This is not the same as tracker states (`To Do`, `In Progress`, etc.). This is the service's internal claim state.

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
   - Claim removed because issue is terminal, non-active, missing, or retry path completed without re-dispatch.

Important nuance:

- A successful worker exit does not mean the issue is done forever.
- The worker may continue through multiple back-to-back coding-agent turns before it exits.
- After each normal turn completion, the worker re-checks the tracker issue state.
- If the issue is still in an active state, the worker should start another turn on the same live coding-agent thread in the same workspace, up to `agent.max_turns`.
- The first turn should use the full rendered task prompt.
- Continuation turns should send only continuation guidance to the existing thread, not resend the original task prompt that is already present in thread history.
- With the comment feed on (`comment_feed.enabled`), every turn, continuation turns included, also carries the issue comments the conversation has not been given: the template places them through `comment_feed`, or the feed appends them after the prompt guidance for that turn. With the feed off, continuation turns read no comments.
- Once the worker exits normally with the issue still active and no handoff state configured, the orchestrator schedules a short continuation retry (about 1 second) so it can re-check whether the issue remains active and needs another worker session. Section 7.3 gives the full set of exit dispositions and the order they are evaluated in.

### 7.2 Run Attempt Lifecycle

A run attempt transitions through these phases:

1. `PreparingWorkspace`
2. `BuildingPrompt`
3. `LaunchingAgentProcess`
4. `InitializingSession`
5. `StreamingTurn`
6. `SelfReviewing`, entered only when `self_review.enabled` is true and the coding turn loop completed successfully (not on turn failure). The loop leaves that state in three ways this phase admits: exhausting the configured turn budget, the agent writing the completion signal to the status file, and the agent declaring that the requested outcome already held and nothing needed changing. Read after a coding turn, a `blocked` signal never admits the run to this state; it remains an immediate exit regardless of configuration. A `blocked` signal the agent writes during a phase turn instead ends the phase and gives the run the blocked disposition of Section 7.3, on any of the three admissions above. A declared run's admission does not by itself confirm the declaration: the phase's own verification commands and review turn are what can falsify it, and any outcome other than exactly one recorded iteration ending on a `pass` verdict with no failing verification result retracts the declaration and returns the run to the disposition it would have taken had it exhausted its turn budget with no status file written.
7. `Finishing`
8. `Succeeded`
9. `Failed`
10. `TimedOut`
11. `Stalled`
12. `CanceledByReconciliation`

Distinct terminal reasons are important because retry logic and logs differ.

**Comment delivery.** With the comment feed on, the worker selects the pending comments at the start of every turn of the attempt (the first turn, each continuation turn, each reaction turn, and each self-review review and fix turn), renders that turn's prompt with them, and appends the feed's block unless the template placed them (Section 12.1). The worker takes the feed's configuration from the snapshot read when the attempt began. Once a turn has started with its prompt, whatever the turn's outcome, the worker reports the comments that prompt delivered to the orchestrator through a per-turn callback, and the orchestrator, which is the only writer of the given-comment record, writes them on its event loop. A report is applied only while the issue still has a running entry for the same dispatch, and a pending cleanup does not prevent it, because the agent received the prompt. Every report queued before a worker result is applied before that result is handled, on the normal path and in the drain at shutdown, so a run's last turn is recorded before its exit disposition runs. A report that is never recorded (the run context ended first, or the write failed) causes those comments to be delivered once more in a later run and loses none. A turn that never starts reports nothing, and the next boundary selects its comments again. A read or store failure at a boundary sends that turn's prompt without comments and never fails the turn.

### 7.3 Transition Triggers

- `Poll Tick`
  - Validate config, which forces a defensive reload, then apply the result to runtime state.
  - Reconcile active runs.
  - Run the periodic workspace sweep when due (Section 8.7).
  - Evaluate the instance spend ceiling (Section 8.4), whether or not validation succeeded.
  - Fetch candidate issues.
  - Dispatch until slots are exhausted, unless a spend window holds dispatch (Section 8.4). Dispatch is the only step gated on validation success.
  - Dispatched workers perform the optional dispatch-time in-progress transition (via `tracker.in_progress_state`) as their first step, before workspace preparation.

- `Worker Exit (normal)`
  - On a recognized status value, the worker leaves the coding turn loop rather than returning from it. The `SelfReviewing` phase runs first when its gate admits the exit (Section 7.2), followed by session teardown, the `after_run` hook, and only then the exit disposition below. The disposition itself is unchanged for both recognized values: what changes is the work performed ahead of it.
  - Unconditional steps, taken on every normal exit: remove the running entry and update aggregate runtime totals. Every exit also persists exactly one completed run attempt to SQLite after any handoff-evidence verdict needed for its status is known and before scheduling a retry. The normal exit-kind mapping supplies the status, unless the policy withholds the transition and the withheld verdict's own verification read (disposition 3 below) does not find the issue terminal: in that case the run instead records `failed`, with the evidence verdict named as the cause (§19.2). A withheld verdict whose verification read does find the issue terminal keeps the exit-kind mapping's status.
  - The exit then takes exactly one disposition. The conditions are evaluated in the order below and the first match wins, so an earlier disposition overrides every later one:

    1. The agent reported itself blocked through a soft stop. Suppress the continuation retry. Where the dispatch drives issue state, park the issue instead of merely releasing the claim: cancel any pending retry, release the claim, record a durable park keyed by issue ID, and apply the configured parking label to the issue through the tracker adapter. The park holds the issue out of dispatch until a later poll tick observes either a change in the issue's tracker state or the removal of a parking label the orchestrator has confirmed reached the tracker (§14.2). Where the dispatch does not drive issue state, cancel any pending retry and release the claim without recording a park. Blocked work has nowhere to continue to either way.
    2. The freshest tracker observation for the issue reports a terminal state, resolved with precedence reconciliation's observation, then the worker's own per-turn observation, then the dispatch-time snapshot, or the withheld path's own verification read below reports one instead. Suppress the handoff transition and the continuation retry, cancel any pending retry, record the terminal outcome (Section 4.1.8), and release the claim. A terminal state is a decision already made about this issue; overwriting it with the handoff state would undo it.
    3. A handoff state is configured, the issue is still in an active state, the exit is not a blocked soft stop, and the dispatch drives issue state. Only where all four conditions hold, apply the run's frozen `tracker.handoff_evidence` policy as a fifth condition:
       - Under `off`, compute no verdict and preserve the previous handoff behavior.
       - `Work observed` permits the handoff. It also resets the issue's consecutive-absence count to zero immediately, even if the later tracker write fails.
       - `Absence of work observed` withholds the handoff.
       - `Evidence not determinable` permits the handoff under `observed` and withholds it under `strict`, where it is treated as an absence for the failure disposition and consecutive count. The verdict is recorded in either policy.
       - A run that declared the requested outcome already held, and whose declaration stood through the self-review phase where that phase ran, is tested ahead of the four branches above and always yields `work observed`, under every policy value including `off`: it runs no workspace inspection and cannot be withheld or classified as undeterminable. Under `off`, where no verdict is otherwise computed, the declaration still selects the transition target. The transition target for a declared run is `tracker.no_change_state` where that field is configured, and `tracker.handoff_state` otherwise; an undeclared run always targets `tracker.handoff_state`.

A verdict that permits the write first tries the hop when the run's rule carries `next` in the configuration in force at the exit, and performs the handoff transition (Section 11.5) when the rule carries none or the hop is not made. A hop replaces the handoff transition with a stage label write:

- The hop runs after the same verification read that precedes the handoff transition, so a terminal result suppresses it and the exit takes disposition 2.
- If the run's rule no longer exists or no longer carries `next`, if the rule `next` names no longer exists, or if that rule no longer carries a stage label, no hop is due: the exit reaches no hop decision, logs no warning, records no result, publishes no stage event, and performs the handoff transition.
- If the issue's consecutive hop count has reached `dispatch.max_consecutive_hops`, the hop is not made. The decision's result is `ceiling`, one warning is logged, and the exit publishes `stage.not_advanced` and records the result on the run's row before it performs the handoff transition. The count, which is durable and per issue, is checked before any tracker call.
- The orchestrator adds the next rule's stage label through the tracker adapter. If the add fails, the hop is not made. The decision's result is `failed`, and the exit publishes `stage.not_advanced` and records the result on the run's row before it performs the handoff transition. A failed hop is not retried by re-running the stage, because that spends a full agent run to repeat one label write.
- After a successful add, the orchestrator records the hop durably (the new count, the source and target rules, the target label, the outcome, and the source dispatch), which also deletes the issue's persisted retry entry in the same transaction. It then releases the issue's runtime state: the queued retry, whichever reaction kind owns it, and its persisted row, every pending reaction entry, the reaction attempt counters, the comment caches, and the claim. The retry slot's incumbent is not kept, because the hop starts a new claim boundary.
- It then removes every other configured stage label that the dispatch read showed on the issue, deduplicated under the stage label comparison. A removal that fails leaves the label in place and logs a warning; the hop stands.
- A made hop records its result on the run's row, `advanced` or, when a removal failed, `partial`, together with the target rule, and publishes `stage.advanced` once the removals are done. It writes no tracker state, publishes the events a normal exit publishes with no retry pending, does not increment the handoff transition counter, and seeds no reaction entries. The issue stays in its active state. Its next dispatch is a fresh claim with a fresh session on the next rule, performed by the poll tick, which holds the issue out of every other rule until a read shows the label the hop added (Section 8.1).

The consecutive hop count resets when the handoff transition succeeds, when the exit takes disposition 2 or the issue is no longer active at exit, when a park is recorded, and when a park is released. Reconciliation, the retry handler, and the dispatch hold reset it when they observe the issue in a terminal state or in any state outside the active states (Section 8.5). A stage label change while the issue stays active, a failed handoff write, a withheld verdict, and a reload do not reset it.

A successful handoff transition records the handoff outcome with the state it wrote, or the terminal outcome when that target is a configured terminal state (Section 4.1.8). On a successful handoff transition, release the claim, unless the retry slot (Section 7.5) is occupied by an incumbent, in which case the incumbent is kept and the claim stays so the poll loop cannot clear it. On transition failure, a soft-stop exit releases the claim; a non-soft-stop exit schedules the continuation retry instead, unless the retry slot is already occupied, in which case the exit defers to the incumbent.

A verdict that withholds the write is, immediately before any of its effects, checked once more against the tracker: a `fetch_issue_states_by_ids` read for the issue, gated the same way the permit path's own pre-write read is gated. When that read reports a terminal state, the exit takes disposition 2 above instead. Only when it does not—because no terminal states are configured, the read reports an active state, the issue is absent from the response, or the read fails—does the withheld verdict make no tracker transition and leave the issue in its active state. Record the unsuccessful attempt, increment the consecutive-absence count, and use the ordinary failure path with exponential backoff and retry-slot arbitration—not the short continuation path. When the count reaches its ceiling, park the issue and stop the sequence as specified in §14.2.
    4. Any other soft stop. Suppress the continuation retry, cancel any pending retry, and release the claim. An unrecognized soft-stop reason is logged before taking this path.
    5. The issue is still in an active state and the dispatch drives issue state. Schedule the continuation retry (attempt `1`) so the next tick can re-check whether the issue needs another worker session, unless the retry slot is already occupied, in which case the exit defers to the incumbent instead.
    6. Otherwise the issue is no longer in an active state. Cancel any pending retry and release the claim, unless the retry slot is occupied by a foreign incumbent, in which case the incumbent is kept and the claim stays to protect it — this is exactly the population mid-session-queued retries need protected, since the issue may have left the active states while a sibling reaction was still queued for it.
  - Reaction entries are enqueued only when the issue was claimed at the moment of exit, the exit either took the handoff disposition or left the issue still claimed, the exit was not suppressed by a terminal observation, and the exit did not make a hop. A made hop releases the issue's reaction state and seeds none. Because dispositions 3 and 6 can now retain the claim solely to protect a foreign retry-slot incumbent, the predicate is evaluated as if that claim had been released whenever it is retained for that reason alone, so protecting an incumbent never widens which reaction kinds a later exit seeds. A label-command dispatch still never satisfies this predicate: it takes disposition 6, and under this rule its retained claim counts as released for the predicate's purposes exactly as an ordinary released claim would.
  - Subject to that predicate, each configured reaction kind records its own pending entry when the workspace SCM metadata (§9.5) satisfies that kind's field requirements. The kinds are independent: several fire from one exit. Every kind except the CI kind creates its entry only when no entry of that kind already exists, which preserves in-progress debounce state; the CI entry is rewritten on each exit so it always carries the ref the latest run pushed.

- `Worker Exit (abnormal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Persist completed run attempt to SQLite.
  - Schedule an exponential-backoff retry when the retry slot (Section 7.5) is free; defer to the incumbent already occupying it otherwise, keeping the claim.

- `Agent Update Event`
  - Update live session fields, token counters, and the agent kind's usage windows.
  - Every agent update and self-review progress message a worker delivered before its exit is applied before that exit is handled, so no message from one run is applied after its exit or to a later run of the same issue. A usage figure applied this way for a run whose exit is being handled or already waiting is evaluated against neither an in-flight ceiling (token or money) nor the token warning threshold, because that run has already ended; the run's recorded total carries the figure to the next dispatch decision.

- `Retry Timer Fired`
  - While an instance spend window holds dispatch, re-arm the retry one `polling.interval_ms` ahead and start nothing, leaving its attempt count, claim, and persisted entry unchanged (Section 8.4).
  - Otherwise re-fetch active candidates and attempt re-dispatch, or release claim if no longer eligible.

- `Reconciliation State Refresh`
  - Stop runs whose issue states are terminal or no longer active.

- `Stall Timeout`
  - Kill the worker unconditionally, then either schedule an exponential-backoff retry when the retry slot (Section 7.5) is free, or defer to the incumbent already occupying it.

- `CI Status Failing`
  - If an escalation stands on the entry (Section 11A.7), re-enqueue it with backoff and take none of the actions below: no retry-slot consult, run-history row, counter increment, dispatch, or escalation action. The escalation ends on a passing verdict, a new head, a `reactions.ci_failure.max_retries` reloaded above the fixes dispatched, a merge, a close, a missing pull request, or a person's answer (the issue moved out of `tracker.handoff_state`, or its park released so Sortie runs it again). Only a budget escalation ended by a reload goes on to the steps below on that pass.
  - Consult the retry slot (Section 7.5) first. If an incumbent occupies it, defer: re-enqueue the pending entry with backoff, leaving its creation time unchanged, and take none of the actions below on this tick: no run-history row, no counter increment, no dispatch, and no escalation.
  - On a free slot: persist CI failure run history (Section 11A.6).
  - If the CI fix counter is below `reactions.ci_failure.max_retries`: increment it and schedule a CI-fix dispatch with failure context injected into the prompt.
  - If the counter has reached `reactions.ci_failure.max_retries`: escalate with that count of fixes dispatched (apply the label or emit the escalation event per the `escalation` posture, Section 5.3.8), cancel retry, release the claim unless a worker runs for the issue (that worker's exit decides it). The entry stays pending with the escalation recorded on it, and the counter stays at the budget while the escalation stands.

- `Review Comments Detected`
  - Compute fingerprint from non-outdated review comment IDs.
  - If fingerprint is unchanged and already dispatched: skip.
  - If no surviving comment is new, that is, every one was given to an earlier run of the issue (the persisted handed-off set) or is carried by the issue's running turn: take no action, keep watching, and mark the fingerprint dispatched when the handed-off set alone covers the set. A set that only lost members, or one that recurs after a larger set, settles here. For `reactions.bot_review`, the reported set counts with the handed-off set.
  - If within debounce window: defer to next tick.
  - If the retry slot is occupied by an incumbent, defer instead (Section 7.5).
  - On a free slot, decide the continuation budget after the slot arbitration. Below `reactions.review_comments.max_continuation_turns`, schedule a review-fix dispatch with review comment context injected into the prompt.
  - If continuation turns are exhausted: escalate (apply the label or emit the escalation event per the `escalation` posture, Section 5.3.8), cancel retry, release the claim unless a worker runs for the issue (that worker's exit decides it), recording nothing in the handed-off set. The entry stays pending with the escalation recorded on it, and so does its counter, until the escalation ends (Section 11B.6); a standing escalation dispatches nothing and is never aged out. The set holds a new comment, because the check above settles every other set. For `reactions.bot_review`, only a new inline comment escalates, and its escalation leaves the claim and the retry in place (Section 11D.5).

- `Merge Completion Observed`
  - Observed on the reconcile tick for an issue still parked in `tracker.handoff_state` and not currently claimed by the orchestrator.
  - While the pull request remains unmerged, wait without aging the missing-identifier condition. The first `Merged == true` response without a commit identifier starts a persisted thirty-minute grace period; retry that condition with pending backoff, then, if it remains unresolved, begin a `missing_merge_commit` escalation: attempt the configured escalation and keep the entry pending, watching for the commit identifier, with no transition. Successful delivery is recorded once; a failed delivery is retried once by the first pass after a restart or by a later fresh pending entry, and never by restarting the grace period.
  - Latch idempotency on the merge commit identifier; a commit already latched and dispatched is dropped without a transition.
  - Transition the issue to `reactions.merge_completion.target_state`, the last orchestrator-driven tracker-state write in an issue's life, after the optional dispatch-time in-progress transition and the handoff transition on worker exit (§11G).
  - On failure, route by tracker error kind (§11G.5): a transport or API failure, or any other kind, retries with backoff bounded by `reactions.merge_completion.max_retries`, then escalates; an auth or payload failure escalates immediately, consuming no retry budget, and an escalation keeps the entry pending and makes no further transition call until it ends (a restart resumes one call, Section 11G.6); a not-found issue stops, marks the fingerprint dispatched, and logs a warning, with no escalation.
  - No orchestration-state transition occurs: the claim was already released when the issue entered the handoff state.

### 7.4 Idempotency and Recovery Rules

- The orchestrator serializes state mutations through one authority to avoid duplicate dispatch.
- `claimed` and `running` checks are required before launching any worker.
- Reconciliation runs before dispatch on every tick.
- Restart recovery uses persisted state from SQLite for retry queues, session metadata, parked issues (including a park a person released that the issue's next primary dispatch has not yet consumed), budget-hold notice records (the durable record that a `budget.held` notice reached at least one destination; a hold with no destination records nothing and is routed again on a later pass), and attention acknowledgements (the durable record that a person has seen an attention item), supplemented by tracker polling for current issue states and filesystem inspection for workspace existence.
- Startup pending reaction recovery uses `run_history`, tracker state, and `.sortie/scm.json` to reconstruct runtime `pending_reactions` for recent handoff-stage runs before the first poll tick. The scan is bounded by `PendingReactionRecoveryLookback` and a fixed candidate cap.
- Startup terminal cleanup maps existing workspace directories to issue identifiers, queries the tracker for the states of those specific issues, and removes the ones in terminal states. An issue that reaches a terminal state through the merge-completion transition (§11G) becomes eligible for this same cleanup without a human relabeling it first.

#### Startup Recovery Sequence (SQLite)

1. Open or create the SQLite database and run schema migrations.
2. Load persisted retry entries from SQLite.
3. Reconstruct retry timers from persisted `due_at` timestamps.
4. Load persisted park records from SQLite and reconstruct the runtime park set before the event loop starts, so a parked issue stays out of dispatch across the restart. A record a person already released loads as a pending release instead, so the issue stays dispatchable and its next primary dispatch carries the hand-over (Section 14.2).
   Then load persisted stage hop records and reconstruct each issue's hop count, latest hop, and hold flag, so the count and the hold survive the restart. A record with an empty issue ID is skipped with a warning, and a failed load starts with none.
5. Load persisted budget-hold notice records from SQLite and reconstruct the runtime notice memory before the event loop starts, so a hold already announced to its destinations before the restart is not announced again after it. A record exists only for a notice that had at least one destination.
6. Load persisted attention acknowledgements from SQLite before the event loop starts, so an attention item whose `id` survives the restart keeps its acknowledgement (§13.7.2). A record that does not parse is skipped with a warning, and a failed load starts with none.
7. Load persisted usage-limit holds from SQLite before the first tick, so a hold a provider usage limit began before the restart still stops new sessions of its agent kind, or of that kind on one model, until its reset. A hold whose reset has passed loads and has no effect, and a failed load starts with none. The restored retry entries keep the reset as their due time and their `usage_limit` origin trigger, so a refused issue's retry is still the first dispatch the hold admits.
8. Map existing workspace directories to issue identifiers, query the tracker for the states of those specific issues, and clean the ones in terminal states.
9. Construct reaction providers and call `RecoverPendingReactions` to restore eligible CI and review pending entries for handoff-stage issues.
10. Query tracker for active issues and reconcile with persisted state.
11. Begin normal polling loop.

### 7.5 Retry-Slot Arbitration

Retry staleness is measured from a monotonic timestamp captured before arming the timer. A pause while scheduling must not make the timer's own due event look premature and leave the retry waiting for overdue recovery.

`retry_attempts` holds at most one entry per issue: the retry slot. The entry's reaction kind records the owner, with the empty string meaning the orchestrator's own continuation lane. Every code path that wants to schedule a retry for an issue (a challenger) checks whether the slot is occupied before writing. A non-nil occupant (the incumbent) means another unit of work already owns the issue's next dispatch; the challenger defers instead of overwriting it, leaving the incumbent untouched and making no other state mutation. The rule keys on occupancy alone: it does not compare owners, rank reaction kinds, or preempt.

Only two writers are exempt from checking first, because each frees the slot itself immediately before writing back into it: the retry-timer handler's inner reschedule and its re-arm under a spend hold (Section 8.4), which pop and delete the entry they are replacing before rescheduling it, and the overdue-retry re-arm pass (below), which cancels the same entry before re-arming it with a zero delay. Every other writer is admitted only into a free slot.

Two liveness bounds keep an arbitrated slot from being held for the process lifetime:

- A reaction retry that the issue's own current state does not permit to dispatch is rescheduled with backoff, but only for as long as it has been pausing consecutively for that reason. Once that dwell reaches 30 minutes, the entry is dropped instead of rescheduled, its persisted row deleted, the claim released, and a warning logged naming the kind and how long it had paused. The dwell resets whenever the entry is held for any other reason, so an otherwise healthy retry is never penalized for an unrelated pause.
- A retry entry whose timer event was dropped (Section 8.4) never fires again on its own. A reconcile pass detects any entry whose due time is more than 60 seconds in the past and still carries an active timer handle, and re-arms it with a zero delay, changing nothing about the entry but its timer. A retry reconstructed at startup and still awaiting its first activation is excluded from this pass, so a restart alone never produces the re-arm warning.
