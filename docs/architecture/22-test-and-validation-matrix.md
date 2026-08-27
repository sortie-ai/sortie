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
- `agent.command` is preserved as a whitespace-delimited argument-vector string
- A non-positive `agent.turn_timeout_ms` is rejected at config parse time; an absent key takes
  the default
- An absent `agent.max_consecutive_absences` key takes the default of `3`; `0` and negative
  values are rejected at config parse time; `SORTIE_AGENT_MAX_CONSECUTIVE_ABSENCES` overrides a
  file-supplied value and is rejected under the same rule
- An absent `reactions.ci_failure.watch_window_ms` key takes the default; a negative value and a
  value above `9223372036854` are rejected at config parse time, naming the field and the value;
  `9223372036854` itself is accepted
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
  for every class the adapter can produce, an additive label write, a candidate's blocker fields
  agreeing with the adapter's declared blocker source (`AssertCandidateBlockerSource`), a resolved
  blocker list's normalization (`AssertBlockerRefsNormalized`), and a blocker ref's identifier shape
  matching the issue's own (`AssertBlockerIdentifiersMatchIssue`)

### 17.4 Orchestrator Dispatch, Reconciliation, and Retry

- Dispatch sort order is priority then oldest creation time
- Issue with non-terminal blockers in a non-running active state is not eligible
- An issue whose blockers could not be resolved is not eligible either
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
- With a withheld handoff-evidence verdict, terminal states configured, and a tracker adapter
  present, one verification read runs before any withheld-handoff effect; a terminal result
  routes the exit to the terminal disposition, and a non-terminal result, a response omitting the
  issue, or a failed read keeps the withheld disposition
- With no terminal states configured, no verification read is issued
- A blocked soft stop releases the claim with no handoff and no continuation retry, and, where
  the dispatch drives issue state, records a durable park, applies the parking label, and holds
  the issue out of dispatch (§14.2)
- A blocked soft stop from a dispatch that does not drive issue state records no park
- `ShouldDispatch` and the dispatch loop's pre-built-set variant return false for a parked issue
  regardless of the configured effort budgets
- A retry timer firing for an already-parked issue releases the claim and deletes the retry row
  without dispatching, under every evidence policy, and this refusal does not disturb the
  retry lane's own ability to park a newly ceiling-exhausted issue
- The release rule: a tracker state change unparks the issue; a confirmed parking label observed
  absent unparks the issue; an unconfirmed label observed absent leaves the park in place; a
  parking label observed while unconfirmed is recorded as confirmed
- A failed parking-label write leaves the park in place with the label unconfirmed, and a later
  release pass that still observes the label absent does not unpark the issue on that evidence
  alone
- A successful parking-label write does not by itself confirm the label; only a later observation
  of the label on the issue does
- Loading persisted park records at startup restores the runtime park set, skipping a malformed
  record and loading a record with no recorded tracker state
- The runtime snapshot reports the parked-issue count always, and the parked issue list and
  reason map only when the parked set is non-empty
- A parked issue absent from the poll tick's candidate set is read for its state through a
  separate, filter-free tracker call, and that call carries only the parked issues the candidate
  fetch did not return
- Parking on the consecutive handoff-absence ceiling and parking on a blocked soft stop produce
  the same durable record shape, differing only in the reason attributed to the park
- Releasing a park resets its consecutive-absence count whatever reason the park carries; for a
  released absence-ceiling park, the same poll tick does not immediately re-derive the exhausted
  count and park the issue again
- A park held under `tracker.handoff_evidence: off` is not released by the policy, and no new
  absence park is taken while the policy is set
- A retry-lane absence park records no observed tracker state; the next poll tick backfills it
  without releasing the park, and a state change observed after the backfill releases it
- A worker exit whose own run is parked for absence and later reports a work-observed verdict
  releases the park
- One park produces exactly one park log record and one park counter increment, whichever
  trigger produced it
- The worker-exit absence park records the same tracker state the run's own terminal observation
  resolved, not an unrecorded state, and is releasable by a later state change without an
  intervening backfill tick
- The consecutive-absence ceiling reads only `agent.max_consecutive_absences` on every lane that
  evaluates it, including shutdown drain: its value does not move when `agent.max_sessions`
  moves, and a worker exit processed during shutdown drain resolves the same ceiling a worker
  exit processed by the ordinary event loop would
- An issue entering the per-issue budget-exhausted set, on either the poll tick's rebuild or the
  retry lane, produces exactly one log record naming the issue, the reason, the used and
  budgeted numbers, and, where the fired ceiling's governing setting is known, the setting itself
- The record and its counter increment fire once per hold: repeated ticks over the same held
  issue produce neither, and an issue that leaves the candidate set and returns still held under
  the same reason produces neither either
- A park carrying a ceiling also names the setting behind it; an `agent_blocked` park, which
  carries no ceiling, names neither
- Whichever lane, poll tick or retry timer, discovers a hold is the only one that announces it;
  the other lane's rebuild or block leaves the announcement memory alone
- The budget-exhausted gauge reports a per-reason level derived from the current set, seeded to
  zero for every declared reason on each recompute, so a reason that clears reports zero rather
  than freezing at its last published value
- Both `GET /api/v1/state` and `GET /api/v1/{identifier}` carry the budget-exhausted record's
  identifier, reason, and numbers, and the per-issue endpoint answers for a budget-blocked issue
  instead of reporting it unknown
- The dashboard renders a budget-blocked card and table only when the exhausted set is non-empty
- A hold entering the budget-exhausted set, on either lane, posts exactly one tracker comment
  naming the fired ceiling and its governing setting; a tick or retry fire that re-observes the
  same hold under the same reason posts nothing further
- A restart does not repeat the comment: a second process loading the same durable notice table
  posts nothing for a hold already announced before the restart, even though its own in-memory
  log latch re-announces the hold
- A hold whose governing ceiling changes posts a second comment naming the new ceiling and
  replaces the durable notice row rather than adding one
- Notices are paced by a wall-clock window shared by both lanes rather than by a per-tick count,
  so the same bound holds at any configured poll interval; a burst of holds larger than the
  window drains over the following windows with nothing lost or duplicated
- Releasing a hold deletes its durable notice row and its memory entry, and both budgets disabled
  clears every row in one statement; re-enabling a ceiling announces the hold that re-forms
- A tracker write that fails to post the comment is logged and counted but does not retry on the
  following tick, and leaves the exhausted set, the claim set, and reconciliation unaffected
- A dispatch that does not drive issue state performs neither the dispatch-time transition nor the
  handoff transition, and enqueues no reaction on its own exit
- Abnormal worker exit increments retries with 10s-based exponential backoff
- Retry backoff cap uses configured `agent.max_retry_backoff_ms`
- Retry queue entries include attempt, due time, identifier, and error
- Stall detection kills stalled sessions and schedules retry
- A turn exceeding the configured turn timeout ends the attempt with the `turn_timeout` failure
  and schedules a retry, while a worker whose context was already cancelled keeps its
  cancellation disposition; this is the enforcing side of the property Section 17.5 lists as
  "Turn timeout is enforced"
- A self-review turn expiry ends the attempt the same way, while every other self-review failure
  (diff generation, a verification command, a verdict parse, a `blocked` status) still exits
  normally
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
- A review pending entry younger than the configured `watch_window_ms` survives and re-enqueues;
  one whose age exactly equals the window still survives; one older than the window is dropped and
  its own `reaction_attempts` counter is deleted, leaving the claim, the retry, the fingerprint row,
  and every sibling kind's entry untouched; a configured `0` leaves an entry far older than thirty
  minutes in place
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
- A bot-review pending entry ages the same way the review kind does, against its own configured
  `watch_window_ms`: younger survives, exactly-at-window survives, older is dropped with only its
  own attempt counter deleted, and a configured `0` leaves an old entry in place
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
  defers and the configured watch window drops the entry without escalating
- A merge-conflict pending entry ages the same way the review kind does, against its own
  configured `watch_window_ms`, and an entry with a non-zero `HeadRecordedAt` still ages from
  `CreatedAt` rather than from `HeadRecordedAt`
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
- An auto-merge pending entry ages the same way the review kind does, against its own configured
  `watch_window_ms`: younger survives, exactly-at-window survives, older is dropped with only its
  own attempt counter deleted, and a configured `0` leaves an old entry in place
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
- The offline validator reports a negative or an out-of-range `watch_window_ms` for each of
  `review_comments`, `bot_review`, `merge_conflicts`, and `auto_merge` as a `Severity: "error"`
  diagnostic under that kind's own `Check`, and reports none for a valid value or for a kind whose
  `provider` is empty
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
- An unmerged pull request can remain pending beyond thirty minutes without creating a missing-SHA
  observation; the first merged-without-SHA response starts a persisted thirty-minute grace period
- Missing-SHA checks use exponential pending backoff without consulting transition `max_retries`;
  a real SHA transitions normally and clears the observation only after the normal latch completes,
  while expiry drops pending, emits the configured escalation, and performs no transition
- Missing-SHA observation tests cover restart persistence, same-identity timestamp preservation,
  delivered-escalation suppression, later-fresh-entry real-SHA recovery, new-PR reset, lifecycle
  cleanup, best-effort delete ordering, external escalation failure remaining stopped in-process,
  and a later fresh entry retrying only undelivered escalation
- Generic terminal release remains fingerprint-agnostic and leaves both normal fingerprints and
  internal missing-SHA observations intact
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
- Self-review is admitted from the agent's completion signal (`needs-human-review`), not only
  from turn-budget exhaustion
- A `blocked` signal skips self-review and remains an immediate exit, without entering the phase
- The status file is removed on admission so self-review does not re-observe the signal that
  admitted it
- A `needs-human-review` signal read inside self-review is consumed and does not abort the loop
- A `blocked` signal read inside self-review aborts the phase, and becomes the run's exit reason
  on either admission path into the phase
- The status file is removed at both in-phase read sites, after a review turn and after a fix
  turn, when the phase reads `blocked`, on either admission path into the phase
- A deployment with self-review disabled treats the completion signal exactly as before, ending
  the run without entering the phase
- A recognized signal read outside the self-review phase, at the read after a coding turn, leaves
  the status file in place at teardown, for `blocked`, `needs-human-review`, and
  `no-change-needed`
- `IsRecognized` returns true for `no-change-needed`, and `ReadStatusFile` maps the literal
  `no-change-needed` to the third status value while `No-Change-Needed`, `no_change_needed`, and
  `nochangeneeded` fall through to unrecognized
- Self-review is admitted from a `no-change-needed` declaration on the same terms as
  `needs-human-review`, run once per admitting value over the same five gate conditions, with
  identical admission and skip outcomes
- A `no-change-needed` declaration stands only where the phase recorded exactly one iteration
  ending on a `pass` verdict with no failing verification result; a non-zero exit, a timeout, a
  command that could not start, a second recorded iteration, or a non-`pass` final verdict each
  retract it, and a phase that did not run leaves it standing
- A declaration written during an in-phase read is consumed and ignored, on the same terms as the
  completion signal, and does not convert a turn-budget admission into a declared run
- `tracker.no_change_state` set with `tracker.handoff_state` unset is rejected offline, and a value
  that is neither `handoff_state` nor a member of the `terminal_states` written in front matter is
  rejected offline, both with no tracker call
- On an SCM platform that exposes no bot-account marker, a `CHANGES_REQUESTED` review whose author
  matches the operator-configured `bot_usernames` allowlist does not trigger the human
  `review_comments` reaction
- A human-authored `CHANGES_REQUESTED` review still dispatches the human `review_comments`
  reaction, covered by the same test as the prior bullet
- An author the platform marks as a bot is excluded exactly as today, an author absent from the
  allowlist dispatches exactly as today, and an allowlisted author the platform does not mark as a
  bot is now excluded from the human loop instead of driving both the human and the bot-review loop

### 17.5 Coding-Agent Adapter Client

- Launch command uses workspace cwd and execs the resolved binary directly with an argument
  vector
- Startup handshake sequence is adapter-defined and tested per adapter
- Policy-related startup payloads use the implementation's documented approval/sandbox settings
- Session identifiers are parsed and `session_started` event is emitted
- Request/response read timeout is enforced
- Turn timeout is enforced
- Partial JSON lines are buffered until newline (for adapters using line-delimited protocols)
- Stdout and stderr are handled separately; protocol JSON is parsed from stdout only
- Non-JSON stderr lines are logged but do not crash parsing
- Command/file-change approvals are handled according to the implementation's documented policy
- Unsupported tool calls are answered by the MCP execution channel with a JSON-RPC error rather
  than a result, not by the adapter, without stalling the session
- User input requests are handled according to the implementation's documented policy and do not
  stall indefinitely
- Normalized token usage events are emitted with `{input_tokens, output_tokens, total_tokens}`
- Each coding-agent adapter carries a token-accounting regression test whose input is output
  captured from the runtime rather than a hand-written constant; the test asserts the
  run-cumulative scope and monotonicity contract of Section 10.3
- Each coding-agent adapter carries a test proving it emits no `token_usage` event and reports
  the run unmeasured when its runtime supplies no usage figure for that run
- `ToolRegistry` is populated at startup; a registered tool appears in the prompt-time
  advertisement when the session's agent kind and launch mode deliver an execution channel, and
  every advertised tool is callable over that channel, asserted per adapter
- `tracker_api` tool:
  - inputs execute against configured tracker auth
  - API-level errors produce `success: false` with a normalized `{kind, message}` error envelope
  - invalid arguments, missing auth, and transport failures return structured failure payloads
  - the tool is scoped to the configured project
- Unsupported tool names return a JSON-RPC error rather than a result over the MCP execution
  channel, not at the adapter level, without stalling the session

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
  absent-label disposition on a removal of an absent label, a correctly ordered label-event
  journal, a label-event entry whose timestamp does not parse failing the read with the payload
  error kind and yielding no events, and a review comment whose timestamp does not parse not
  failing the read and carrying the zero timestamp. A forge whose review decision is folded from
  per-review submission times fails that read on an unparseable timestamp of a review that can
  change the verdict, an adapter-specific obligation with no shared assertion. A forge whose
  review-comment route reports the two diff sides as separate line fields returns each comment
  with the line of the side it is anchored to and marks no comment outdated, an adapter-specific
  obligation with no shared assertion.
- Each CI provider's suite exercises the shared CI-aggregate conformance assertion against a result
  containing at least one completed-failing run, one in-progress run, and one completed-success
  run, confirming the provider's aggregate status and failing count agree with the forge decision
  core.
