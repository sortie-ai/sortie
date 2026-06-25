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

### 14.3 Partial State Recovery (Restart)

Sortie uses SQLite persistence to improve restart recovery semantics:

- Retry entries with future `due_at` timestamps are restored from SQLite and rescheduled on
  startup.
- Session metadata from the previous run is available for observability and debugging.
- Run history is preserved in SQLite for operational review.
- Running sessions are not recoverable (agent subprocesses do not survive restart), but the
  orchestrator knows which issues were in-flight at shutdown and re-dispatches them immediately
  rather than waiting for the next polling cycle to discover them.

### 14.4 Operator Intervention Points

Operators can control behavior by:

- Editing `WORKFLOW.md` (prompt and most runtime settings).
- `WORKFLOW.md` changes should be detected and re-applied automatically without restart.
- Changing issue states in the tracker:
  - terminal state -> running session is stopped and workspace cleaned when reconciled
  - non-active state -> running session is stopped without cleanup
- Restarting the service for process recovery or deployment (not as the normal path for applying
  workflow config changes).

