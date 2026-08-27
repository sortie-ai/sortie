## 13. Logging, Status, and Observability

### 13.1 Logging Conventions

Required context fields for issue-related logs:

- `issue_id`
- `issue_identifier`

Required context for coding-agent session lifecycle logs:

- `session_id`

Message formatting requirements:

- Use stable `key=value` phrasing.
- Include action outcome (`completed`, `failed`, `retrying`, etc.).
- Include concise failure reason when present.
- Avoid logging large raw payloads unless necessary.

Handoff-evidence records are part of the required operator surface:

- A withheld verdict whose verification read (§11.5, §14.2) does not find the issue terminal emits
  a `Warn` record naming the verdict and carrying `turns_completed` plus the resulting
  `consecutive_absences` count. The standard issue context fields identify the affected issue.
- A withheld verdict whose verification read does find the issue terminal emits an `Info` record at
  the read site naming the discarded verdict, its reason, `state_source="verified"`, and
  `turns_completed`, instead of the `Warn` record above. The terminal disposition's own `Info`
  record then follows, carrying the verified state and the same `state_source="verified"` value.
- An `evidence not determinable` verdict emits an `Info` record under both `observed` and `strict`,
  carrying the policy and `turns_completed`. Under `strict`, the separate withheld warning is also
  emitted because that policy converts the verdict into the absence disposition, unless the
  verification read above finds the issue terminal.
- A run frozen to `tracker.handoff_evidence: off` emits none of these evidence records.

Parking an issue, whichever trigger produced it, emits exactly one `Warn` record, message
`"issue parked"`, carrying `reason` (`agent_blocked` or `handoff_absence`), `parked_state`
(the tracker state recorded, empty when unobserved), and `label` (the parking label applied).
The consecutive-absence ceiling's park additionally carries `consecutive_absences`,
`absence_ceiling`, and `ceiling_setting` (the dotted configuration path that produced the
ceiling); a `blocked` park carries none of the three. A failed label write is recorded separately,
message `"park label write failed"`, at `Warn`, and does not suppress the parking record. Lifting
a park emits one `Info` record, message `"issue unparked"`, carrying `trigger`
(`state_changed`, `label_removed`, or `evidence_observed`) and `reason`.

An issue entering the per-issue budget-exhausted set, on either the poll tick's rebuild or the
retry lane, emits exactly one `Warn` record, message `"candidate held by budget ceiling"`, carrying
the standard issue context fields plus `reason` (`token_budget` or `session_budget`),
`used_sessions`, `budget_sessions`, and, when the token ceiling was evaluated for that issue,
`used_tokens` and `budget_tokens`. It also carries `ceiling_setting`, the dotted configuration
path of the setting that governs the fired ceiling, when `reason` maps to a known setting; a
reason with no known governing setting emits no `ceiling_setting` attribute rather than an empty
or invented one. The record fires once per hold: a tick that re-observes an
already-announced hold under the same reason emits nothing further, and whichever lane discovers a
hold is the only one that announces it.

The tracker comment this same hold posts is recorded separately, at the write site rather than
alongside the log record above. A successful write emits one `Info` record, message
`"budget hold notice posted"`, carrying the standard issue context fields. A failed write emits
one `Warn` record, message `"budget hold notice failed"`, carrying the standard issue context
fields and `error`, and does not suppress the log record above.

The periodic workspace sweep emits exactly one summary record per pass, at `Info` level, message
`"sweep: pass complete"`, on every pass that produced a candidate set, including a pass over zero
keys, a pass whose tracker read failed, and a pass that removed nothing. This is deliberate: a
sweep that finds nothing to remove and says nothing is indistinguishable from a sweep that is not
running at all, which is the failure mode this record exists to close. The record carries thirteen
attributes:

- `candidates`, `excluded_running`, `excluded_retry`, `excluded_reaction`, `removed_terminal`,
  `removed_age`, `retained_in_window`, `retained_no_activity`, `retained_not_evaluated`, and
  `failed` are integer counts. The nine counters after `candidates` partition the candidate set, so
  `candidates` equals their sum on every pass; `retained_not_evaluated` is what makes that identity
  hold on a pass where the age bound did not evaluate anything.
- `retention_days` is the configured `workspace.retention_days` value for that pass; `0` means the
  bound is off.
- `age_pass` is `"on"` when the age bound evaluated its candidates that pass, `"off"` when
  `retention_days` is `0` or below the floor (`WorkspaceRetentionMinDays`), and `"unavailable"`
  when the persistence read is not configured or its query failed.
- `tracker_read` is `"ok"` or `"failed"`.

Each workspace removed by the age bound also emits its own `Info` record, message `"sweep:
removed expired workspace"`, carrying `workspace_key`, `last_activity` (RFC3339), and `age_days`.

The `"tick completed"` `Info` record carries four integer attributes for the blocker gate in
addition to its existing ones: `held_by_blockers`, `blockers_unresolved`, `blockers_not_read`, and
`blockers_incomplete`, each counting the candidates the tick held for that reason. It also carries
`budget_exhausted`, the number of this tick's candidates in the per-issue budget-exhausted set
after the rebuild.

A candidate the dispatch gate holds for a blocker reason produces exactly one of five per-issue
records, and a pass whose reads were refused by the forge produces exactly one pass-level record in
addition:

- Held by a non-terminal blocker: one `Debug` record, message `"candidate held by blocker"`,
  carrying the issue context fields plus `blocker_identifier` and `blocker_state` for the first
  non-terminal blocker.
- Held because its own blocker read was attempted and failed: one `Warn` record, message
  `"candidate blockers unresolved, holding issue"`, carrying the issue context fields and `error`.
- Held because the pass had already halted on another candidate's failure: one `Debug` record,
  message `"candidate blockers not read this tick, pass halted"`, carrying `error_kind` and no
  `error`, because nothing failed on this candidate.
- Held because its producer declared the list incomplete, with no read attempted: one `Debug`
  record, message `"candidate blocker list incomplete, holding issue"`, carrying the issue context
  fields and no `error`.
- Held because the pass had already spent its read budget: one `Debug` record, message `"candidate
  blockers not read this tick, holding issue"`, carrying `reads_spent` and no `error`.

A pass whose reads halted emits exactly one `Error` record, message `"blocker reads halted for this
tick"`, carrying `error_kind`, `http_status` (`0` when the failure carried none), `operation`
(`"fetch_blockers"`), and `held_unread`. A tick that dispatches nothing because every blocker read
it attempted failed transiently, and that did not already emit the pass-level `Error` above, emits
one additional `Warn` record, message `"tick dispatched nothing: every attempted candidate blocker
read failed"`, carrying `reads_failed`, the count of reads the pass attempted and lost.

### 13.2 Logging Outputs and Sinks

Sortie does not prescribe where logs must go (stderr, file, remote sink, etc.).

Requirements:

- Operators must be able to see startup/validation/dispatch failures without attaching a debugger.
- Sortie may write to one or more sinks.
- If a configured log sink fails, Sortie continues running when possible and emits an
  operator-visible warning through any remaining sink.

### 13.3 Runtime Snapshot / Monitoring Interface

If the implementation exposes a synchronous runtime snapshot (for dashboards or monitoring), it
should return:

- `running` (list of running session rows)
- each running row should include `turn_count`
- each running row should include `tokens_measured`, meaning at least one usage measurement has
  been reported so far in that session
- `retrying` (list of retry queue rows)
- `agent_totals`
  - `input_tokens`
  - `output_tokens`
  - `total_tokens`
  - `cache_read_tokens`
  - `seconds_running` (aggregate runtime seconds as of snapshot time, including active sessions)
- `rate_limits` (latest coding-agent rate limit payload, if available)
- `budget_exhausted_count` (number of issues currently blocked by a re-dispatch budget; always present)
- `budget_exhausted` (list of blocked-issue records, sorted by identifier; always present, empty
  when the set is empty). Each record carries the issue's ID and identifier, the reason
  (`token_budget` or `session_budget`; `token_budget` takes precedence over `session_budget` when
  one issue reaches both gates), the used and budgeted session and token counts, and the time the
  hold began. The set is rebuilt per tick from those two gates (Section 8.4) and also updated by
  the retry lane when it discovers a hold between ticks.
- `parked_count` (number of issues currently held out of primary dispatch; always present)
- `parked` (sorted list of parked issue IDs; omitted when the set is empty)
- `parked_reason` (map from parked issue ID to the park's reason, `agent_blocked` or
  `handoff_absence`; omitted when the set is empty)

Recommended snapshot error modes:

- `timeout`
- `unavailable`

### 13.4 Optional Human-Readable Status Surface

A human-readable status surface is optional and implementation-defined. When the HTTP server is
enabled (Section 13.7), the HTML dashboard served at `/` (Section 13.7.1) is the concrete
realization of this surface.

If present, it should draw from orchestrator state/metrics only and must not be required for
correctness.

Live status answers what is happening now. A second, offline reporting surface answers what
happened over a past window: it reads persisted run history without contacting the tracker, the
coding agent, or a running orchestrator, and summarizes outcomes, durations, turn counts, and token
spend, optionally grouped by the recorded run attributes and bounded by a time range. It prices
token spend from the operator-supplied rates in workflow configuration; with no rates configured it
reports the countable figures and omits cost rather than guessing at one. Because it reads only
what earlier runs already recorded, a database written by an older schema yields a reduced set of
figures, and the surface reports which figures it could not derive instead of presenting a zero.
This surface performs no state mutation and is never required for orchestrator correctness.

A figure can be missing for two distinct reasons: the database predates the schema that records
it, or nothing measured it because the coding agent behind the run reported no token usage. The
schema tier alone no longer separates the two: a full-tier report can still carry a null `tokens`
summary or breakdown row when every run in range is unmeasured. `tokens_unmeasured_runs` is the
field that distinguishes a figure missing for want of a schema from one missing for want of a
measurement. This supersedes the clause in ADR-0019
(`docs/decisions/0019-keep-usage-data-on-the-host.md`) stating that `schema_tier` alone tells a
consumer whether token and cost figures were available at all.

### 13.5 Session Metrics and Token Accounting

A run is measured when the runtime reported at least one usage figure for the session and the
adapter carried it into the recorded counters, or when the worker never entered an agent turn,
because a run that launched no agent spent exactly zero. A run is unmeasured when an agent turn
began and no usage figure ever arrived; its recorded token figures are zero and that zero carries
no information. A measurement of zero is a measured run whose reported figures are zero, which is
a legitimate statement recorded as measured.

The run record carries this distinction alongside the four token counters. An unmeasured run
contributes nothing to any token counter and is excluded from cost pricing. It advances no
Prometheus token counter and creates no series, the same as a run that never emitted a usage
event.

Token accounting rules:

- Agent adapters normalize token counts before emitting events. The orchestrator receives
  `{input_tokens, output_tokens, total_tokens, cache_read_tokens}` directly.
- For absolute totals, track deltas relative to last reported totals to avoid double-counting.
  The `cache_read_tokens` field follows the same cumulative-delta accounting as
  `input_tokens` / `output_tokens`. Deltas are accumulated from any event carrying a non-zero
  usage payload, not only `token_usage` events, so an adapter can attach the authoritative
  run-cumulative snapshot to a turn-finalization event without losing it.
- `api_request_count` is incremented monotonically, and only, per `token_usage` event; a
  usage-bearing terminal event does not count as an additional request.
- Accumulate aggregate totals in orchestrator state (`agent_totals`).
- At session exit, the session's token totals are written to the `run_history` row alongside
  the aggregate update. The run's final usage is reconciled from the worker result before that
  row is written, so a dropped or late event cannot lower the recorded total. The per-issue
  token budget (`agent.max_tokens`) sums `run_history` `total_tokens` per issue; the
  `cost_budget` tool reads the same sum plus the running session's recorded total.

Timing accounting rules:

- `api_time_ms` is the cumulative LLM API wait time in milliseconds for the session.
  Accumulated from `api_duration_ms` fields on any agent event that carries timing data.
- `tool_time_ms` is the cumulative tool execution time in milliseconds.
  Accumulated from `tool_result` events that carry `duration_ms`.
- `tool_time_percent` and `api_time_percent` are computed at render time as
  `(cumulative_ms / session_elapsed_ms) * 100`. Displayed as null/"N/A" when no timing
  data has been received.

Runtime accounting:

- Runtime should be reported as a live aggregate at snapshot/render time.
- Sortie maintains a cumulative counter for ended sessions and adds active-session elapsed time
  derived from `running` entries (for example `started_at`) when producing a snapshot/status view.
- Add run duration seconds to the cumulative ended-session runtime when a session ends (normal exit
  or cancellation/termination).
- Continuous background ticking of runtime totals is not required.

Rate-limit tracking:

- Track the latest rate-limit payload seen in any agent update.
- Any human-readable presentation of rate-limit data is implementation-defined.

### 13.6 Humanized Agent Event Summaries (Optional)

Humanized summaries of raw agent protocol events are optional.

If implemented:

- Treat them as observability-only output.
- Do not make orchestrator logic depend on humanized strings.

### 13.7 HTTP Server

Sortie includes an embedded HTTP server for observability and operational control. The
server starts unconditionally on port **7678** unless explicitly disabled. It is not
required for orchestrator correctness, but its absence silently removes health probes,
Prometheus metrics, and the dashboard.

Enablement and configuration:

- The HTTP server starts by default on `127.0.0.1:7678` with no flags required.
- `--port N` overrides the listening port. Port `0` disables the server entirely.
- `--host ADDR` overrides the bind address. `ADDR` must be a parseable IP address.
  Default: `127.0.0.1`. Container deployments use `0.0.0.0`.
- `server.port` and `server.host` extension keys provide the same overrides via workflow
  front matter. CLI flags take precedence over extension keys.
- `server.port` must be an integer in the range 1–65535, or `0` to disable.
- `server.host` must be a parseable IP address string. DNS hostnames are not accepted.
- When the default port (7678) is occupied and the operator did not explicitly request a
  port (`--port` absent, `server.port` extension absent), Sortie logs a warning and starts
  without the HTTP server; the orchestrator continues normally. When the operator explicitly
  requested a port (via `--port` or `server.port`) and it is already in use, Sortie exits
  with code 1 and a descriptive error. No automatic port selection occurs.
- The `--dry-run` flag suppresses server startup regardless of port or host settings.
- Changes to HTTP listener settings require restart (hot-rebind is not supported).

#### 13.7.1 Human-Readable Dashboard (`/`)

- Host a human-readable dashboard at `/`.
- The returned document should depict the current state of the system (for example active sessions,
  retry delays, token consumption, runtime totals, recent events, health/error indicators, and run
  history from SQLite).
- It is up to the implementation whether this is server-generated HTML or a client-side app that
  consumes the JSON API below.

#### 13.7.2 JSON REST API (`/api/v1/*`)

Provide a JSON REST API under `/api/v1/*` for current runtime state and operational debugging.

Minimum endpoints:

- `GET /api/v1/state`
  - Returns a summary view of the current system state (running sessions, retry queue/delays,
    aggregate token/runtime totals, latest rate limits, and any additional tracked summary fields).
  - Suggested response shape:

    ```json
    {
      "generated_at": "2026-02-24T20:15:30Z",
      "counts": {
        "running": 2,
        "retrying": 1,
        "budget_exhausted": 1
      },
      "running": [
        {
          "issue_id": "abc123",
          "issue_identifier": "MT-649",
          "state": "In Progress",
          "session_id": "thread-1-turn-1",
          "turn_count": 7,
          "last_event": "turn_completed",
          "last_message": "",
          "started_at": "2026-02-24T20:10:12Z",
          "last_event_at": "2026-02-24T20:14:59Z",
          "tokens": {
            "input_tokens": 1200,
            "output_tokens": 800,
            "total_tokens": 2000,
            "cache_read_tokens": 400
          },
          "model_name": "claude-sonnet-4-20250514",
          "api_request_count": 3,
          "requests_by_model": {"claude-sonnet-4-20250514": 3},
          "tool_time_percent": 12.3,
          "api_time_percent": 45.6,
          "tokens_measured": true
        }
      ],
      "retrying": [
        {
          "issue_id": "def456",
          "issue_identifier": "MT-650",
          "attempt": 3,
          "due_at": "2026-02-24T20:16:00Z",
          "error": "no available orchestrator slots"
        }
      ],
      "budget_exhausted": [
        {
          "issue_id": "ghi789",
          "issue_identifier": "MT-651",
          "reason": "session_budget",
          "used_sessions": 3,
          "budget_sessions": 3,
          "used_tokens": null,
          "budget_tokens": 0,
          "unmeasured_sessions": null,
          "exhausted_at": "2026-02-24T20:12:00Z"
        }
      ],
      "agent_totals": {
        "input_tokens": 5000,
        "output_tokens": 2400,
        "total_tokens": 7400,
        "cache_read_tokens": 1500,
        "seconds_running": 1834.2
      },
      "rate_limits": null
    }
    ```

- `GET /api/v1/<issue_identifier>`
  - Returns issue-specific runtime/debug details for the identified issue, including any information
    tracked that is useful for debugging.
  - Suggested response shape:

    ```json
    {
      "issue_identifier": "MT-649",
      "issue_id": "abc123",
      "status": "running",
      "workspace": {
        "path": "/tmp/sortie_workspaces/MT-649"
      },
      "attempts": {
        "restart_count": 1,
        "current_retry_attempt": 2
      },
      "running": {
        "session_id": "thread-1-turn-1",
        "turn_count": 7,
        "state": "In Progress",
        "started_at": "2026-02-24T20:10:12Z",
        "last_event": "notification",
        "last_message": "Working on tests",
        "last_event_at": "2026-02-24T20:14:59Z",
        "tokens": {
          "input_tokens": 1200,
          "output_tokens": 800,
          "total_tokens": 2000
        }
      },
      "retry": null,
      "budget_exhausted": null,
      "logs": {
        "agent_session_logs": [
          {
            "label": "latest",
            "path": "/var/log/sortie/agent/MT-649/latest.log",
            "url": null
          }
        ]
      },
      "recent_events": [
        {
          "at": "2026-02-24T20:14:59Z",
          "event": "notification",
          "message": "Working on tests"
        }
      ],
      "last_error": null,
      "tracked": {}
    }
    ```

  - `status` is `"running"` when the issue has a running session, otherwise `"retrying"` when it has
    a pending retry, otherwise `"budget_exhausted"` when it is held out of dispatch by a per-issue
    budget ceiling; `budget_exhausted` in the response body carries the same record shape as the
    `budget_exhausted` array on `GET /api/v1/state` and is `null` when the issue is not in that set.
  - If the issue is unknown to the current in-memory state, return `404` with an error response
    (for example `{"error":{"code":"issue_not_found","message":"..."}}`); an issue held by a budget
    ceiling is not unknown to it, so this case is distinct from that one.

- `POST /api/v1/refresh`
  - Queues an immediate tracker poll + reconciliation cycle (best-effort trigger; implementations
    may coalesce repeated requests).
  - Suggested request body: empty body or `{}`.
  - Suggested response (`202 Accepted`) shape:

    ```json
    {
      "queued": true,
      "coalesced": false,
      "requested_at": "2026-02-24T20:15:30Z",
      "operations": ["poll", "reconcile"]
    }
    ```

API design notes:

- The JSON shapes above are the recommended baseline for interoperability and debugging ergonomics.
- Implementations may add fields, but should avoid breaking existing fields within a version.
- Endpoints should be read-only except for operational triggers like `/refresh`.
- Unsupported methods on defined routes should return `405 Method Not Allowed`.
- API errors should use a JSON envelope such as `{"error":{"code":"...","message":"..."}}`.
- If the dashboard is a client-side app, it should consume this API rather than duplicating state
  logic.

#### 13.7.3 Prometheus Metrics Endpoint (`/metrics`)

When the HTTP server is enabled, Sortie exposes a Prometheus exposition-format scrape endpoint at
`/metrics`, backed by a dedicated registry so the process exports only its own series and none of
the runtime's defaults. See ADR-0008. The endpoint is co-located with the JSON
API and HTML dashboard on the same address and port; no separate configuration is required.

Implementation requirements:

- Use a dedicated `prometheus.Registry` (not the global default) to prevent pollution from
  unrelated collectors and to enable isolated test assertions.
- Register the handler via `promhttp.InstrumentMetricHandler(registry, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))`.
  `InstrumentMetricHandler` wraps `HandlerFor` and registers `promhttp_metric_handler_*` counters
  on the same dedicated registry automatically, ensuring scrape self-instrumentation appears in
  scrape output rather than landing silently on the global default.
- Register standard Go runtime and process collectors (`collectors.NewGoCollector`,
  `collectors.NewProcessCollector`) on the dedicated registry so that `go_*` and `process_*`
  metrics appear in scrape output alongside Sortie's own metrics.

Defined metrics (label sets and buckets are specified here; see ADR-0008 for historical rationale):

| Name | Type | Description |
|------|------|-------------|
| `sortie_sessions_running` | Gauge | Number of agent sessions currently executing. |
| `sortie_sessions_retrying` | Gauge | Number of issues in the retry queue. |
| `sortie_slots_available` | Gauge | Remaining dispatch capacity under current concurrency limits. |
| `sortie_active_sessions_elapsed_seconds` | Gauge | Cumulative wall-clock elapsed time across all currently running sessions. |
| `sortie_ssh_host_usage{host}` | Gauge | Current session count per remote SSH host, partitioned by host. Recomputed from the host-usage map after each tick, worker exit, and retry-timer event; hosts removed by config reload decrement to zero rather than freezing at their last value. |
| `sortie_tokens_total{type}` | Counter | Tokens consumed, partitioned by type (`input`, `output`). |
| `sortie_agent_runtime_seconds_total` | Counter | Cumulative agent-session wall-clock time for completed sessions. |
| `sortie_dispatches_total{outcome}` | Counter | Dispatch attempts, partitioned by outcome (`success`, `error`). |
| `sortie_worker_exits_total{exit_type}` | Counter | Worker exits, partitioned by exit type (`normal`, `error`, `cancelled`, `soft_stop`). A soft-stop exit reports `soft_stop` rather than the exit kind it would otherwise map to. |
| `sortie_retries_total{trigger}` | Counter | Retry schedule events, partitioned by trigger (`error`, `continuation`, `timer`, `stall`, `ci_fix`). |
| `sortie_reconciliation_actions_total{action}` | Counter | Reconciliation outcomes per issue, partitioned by action (`stop`, `cleanup`, `keep`, `sweep_cleanup`, `sweep_expired`). |
| `sortie_poll_cycles_total{result}` | Counter | Poll tick completions, partitioned by result (`success`, `error`, `skipped`). |
| `sortie_tracker_requests_total{operation,result}` | Counter | Tracker adapter API calls, partitioned by operation (`fetch_candidates`, `fetch_issue`, `fetch_by_states`, `fetch_states_by_ids`, `fetch_states_by_identifiers`, `fetch_comments`, `fetch_blockers`, `transition`, `comment`, `add_label`) and result (`success`, `error`). |
| `sortie_tracker_comments_total{lifecycle,result}` | Counter | Tracker comment attempts, partitioned by lifecycle point (`dispatch`, `completion`, `failure`, `budget_hold`, among others the project may add) and result (`success`, `error`). |
| `sortie_handoff_transitions_total{result}` | Counter | Handoff-state dispositions, partitioned by result (`success`, `error`, `skipped`, `withheld`). `withheld` means the handoff-evidence policy selected the absence failure path before any transition attempt, distinguishing it from an ordinary worker failure; a withholding verdict whose own verification read then finds the issue terminal does not increment this label, it increments `skipped` instead. `skipped` names three causes and does not distinguish them: the issue was no longer in an active state at worker exit, the issue was already reported terminal and the handoff write was suppressed (Section 11.5), or a withheld verdict's own verification read reported the issue terminal and suppressed the absence-failure recording (Section 14.2). All four values are counted only when `tracker.handoff_state` is configured. |
| `sortie_dispatch_transitions_total{result}` | Counter | Dispatch-time in-progress transition attempts, partitioned by result (`success`, `error`, `skipped`). `skipped` indicates the issue was already in the target state. |
| `sortie_issue_parks_total{reason}` | Counter | Issue park events, partitioned by reason (`agent_blocked`, `handoff_absence`). Incremented once per park, whichever trigger produced it. |
| `sortie_dispatch_rule_match_total{layer,rule}` | Counter | Dispatch rule match outcomes, partitioned by resolution layer (`rule`, `default`, `fallback`) and matched rule name. Empty rule names report as `<none>` to bound label cardinality. |
| `sortie_candidate_holds_total{reason}` | Counter | `IncCandidateHolds`. Candidates the dispatch loop held, partitioned by reason (`blocked_by`, `blockers_unresolved`, `blockers_not_read`, `blockers_incomplete`). Incremented once per held candidate; never incremented for a candidate rejected by an eligibility or capacity gate, and never incremented a second time for the pass-level `blocker reads halted for this tick` ERROR that accompanies a run of `blockers_unresolved` holds. |
| `sortie_budget_exhaustions_total{reason}` | Counter | `IncBudgetExhaustions`. Issue entries into the per-issue budget-exhausted set, partitioned by reason (`token_budget`, `session_budget`, open to a later value). Incremented once per hold, from whichever lane discovered it; never incremented on a tick that merely re-observes an already-announced hold. |
| `sortie_budget_exhausted_issues{reason}` | Gauge | `SetBudgetExhaustedIssues`. Issues currently held out of dispatch, partitioned by reason. Recomputed from the full budget-exhausted set on every gauge update, including every declared reason at zero when nothing is held, so a reason that clears reports zero rather than freezing at its last value. |
| `sortie_tool_calls_total{tool,result}` | Counter | Agent tool call completions, partitioned by tool name and result (`success`, `error`). |
| `sortie_ci_status_checks_total{result}` | Counter | CI status check outcomes, partitioned by result (`passing`, `pending`, `failing`, `error`). |
| `sortie_ci_escalations_total{action}` | Counter | CI escalation actions when fix retries are exhausted, partitioned by action (`label`, `comment`, `error`). |
| `sortie_reactions_auto_merge_total{result}` | Counter | Auto-merge reaction outcomes, partitioned by result (`merged`, `error`, `escalated`). |
| `sortie_review_checks_total{result}` | Counter | Review comment check outcomes, partitioned by result (`dispatched`, `error`, `skipped`). |
| `sortie_review_escalations_total{action}` | Counter | Review escalation actions when continuation turns are exhausted, partitioned by action (`label`, `comment`, `error`). |
| `sortie_self_review_iterations_total{verdict}` | Counter | Self-review iterations, partitioned by per-iteration verdict (`pass`, `iterate`, `none`). `none` indicates a missing or unparseable verdict for that iteration. |
| `sortie_self_review_sessions_total{final_verdict}` | Counter | Self-review sessions, partitioned by final verdict (`pass`, `iterate`, `none`). |
| `sortie_self_review_cap_reached_total` | Counter | Self-review sessions that reached the iteration cap without a passing verdict. |
| `sortie_poll_duration_seconds` | Histogram | Wall-clock time per poll cycle; buckets via `ExponentialBuckets(0.1, 2, 10)` (0.1 s–51.2 s). |
| `sortie_worker_duration_seconds{exit_type}` | Histogram | Worker session wall-clock time; buckets via `ExponentialBuckets(10, 2, 12)` (10 s–5.7 h). `exit_type` carries the same values as `sortie_worker_exits_total`. |
| `sortie_self_review_verification_duration_seconds{command}` | Histogram | Per-command verification duration during self-review; buckets via `ExponentialBuckets(10, 2, 12)` (10 s–5.7 h). The `command` label is truncated to the first 64 characters. |
| `sortie_build_info{version,go_version}` | Gauge | Always `1`; carries build metadata as labels. |

### 13.8 Self-Review Observability on an Admitting Status Signal

A run that ends on the `needs-human-review` status signal, or on a `no-change-needed` declaration,
and passes through the self-review phase records review metadata on `run_history` exactly as a run
that reached the phase by exhausting the turn budget does: the JSON document under
`review_metadata`, described in Section 19, carries a value rather than null for this population.

The `after_run` hook on that path receives the run's real `SORTIE_SELF_REVIEW_STATUS` value
(`passed`, `cap_reached`, or `error`) instead of the `disabled` value it receives when self-review
is not configured or the run did not reach the phase. `SORTIE_SELF_REVIEW_SUMMARY_PATH` is
populated on the same terms as on any other path that ran the phase. A declared run supplies both
values on these same terms whether the phase confirms the declaration or retracts it: the
retraction is a separate outcome recorded in the run's own log, not a change to what this section
populates.
