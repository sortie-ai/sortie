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

### 14.2 Recovery Behavior

- Dispatch validation failures:
  - Skip new dispatches.
  - Keep service alive.
  - Continue reconciliation where possible.

- Worker failures:
  - Convert to retries with exponential backoff.

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
  - Authentication or payload failure: escalate immediately, consuming no retry budget.
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
- Pending reaction recovery reconstructs runtime reaction entries after a restart by reading each
  candidate's workspace SCM metadata, and it considers only a candidate whose latest activity
  falls inside its lookback window. `workspace.retention_days` and this recovery lookback are
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

