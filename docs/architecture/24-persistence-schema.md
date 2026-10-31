## 19. Persistence Schema

### 19.1 Overview

Sortie uses an embedded SQLite database for durable state. The database file path defaults to `.sortie.db` in the same directory as `WORKFLOW.md` and can be overridden with the `db_path` front matter field (see Section 5.3.6). On startup, Sortie opens or creates the database and runs all pending schema migrations before beginning normal operation. The last price tables accepted from a download (Section 13.5) are files beside the database file, outside the schema.

### 19.2 Tables

**`retry_entries`**: pending retries to be reconstructed on restart

| Column       | Type    | Notes                                              |
| ------------ | ------- | -------------------------------------------------- |
| `issue_id`   | TEXT PK | Tracker-internal issue ID                          |
| `identifier` | TEXT    | Human-readable ticket key                          |
| `attempt`    | INTEGER | Retry attempt number (1-based)                     |
| `due_at_ms`  | INTEGER | Unix epoch milliseconds; used to reconstruct timer |
| `error`      | TEXT    | Last error message, may be null                    |
| `session_id` | TEXT    | Adapter-assigned session ID from previous attempt, may be null |
| `rule_name`   | TEXT   | Dispatch rule that routed the original dispatch; empty when none matched |
| `template_id` | TEXT   | Prompt template the rule selected |
| `agent_kind`  | TEXT   | Agent adapter the rule selected; empty on a pre-migration entry |
| `stage_previous` | TEXT | Source rule of the stage hop the frozen selection carries into its `stage.previous`; empty when none |
| `stage_previous_outcome` | TEXT | Outcome of that hop, `succeeded` or `no_change`; empty when `stage_previous` is empty |
| `chain_id` | TEXT | Stage chain identifier frozen with the selection; empty on an entry written before the column existed |
| `display_identifier` | TEXT | Qualified form of `identifier` (for example `owner/repo#9`); empty when `identifier` suffices, and on an entry written before the column existed |
| `issue_title` | TEXT | The tracker's title of the issue as read when the retry was scheduled; empty on an entry written before the column existed, and while the retry holds no read of its issue |
| `issue_url` | TEXT | The tracker's web link of the issue as read then, under the same rule as `issue_title` |
| `trigger` | TEXT | The `sortie_retries_total` label of the schedule that created the entry (`error`, `usage_limit`, `continuation`, `timer`, `stall`, or `ci_fix`); empty when that schedule records none, and `''` on an entry written before the column existed until the entry is next written |
| `configured_model` | TEXT | The `model` the dispatch's resolved settings block carried, frozen with the selection; empty when unset. Its migration fills an older entry from the `configured_model` of the newest agent run of the issue in `run_history` that shares the entry's `chain_id`; empty when no such run exists |
| `issue_type` | TEXT | The tracker's issue type as read when the retry was scheduled; empty on an entry written before the column existed, and while the retry holds no read of its issue |
| `issue_labels` | TEXT | The issue's labels as read then, as a JSON array of strings; `[]` for a read that found none, and `''` for no read, on an entry written before the column existed, and for a value that does not decode as an array of strings |
| `issue_state` | TEXT | The tracker state of the issue as read then, under the same rule as `issue_type` |
| `dispatched_at` | TEXT | RFC 3339 timestamp, in UTC with nanosecond precision, of the start of the first run of the pass the entry's `chain_id` names. Its migration fills an older entry with the earliest `started_at`, compared as an instant, among the issue's agent runs in `run_history` that share the entry's non-empty `chain_id`; an agent run is a row with a non-empty `agent_adapter`, so the synthetic `ci_failed` row is not one. Empty when `chain_id` is empty or no agent run shares it |
| `origin_trigger` | TEXT NOT NULL | What started the retry: the `sortie_retries_total` label of the schedule that created it (`error`, `usage_limit`, `continuation`, `stall`, or `ci_fix`), kept through every put-back by the retry timer, whose `timer` label stays in `trigger`. Empty for a retry no label started, such as a reaction continuation. Its migration fills an older entry from `trigger` when `trigger` is neither `timer` nor empty, and leaves the rest empty, because those entries never stored what started them |
| `usage_limit_notified` | INTEGER NOT NULL DEFAULT 0 | `1` when the run the retry follows ended in a provider usage-limit refusal, so the run the retry dispatches publishes no `session.started` event and, when refused again, no `session.failed` event; `0` otherwise, and on an entry written before the column existed |

Note: `timer_handle` is runtime-only and is not stored.

**`run_history`**: completed run attempts

| Column          | Type    | Notes                                     |
| --------------- | ------- | ----------------------------------------- |
| `id`            | INTEGER | Auto-increment primary key                |
| `issue_id`      | TEXT    | Tracker-internal issue ID                 |
| `identifier`    | TEXT    | Human-readable ticket key                 |
| `attempt`       | INTEGER | Attempt number at time of run             |
| `agent_adapter` | TEXT    | Agent adapter kind used                   |
| `workspace`     | TEXT    | Workspace path                            |
| `started_at`    | TEXT    | ISO-8601 timestamp                        |
| `completed_at`  | TEXT    | ISO-8601 timestamp                        |
| `status`          | TEXT    | Run disposition (`succeeded`, `failed`, `cancelled`, `ci_failed`, `needs_person`, `budget_stopped`, or `usage_limited`) |
| `error`           | TEXT    | Error message if failed, may be null      |
| `workflow_file`   | TEXT    | Workflow file name the run was driven by, may be null |
| `turns_completed`   | INTEGER | Agent turns the attempt completed |
| `display_identifier` | TEXT | Display identifier carried through dispatch, may be null |
| `review_metadata` | TEXT    | JSON-encoded self-review metadata, may be null |
| `rule_name`       | TEXT    | Dispatch rule that routed the run; empty when none matched |
| `template_id`     | TEXT    | Prompt template the rule selected |
| `input_tokens`      | INTEGER | Accumulated input tokens, 0 for pre-migration rows |
| `output_tokens`     | INTEGER | Accumulated output tokens, 0 for pre-migration rows |
| `total_tokens`      | INTEGER | Accumulated total tokens, 0 for pre-migration rows |
| `cache_read_tokens` | INTEGER | Accumulated cache-read tokens, 0 for pre-migration rows |
| `cache_write_tokens` | INTEGER | Accumulated cache-write tokens, 0 for pre-migration rows |
| `tokens_measured`   | INTEGER | `1` when the five token columns above carry a figure the coding agent's runtime reported; `0` when the run's spend is unknown and all five are zero |
| `unaccounted_turns` | INTEGER | Turns that issued a model request no token figure was proven to account for; a non-zero count makes `total_tokens` a lower bound |
| `configured_model`  | TEXT    | The `model` the attempt's resolved settings block carried; empty when unset, which leaves the runtime's own default, and on a row written before the column existed |
| `configured_effort` | TEXT    | The `effort` the attempt's resolved settings block carried, empty by the same rule |
| `reported_model`    | TEXT    | The model the runtime reported running, from the last token-usage event that named one; empty when it reported none. The `ci_failed` row leaves all three columns empty |
| `stage_previous`    | TEXT    | Source rule of the stage hop the run's selection carried into its `stage.previous`; empty when none. Reaction recovery restores it |
| `stage_previous_outcome` | TEXT | Outcome of that hop, `succeeded` or `no_change`; empty when `stage_previous` is empty |
| `chain_id` | TEXT | Stage chain identifier of the run; empty on a row written before the column existed, or on a `ci_failed` row whose pending reaction was recovered from such a row |
| `stage_target` | TEXT | Dispatch rule the run's hop decision targeted; empty when the run reached no hop decision |
| `stage_result` | TEXT | Result of that decision: `advanced`, `partial`, `failed`, or `ceiling`; empty exactly when `stage_target` is empty |
| `issue_title` | TEXT | The tracker's title of the issue as read when the run was dispatched. Reaction recovery restores it; empty on a row written before the column existed, or on a `ci_failed` row whose pending reaction was recovered from such a row |
| `issue_url` | TEXT | The tracker's web link of the issue as read then, restored and emptied under the same rule as `issue_title` |
| `dispatched_at` | TEXT | RFC 3339 timestamp, in UTC with nanosecond precision, of the start of the first run of the pass the row's `chain_id` names. Reaction recovery restores it. Its migration fills an older row under the rule of `retry_entries.dispatched_at`; empty when `chain_id` is empty or no agent run shares it |
| `trigger` | TEXT | What started the retry whose fire dispatched the run, as `retry_entries.origin_trigger` holds it: `error`, `usage_limit`, `continuation`, `stall`, or `ci_fix`. Empty for a dispatch from the poll tick and for a retry a reaction other than the CI fix started; the CI fix records `ci_fix`. `NULL` on a row written before the column existed, and on a run whose retry never stored what started it, which means "not recorded" |
| `reaction_kind` | TEXT | The reaction kind of the retry that dispatched the run; empty when none. `NULL` on a row written before the column existed |
| `session_id` | TEXT | The agent session the run used, the value the session-metadata write at the same exit takes; empty when the run started no session. `NULL` on a row written before the column existed, which makes it the column that tells a recorded origin from an unrecorded one |
| `ssh_host` | TEXT | The SSH worker the run executed on; empty for a local run. `NULL` on a row written before the column existed |
| `first_after_hop` | INTEGER | `1` on the first run dispatched after the stage hop `stage_previous` names, `0` on every other run, a later dispatch into that stage while the `stage_hops` row stands included. Its migration fills an older row with `1` when `stage_previous` and `chain_id` are non-empty and no row with a lower `id` shares its `issue_id`, `chain_id`, `rule_name`, and `stage_previous`, and with `0` otherwise; the rule reads neither `trigger` nor `reaction_kind` |
| `dispatch_id` | TEXT | The dispatch ID of the run, the key of its rows in `run_events`. The empty string on the `ci_failed` row, which no dispatch produced and which has no events. `NULL` on a row written before the column existed, which is not backfilled and has no events |
| `agent_blocked` | INTEGER | `1` when the run's agent ended it with a `blocked` soft stop (§21.1), `0` otherwise and on a row written before the column existed, which is not backfilled. A blocked soft stop records status `succeeded` with no error and no hop result, so this column and `declared_outcome` tell the run from a handoff |
| `estimated_cost_usd` | REAL | The run's estimated cost in USD, recorded when the row is written: the cost of its `run_model_usage` rows: their recorded costs summed, except that the rows one per-kind `token_rates` entry prices are priced together on their summed tokens, so the run's cost can differ from the sum of their recorded costs. `NULL` when `cost_status` is `unpriced` or `unmeasured` |
| `cost_status` | TEXT | `estimated`, `partial`, `unpriced`, or `unmeasured`, recorded with `estimated_cost_usd` under the rules of Section 13.5. Never `NULL` once a start after the column's migration has priced the rows written before it |
| `declared_outcome` | TEXT | The recognized `.sortie/status` token that ended the run (`blocked`, `needs-human-review`, or `no-change-needed`), as written (§21.1). The empty string when the run ended on no recognized token, and on a row written before the column existed, which is not backfilled |
| `agent_text` | TEXT | The statement that came with that token, normalized and masked as §21.1 "Publication" states, the text the `session.stopped` event carries as agent text. The empty string when `declared_outcome` is empty, when the agent wrote no statement, when normalization empties it, and on a row written before the column existed, which is not backfilled |
| `skills` | TEXT | JSON array of the run's skill calls in completion order, each an object with `skill` and, for a failed call, `tool_error` set to `true`, such as `[{"skill":"ce-plan"},{"skill":"ce-work","tool_error":true}]`; no entry limit, repeats included. The orchestrator marshals it at worker exit from the list it built from every `tool_result` event on arrival, and the store carries it as text, as it carries `review_metadata`. `[]` when the run called no skill, which includes a run whose agent reports none; the worker exit and a recorded CI result both write it. `NULL` only on a row written before the column existed, which is not backfilled, and when marshaling the list failed at worker exit |
| `usage_provenance` | TEXT | Every source whose readings settled the run's token figure, as `source:strategy` pairs from the usage component's vocabulary (sources `protocol`, `output`, and `journal`; strategies `live` and `records`), each pair once, sorted, and comma-joined, such as `journal:records,output:live`. The empty string when no source settled a figure: an unmeasured run, the `ci_failed` row, or a run with no agent turn. `NULL` on a row written before the column existed, which is not backfilled and reads as provenance unknown |
| `usage_completeness` | TEXT | `complete`, `lower_bound` when the run ran turns whose spend no source accounted, or `unmeasured`. `complete` on the `ci_failed` row. `NULL` on a row written before the column existed, under the same rule as `usage_provenance` |

The origin columns hold `NULL` for "not recorded" and the empty string for "recorded, none", so an upgrade never reports an earlier run as a local poll-tick dispatch. Every writer binds text; the `trigger` of a run dispatched by a retry whose row never stored what started it is the only value a writer leaves `NULL`. The `ci_failed` row records the empty string in each origin column, because it records a CI verdict and nothing started it. `sortie stats` and `/api/v1/stats` group runs by `trigger` when the database carries the `trigger` column, the empty string as `<none>` and `NULL` as `<unknown>`, and otherwise report no such breakdown. They also group runs by origin from `trigger`, `reaction_kind`, `status`, and `first_after_hop`, and the first rule that matches decides a run's group: a `NULL` `trigger` is `<unknown>`, a non-empty `trigger` names the group, a non-empty `reaction_kind` names it otherwise, a `ci_failed` row is `<none>`, a row with `first_after_hop` set is `stage_hop`, and every other run is `poll_tick`. A database without the `trigger` column gets neither grouping, and one without `first_after_hop` no grouping by origin. An index on `identifier` serves the identifier-filtered reads of the run history API, as the index on `issue_id` serves the reads by issue ID. An index on `display_identifier` serves the issue key of run history by issue, which matches the qualified form (for example `owner/repo#9`) a tracker records beside the bare identifier. An index on `completed_at` serves the read of a window's recorded cost, the aggregate over the rows whose completion time lies in a half-open range. The read binds its bounds in RFC 3339 UTC, the form every writer stores, so the text comparison orders instants correctly.

A stage chain is the set of runs that share a `chain_id`. Every dispatch freezes a chain identifier with its selection, beside the previous-stage pair. A dispatch that no hop reached starts a chain with a fresh random value that is never a dispatch or session identifier, so a public comment cannot expose session identity. A dispatch a hop reached inherits the chain of the hop record, and a retry or reaction continuation that keeps its rule carries the chain of the run it continues, across restarts. The dispatch also freezes the pass dispatch time (`dispatched_at`) with the chain identifier: a dispatch that carries no time records its own start, so the first run of a pass dates the pass, and every carrier keeps the value. A dispatch a hop reached inherits the hop record's time beside its chain, a retry or reaction continuation that keeps its rule carries the time of the run it continues, and a retry routed afresh to a rule no hop targets starts a pass dated by its own start. The run's row, the retry entry, and the hop record each persist the time, so it survives a restart. An empty carried chain, from a row written before chain identifiers were stored, is replaced by a fresh identifier at dispatch. After its hop decision, the exit writes the decision's target rule and result onto the row of the run that reached it. Rows written before these columns existed keep `''` in them and are not backfilled. `sortie stats` groups runs by `chain_id`, leaves out rows without a chain, and lists only chains in which a hop was due or made. It also counts the passes of each chain definition stage by stage, keyed by `chain_id` and `rule_name`, and reads `retry_entries` and `live_passes` for the passes held now.

The configured and reported model columns record what the operator asked for beside what ran, which differ under routing aliases and runtime-side fallbacks. A reported value never overwrites a configured one and nothing compares them. `sortie stats` groups a run that recorded no per-model spend by `reported_model`, or by `configured_model` for a run whose runtime reported none, and splits any other run across the models it spent tokens on, when the database carries the migration and otherwise reports no such breakdown. The token, agent-runtime, and worker-exit metrics carry the run's model as `model` (Section 13.7.3), the agent-runtime and worker-exit value being the run's reported model, else its configured one; no metric carries an effort label.

`status` is no longer a pure mapping from the worker's exit kind. A normal exit that reaches an otherwise-eligible handoff and is withheld by the handoff-evidence policy records `failed`, with its `error` value naming the evidence verdict as the cause, only when a verification read taken immediately before that recording does not find the issue in a terminal tracker state (§11.5, §14.2). When that read does find the issue terminal, the exit takes the terminal disposition instead and records `succeeded` with no error, exactly as an exit whose observation was terminal earlier already does. For an otherwise-normal exit on that handoff path, `succeeded` means the policy did not withhold it, or withheld it but the verification read then found the issue terminal. It does not assert that work was positively observed: an undeterminable verdict under `observed`, every normal exit under `off`, and a withheld verdict suppressed by that verification read may all still record `succeeded`.

Rows written before this rule and rows written under `tracker.handoff_evidence: off` retain the earlier exit-kind-only meaning and are not rewritten. Reports spanning the change therefore span three definitions of `succeeded`, not two, and must not present a changed success rate as proof that agent behavior changed.

No column stores the handoff-evidence verdict. The verdict is logged and counted, and a withheld run carries it in the existing `error` field. The stored text begins with `handoff withheld: ` and is the key the consecutive-absence count matches on; the run API words it as a plain sentence when it serves the run. In particular, this change adds no evidence field to `run_history` and requires no rewrite of historical rows. Because `succeeded` does not assert that work was observed, it cannot serve as the reset for a consecutive-absence sequence; that reset is recorded per issue in `handoff_absence_resets` below.

`budget_stopped` records a run a per-issue budget ceiling, the token ceiling or the money ceiling, stopped while it was in flight, distinct from `cancelled`, which still covers a run stopped by stall detection, a terminal tracker state, or shutdown. The distinction lets a consumer attribute a stop to the budget ceiling from the durable record alone, without parsing `error` text. A run that stall detection, a tracker state, or shutdown cancelled before the ceiling decided its stop, or that ended on its own before the ceiling's cancellation reached it, records the disposition it would record without the ceiling, never `budget_stopped`. A run whose self-review phase the ceiling cut short or kept from starting records `budget_stopped`. A money stop records the same status; the `error` field carries the money ceiling text, which names the issue's estimated cost against the ceiling.

A `cancelled` run records the reason the orchestrator cancelled it in `error`, so a consumer can tell the causes apart without a new status: `stall timeout exceeded` for stall detection, `issue reached terminal state: <state>` for a terminal tracker state, `issue left the active states: <state>` for a tracker state that is neither active nor terminal (the `: <state>` part is absent when the tracker reported none), and `service shutting down` for shutdown. That reason replaces any error the worker reported while the cancellation landed. The status values are unchanged.

`usage_limited` records a run that a provider usage limit refused before it produced output: the refusal carried a reset time after the exit, and the run's token figure is measured with zero output tokens, so the agent did nothing. Sortie decided the stop, so the status is neither `failed` nor `cancelled`. A refusal with no usable reset time, one after output, one on a run with no token figure, and one on a run whose spend is known only in vendor units record the status they would record without the refusal, normally `failed`. The per-issue session count (`agent.max_sessions`) and the exhausted-issue rebuild skip `usage_limited` rows.

A row with `tokens_measured = 0` always carries zero in all five token columns, `cache_write_tokens` included; the invariant runs in one direction only, because a measured run can legitimately report a zero spend. Every writer sets `tokens_measured` explicitly rather than relying on the column's default: the SQL default is `1`, but the Go zero value of the corresponding struct field is `false`, so a writer that omits the field records an unmeasured run rather than inheriting the default. The column default of `1` exists solely to answer for rows written before the column existed, whose provenance is not recoverable and are treated as measured.

The token columns mirror those on `session_metadata`. The per-issue token budget (`agent.max_tokens`) sums `total_tokens` less `cache_read_tokens` per row, each row floored at zero, so prompt-cache reads never count toward it; `sortie stats` reads `estimated_cost_usd` and `cost_status` and prices nothing. The in-flight lane that stops a run mid-turn adds the running session's own live in-memory uncached tokens to that sum, rather than waiting for the session's own row to land. The `session_metadata` write cadence changed to throttled-incremental during a running session (at most one write per issue per two seconds, on the orchestrator event loop) so the `cost_budget` tool can read in-flight spend before the session's `run_history` row exists. Some writes are exempt from that bound. The write that carries the usage figure reaching the configured token warning threshold lands immediately, so the tool's reading of the warning condition is never held back by the throttle. A change in the running session's unaccounted-turn count is likewise written immediately, into the session's existing row only. Every in-flight write carries the session's live estimate beside its token figures, so the `cost_budget` tool reads the issue's cost as the recorded costs of its `run_history` rows plus that estimate without reading any rate.

`tokens_measured` is not mirrored onto `session_metadata`, and does not need to be. The throttled in-flight write is gated on a usage-bearing event, so while a session runs its row comes into existence exactly when that session has reported a measurement the orchestrator records, which a session resolving `none` never has, and the presence of a row whose `dispatch_id` matches the running session's dispatch ID is itself the measurement signal the `cost_budget` tool reads. A running session with no matching row is what makes that tool's `used_tokens_complete` false, on the same footing as a completed run with `tokens_measured = 0`. A matched row whose `unaccounted_turns` is above zero also makes the tool's `used_tokens_complete` false. The unconditional write at session exit is outside that window: it stores an empty `dispatch_id` and runs before the run's `run_history` row is written.

The request count needs a column of its own for the reason the token measurement does not. The argument above rests on the in-flight write being gated on a usage-bearing event, so the row's existence is itself the token measurement signal. That gate fires for a kind that reports usage only when a turn ends too, and such a kind counts turns rather than requests, so the row's existence says nothing about whether the request-counting path ran. `api_requests_measured` is what says it. A row written before the column existed reads `0`, because its count was written by the unconditional rule that column exists to qualify; the row self-heals on that issue's next session.

The recorded cost is written once, in the same write as the row, priced under the rates in force at that moment (Section 13.5), and no writer changes it afterwards: a rate edit, a reload, a restart, an upgrade, and a newly fetched price table leave every recorded cost as it is. Every reader of a finished run, the dashboard, the JSON API, `GET /api/v1/stats`, `sortie stats`, and the `cost_budget` tool alike, reads these columns and never re-prices a row. Rows written before the cost columns existed are priced once, by the first start that applies their migration, under the rates in force at that start: each measured row with a non-zero input, output, cache-read, or cache-write column and no `run_model_usage` row becomes one `run_model_usage` row under its `reported_model`, else its `configured_model`, carrying the row's token columns, and a row that already has `run_model_usage` rows keeps them and is priced from them. The result is recorded with its price source, table version, and unrated classes like any other. An unmeasured row records `unmeasured` and no `run_model_usage` row. A measured row that spent tokens in no class records `unpriced` and no `run_model_usage` row. After that start those rows never change. A start that fails to price them logs the error and starts anyway; they read as `unpriced` until a later start prices them, under the rates in force at that start.

**`run_model_usage`**: each run's token counts and recorded cost per model

| Column | Type | Notes |
| --- | --- | --- |
| `run_id` | INTEGER | The `run_history.id` of the run (composite PK with `model`) |
| `model` | TEXT | The model the tokens were spent on, as the runtime named it; tokens that arrive counted per model are credited to the model that count names, and tokens that arrive without one are credited to the model current when they arrive: the event's model, else the model the run's events named last, else the `configured_model`, else the empty string. A run whose events name one model from its first usage records one row under that model |
| `input_tokens` | INTEGER | Input tokens spent on the model, cache counters included as in `run_history.input_tokens` |
| `output_tokens` | INTEGER | Output tokens spent on the model |
| `cache_read_tokens` | INTEGER | Cache-read tokens spent on the model |
| `cache_write_tokens` | INTEGER | Cache-write tokens spent on the model |
| `estimated_cost_usd` | REAL | The model's share of the run's cost; `NULL` when no rate prices any class the model spent |
| `cost_status` | TEXT | `estimated`, `partial` when a class the model spent has no rate or when the agent-reported amount that priced the share is a lower bound, or `unpriced` |
| `price_source` | TEXT | The layer that priced the share: `operator`, `fetched`, or `embedded` (Section 13.5), or `agent` when the US-dollar amount the agent runtime reported for the model priced a share no layer prices; empty when `cost_status` is `unpriced` |
| `price_table_version` | TEXT | The version of the rates that priced the share: the pinned upstream revision of the embedded table, or a content digest of the fetched table, the operator's table, or the operator's `token_rates`; for a share priced by a per-model `token_rates` entry that takes some of its rates from a table, the `token_rates` digest, a `+`, and that table's version; empty when `price_source` is empty or `agent` |
| `unrated_classes` | TEXT | The token classes whose missing rate left the share `partial` or `unpriced`, comma-joined; empty for a share priced by an agent-reported amount; see the reading below |

A measured run writes one row per model it spent tokens on, in the same transaction as its `run_history` row, and the rows sum to the run's token columns class by class; an unmeasured run writes none. The per-model counts follow the same resume baseline as the run's totals, so a resumed session's earlier spend is not counted again. `run_history.reported_model` keeps its meaning, the model the last token-usage event named, and is not derived from these rows.

`unrated_classes` reads as follows:

| Value | Meaning |
| --- | --- |
| `NULL` | Not recorded: the share's `cost_status` is `NULL`, or the share was priced before the column existed |
| empty string | Recorded, no class named: every class the model spent had a rate, or its spend was reported as a total without classes (`unpriced`) |
| `cache_read,cache_write` | Recorded: the unrated class names, comma-joined, in the order `input`, `output`, `cache_read`, `cache_write` |

A recorded value is written in the statement that writes the share's cost, and a share priced before the column existed keeps `NULL`, because a recorded price never changes.

**`run_reported_amounts`**: each run's amounts the agent runtime reported, in the runtime's own units

| Column | Type | Notes |
| --- | --- | --- |
| `run_id` | INTEGER | The `run_history.id` of the run (composite PK with `unit` and `model`) |
| `unit` | TEXT | The unit the runtime reported the amount in: `USD`, another ISO 4217 currency code, or a vendor unit's plural display name such as `premium requests`. Never converted |
| `model` | TEXT | The model the runtime's source attributes the amount to; the empty string when it attributes none |
| `amount` | REAL | The run-cumulative amount in `unit`, finite and not negative |
| `lower_bound` | INTEGER | `1` when the run may have spent more in `unit` than `amount` states, `0` otherwise |

A run writes one row per unit and model of its reported amounts, in the same transaction as its `run_history` row, whether or not the run's token figure is measured. A run whose runtime reported no amount writes none. A run whose `usage_provenance` is `NULL` has no row, and its amounts are not recorded rather than absent. An amount in a unit other than `USD` is recorded and read by no ceiling, cost total, or token total. A `USD` amount prices a per-model share only where no rate layer prices the model, and that share records the price source `agent`.

**`session_metadata`**: last known session metadata per issue (for observability and debug)

| Column              | Type    | Notes                             |
| ------------------- | ------- | --------------------------------- |
| `issue_id`          | TEXT PK | Tracker-internal issue ID         |
| `session_id`        | TEXT    | Last session ID                   |
| `agent_pid`         | TEXT    | Last known agent PID, may be null |
| `input_tokens`      | INTEGER | Accumulated input tokens          |
| `output_tokens`     | INTEGER | Accumulated output tokens         |
| `total_tokens`      | INTEGER | Accumulated total tokens          |
| `cache_read_tokens` | INTEGER | Accumulated cache-read tokens |
| `cache_write_tokens` | INTEGER | Accumulated cache-write tokens, 0 for pre-migration rows |
| `model_name`        | TEXT    | Last reported LLM model identifier |
| `api_request_count` | INTEGER | Measured model API requests; a current writer stores zero when `api_requests_measured` is `0` |
| `api_requests_measured` | INTEGER | `1` when `api_request_count` is a measurement; `0` when it is not. A row predating the column reads `0` here while keeping the count it already had, until that issue's next session write |
| `dispatch_id`       | TEXT    | The dispatch ID of the running session whose in-flight write last stored the row; empty after the session-exit write and on a row written before the column existed |
| `unaccounted_turns` | INTEGER | The running session's count of turns that spent tokens no figure was proven to account for, as of the last in-flight write; `0` after the session-exit write and on a row written before the column existed |
| `estimated_cost_usd` | REAL | The running session's live estimate in USD under the rates in force, as of the last in-flight write; `NULL` when it is unpriced, after the session-exit write, and on a row written before the column existed |
| `cost_status` | TEXT | `estimated`, `partial`, or `unpriced` for that estimate, under the rules of Section 13.5; empty after the session-exit write and on a row written before the column existed |
| `updated_at`        | TEXT    | ISO-8601 timestamp of last update |

**`aggregate_metrics`**: global token and runtime totals

| Column              | Type    | Notes                             |
| ------------------- | ------- | --------------------------------- |
| `key`               | TEXT PK | Metric key (e.g., `agent_totals`) |
| `input_tokens`      | INTEGER |                                   |
| `output_tokens`     | INTEGER |                                   |
| `total_tokens`      | INTEGER |                                   |
| `cache_read_tokens` | INTEGER | Cumulative cache-read tokens |
| `cache_write_tokens` | INTEGER | Cumulative cache-write tokens, 0 for pre-migration rows |
| `seconds_running`   | REAL    | Cumulative runtime seconds        |
| `unmeasured_sessions` | INTEGER | Cumulative count of ended sessions whose usage was never recorded. The migration sets an existing `agent_totals` row to the number of `run_history` rows with `tokens_measured = 0`; a `run_history` row written before `tokens_measured` existed reads `1` there and is not counted |
| `updated_at`        | TEXT    | ISO-8601 timestamp                |

`unmeasured_sessions` names what the five token columns above already exclude: a session whose worker entered an agent turn and never got a usage figure, whatever the reason. It is incremented once per such session at the same write that updates the token columns, so a restart restores both together and neither can drift ahead of the other.

**`reaction_fingerprints`**: cross-restart reaction deduplication

| Column        | Type    | Notes                                                         |
| ------------- | ------- | ------------------------------------------------------------- |
| `issue_id`    | TEXT    | Tracker-internal issue ID (composite PK with `kind`)          |
| `kind`        | TEXT    | Reaction kind; one row per issue per kind                     |
| `fingerprint` | TEXT    | The observed value this kind deduplicates on                  |
| `dispatched`  | INTEGER | `1` when the action for this fingerprint has been performed   |
| `updated_at`  | TEXT    | ISO-8601 timestamp                                            |

Primary key: `(issue_id, kind)`. Every reaction kind that deduplicates across restarts owns a row here under its own `kind` discriminator, so one issue may hold several rows at once without the kinds interfering. Upserts reset `dispatched` to `0` when the fingerprint value changes, which re-arms the latch for exactly one further action.

What the fingerprint holds is defined by the owning kind, not by this table: a digest of the comment set last acted on, the git ref last checked, a journal high-water mark, or a merge commit identifier, whichever value identifies "the same observation" for that kind. The column is opaque text and this schema attaches no meaning to it beyond equality.

The `ci` row holds the pull request head this process last evaluated, not a ref captured once at worker exit; `updated_at` is refreshed on every evaluation, including one that finds the head unchanged, so it names the last evaluation rather than a first-seen timestamp for this kind. A differing stored head is this kind's head-change detection, and the row is never deleted: neither a passing result nor an escalation removes it, because it is the durable half of the feedback epoch record (§11A.9).

`dispatched` likewise means "the action this kind performs has been performed for this fingerprint", and the row's lifecycle after that point is the owning kind's to define. Most kinds treat the row as spent. The merge-completion kind (§11G.4) instead retains a dispatched row rather than deleting it, because deleting it would let the next poll observe the same merge as new.

The internal `merge-completion-missing-sha` observation kind reuses this table without changing the normal `merge-completion` fingerprint. Its `fingerprint` is normalized `owner/repo#number`, `updated_at` is the first time that PR identity was reported merged without a commit identifier, and `dispatched` means the bounded-wait escalation was successfully delivered. Its observation upsert has stricter timestamp semantics than an ordinary fingerprint upsert: the same identity preserves `updated_at`, while a different identity replaces it and resets `dispatched`. Marking this observation dispatched happens only after the configured tracker write succeeds, only if the row still carries the same PR identity, and also preserves `updated_at`, so a restart cannot reset or obscure the thirty-minute grace-period origin. A failed tracker write leaves the expired observation undispatched; the current pending entry stays pending under its escalation, while the first pass after a restart or a later fresh entry can retry only that delivery.

Cleanup is owned by an active merge-completion pass. Such a pass deletes the observation when it sees a different pull request identity, a missing pull request, an issue missing from the tracker response, an issue outside the handoff state, or a real commit identifier whose normal merge latch has completed. Generic terminal-state release deliberately leaves every `reaction_fingerprints` row intact. Consequently, after generic terminal-state release has removed the last pending entry, this internal row may remain indefinitely. The residue is inert: the same delivered identity stops, a different identity resets it, and a later fresh entry with a real identifier clears it after the normal latch completes. Because `updated_at` is frozen at first observation, any future retention job MUST handle this kind explicitly; it MUST NOT infer safe expiry from `updated_at` alone or it would re-arm a one-shot escalation.

**`reaction_escalations`**: the escalation of each reaction kind, standing or past

| Column               | Type    | Notes                                                                                                       |
| -------------------- | ------- | ----------------------------------------------------------------------------------------------------------- |
| `issue_id`           | TEXT    | Tracker-internal issue ID (composite PK with `kind`)                                                        |
| `kind`               | TEXT    | Reaction kind that escalated; one row per issue per kind                                                    |
| `subject`            | TEXT    | What the latest escalation judged: the pull request head, the comment set fingerprint, the merge fingerprint, the merge commit, or the pull request identity; empty when it has none. Defaults to empty |
| `escalated_at`       | TEXT    | RFC 3339 with nanoseconds; NULL once the escalation ended                                                   |
| `escalation_trigger` | TEXT    | `budget_exhausted`, `triage`, `refused`, or `missing_merge_commit`                                          |
| `attempts`           | INTEGER | The kind's attempt counter at the escalation. Defaults to `0`                                               |
| `pr_number`          | INTEGER | Pull request number                                                                                         |
| `pr_owner`           | TEXT    | Pull request owner                                                                                          |
| `pr_repo`            | TEXT    | Pull request repository                                                                                     |
| `pr_branch`          | TEXT    | Pull request head branch; empty for a kind that keeps none. Defaults to empty                               |

Primary key: `(issue_id, kind)`. Every kind that escalates writes its row when an escalation begins. Ending an escalation sets `escalated_at` to NULL and keeps the row, because its `subject` latches the escalation action to once per subject: an escalation whose subject equals the stored one records its time and trigger and applies no action. The release of the issue (a terminal state, or a stage hop) deletes every row of the issue.

A row with a non-NULL `escalated_at` is a standing escalation, and the startup restore turns it back into a pending entry. The pull request columns let the restore build the entry without the workspace; it reads the issue's newest successful run for the rest and ends a row whose kind is not configured or whose issue has no such run. The time is stored at nanosecond precision so the identifier and start time of the attention item built from the escalation survive a restart. The table starts empty on upgrade, so an escalation standing at the upgrade's restart is not restored, and the next escalation of its kind records one.

**`reaction_activity`**: what each pull request reaction did in each pass of an issue

| Column        | Type                       | Notes                                                                                                             |
| ------------- | -------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `id`          | INTEGER PRIMARY KEY AUTOINCREMENT | Row identifier                                                                                             |
| `issue_id`    | TEXT NOT NULL              | Tracker-internal issue ID                                                                                         |
| `chain_id`    | TEXT NOT NULL              | The stage chain identifier of the pass, as `run_history.chain_id` holds it                                        |
| `kind`        | TEXT NOT NULL              | Reaction kind: `ci`, `review`, `bot-review`, `merge`, `merge-conflict`, `label-review`, `label-fix`, or `merge-completion` |
| `activity`    | TEXT NOT NULL              | `watch`, `continuation`, `action`, or `escalation`                                                                |
| `recorded_at` | TEXT NOT NULL              | RFC 3339 with nanoseconds, in UTC; the time of the iteration that acted                                           |

An index on (`issue_id`, `chain_id`) serves the read of a pass. A unique partial index on (`issue_id`, `chain_id`, `kind`) where `activity` is `watch` keeps one `watch` row per issue, pass, and kind, so a write of a second one is ignored; every other activity appends a row.

The table is an append-only record. Nothing updates, deletes, or prunes a row, as nothing prunes `run_history`, and the release of an issue leaves its rows. A pass adds one `watch` row for each kind its pull request seeds and one row for each act. The table starts empty on upgrade and no backfill fills it.

The orchestrator writes it on its event loop, in the iteration that acts, and a failed write does not stop the act (Section 13.1). Each activity has its writer:

| `activity` | Written when |
| --- | --- |
| `watch` | A worker exit seeds a pending entry of the kind; the row belongs to the pass of the exiting run |
| `continuation` | A reconcile pass schedules a run of the kind, a CI fix, a review or bot review fix, a merge conflict fix, or a label command run; the row belongs to the pass of the entry that scheduled it. A retry of a failed reaction run records nothing |
| `action` | A reaction first applies a triage verdict, once; Sortie makes a merge call for `merge`; Sortie makes a tracker transition for `merge-completion`, whatever the forge or the tracker answers |
| `escalation` | An escalation begins on an entry that has none standing, a latched subject included; an escalation that stands records nothing more |

Startup recovery and the escalation restore record nothing, so a kind first watched after a restart has no `watch` row until it acts.

The run-history reader behind `pr_loop` (Section 13.7.2) reads the rows of the passes a request serves and sums them per kind. It takes the `applied_at` that `schema_migrations` holds for the migration that creates this table as the start of the recorded passes: a pass dispatched before it, a pass without a chain identifier, and a pass without a dispatch time read as unrecorded whatever rows exist. The reader reads `applied_at` before it opens its row query, because the store has one connection.

**`reaction_triage_outcomes`**: the triage answer each pull request reaction applied for each subject

| Column        | Type                       | Notes                                                                                                             |
| ------------- | -------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `id`          | INTEGER PRIMARY KEY AUTOINCREMENT | Row identifier; the greatest `id` of a subject is its newest answer                                        |
| `issue_id`    | TEXT NOT NULL              | Tracker-internal issue ID                                                                                         |
| `kind`        | TEXT NOT NULL              | Reaction kind, as `reaction_activity.kind` holds it                                                               |
| `fingerprint` | TEXT NOT NULL              | The subject the triage run was started for; for a comment kind, the digest of the comment set                     |
| `disposition` | TEXT NOT NULL              | `handled`, `dispatch-agent`, or `escalate`, as applied                                                            |
| `fallback`    | TEXT NOT NULL              | Why `dispatch-agent` was applied by default; empty when the command answered                                     |
| `elapsed_ms`  | INTEGER NOT NULL           | How long the command ran, in whole milliseconds                                                                   |
| `chain_id`    | TEXT NOT NULL              | The stage chain identifier of the entry; empty when it has none                                                   |
| `recorded_at` | TEXT NOT NULL              | RFC 3339 with nanoseconds, in UTC                                                                                 |

An index on (`issue_id`, `kind`, `fingerprint`) serves the read of the newest answer for a subject.

The table is an append-only record. Nothing updates, deletes, or prunes a row, and the table starts empty on upgrade with no backfill, so an answer applied before the upgrade has no row. The fallback and the duration let a later reader tell a script's `dispatch-agent` answer from a failure fallback, and `chain_id` joins a row to the runs of its pass.

The triage gate writes one row the first time a reaction applies a finished outcome, for every disposition and every kind that offers the `triage` block; a replay of the retained outcome writes none. A failed write logs a warning and does not stop the pass (Section 13.1). The bot review check reads the newest row for the issue, the kind, and the fingerprint of the comment set it found, and compares the recorded disposition rather than the gate's verdict, because the gate replays a retained `escalate` as `handled`. The answer is never replayed from the table: a subject whose dispatched mark never landed is triaged again.

**`reaction_handoffs`**: review comments each issue's runs were given

| Column       | Type | Notes                                                                |
| ------------ | ---- | -------------------------------------------------------------------- |
| `issue_id`   | TEXT | Tracker-internal issue ID (composite PK with `kind` and `comment_id`) |
| `kind`       | TEXT | `review` or `bot-review`                                             |
| `comment_id` | TEXT | SCM-platform review comment ID                                       |

Primary key: `(issue_id, kind, comment_id)`. One row per comment a run's first prompt presented under the kind's prompt variable (`review_comments` or `bot_review_comments`) when the run exited normally. The orchestrator's worker-exit handler is the only writer: it inserts the absent IDs in one transaction and ignores stored ones. Nothing deletes a row: the set outlives watch windows, escalations, the terminal release, and restarts, so a comment an agent already received is never handed over as new a second time. The orchestrator loads a key's rows on first use and keeps them as a cache, so a failed load defers the review pass instead of dispatching an empty turn. The table starts empty on upgrade, so a comment given before the upgrade counts as new once. The runtime-only reported set of `bot-review` never reaches this table.

**`handoff_absence_resets`**: end of a consecutive handoff-absence sequence

| Column         | Type    | Notes                                                              |
| -------------- | ------- | ------------------------------------------------------------------ |
| `issue_id`     | TEXT PK | Tracker-internal issue ID                                          |
| `reset_run_id` | INTEGER | Highest `run_history.id` for the issue at the reset; `0` when none |
| `updated_at`   | TEXT    | ISO-8601 timestamp                                                 |

One row per issue, written only when a run's evidence verdict is `work observed`. The consecutive-absence count (§14.2) is the number of the issue's absence-marked `run_history` rows with an `id` above `reset_run_id`, so the count returns to zero the moment a work-observed run is recorded and cannot be restored by a later handoff-write failure. A run outcome that carries no verdict never writes here on its own account, which is what keeps a blocked soft stop, a reaction run, an undeterminable verdict, or a run under `handoff_evidence: off` from resetting a sequence that run says nothing about.

The reset point is read from `run_history` inside the write, so a work-observed run whose own history row could not be persisted still clears the absences recorded before it. This table holds no verdict history: it is a per-issue position that is overwritten, and deleting a row only restores the issue's full recorded absence sequence. A work-observed run is no longer the table's only writer: releasing a parked issue, whatever reason the park carries, also writes a reset here, so the loop does not immediately re-derive the exhausted count and park the issue again on the tick that released it.

**`parked_issues`**: issues held out of primary dispatch until a human acts

| Column          | Type    | Notes                                                              |
| --------------- | ------- | ------------------------------------------------------------------ |
| `issue_id`      | TEXT PK | Tracker-internal issue ID                                          |
| `identifier`    | TEXT    | Human-readable ticket key                                          |
| `display_id`    | TEXT    | Qualified display form; empty when the tracker needs none          |
| `reason`        | TEXT    | `agent_blocked` or `handoff_absence`                                |
| `parked_state`  | TEXT    | Tracker state observed when the park was recorded; empty when unobserved |
| `label`         | TEXT    | Parking label the orchestrator applied                             |
| `label_applied` | INTEGER | `1` once the orchestrator has confirmed the label reached the tracker |
| `parked_at`     | TEXT    | ISO-8601 timestamp                                                  |
| `issue_title`   | TEXT    | The tracker's title of the issue as read when the park was recorded, or at the first read after a park recorded or restored without one; `''` on a row written before the column existed until the row is next written |
| `issue_url`     | TEXT    | The tracker's web link of the issue as read then, under the same rule as `issue_title` |
| `rule_name` | TEXT | Dispatch rule of the run the park follows; empty when none matched, and while the park records no run. Its migration fills an older row from the `rule_name` of the park's run: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`; the synthetic `ci_failed` row is not an agent run. Empty when no row qualifies or a time does not parse |
| `agent_kind` | TEXT | Agent adapter of the run the park follows; empty while the park records no run, and a park records a run exactly when it is non-empty. Its migration fills an older row from the `agent_adapter` of the park's run: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`; the synthetic `ci_failed` row is not an agent run. Empty when no row qualifies or a time does not parse |
| `configured_model` | TEXT | The `model` the run's resolved settings block carried; empty when unset. Its migration fills an older row from the `configured_model` of the park's run: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`; the synthetic `ci_failed` row is not an agent run. Empty when no row qualifies or a time does not parse |
| `stage_previous` | TEXT | Source rule of the stage hop the run's selection carried into its `stage.previous`; empty when none. Its migration fills an older row from the `stage_previous` of the park's run: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`; the synthetic `ci_failed` row is not an agent run. Empty when no row qualifies or a time does not parse |
| `stage_previous_outcome` | TEXT | Outcome of that hop, `succeeded` or `no_change`; empty when `stage_previous` is empty. Its migration fills an older row from the `stage_previous_outcome` of the park's run: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`; the synthetic `ci_failed` row is not an agent run. Empty when no row qualifies or a time does not parse |
| `chain_id` | TEXT | Stage chain identifier of the run the park follows. Its migration fills an older row from the `chain_id` of the park's run: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`; the synthetic `ci_failed` row is not an agent run. Empty when no row qualifies or a time does not parse |
| `dispatched_at` | TEXT | RFC 3339 timestamp, in UTC with nanosecond precision, of the start of the first run of the pass the run belongs to, never the park time. Its migration fills an older row from the `dispatched_at` of the park's run: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`; the synthetic `ci_failed` row is not an agent run. Empty when no row qualifies or a time does not parse |
| `issue_type` | TEXT | The tracker's issue type as read for the park; empty while the park holds no read of it, and on a row written before the column existed, which no table can fill. A park holding a title or link and no labels takes the type of the next poll tick's candidate |
| `issue_labels` | TEXT | The issue's labels as read for the park, less the park's own parking label, as a JSON array of strings; `[]` for a read that found none, and `''` for no read, on a row written before the column existed, and for a value that does not decode as an array of strings |
| `session_id` | TEXT | The agent session the park hands to the first run after a release, taken from the run the park follows only when that run had no reaction kind and either succeeded or failed with a withheld handoff; empty otherwise, and while the park records no run. Its migration fills an older row from the `session_id` of the park's run under the same rule: the issue's `run_history` row with the greatest `id` among rows whose `agent_adapter` is non-empty and whose `started_at`, compared as an instant, is at or before the row's `parked_at`. Empty when no row qualifies, the run does not meet the rule, or a time does not parse |
| `template_id` | TEXT | Prompt template of that run, kept under the same rule as `session_id` and filled by the same migration step; empty for the workflow body template and when the run does not meet the rule |
| `statement` | TEXT | The stop statement of the run the park follows, as that run's `agent_text` stores it, kept whatever the run's reaction kind or status; empty when the run declared none or the park records no run. Its migration fills an older row from the `agent_text` of the park's run, selected as above |
| `released_at` | TEXT | RFC 3339 timestamp in UTC, whole seconds, of the gesture that released the park; empty while the park holds |
| `release_gesture` | TEXT | `label_removed` or `state_changed`; empty while the park holds |

One row per parked issue, holding current state rather than history. The row moves through stages. While the park holds, `released_at` is empty. A release by a person's gesture, a label removal or a state change, keeps the row and sets `released_at` and `release_gesture`: the row now records a pending release that the issue's next primary dispatch from the poll tick consumes by deleting the row. A release by a worker exit that observed work deletes the row at once, since it is not a person's answer. A new park of the same issue replaces the row entirely, which clears the release columns. Startup loads a row with `released_at` set as a pending release and every other row as a park, so a restart neither undoes a release nor drops its hand-over. `parked_state` is nullable in meaning though not in type: an empty string means no tracker state was observed at park time, which only a park taken from the retry lane produces, since that lane parks ahead of its own tracker fetch by design (§14.2). `label_applied` has exactly one writer: the orchestrator's release-rule observation of the label on a later fetch of the issue, never the outcome of the label write itself. A park the poll tick records reads its run from `run_history` and writes it with the row; a failed read writes the row with the run columns empty, and they stay empty until the park is released.

**`budget_hold_notices`**: cross-restart dedup for the `budget.held` notice delivered for an issue held by a per-issue budget ceiling

| Column       | Type    | Notes                                                       |
| ------------ | ------- | ------------------------------------------------------------ |
| `issue_id`   | TEXT PK | Tracker-internal issue ID                                    |
| `reason`     | TEXT    | `session_budget`, `token_budget`, or `money_budget`           |
| `noticed_at` | TEXT    | ISO-8601 timestamp the posted notice reported                 |

One row per issue whose current budget hold has been announced to at least one destination. A row records that the notice had a destination: a hold whose `budget.held` event reached none writes no row, and the issue is routed again on a later pass, so a destination subscribed afterward still receives the notice. The row is written before the delivery and survives a failed delivery. The table holds current state rather than history: the row is deleted when the hold clears, on the same evidence rule that prunes the in-memory announcement latch, or when every budget is disabled. An issue held by a ceiling and then closed, or otherwise never observed as a candidate again, never has its row deleted, so the row persists for the rest of the deployment's lifetime. `noticed_at` exists so an operator reading the database can align a row with the notice, which is the comment on the issue when the `tracker_comment` destination received it; no runtime decision reads it.

**`usage_limit_holds`**: the new sessions a provider usage limit stops until it resets

| Column          | Type    | Notes                                                                                   |
| --------------- | ------- | --------------------------------------------------------------------------------------- |
| `agent_kind`    | TEXT    | Agent kind the hold stops; part of the primary key                                      |
| `model_only`    | INTEGER | `1` when the hold stops that kind on one configured model alone, `0` for every model; part of the primary key |
| `model`         | TEXT    | The configured model of a `model_only` hold; empty for a kind-wide hold and for the runtime's default model; part of the primary key |
| `resets_at_ms`  | INTEGER | Unix epoch milliseconds the usage limit resets, already capped at 7 days from the exit that recorded it |
| `held_since_ms` | INTEGER | Unix epoch milliseconds the hold began; a later reset under an active hold keeps it |

The primary key is `(agent_kind, model_only, model)`, so a kind-wide hold and a hold on one model of the same kind are separate rows. A row is written when a refusal records or extends a hold and deleted when a run that started after `held_since_ms` exits normally on the kind, and on the model for a `model_only` row. The table is loaded before the first tick. A row whose `resets_at_ms` has passed loads and has no effect.

**`stage_hops`**: current stage hop record per issue

| Column               | Type    | Notes                                                              |
| -------------------- | ------- | ------------------------------------------------------------------ |
| `issue_id`           | TEXT PK | Tracker-internal issue ID                                          |
| `identifier`         | TEXT    | Human-readable ticket key                                          |
| `hop_count`          | INTEGER | Consecutive automatic hops since the last reset, the latest included |
| `source_rule`        | TEXT    | Dispatch rule whose run made the latest hop                        |
| `target_rule`        | TEXT    | Dispatch rule the latest hop advanced the issue to                 |
| `target_label`       | TEXT    | Stage label the latest hop added                                   |
| `previous_outcome`   | TEXT    | `succeeded`, or `no_change` when the source run declared that no change was needed |
| `source_dispatch_id` | TEXT    | Dispatch ID of the run that made the latest hop                    |
| `target_observed`    | INTEGER | `1` once a read has shown `target_label` on the issue; `0` while the hold is open |
| `hopped_at`          | TEXT    | RFC 3339 timestamp of the latest hop                               |
| `chain_id`           | TEXT    | Stage chain identifier of the run whose exit made the latest hop; empty on a row written before the column existed |
| `display_identifier` | TEXT    | Qualified form of `identifier` at the latest hop; empty when `identifier` suffices. Its migration fills an older row from the newest `run_history` row of the issue that holds a title or link; empty when none does |
| `issue_title`        | TEXT    | The tracker's title of the issue as read at the latest hop, filled by its migration from the same `run_history` row as `display_identifier`; empty when the hop was recorded without a read |
| `issue_url`          | TEXT    | The tracker's web link of the issue as read then, under the same rule as `issue_title` |
| `dispatched_at`      | TEXT    | RFC 3339 timestamp, in UTC with nanosecond precision, of the start of the first run of the pass the hop's `chain_id` names. Its migration fills an older row under the rule of `retry_entries.dispatched_at`; empty when `chain_id` is empty or no agent run shares it |
| `run_dispatched`     | INTEGER | `1` once a run of the issue was dispatched after the hop, which ends the issue's listing between stages; `0` while none was. Its migration sets `1` on an older row when an agent run of the issue started at or after `hopped_at`, and `0` otherwise |
| `issue_type` | TEXT | The tracker's issue type as the hopping run read it; empty when the hop was recorded without a read, and on a row written before the column existed |
| `issue_labels` | TEXT | The issue's labels as the hop leaves them: the run's dispatch read without the stage labels the hop removes, followed by the stage label it adds, as a JSON array of strings; `[]` for a read that found none, and `''` for no read, on a row written before the column existed, and for a value that does not decode as an array of strings |
| `issue_state` | TEXT | The tracker state of the issue as the hopping run read it, under the same rule as `issue_type` |

One row per issue, holding current state since the last reset rather than history: the row is deleted when the count resets (a handoff write succeeds, the issue turns terminal or non-active, or the issue is parked or released from a park). A reading of no configured state deletes the row only when it repeats on the next reading of that issue, because a healthy label edit can show an issue with no state label for a moment; the flag that tracks the first reading is runtime-only and the table gains no column for it, so after a restart the next no-state reading counts as the first. The row serves the per-issue ceiling on consecutive hops, the hold that keeps a lagging candidate listing from sending the issue back to the stage it left, the `stage.previous` pair of the target's dispatch, whether that dispatch is the first run after the hop, and the issue's listing between stages, which lasts until a dispatch of the issue sets `run_dispatched`. Setting the flag updates that column alone and does nothing for an issue without a row. Recording a hop upserts the row and deletes the issue's `retry_entries` row in one transaction, so a restart cannot restore the retry of the stage the hop left. Recording a second hop with the `source_dispatch_id` already stored keeps the stored `hop_count`, so one source run is counted once. A row written before these columns existed holds no issue type, labels, or state, and they stay empty until a hop replaces the row or the row is deleted. Startup loads every row before the event loop starts, after `parked_issues`.

**`issue_outcomes`**: how each issue's latest work ended, as the orchestrator observed it

| Column                | Type                 | Notes                                                                 |
| --------------------- | -------------------- | --------------------------------------------------------------------- |
| `issue_id`            | TEXT PK              | Tracker-internal issue ID                                             |
| `kind`                | TEXT NOT NULL        | `handed_off`, `merged`, or `terminal`                                 |
| `observed_at`         | TEXT NOT NULL        | RFC 3339 timestamp in UTC at second precision, the format of `run_history.completed_at`; when the orchestrator made or observed the ending |
| `state`               | TEXT NOT NULL        | `handed_off`: the state the handoff wrote. `terminal`: the terminal state read, or written by a handoff. `merged`: empty |
| `merge_reaction_kind` | TEXT NOT NULL        | `merged`: the reaction kind that observed the merge, `merge` or `merge-completion`. Empty otherwise |
| `pr_number`           | INTEGER NOT NULL     | `merged`: the pull request number. `0` otherwise                       |
| `pr_repository`       | TEXT NOT NULL        | `merged`: the pull request's `owner/repo`. Empty otherwise             |

One row per issue, holding current state rather than history. The orchestrator writes the row on its event loop at the observation sites and applies the record rule in one statement, so a stored merge survives a later terminal read; the poll tick's dispatch deletes the row when it picks the issue up again (Section 4.1.8). The row is its own table, not a column of `run_history`, because an ending can be observed while a run is in flight, before that run's row exists, and a handoff or a merge belongs to the issue rather than to one run. The table has no index beyond its primary key and no backfill: an issue that ended before the table existed has no row and reports no outcome. `GET /api/v1/runs` selects runs by this table's `observed_at` through the ending bounds (Section 13.7.2).

**`attention_acknowledgements`**: attention items a person has seen

| Column            | Type             | Notes                                                                                   |
| ----------------- | ---------------- | --------------------------------------------------------------------------------------- |
| `item_id`         | TEXT PRIMARY KEY | The `id` of the attention item (§13.7.2)                                                |
| `acknowledged_at` | TEXT NOT NULL    | RFC 3339 with nanoseconds, in UTC; when the item was first acknowledged                 |

One row per acknowledged item, holding current state rather than history. The orchestrator writes the row on its event loop when it records an acknowledgement, inserting the row or replacing the row with the same `item_id`; it saves the row before it changes the in-memory map, so every acknowledgement a client was told about survives a restart. A repeat request writes nothing, which keeps the first time. The poll tick deletes the rows of every item the state no longer derives, in one statement, and only on a tick whose candidate fetch succeeded (Section 8.1). The startup load reads every row before the event loop starts, and a row with an empty `item_id` or a time that does not parse is skipped. The table references no other table, because no budget hold survives a restart: the first tick that reads the candidate issues rebuilds it.

**`live_passes`**: the passes the orchestrator holds outside the retry queue

| Column           | Type          | Notes                                                                                                   |
| ---------------- | ------------- | ------------------------------------------------------------------------------------------------------- |
| `issue_id`       | TEXT NOT NULL | Tracker-internal issue ID                                                                               |
| `kind`           | TEXT NOT NULL | `running`: a running stage session. `budget`: a budget hold. The primary key is (`issue_id`, `kind`)    |
| `chain_id`       | TEXT NOT NULL | The pass's stage chain identifier, as `run_history.chain_id` holds it                                   |
| `rule_name`      | TEXT NOT NULL | The dispatch rule of the stage the pass is in                                                           |
| `stage_previous` | TEXT NOT NULL | The rule whose hop reached the pass; empty when none did                                                |

One row per issue and kind, holding current state rather than history, so an issue holds at most one row of each kind. A pass that waits in the retry queue is already stored in `retry_entries`, so the table never repeats it, and the stats report reads that table beside this one. The table exists because nothing else durable records a running session or a budget hold, which `sortie stats` and `GET /api/v1/stats` count in the stage funnels without contacting the orchestrator. The orchestrator writes it on its event loop. A dispatch that carries a rule and a chain identifier and no reaction kind upserts the `running` row, at the poll tick and in the retry timer, after the dispatched entry carries its rule, lineage, and reaction kind; the retry timer writes it before it deletes the retry's `retry_entries` row, so no instant shows the pass in neither table. Every worker exit deletes the `running` row, after the exit's `run_history` row, its hop result, and any retry row it saves. The retry timer upserts the `budget` row when a budget refuses the retry, before it deletes the retry's row. The rebuild of budget holds on each poll tick upserts the row of a hold that starts or changes its rule, chain, or previous rule, and deletes the row of a hold that ends. Startup deletes every row before the orchestrator runs and restores nothing, so a crash leaves its `running` rows in place until the next start. A failed write is logged and changes nothing else. Only the stats report reads the table, and it reads the table together with `retry_entries` in one statement.

**`run_events`**: the retained agent events of finished runs

| Column        | Type             | Notes                                                                                                  |
| ------------- | ---------------- | ------------------------------------------------------------------------------------------------------ |
| `dispatch_id` | TEXT NOT NULL    | The run's dispatch ID, as `run_history.dispatch_id` holds it                                            |
| `seq`         | INTEGER NOT NULL | The event's number within the run (§13.9); the primary key is (`dispatch_id`, `seq`)                    |
| `at`          | TEXT NOT NULL    | RFC 3339 timestamp with nanoseconds, in UTC, of the event                                              |
| `event`       | TEXT NOT NULL    | The normalized event type                                                                              |
| `payload`     | TEXT NOT NULL    | The event's retained members other than the three above, as a JSON object: the masked and cut `message`, and the type's own members (§13.9). `text` is never stored |
| `completed_at`| TEXT NOT NULL    | The run's `run_history.completed_at`, repeated on each row so pruning reads no other table            |
| `size`        | INTEGER NOT NULL | The byte length of `at`, `event`, and `payload` together, the measure of the 2 GiB bound                |

The rows of a run are written once, at worker exit, in the transaction that appends the run's `run_history` row, so a run's events are never stored without its history row; a running run's events live only in memory. When writing the events fails, the history row is kept and Sortie retries the events from memory, each retry in a transaction of its own (§13.9). Each write first deletes the rows of at most 4 runs whose `completed_at` is more than 365 days older than the run being written, then deletes the rows of whole runs, oldest `completed_at` first, until the sum of `size` is at most 2 GiB; size pruning never deletes the run being written. Pruning runs only on these writes, so 365 days is the least a finished run's events stay readable, not an expiry, and expired events can outlast it until later writes reach them. A run with no rows is not retained (§13.9): a pruned run, a run whose events were not written, a run lost in a crash, a run whose history row predates the `dispatch_id` column, the `ci_failed` row, and a run that retained no event. An index on (`completed_at`, `dispatch_id`, `size`) serves pruning: the walk that picks the runs to delete reads the index alone, never `payload`. Reads go through the store's single connection on the request goroutine, as history reads do.

**`run_events_total`**: the sum of `run_events.size`

| Column  | Type                            | Notes                                                                 |
| ------- | ------------------------------- | --------------------------------------------------------------------- |
| `id`    | INTEGER PRIMARY KEY, CHECK = 1  | The table holds one row                                               |
| `bytes` | INTEGER NOT NULL                | The sum of `size` over `run_events`                                   |

Every write of `run_events` updates `bytes` in its own transaction, so the figure the 2 GiB bound reads is exact and a write never sums the table, a cost that grows with the stored volume. The migration creates the row at 0 beside the empty table.

### 19.3 Migration Strategy

- Migrations are numbered sequentially and applied in order at startup.
- Applied migrations are tracked in a `schema_migrations` table. The `applied_at` of the migration that creates `reaction_activity` marks the start of the passes whose reaction activity is recorded (Section 19.2).
- Migrations are additive (new columns/tables) where possible; destructive migrations require explicit versioning.
- The migration that adds the recorded cost leaves the pricing of the rows written before it to the first start that applies it, which prices them once under the rates in force at that start and never again (Section 19.2).
- The migration that adds provenance, completeness, and reported amounts adds nullable columns with no default and backfills nothing, so a row written before it reads as provenance unknown and has no amount row (Section 19.2).
