## 14. Failure Model and Recovery Strategy

### 14.1 Failure Classes

1. `Workflow/Config Failures`
   - Missing `WORKFLOW.md`
   - Invalid YAML front matter
   - Unsupported tracker kind or missing tracker credentials/project
   - Missing coding-agent executable

2. `Workspace Failures`
   - Workspace directory creation failure
   - Workspace population/synchronization failure (implementation-defined; may come from hooks)
   - Invalid workspace path configuration
   - Hook timeout/failure

3. `Agent Session Failures`
   - Startup handshake failure
   - Turn failed/cancelled
   - Turn timeout
   - User input requested (hard fail)
   - Subprocess exit
   - Stalled session (no activity)
   - Credential not verified before the first turn
   - Provider usage limit reached: the provider refuses a request because the account's usage allowance is exhausted, and the allowance reopens at a reset time the provider can report

4. `Tracker Failures`
   - API transport errors
   - Non-200 status
   - API-level errors
   - Malformed payloads

5. `Observability Failures`
   - Dashboard request errors
   - Log sink configuration failure
   - Delivery of an orchestrator event to a backend destination

6. `CI Feedback Failures`
   - CI status fetch errors (transport, auth, API, not-found, payload)
   - Escalation failures (label write or `tracker_comment` delivery to the tracker, and delivery of the escalation event to a backend destination)
   - Missing or malformed `.sortie/scm.json`

7. `Handoff Evidence Failures`
   - Absence of work observed where the four pre-existing handoff conditions otherwise permit the transition
   - Evidence not determinable under the `strict` policy, which deliberately treats that verdict as an absence
   - A run whose declaration that the requested outcome already held stood through the self-review phase where that phase ran produces neither: it is tested ahead of the workspace inspection and always yields a work-observed verdict, so this failure class never applies to it.

### 14.2 Recovery Behavior

- Dispatch validation failures:
  - Skip new dispatches.
  - Keep service alive.
  - Continue reconciliation where possible.

- Worker failures:
  - Convert to retries with exponential backoff.

- Provider usage limit reached:
  - Recover by waiting for the reset. A refusal that reports a future reset time schedules the issue's retry at the later of the reset and the ordinary backoff, and records a hold on new sessions of the agent kind, or of that kind on one model when the exhausted allowance covers one model. Running sessions finish, and issues routed to other agent kinds or models keep dispatching.
  - The refused issue's retry fires at the reset and probes the provider first. Every other dispatch of the held kind waits one `polling.interval_ms` more, so a probe refused again does not refuse them all.
  - A refusal that reports no usable reset time takes the ordinary exponential backoff and records no hold.
  - The hold lifts when its reset passes, or earlier when a run of the kind that started after the hold began exits normally.
  - A run refused before it produced output does not count toward `agent.max_sessions`; every other refusal counts, so an issue the agent cannot finish still reaches the ceiling.

- Failed read of an instance spend window (Section 8.4):
  - Log one warning naming the window and the error on each evaluation that fails.
  - Keep the window's last result when it is for the same window start; a window with no earlier result for its start counts as not holding.
  - Never block the tick or a retry timer fire on the failure.

- Instance spend ceiling reached:
  - Start no new agent session for any issue; running sessions finish.
  - A retry or reaction continuation that comes due is re-armed one `polling.interval_ms` ahead with its attempt count, claim, and persisted entry unchanged, so the hold costs the work no retry budget and no backoff.
  - Lift the hold with no operator action when the window resets or a reload raises or removes the ceiling.

- Failed delivery of an orchestrator event to a backend destination:
  - Log a warning naming the event type, the destination, and the notifier kind.
  - Never retry, and never let it block another destination or the action a producer uses to confirm its own delivery.

- Recognized `.sortie/status` signal:
  - All three values keep their existing dispositions. A `blocked` soft stop suppresses the handoff transition and the continuation retry, and, where the dispatch drives issue state, parks the issue instead of merely releasing the claim: it records a durable park and applies the parking label, sharing both the mechanism and the release rule the consecutive-absence ceiling uses below. A completion signal (`needs-human-review`) suppresses the continuation retry and takes the ordered handoff disposition (§7.3) unchanged: the transition fires only where a handoff state is configured, the issue is still active, the dispatch drives issue state, no terminal observation suppresses it, and the evidence verdict permits the write. A verdict that withholds the write routes this exit to the withheld-handoff recovery below, exactly as it routes any other exit that reaches it. A declaration (`no-change-needed`) that stands suppresses the continuation retry and takes the same ordered handoff disposition, except that its evidence verdict is always work observed: it never reaches the withheld-handoff recovery below, and its transition target is `tracker.no_change_state` where that field is configured, `tracker.handoff_state` otherwise.
  - Where self-review is enabled and its gate admits the exit, the completion signal and the declaration both run the self-review phase before that disposition is computed. The phase runs while the session is live and before session teardown; the disposition itself is unchanged for a completion signal. For a declaration, the phase's outcome decides whether the disposition above applies at all: the phase's verification commands and review turn are what can falsify the declaration, and any outcome other than exactly one recorded iteration ending on a `pass` verdict with no failing verification result retracts it, returning the run to the disposition it would have taken had it exhausted its turn budget with no status file written. A `blocked` value read during a phase turn instead gives the run the blocked disposition above, on any admission path into the phase.
  - A declaration that stands resets the consecutive-absence count and releases a park held for any reason, on the same terms as any other work-observed verdict (below): the release rule reads no reason from the park record. Under `tracker.handoff_evidence: off`, no verdict is computed for a declaration, so neither the reset nor the release occurs; the declaration's only effect there is the transition target.

- Withheld handoff evidence:
  - Immediately before recording an absence failure, re-read the issue's tracker state once, gated the same way the permit path's own pre-write read is gated (§11.5). A terminal result routes the exit to the terminal disposition (§7.3) instead: no failure record, no failure comment, no retry, no absence count. Only when that read finds no terminal state does the withheld disposition below apply: leave the issue in its active tracker state, record the run as `failed` with the evidence verdict in its error reason, and schedule the ordinary exponential-backoff failure path. Do not use the fixed-delay continuation path.
  - Maintain a count of consecutive outcomes treated as handoff absences for each issue. `Absence of work observed` increments it, as does `evidence not determinable` under `strict`. A `work observed` verdict resets it to zero immediately; a later handoff-write error cannot restore the old count. Outcomes that provide no work-observed verdict do not pretend to reset it.
  - The consecutive-absence ceiling is `agent.max_consecutive_absences`, a dedicated integer field that defaults to `3` and rejects `0` and negative values as a configuration error. The comparison is against the incremented **consecutive-absence count**, and parking occurs when `count >= ceiling`. Thus the default sequence is absence `1`, retry; absence `2`, retry; absence `3`, park. It is the initial run plus two retries, not three retries. The separate `agent.max_sessions` effort budget continues to enforce its own all-run ceiling independently, so whichever applicable gate is reached first stops re-dispatch.
  - The derived default of `3` follows the `max_retries: 2` default used by most reaction kinds: one initial attempt plus two retries. This avoids treating one legitimate no-change conclusion as immediately terminal, gives two further sessions a chance to surface work, and bounds the cost of full agent sessions. Turn, tool, or model-iteration limits inside one session do not set this ceiling and are not comparable to it.
  - On reaching the ceiling, cancel this retry sequence, apply the primary-dispatch parking label, and release the claim. The sequence cancelled is the primary-dispatch one the count is kept for: a reaction continuation retry on the same issue carries its own sequence, cannot record an absence, and is not cancelled here. Ceiling exhaustion is an eligibility gate rather than only a cancelled timer: ordinary polling and restart recovery must not silently begin another run in the same exhausted absence sequence. The representation is fixed and shared with the `blocked` park: the same durable park record, the same runtime gate, and the same release rule, tagged with the reason this ceiling produced it rather than the reason a `blocked` signal did. The rule requires no durable verdict column on `run_history`.
  - Under `tracker.handoff_evidence: off`, no absence is ever recorded, so the count is never incremented and no *new* park is taken by this ceiling on any path: polling, retry firing, and worker exit all skip the trigger, and the policy costs nothing while it is selected. This governs only whether a *new* park is taken, not whether an existing one holds: a park already recorded, by either trigger, is not lifted by selecting `off` and is released only by the rules below. Raising `agent.max_consecutive_absences` above the recorded count has the same narrowed effect: it changes the ceiling a future count is compared against, but it does not lift a park already taken, because the park is a row rather than a value re-derived from the current ceiling on every tick.
  - An operator releases a park with a gesture, whichever of the following the orchestrator observes first, and each applies to every trigger that parks an issue, not only to this ceiling: moving the issue to a tracker state different from the one recorded when it was parked, or removing a parking label the orchestrator has confirmed reached the tracker. The label gesture carries a confirmation guard: confirmation comes only from observing the label present on a later fetch of the issue, never from the label write itself returning without error, because the candidate listing can lag the parking-label write (§11.1). Releasing on an unconfirmed label's absence would let a listing that has not yet caught up undo the park silently. A park taken from the retry lane records no observed state at the moment it is taken, since the retry lane parks ahead of its own tracker fetch by design; the first later observation backfills the state without releasing the park on that same tick, so the operator's own action is never mistaken for the very read that first captured the baseline. A parked issue absent from the poll tick's candidate set, because it now sits in a state the deployment does not call active or because `tracker.query_filter` excludes it, is still read for its state and labels through a separate, filter-free tracker call, so both gestures reach it exactly as they would if the issue were still a candidate. Release resets the absence counter whatever reason produced the park, so a released absence-ceiling park does not immediately re-derive the same exhausted count and park itself again; release is evaluated ahead of the ceiling trigger on the same tick for exactly this reason. A park released by a move to a terminal state records the terminal outcome with that state (Section 4.1.8). A gesture release keeps the park record, with its release time and gesture, as a pending release until the issue's next primary dispatch from the poll tick consumes it or a new park replaces it. That dispatch reads the issue's comments, then the record of the comments Sortie posted, resumes the agent session of the run the park follows where that run recorded one the retry policy would keep and the agent kind can resume, and renders the `park` template variable on turn 1 with the agent's stop statement, the release, and the comments as `park.comments`: with the comment feed off, the comments created since the parked run's last prompt less every comment Sortie posted; with the feed on, the run's pending comments, those earlier sessions on the issue were given first, then the new ones (Section 12.1). A failed comment read or a failed read of the record of posted comments holds the issue with the hold reason `park_comments_unread` and the next tick reads again; the issue never starts on a partial view, so Sortie's own comments never reach the run for want of the record. A release observed in a terminal state stays pending until the reopened issue's next primary dispatch. Retry fires, reaction dispatches, and startup re-dispatch neither render `park` nor consume a pending release.
  - Another release takes no gesture: a worker exit whose evidence verdict is `work observed` lifts the park of the issue it ran on, on the same verdict that resets the absence count, and it too applies to every trigger that parks an issue. This is the one release the evidence policy governs, since `off` computes no verdict, so a deployment that selects it keeps the gestures above as the only way out of a park. This release is not a person's answer: it deletes the park record and hands nothing over to the next run.
  - The parking label is the resolved non-empty `reactions.review_comments.escalation_label` captured when the orchestrator starts, or `needs-human` when that block or value is absent or empty. This lookup deliberately reuses only the label name: review-comments need not be enabled, `escalation: comment` does not turn primary parking into a comment, and labels configured under `ci_failure`, `merge_conflicts`, `auto_merge`, or any other reaction do not participate. This explicit one-way dependency keeps the existing operator-visible escalation channel without creating a second configuration field. `review_comments` is the source because this parking happens immediately before human review; CI, merge-conflict, and auto-merge labels describe different recovery domains.
  - Announce parking with the issue, the consecutive count, the ceiling, and the label. A label-write failure follows the existing escalation-failure rule: log and count the error and keep the absence sequence stopped rather than resuming an unbounded loop.

- Stage hop failures (§7.3):
  - A stage hop that is not made falls back to the handoff write the exit would have taken without `next`: a count that would pass `dispatch.max_consecutive_hops` stops the chain at the ceiling, and a failed label add leaves the source stage in place. Either way the fallback follows the handoff rules above, including its verification read, and a successful write ends the issue's hop count. A failed add also reads the issue once when the dispatch read showed a stage label, and the warning names each configured stage label the failed add lost, since an adapter that keeps stage labels in a single-select group can remove the source label before a refused add. The orchestrator does not restore a lost label.
  - A stage hop whose add succeeded is made even when a later step fails. A failed removal of a source stage label logs a warning naming the labels left, and the hop stands: the next dispatch routes by the hop record, which selects its target ahead of any leftover label of another stage. A failed write of the hop record keeps the runtime record, so the hold and the count work until a restart; the failure is logged. A failed write of the observation flag, of the dispatch flag that ends the issue's listing between stages, or of the record's deletion is logged and the runtime value stands. A failed write of the hop result onto the run's row is logged and the hop stands: the row keeps empty decision fields, and the hop's record and its stage event are unaffected.
  - The hold reads the issue once when the candidate listing lacks the target label. A failed read holds the issue for that tick and tries again on the next. A read that shows no state also holds the issue for that tick and leaves the hop record untouched; reconciliation ends the count at the second consecutive no-state reading, because a healthy label edit can show an issue with no state label for a moment. A read that shows a terminal or named non-active state ends the hop count and dispatches nothing. A read that shows an active state without the target label releases the hold and routes the issue by the labels it returned.

- Tracker candidate-fetch failures:
  - Skip this tick.
  - Try again on next tick.

- Reconciliation state-refresh failures:
  - Keep current workers.
  - Retry on next tick.

- Dashboard/log failures:
  - Do not crash the orchestrator.

- CI feedback failures:
  - CI status fetch failure: re-enqueue pending check for next tick.
  - Escalation failure: log and count error, but release claim anyway.
  - Missing/malformed SCM metadata: skip CI check silently (degrade to no-CI behavior).

- Merge-completion reaction transition failures, routed by tracker error kind (§11G.5):
  - Transport or API failure: retry with backoff, bounded by `max_retries`, then escalate.
  - Authentication or payload failure: escalate immediately, skipping the over-limit comparison against `max_retries`; the attempt counter still increments on the failed call.
  - Issue not found: stop, mark the fingerprint dispatched, log a warning, no escalation.

Not every orchestrator-driven tracker write in this section gets a later re-attempt. A stalled dispatch is retried and a CI check re-enqueues, but a dispatch-time in-progress transition failure is only logged and never retried, and a handoff transition failure on a soft-stop worker exit releases the claim without scheduling a continuation; both fail quietly with no re-attempt to follow. An escalation failure changes nothing about the pending entry, which stays pending under the escalation and makes no further transition call (§11G.6). What distinguishes merge-completion is not the absence of any fallback, but where that fallback lives. A failed transition escalation deliberately leaves the normal merge fingerprint undispatched (§11G.4), so the one transition call a restart resumes reconciles the same merge commit and retries the transition the escalated attempt could not complete.

Missing-identifier escalation follows the same cross-run posture without changing that normal fingerprint: its separate observation is marked dispatched only after the action that confirms delivery succeeds: the label under `escalation: label`, and the `tracker_comment` write under `comment` or `none` when that destination receives the event (§11G.6). The outcome of a backend destination never confirms delivery and never blocks it. A failed write leaves the expired observation undispatched and the current pending entry in place under its `missing_merge_commit` escalation. The first pass after a restart, or a fresh entry that a later worker exit or startup recovery seeds, retries only that undelivered operator signal once; it does not restart the thirty-minute grace period. This is why every disposition above ends in a bounded retry followed by escalation, an immediate escalation, or an explicit logged stop, and never a silent drop: the durable undispatched state gives the failed action another chance across a restart or a fresh entry.

### 14.3 Partial State Recovery (Restart)

Sortie uses SQLite persistence to improve restart recovery semantics:

- Retry entries with future `due_at` timestamps are restored from SQLite and rescheduled on startup.
- Parked issues are reloaded from SQLite before the event loop starts, so an issue parked before a restart stays out of dispatch across it without depending on any tracker read to rediscover the park. A record a person already released reloads as a pending release rather than as a park, so the issue's next primary dispatch still carries the hand-over.
- Stage hop records are reloaded from SQLite after the parks and before the event loop starts, so the per-issue hop count and the hold survive a restart: an issue that hopped before the restart is not dispatched on its target stage until a read shows the target label, and a label cycle still stops at the ceiling. A record that cannot be loaded starts the orchestrator with none, and the count restarts at zero. The claim, the pending reactions, and the queued retry of the stage the hop left are not recovered, and reaction recovery rebuilds entries only for issues in the handoff state, which a mid-chain issue is not. A hop whose record is durable has also deleted the issue's retry row in the same transaction, so a restart cannot restore the previous stage's retry. A crash between the hop's steps resolves as follows:
  - Before the label add reaches the tracker: no record exists and the issue keeps its source label, so the next tick dispatches the source rule again and the stage re-runs, as after a crash before a handoff write.
  - After the add and before the record is committed: the issue carries both labels and no record. A listing that shows the target label routes the issue to the target, whose `stage.previous` renders empty and whose hop goes uncounted, and which starts a new chain because no record carries the source's chain. A listing that lags the add re-runs the source stage, whose exit hops again with a no-op add.
  - After the record is committed and before the removals finish: the restart restores the count and the hold, the target dispatches once a read shows its label with `stage.previous` filled and the chain the record restored, and a leftover source label stays until the next hop removes it or a person does. The source run's row can lose its decision, because the exit that writes it did not finish, and its stage event is never sent.
  - After the removals and before the hop is logged: the state is complete and only the log record is lost; the source run's row can still lose its decision and its stage event is not sent.
- Budget-hold notice records are reloaded from SQLite before the event loop starts, so a hold announced to its destinations before a restart is not announced again after it. A record means the `budget.held` notice had at least one destination; a hold with none records nothing, so a destination subscribed after a restart still receives its notice.
- Escalation records are reloaded from SQLite after pending reaction recovery and before the event loop starts, so an escalation that stood before a restart still stands after it, with its time, trigger, subject, and the tries it used, and the restart spends no second fix budget and applies no second action. An entry that recovery rebuilt takes its record. A record whose issue recovery did not reach, because the issue lies outside the lookback or the candidate cap or sits outside the handoff state, is rebuilt as an entry from the issue's newest successful run and the record's pull request, with no tracker read and no workspace. A record whose kind is not configured, or whose issue has no successful run, is ended. The first tick's tracker refresh releases the records of a terminal issue and ends those of an issue it reads outside the handoff state while Sortie does not drive it, before any reaction pass runs. A restored `merge-completion` escalation resumes its transition once (§11G.6). The table starts empty on upgrade, so an escalation standing at the upgrade's restart is not restored, and the next escalation of its kind records one.
- Attention acknowledgements are reloaded from SQLite before the event loop starts, so an attention item whose `id` survives the restart keeps its acknowledgement: a park and an escalation, which are restored with their `id` before the first tick, and a budget hold once the first tick that reads the candidate issues rebuilds it. That tick deletes the acknowledgement of every item it no longer derives, and a tick that cannot read the candidate issues deletes none, so a tracker outage right after a restart does not return long-standing items to the attention count. A process-wide item (a failed workflow load or preflight) cannot span a restart, so its acknowledgement goes at that first tick. A record that does not parse is skipped with a warning, and a failed load starts with none; the rows load at the next start.
- The review comments each issue's runs were given are reloaded from SQLite on first use, so a restart does not dispatch a turn that only repeats comments an earlier run already received. A set that cannot be loaded defers the review pass for one poll interval and dispatches nothing. The record starts empty on upgrade, so a comment given before the upgrade counts as new once.
- Usage-limit holds are reloaded from SQLite before the first tick, and the retry entry of a refused issue keeps its reset as its due time. A restart therefore neither re-dispatches the held kind into a refusal nor shortens the wait. A hold whose reset has passed loads and has no effect. A record that cannot be loaded starts the orchestrator with no hold.
- Session metadata from the previous run is available for observability and debugging.
- Run history is preserved in SQLite for operational review. An instance spend hold persists nothing of its own: the first tick or retry timer fire after a restart reads each configured window from run history, so the recorded part of a window and its hold survive a restart unchanged, and the hold's attention item keeps its `id` and acknowledgement (Section 13.7.2).
- Running sessions are not recoverable (agent subprocesses do not survive restart), but the orchestrator knows which issues were in-flight at shutdown and re-dispatches them immediately rather than waiting for the next polling cycle to discover them, unless an instance spend ceiling or a usage-limit hold holds dispatch.
- Pending reaction recovery reconstructs runtime reaction entries after a restart from the most recent `run_history` row recorded as `succeeded` for each issue and by reading that candidate's workspace SCM metadata. A handoff withheld for absence still records `failed` and is not selectable by this query. A handoff withheld by evidence but suppressed by a verified terminal state (§11.5, §14.2) records `succeeded` and is selectable, so it becomes its issue's candidate for as long as it is that issue's newest such row; no query-shape change is required either way. `docs/decisions/0026-re-read-issue-state-before-recording-absence-failure.md`, "Re-Read the Issue State Before Recording an Absence Failure", ratifies this reach into restart reaction recovery and bounds what a suppressed row's candidacy can actually change. Recovery considers only a candidate whose selected successful activity falls inside its lookback window. The pass narrows in a fixed order: it first drops any candidate the orchestrator already holds as running, retry-queued, or claimed, then truncates what is left to a fixed cap on candidate issues per startup pass, and only then reads tracker state and keeps the candidates still sitting in the configured handoff state. Because the cap is applied before the tracker read, an issue beyond it is skipped without its state ever being fetched, and its entry is not rebuilt until a later worker exit or a subsequent startup reaches it. A standing escalation is restored separately, from its own record.
- Recovery is decided per reaction kind, not once for the issue: each kind rebuilds its own entry only when that kind is configured for this process and the candidate's SCM metadata satisfies that kind's own field requirements. A kind that requires a head branch is not recovered from metadata that records only a pull request, and a kind that is switched off recovers nothing even when a sibling kind on the same issue does.
- `workspace.retention_days` and this recovery lookback are coupled: the retention floor (`WorkspaceRetentionMinDays`) in days equals the recovery lookback in days, so any workspace the periodic sweep's age bound is permitted to remove is one recovery would already have skipped as stale. Changing either window without the other reintroduces the defect the coupling prevents. A shorter retention floor would let the age bound remove a workspace recovery still regards as live, silently breaking reaction recovery for that issue after a restart.

### 14.4 Operator Intervention Points

Operators can control behavior by:

- Editing `WORKFLOW.md` (prompt and most runtime settings).
- `WORKFLOW.md` changes should be detected and re-applied automatically without restart.
- Changing issue states in the tracker:
  - terminal state -> running session is stopped and workspace cleaned when reconciled
  - non-active state -> running session is stopped without cleanup
  - no state (on a label-derived tracker, removing the issue's state label) -> the same stop without cleanup, taken at the second consecutive reconciliation that reads no state; a run that finishes first hands its work off as usual, and labeling the issue again makes it a candidate at the next poll
- Raising or removing `agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, or `agent.max_monthly_cost_usd` in `WORKFLOW.md`, which lifts an instance spend hold at the next poll tick or retry timer fire.
- Requesting a retry of a refused issue, which dispatches it at once instead of at the reset. The request bypasses a usage-limit hold, so it is the early probe of the provider's allowance when the operator knows it reopened sooner. When the run ends normally, the holds it proves open lift.
- Restarting the service for process recovery or deployment (not as the normal path for applying workflow config changes).
