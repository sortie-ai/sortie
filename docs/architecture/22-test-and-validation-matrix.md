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
- Each tracker adapter's suite exercises the shared tracker conformance assertions: normalized
  issue shape, ascending comment order, an empty-but-non-nil result on an empty list operation, a
  state map that omits an unknown id, no request issued on empty input, tracker error kind mapping
  for every class the adapter can produce, and an additive label write

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
- Bot-review reconciliation is skipped when no SCM adapter is constructed or when bot-review is not
  configured
- Bot classification is the union of the platform bot marker and the `bot_usernames` allowlist, so
  on a provider that reports no bot marker an empty allowlist selects nothing and the kind never
  dispatches
- Bot-review selection requires no `CHANGES_REQUESTED` review state, and outdated comments are
  filtered before the fingerprint is computed
- New actionable bot comments dispatch on the tick they are detected, with no debounce window and no
  `debounce_ms` field
- The bot-review budget is `max_continuation_turns` with a per-kind default of `5` and a poll
  interval defaulting to `60000`, both independent of the `review` kind's values
- The bot-review fingerprint is the sorted non-outdated comment-ID hash under its own kind row; a
  changed comment set resets the dispatched flag and re-arms a dispatch, leaving the `review`
  fingerprint untouched
- A non-nil retry-slot incumbent defers the bot-review pass without dispatching
- Bot-review escalation at the cap clears only the `bot-review` pending entry and fingerprint row,
  leaving the residual attempt counter, the pending retry, and the claim for the terminal-state path
- Bot-review escalation tracker-call failures are logged and counted but do not block the
  slot-scoped cleanup
- Worker-exit seeding and startup recovery create a bot-review entry only when SCM metadata reports
  `pr_number > 0` and non-empty `owner`, `repo`, and `branch`, and recovery is gated on the
  configured flag so a configured-but-providerless setup recovers none
- Merge-conflict reconciliation is skipped when no SCM adapter is constructed or when merge-conflict
  is not configured, and the pass runs after bot-review and before auto-merge
- Only the normalized `dirty` state arms the reaction; `unknown` defers at the poll interval without
  touching the fingerprint or the attempt counter
- A provider whose mergeability mapping never yields `dirty` leaves the kind inert: every due tick
  defers and the pending TTL drops the entry without escalating
- The mergeability read runs on every due tick with no retry-budget check ahead of it, so the
  not-dirty branch that closes the episode stays reachable
- The retry-slot guard, the empty head-SHA guard, and the empty base-branch guard all run before the
  attempt increment, so a deferral never burns an attempt
- The merge-conflict fingerprint is the SHA-256 of the head SHA under its own kind row; a same-head
  dispatched observation re-enqueues without incrementing or dispatching, a new head re-arms a fresh
  attempt, and the not-dirty branch deletes the row so the next dirty observation dispatches
- The merge-conflict cap is a strict over-limit comparison against `max_retries`, whose per-kind
  default is `1`, so a configured `0` escalates on the first conflict detection
- Both merge-conflict episode exits, the not-dirty branch and escalation, delete the per-episode
  attempt counter, so a later independent conflict opens a fresh episode at attempt 1
- Merge-conflict escalation scopes every deletion to the `merge-conflict` kind, cancels and deletes
  no retry, releases no claim, and leaves parallel `ci`, `review`, `bot-review`, and `merge`
  reactions intact
- The rebase continuation carries the PR's base branch read live on the dispatching tick rather than
  an assumed default branch
- Auto-merge reconciliation is skipped when `reactions.auto_merge.provider` is empty or no SCM
  adapter is present
- The auto-merge pass runs after the CI, review-comment, bot-review, and merge-conflict passes and
  before the two label-command passes
- A sticky auth-class preflight failure drops every `merge`-kind pending entry on each later tick, a
  transport-class preflight failure schedules exactly one retry before the flag sticks, and absent
  scope information fails open with auto-merge enabled
- A draft PR, a mergeability outside `clean` and `unstable`, a review decision other than `APPROVED`
  or `NOT_REQUIRED`, and a CI conclusion other than success while `require_ci` holds each re-enqueue
  at the poll interval instead of merging
- Count-based auto-merge escalation applies only when `max_retries > 0`, whose default is `2`, so a
  configured `0` disables it rather than escalating on the first attempt
- An auth-class or payload-class `MergePR` failure escalates immediately, bypassing the
  `max_retries` check, including when it is `0`; any other conflict re-enqueues at the poll interval
- A merge rejection carrying the already-merged marker is treated as idempotent success and takes
  the same post-merge actions as a completed merge
- The merge fingerprint is the SHA-256 of the head SHA, a newline byte, and the review-decision
  string under kind `merge`; a changed head SHA or review decision refreshes it, and a successful
  merge clears it
- A branch-delete or tracker-comment failure after a completed merge is not rolled back
- Neither the post-merge success path nor the auto-merge escalation path cancels or deletes a retry,
  clears every entry an issue holds, or deletes the issue's claim, so parallel `ci` and `review`
  continuations survive
- The already-merged disposition clears only the `merge`-kind pending entry and fingerprint and
  never transitions the tracker issue
- Each label-command pass is skipped when no SCM adapter is constructed or its own command is not
  configured, and an absent or empty `provider` means no journal read happens for either command
- A fix-only configuration, with `review_label` empty and `fix_label` non-empty, activates the block
  and constructs the SCM adapter exactly as a review-only configuration does
- A `provider` set while both command labels are empty is rejected offline
- The high-water mark advances to the newest examined event on every tick that reads new events,
  including `unlabeled` entries, foreign labels, and retracted gestures
- All matching `labeled` events in one batch collapse to at most one command
- A command is confirmed only when its label is still present on the PR at detection time; otherwise
  the mark advances and nothing dispatches
- The mark is persisted before the dispatch is scheduled, so a crash between the two loses the
  command rather than duplicating it
- The `dispatched` flag is not a deduplication input for either label kind, and the review pass's
  "stored fingerprint matches and dispatched" skip is not adopted
- A failed mark read backs off without dispatching, while a failed mark upsert proceeds
- Neither label kind carries a TTL, a drop-on-age branch, an attempt counter, or an escalation
  posture
- A foreign-kind retry-slot incumbent defers with the mark unchanged and no label removal, a
  same-kind incumbent or a running command of the same kind advances the mark and collapses the
  gesture, and only a free slot dispatches
- Each label-command pass re-enqueues its own detection entry on dispatch, because neither command's
  exit satisfies the reaction-enqueue gate and so re-seeds nothing
- A `label-review` entry seeds and recovers on PR metadata without a branch, while a `label-fix`
  entry additionally requires a non-empty head branch and seeds none without one
- A `label-review` dispatch runs the read-only, no-clone posture with the operator hooks skipped and
  a `label-fix` dispatch takes the normal workspace preparation path with the hooks run; both
  suppress the dispatch-time transition, the dispatch comment, the per-turn tracker-state refresh,
  the self-review loop, the handoff transition, and the active-issue continuation retry
- Each label-command pass scopes every `pending_reactions` and `reaction_fingerprints` mutation to
  its own kind, so the relative ordering of the two passes does not affect correctness
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
- Each coding-agent adapter carries a token-accounting regression test whose input is output
  captured from the runtime rather than a hand-written constant; the test asserts the
  run-cumulative scope and monotonicity contract of Section 10.3
- Each coding-agent adapter carries a test proving it emits no `token_usage` event and reports
  the run unmeasured when its runtime supplies no usage figure for that run
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
- An unmeasured run is distinguishable from a zero-consumption run in the persisted row, in both
  `sortie stats` output forms, on the dashboard, and in the `cost_budget` result; an unmeasured
  run creates no Prometheus series

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

### 17.9 Source-control Adapter and CI Provider Roles (Core Conformance)

- Each source-control adapter's suite exercises the shared source-control conformance assertions:
  an empty-but-non-nil result on each list method with no results, source-control error kind
  mapping for the auth, not-found, payload, and API classes, the already-merged marker on a merge
  that raced an external merge, the absent-branch disposition on a delete of a missing branch, the
  absent-label disposition on a removal of an absent label, and a correctly ordered label-event
  journal.
- Each CI provider's suite exercises the shared CI-aggregate conformance assertion against a result
  containing at least one completed-failing run, one in-progress run, and one completed-success
  run, confirming the provider's aggregate status and failing count agree with the forge decision
  core.

