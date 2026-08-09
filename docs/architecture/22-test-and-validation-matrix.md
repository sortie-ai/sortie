## 17. Test and Validation Matrix

Sortie's tests cover the behaviors defined in this architecture document.

Validation profiles:

- `Core Conformance`: deterministic tests required for all core features.
- `Extension Conformance`: required only for optional features that are implemented.
- `Real Integration Profile`: environment-dependent smoke/integration checks recommended before
  production use.

Unless otherwise noted, Sections 17.1 through 17.7 are `Core Conformance`. Bullets that begin with
`If ... is implemented` are `Extension Conformance`.

### 17.1 Workflow and Config Parsing

- Workflow file path precedence:
  - explicit runtime path is used when provided
  - cwd default is `WORKFLOW.md` when no explicit runtime path is provided
- Workflow file changes are detected and trigger re-read/re-apply without restart
- Invalid workflow reload keeps last known good effective configuration and emits an
  operator-visible error
- Missing `WORKFLOW.md` returns typed error
- Invalid YAML front matter returns typed error
- Front matter non-map returns typed error
- Config defaults apply when optional values are missing
- `tracker.kind` validation enforces registered tracker adapters
- `tracker.api_key` works (including `$VAR` indirection)
- `$VAR` resolution works for tracker API key and path values
- `~` path expansion works
- `agent.command` is preserved as a shell command string
- Per-state concurrency override map normalizes state names and ignores invalid values
- Prompt template renders `issue`, `attempt`, and `run`
- Prompt rendering fails on unknown variables (strict mode)

### 17.2 Workspace Manager and Safety

- Deterministic workspace path per issue identifier
- Missing workspace directory is created
- Existing workspace directory is reused
- Existing non-directory path at workspace location is handled safely (replace or fail per
  implementation policy)
- Optional workspace population/synchronization errors are surfaced
- `after_create` hook runs only on new workspace creation
- `before_run` hook runs before each attempt and failure/timeouts abort the current attempt
- `after_run` hook runs after each attempt and failure/timeouts are logged and ignored
- `before_remove` hook runs on cleanup and failures/timeouts are ignored
- Workspace path sanitization and root containment invariants are enforced before agent launch
- Agent launch uses the per-issue workspace path as cwd and rejects out-of-root paths
- Hook environment variables (`SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`, `SORTIE_WORKSPACE`,
  `SORTIE_ATTEMPT`) are set correctly
- The periodic sweep excludes workspace keys held by running entries and scheduled retries, and
  keys held by a pending reaction entry whose kind pins its workspace, while a non-pinning kind
  leaves its key a candidate
- Within one sweep pass the terminal check runs before the age bound, and a key removed by the
  terminal check is not re-evaluated by the age bound
- The age bound removes a workspace whose latest recorded activity precedes the retention window,
  anchoring on the later of the recorded completion and the recorded push
- A workspace with no parseable activity timestamp is retained regardless of age
- A retention value of `0` or below the floor disables the age bound; a value between `1` and the
  floor is rejected at config parse time
- A failed tracker state read still lets the age bound evaluate and remove eligible workspaces on
  that pass
- One sweep summary record is emitted per pass that produced a candidate set, including a pass
  that removed nothing, and its outcome counters sum to the candidate count

### 17.3 Issue Tracker Client

- Candidate issue fetch uses active states and project identifier
- Adapter contract tests cover: normalized field mapping, pagination order, error categories
- Each tracker adapter ships its own integration test suite
- Empty `fetch_issues_by_states([])` returns empty without API call
- Pagination preserves order across multiple pages
- Labels are normalized to lowercase
- Issue state refresh by ID returns minimal normalized issues
- Error mapping covers transport errors, auth errors, API errors, and malformed payloads
- `query_filter` is appended to candidate fetch JQL when non-empty
- `query_filter` is appended to terminal-state fetch JQL when non-empty
- `query_filter` is NOT appended to state-refresh-by-IDs JQL
- Empty `query_filter` produces the same JQL as before (no trailing AND)
- `query_filter` containing OR operators is wrapped in parentheses
- SQLite persistence layer correctly saves and restores retry entries across simulated restart
- Startup recovery from SQLite reconstructs retry timers with correct remaining delays
- Run history is queryable after session completion

### 17.4 Orchestrator Dispatch, Reconciliation, and Retry

- Dispatch sort order is priority then oldest creation time
- Issue with non-terminal blockers in a non-running active state is not eligible
- Issue with terminal blockers is eligible
- Active-state issue refresh updates running entry state
- Non-active state stops running agent without workspace cleanup
- Terminal state stops running agent and cleans workspace
- Reconciliation with no running issues is a no-op
- An issue reported terminal releases its pending reaction entries, reaction attempt counters,
  pending retry, and claim, whether or not it has a running worker, leaving fingerprint rows intact
- A failed tracker state refresh keeps workers running and releases nothing
- Normal worker exit with the issue still active and no handoff state configured schedules a short
  continuation retry (attempt 1)
- Normal worker exit with a handoff state configured and the issue still active performs the
  handoff transition and releases the claim without scheduling a continuation retry
- Handoff transition failure schedules a continuation retry on a non-soft-stop exit and releases
  the claim on a soft-stop exit
- Normal worker exit whose freshest observation is terminal suppresses the handoff transition, the
  continuation retry, and every reaction enqueue, from each of the three observation sources
  (reconciliation, the worker's own refresh, and the dispatch-time snapshot)
- With terminal states configured and a non-terminal observation, one verification read runs
  immediately before the handoff write; a terminal result suppresses the write and a failed read
  lets it proceed
- With no terminal states configured, no verification read is issued
- A blocked soft stop releases the claim with no handoff and no continuation retry
- A dispatch that does not drive issue state performs neither the dispatch-time transition nor the
  handoff transition, and enqueues no reaction on its own exit
- Abnormal worker exit increments retries with 10s-based exponential backoff
- Retry backoff cap uses configured `agent.max_retry_backoff_ms`
- Retry queue entries include attempt, due time, identifier, and error
- Stall detection kills stalled sessions and schedules retry
- Slot exhaustion requeues retries with explicit error reason
- Dispatch-time in-progress transition calls `TransitionIssue` when `tracker.in_progress_state`
  is configured
- Dispatch-time transition failure is non-fatal: the worker continues to workspace preparation
- Dispatch-time transition is skipped when `tracker.in_progress_state` is absent
- Dispatch-time transition is skipped (debug log only) when the issue is already in the target state
- If a snapshot API is implemented, it returns running rows, retry rows, token totals, and rate
  limits
- If a snapshot API is implemented, timeout/unavailable cases are surfaced
- CI status reconciliation is skipped when neither `ci_feedback.kind` nor `reactions.ci_failure`
  is configured
- CI status passing clears CI fix attempts for the issue
- CI status pending re-enqueues the pending check for the next tick
- CI status failing within `max_retries` schedules a CI-fix dispatch with failure context
- CI status failing beyond `max_retries` escalates (label or comment) and releases the claim
- CI failure context is injected into turn 1 prompt via `prompt.WithContinuationContext`
- Escalation label failure is logged but does not block claim release
- `.sortie/scm.json` symlink rejection prevents CI check enqueue
- `.sortie/scm.json` oversized or malformed files degrade to no-CI behavior
- Review comment reconciliation is skipped when `reactions.review_comments` is not configured
- Review comment poll throttle respected (PendingRetryAt in future → skip)
- Review comment fetch error increments backoff and re-enqueues
- No actionable review comments re-enqueues with poll interval delay
- Review comment fingerprint unchanged and dispatched → skip
- Review comment fingerprint changed, debounce not elapsed → defer
- Review comment fingerprint changed, debounce elapsed → dispatch with review context
- Review comment continuation turn cap exceeded → escalate and release claim
- Review comment outdated comments filtered before fingerprint computation
- Review comment context injected into turn 1 prompt via `prompt.WithContinuationContext`
- Review escalation failure is logged but does not block claim release
- Worker exit with `scm.json` containing `pr_number > 0`, `owner`, and `repo` creates review
  pending reaction; missing fields degrade to no-review behavior
- Worker exit does not overwrite existing pending review entry (preserves debounce state)
- Merge-completion reconciliation is skipped when `reactions.merge_completion` is not configured,
  or when no SCM adapter or no tracker adapter is present
- Merge-completion drops an entry whose issue is missing from the state response, already terminal,
  or has left the handoff state, and defers one whose issue is still claimed
- Merge-completion transitions the issue to `target_state` once per merge commit identifier, and a
  second reconcile of the same identifier performs no further transition
- A merged pull request reporting no merge commit identifier re-enqueues rather than latching
- Merge-completion transition failure routes by tracker error kind: transport and API retry with
  backoff to `max_retries` then escalate, auth and payload escalate immediately, and not-found
  stops while marking the fingerprint dispatched
- A `target_state` that equals the handoff state, is a member of the active states, or is absent
  from the configured terminal states is rejected offline
- A `target_state` that drifts out of a reloaded terminal-state list logs one warning per onset and
  does not change any entry's disposition
- Self-review disabled adds zero overhead (no review turns, no review metadata)
- Self-review runs verification commands and passes results to agent
- Review verdict "pass" terminates loop
- Review verdict "iterate" triggers fix turn and next iteration
- Iteration cap enforced; worker exits with cap_reached metadata
- Missing verdict treated as iterate (non-final) / pass (final)
- Verification command timeout does not block remaining commands
- Review progress visible in runtime snapshot via selfReviewCh

### 17.5 Coding-Agent Adapter Client

- Launch command uses workspace cwd and invokes the configured shell
- Startup handshake sequence is adapter-defined and tested per adapter
- Policy-related startup payloads use the implementation's documented approval/sandbox settings
- Session identifiers are parsed and `session_started` event is emitted
- Request/response read timeout is enforced
- Turn timeout is enforced
- Partial JSON lines are buffered until newline (for adapters using line-delimited protocols)
- Stdout and stderr are handled separately; protocol JSON is parsed from stdout only
- Non-JSON stderr lines are logged but do not crash parsing
- Command/file-change approvals are handled according to the implementation's documented policy
- Unsupported dynamic tool calls are handled at the adapter level without stalling the session
- User input requests are handled according to the implementation's documented policy and do not
  stall indefinitely
- Normalized token usage events are emitted with `{input_tokens, output_tokens, total_tokens}`
- `ToolRegistry` is populated at startup and all registered tools appear in prompt-time
  advertisement
- `tracker_api` tool:
  - inputs execute against configured tracker auth
  - API-level errors produce `success: false` with a normalized `{kind, message}` error envelope
  - invalid arguments, missing auth, and transport failures return structured failure payloads
  - the tool is scoped to the configured project
- Unsupported tool names return a failure result at the adapter level without stalling the
  session

### 17.6 Observability

- Validation failures are operator-visible
- Structured logging includes issue/session context fields
- Logging sink failures do not crash orchestration
- Token/rate-limit aggregation remains correct across repeated agent updates
- If a human-readable status surface is implemented, it is driven from orchestrator state and does
  not affect correctness
- If humanized event summaries are implemented, they cover key agent event classes without changing
  orchestrator behavior

### 17.7 CLI and Host Lifecycle

- CLI accepts an optional positional workflow path argument (`path-to-WORKFLOW.md`)
- CLI uses `./WORKFLOW.md` when no workflow path argument is provided
- CLI errors on nonexistent explicit workflow path or missing default `./WORKFLOW.md`
- CLI surfaces startup failure cleanly
- CLI exits with success when application starts and shuts down normally
- CLI exits nonzero when startup fails or the host process exits abnormally

### 17.8 Real Integration Profile (Recommended)

These checks are recommended for production readiness and may be skipped in CI when credentials,
network access, or external service permissions are unavailable.

- A real tracker smoke test can be run with valid credentials supplied by the appropriate tracker
  credential environment variable or a documented local bootstrap mechanism.
- Real integration tests should use isolated test identifiers/workspaces and clean up tracker
  artifacts when practical.
- A skipped real-integration test should be reported as skipped, not silently treated as passed.
- If a real-integration profile is explicitly enabled in CI or release validation, failures should
  fail that job.

