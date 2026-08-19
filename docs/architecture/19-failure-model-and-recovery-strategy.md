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

4. `Tracker Failures`
   - API transport errors
   - Non-200 status
   - API-level errors
   - Malformed payloads

5. `Observability Failures`
   - Snapshot timeout
   - Dashboard render errors
   - Log sink configuration failure

6. `CI Feedback Failures`
   - CI status fetch errors (transport, auth, API, not-found, payload)
   - Escalation failures (label or comment write to tracker)
   - Missing or malformed `.sortie/scm.json`

7. `Handoff Evidence Failures`
   - Absence of work observed where the four pre-existing handoff conditions otherwise permit the
     transition
   - Evidence not determinable under the `strict` policy, which deliberately treats that verdict as
     an absence

### 14.2 Recovery Behavior

- Dispatch validation failures:
  - Skip new dispatches.
  - Keep service alive.
  - Continue reconciliation where possible.

- Worker failures:
  - Convert to retries with exponential backoff.

- Recognized `.sortie/status` signal:
  - Both values keep their existing dispositions. A `blocked` soft stop suppresses the handoff
    transition and the continuation retry, and, where the dispatch drives issue state, parks the
    issue instead of merely releasing the claim: it records a durable park and applies the
    parking label, sharing both the mechanism and the release rule the consecutive-absence
    ceiling uses below. A completion signal (`needs-human-review`) suppresses
    the continuation retry and takes the ordered handoff disposition (§7.3) unchanged: the
    transition fires only where a handoff state is configured, the issue is still active, the
    dispatch drives issue state, no terminal observation suppresses it, and the evidence verdict
    permits the write. A verdict that withholds the write routes this exit to the withheld-handoff
    recovery below, exactly as it routes any other exit that reaches it.
  - Where self-review is enabled and its gate admits the exit, the completion signal now runs the
    self-review phase before that disposition is computed. The phase runs while the session is
    live and before session teardown; the disposition itself is unchanged.

- Withheld handoff evidence:
  - Leave the issue in its active tracker state, record the run as `failed` with the evidence verdict
    in its error reason, and schedule the ordinary exponential-backoff failure path. Do not use the
    fixed-delay continuation path.
  - Maintain a count of consecutive outcomes treated as handoff absences for each issue. `Absence of
    work observed` increments it, as does `evidence not determinable` under `strict`. A
    `work observed` verdict resets it to zero immediately; a later handoff-write error cannot restore
    the old count. Outcomes that provide no work-observed verdict do not pretend to reset it.
  - Derive the consecutive-absence ceiling from `agent.max_sessions`, without adding a setting:
    when `agent.max_sessions > 0`, use that value verbatim; when it is `0`, use `3` for this ceiling
    only while retaining `0` as unlimited for the ordinary total-session budget. The comparison is
    against the incremented **consecutive-absence count**, and parking occurs when `count >= ceiling`.
    Thus the default sequence is absence `1`, retry; absence `2`, retry; absence `3`, park. It is the
    initial run plus two retries, not three retries. A positive `agent.max_sessions` continues to
    enforce its existing all-run effort budget independently, so whichever applicable gate is
    reached first stops re-dispatch.
  - The derived default of `3` follows the `max_retries: 2` default used by most reaction kinds: one
    initial attempt plus two retries. This avoids treating one legitimate no-change conclusion as
    immediately terminal, gives two further sessions a chance to surface work, and bounds the cost
    of full agent sessions. Turn, tool, or model-iteration limits inside one session do not set this
    ceiling and are not comparable to it.
  - On reaching the ceiling, cancel this retry sequence, apply the primary-dispatch parking label,
    and release the claim. The sequence cancelled is the primary-dispatch one the count is kept for:
    a reaction continuation retry on the same issue carries its own sequence, cannot record an
    absence, and is not cancelled here. Ceiling exhaustion is an eligibility gate rather than only a
    cancelled timer: ordinary polling and restart recovery must not silently begin another run in
    the same exhausted absence sequence. The representation is fixed and shared with the `blocked`
    park: the same durable park record, the same runtime gate, and the same release rule, tagged
    with the reason this ceiling produced it rather than the reason a `blocked` signal did. The rule
    requires neither a second numeric setting nor a durable verdict column on `run_history`.
  - Under `tracker.handoff_evidence: off`, no absence is ever recorded, so the count is never
    incremented and no *new* park is taken by this ceiling on any path: polling, retry firing, and
    worker exit all skip the trigger, and the policy costs nothing while it is selected. This
    governs only whether a *new* park is taken, not whether an existing one holds: a park already
    recorded, by either trigger, is not lifted by selecting `off` and is released only by the rules
    below. Raising `agent.max_sessions` above the recorded count has the same narrowed effect: it
    changes the ceiling a future count is compared against, but it does not lift a park already
    taken, because the park is a row rather than a value re-derived from the current ceiling on
    every tick.
  - An operator releases a park with either of two gestures, whichever the orchestrator observes
    first, and both apply to every trigger that parks an issue, not only to this ceiling: moving
    the issue to a tracker state different from the one recorded when it was parked, or removing
    a parking label the orchestrator has confirmed reached the tracker. The label gesture carries
    a confirmation guard: confirmation comes only from observing the label present on a later
    fetch of the issue, never from the label write itself returning without error, because a
    tracker adapter is permitted to accept a label write and record nothing. Releasing on an
    unconfirmed label's absence would let such an adapter undo the park silently. A park taken
    from the retry lane records no observed state at the moment it is taken, since the retry lane
    parks ahead of its own tracker fetch by design; the first later observation backfills the
    state without releasing the park on that same tick, so the operator's own action is never
    mistaken for the very read that first captured the baseline. A parked issue absent from the
    poll tick's candidate set, because it now sits in a state the deployment does not call
    active, is still read for its state through a separate, filter-free tracker call, so the
    state gesture reaches it exactly as it would if the issue were still a candidate. Release
    resets the counter that produced the park, so a released absence-ceiling park does not
    immediately re-derive the same exhausted count and park itself again; release is evaluated
    ahead of the ceiling trigger on the same tick for exactly this reason.
  - A third release takes no gesture: a worker exit whose evidence verdict is `work observed` lifts
    the park of the issue it ran on, on the same verdict that resets the absence count, and it too
    applies to every trigger that parks an issue. This is the one release the evidence policy
    governs, since `off` computes no verdict, so a deployment that selects it keeps the two
    gestures above as the only way out of a park.
  - The parking label is the resolved non-empty
    `reactions.review_comments.escalation_label` captured when the orchestrator starts, or
    `needs-human` when that block or value is absent or empty. This lookup deliberately reuses only
    the label name: review-comments need not be enabled, `escalation: comment` does not turn primary
    parking into a comment, and labels configured under `ci_failure`, `merge_conflicts`,
    `auto_merge`, or any other reaction do not participate. This explicit one-way dependency keeps
    the existing operator-visible escalation channel without creating a second configuration field.
    `review_comments` is the source because this parking happens immediately before human review;
    CI, merge-conflict, and auto-merge labels describe different recovery domains.
  - Announce parking with the issue, the consecutive count, the ceiling, and the label. A label-write
    failure follows the existing escalation-failure rule: log and count the error and keep the
    absence sequence stopped rather than resuming an unbounded loop.

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
  - Authentication or payload failure: escalate immediately, skipping the over-limit comparison
    against `max_retries`; the attempt counter still increments on the failed call.
  - Issue not found: stop, mark the fingerprint dispatched, log a warning, no escalation.

  Not every orchestrator-driven tracker write in this section gets a later re-attempt. A stalled
  dispatch is retried and a CI check re-enqueues, but a dispatch-time in-progress transition
  failure is only logged and never retried, and a handoff transition failure on a soft-stop worker
  exit releases the claim without scheduling a continuation; both fail quietly with no re-attempt
  to follow. Escalation failures do not reopen the dropped entry within the same process run (see
  the escalation bullet above). What distinguishes merge-completion is not the absence of any
  fallback, but where that fallback lives. A failed transition escalation deliberately leaves the
  normal merge fingerprint undispatched (§11G.4), so a freshly seeded pending entry reconciles the
  same merge commit and retries the transition the escalated attempt could not complete.

  Missing-identifier escalation follows the same cross-run posture without changing that normal
  fingerprint: its separate observation is marked dispatched only after the operator label or
  comment is delivered. A failed write leaves the expired observation undispatched and the current
  pending entry dropped. A later worker exit or startup recovery may seed a fresh entry, which
  retries only that undelivered operator signal and drops again; it neither restarts the
  thirty-minute grace period nor restores an in-process polling loop. This is why every disposition
  above ends in a bounded retry followed by escalation, an immediate escalation, or an explicit
  logged stop, and never a silent drop: the durable undispatched state gives the failed action
  another chance across a fresh entry.

### 14.3 Partial State Recovery (Restart)

Sortie uses SQLite persistence to improve restart recovery semantics:

- Retry entries with future `due_at` timestamps are restored from SQLite and rescheduled on
  startup.
- Parked issues are reloaded from SQLite before the event loop starts, so an issue parked before
  a restart stays out of dispatch across it without depending on any tracker read to rediscover
  the park.
- Session metadata from the previous run is available for observability and debugging.
- Run history is preserved in SQLite for operational review.
- Running sessions are not recoverable (agent subprocesses do not survive restart), but the
  orchestrator knows which issues were in-flight at shutdown and re-dispatches them immediately
  rather than waiting for the next polling cycle to discover them.
- Pending reaction recovery reconstructs runtime reaction entries after a restart from the most
  recent `run_history` row recorded as `succeeded` for each issue and by reading that candidate's
  workspace SCM metadata. A later handoff withheld for absence is recorded as `failed`, so it no
  longer displaces an earlier producing run in this selection; no query-shape change is required.
  Recovery considers only a candidate whose selected successful activity falls inside its lookback
  window. The pass narrows in a fixed order: it first drops any candidate
  the orchestrator already holds as running, retry-queued, or claimed, then truncates what is left
  to a fixed cap on candidate issues per startup pass, and only then reads tracker state and keeps
  the candidates still sitting in the configured handoff state. Because the cap is applied before
  the tracker read, an issue beyond it is skipped without its state ever being fetched, and its
  entry is not rebuilt until a later worker exit or a subsequent startup reaches it.
- Recovery is decided per reaction kind, not once for the issue: each kind rebuilds its own entry
  only when that kind is configured for this process and the candidate's SCM metadata satisfies
  that kind's own field requirements. A kind that requires a head branch is not recovered from
  metadata that records only a pull request, and a kind that is switched off recovers nothing even
  when a sibling kind on the same issue does.
- `workspace.retention_days` and this recovery lookback are
  coupled: the retention floor (`WorkspaceRetentionMinDays`) in days equals the recovery lookback
  in days, so any workspace the periodic sweep's age bound is permitted to remove is one recovery
  would already have skipped as stale. Changing either window without the other reintroduces the
  defect the coupling prevents. A shorter retention floor would let the age bound remove a
  workspace recovery still regards as live, silently breaking reaction recovery for that issue
  after a restart.

### 14.4 Operator Intervention Points

Operators can control behavior by:

- Editing `WORKFLOW.md` (prompt and most runtime settings).
- `WORKFLOW.md` changes should be detected and re-applied automatically without restart.
- Changing issue states in the tracker:
  - terminal state -> running session is stopped and workspace cleaned when reconciled
  - non-active state -> running session is stopped without cleanup
- Restarting the service for process recovery or deployment (not as the normal path for applying
  workflow config changes).
