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
    transition and the continuation retry. A completion signal (`needs-human-review`) suppresses
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
    the same exhausted absence sequence. Its representation is an implementation detail; the rule
    requires neither a second numeric setting nor a durable verdict column on `run_history`.
  - Under `tracker.handoff_evidence: off` no absence is ever recorded, so neither the count nor the
    gate is evaluated on any path: polling, retry firing, and worker exit all skip it. The policy
    costs nothing and no issue is parked by this mechanism while it is selected.
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
  to follow. Escalation failures, including merge-completion's own, are logged and counted but do
  not reopen the dropped entry (see the escalation bullet above). What distinguishes
  merge-completion is not the absence of any fallback, but where that fallback lives: within a
  single process run, an escalated merge-completion transition fires once and nothing later in
  that run revisits it. Across a restart, or a subsequent worker exit on the same issue, escalation
  deliberately leaves the fingerprint row undispatched (§11G.4), so a freshly seeded pending entry
  reconciles the same merge commit and retries the transition the escalated attempt could not
  complete. This is why every disposition above ends in a bounded retry followed by escalation, an
  immediate escalation, or an explicit logged stop, and never a silent drop: escalation is the
  operator-visible signal within a run, and the undispatched fingerprint is what gives the
  transition another chance across one.

### 14.3 Partial State Recovery (Restart)

Sortie uses SQLite persistence to improve restart recovery semantics:

- Retry entries with future `due_at` timestamps are restored from SQLite and rescheduled on
  startup.
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
