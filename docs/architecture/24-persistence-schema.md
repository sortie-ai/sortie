## 19. Persistence Schema

### 19.1 Overview

Sortie uses an embedded SQLite database for durable state. The database file path defaults to
`.sortie.db` in the same directory as `WORKFLOW.md` and can be overridden with the `db_path`
front matter field (see Section 5.3.7). On startup, Sortie opens or creates the database and
runs all pending schema migrations before beginning normal operation.

### 19.2 Tables

**`retry_entries`**: pending retries to be reconstructed on restart

| Column       | Type    | Notes                                              |
| ------------ | ------- | -------------------------------------------------- |
| `issue_id`   | TEXT PK | Tracker-internal issue ID                          |
| `identifier` | TEXT    | Human-readable ticket key                          |
| `attempt`    | INTEGER | Retry attempt number (1-based)                     |
| `due_at_ms`  | INTEGER | Unix epoch milliseconds; used to reconstruct timer |
| `error`      | TEXT    | Last error message, may be null                    |
| `session_id` | TEXT    | Adapter-assigned session ID from previous attempt, may be null (migration 009) |
| `rule_name`   | TEXT   | Dispatch rule that routed the original dispatch; empty when none matched (migration 010) |
| `template_id` | TEXT   | Prompt template the rule selected (migration 010) |
| `agent_kind`  | TEXT   | Agent adapter the rule selected; empty on a pre-migration entry (migration 010) |

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
| `status`          | TEXT    | Run disposition (`succeeded`, `failed`, `cancelled`, or `ci_failed`) |
| `error`           | TEXT    | Error message if failed, may be null      |
| `workflow_file`   | TEXT    | Workflow file name the run was driven by, may be null (migration 003) |
| `turns_completed`   | INTEGER | Agent turns the attempt completed (migration 005) |
| `display_identifier` | TEXT | Display identifier carried through dispatch, may be null (migration 006) |
| `review_metadata` | TEXT    | JSON-encoded self-review metadata, may be null (migration 007) |
| `rule_name`       | TEXT    | Dispatch rule that routed the run; empty when none matched (migration 010) |
| `template_id`     | TEXT    | Prompt template the rule selected (migration 010) |
| `input_tokens`      | INTEGER | Accumulated input tokens, 0 for pre-migration rows (migration 011) |
| `output_tokens`     | INTEGER | Accumulated output tokens, 0 for pre-migration rows (migration 011) |
| `total_tokens`      | INTEGER | Accumulated total tokens, 0 for pre-migration rows (migration 011) |
| `cache_read_tokens` | INTEGER | Accumulated cache-read tokens, 0 for pre-migration rows (migration 011) |
| `tokens_measured`   | INTEGER | `1` when the four token columns above carry a figure the coding agent's runtime reported; `0` when the run's spend is unknown and all four are zero (migration 012) |

`status` is no longer a pure mapping from the worker's exit kind. A normal exit that reaches an
otherwise-eligible handoff and is withheld by the handoff-evidence policy records `failed`; its
`error` value names the evidence verdict as the cause. For an otherwise-normal exit on that handoff
path, `succeeded` means the policy did not withhold it. It does not assert that work was positively
observed: an undeterminable verdict under `observed`, and every normal exit under `off`, may still
record `succeeded`.

Rows written before this rule and rows written under `tracker.handoff_evidence: off` retain the
earlier exit-kind-only meaning and are not rewritten. Reports spanning the change therefore span two
definitions of `succeeded` and must not present a changed success rate as proof that agent behavior
changed.

No column stores the handoff-evidence verdict. The verdict is logged and counted, and a withheld
run carries it in the existing `error` field. In particular, this change adds no evidence field to
`run_history` and requires no rewrite of historical rows. Because `succeeded` does not assert that
work was observed, it cannot serve as the reset for a consecutive-absence sequence; that reset is
recorded per issue in `handoff_absence_resets` below.

A row with `tokens_measured = 0` always carries zero in all four token columns; the invariant
runs in one direction only, because a measured run can legitimately report a zero spend. Every
writer sets `tokens_measured` explicitly rather than relying on the column's default: the SQL
default is `1`, but the Go zero value of the corresponding struct field is `false`, so a writer
that omits the field records an unmeasured run rather than inheriting the default. The column
default of `1` exists solely to answer for rows written before migration 012, whose provenance
is not recoverable and are treated as measured.

The token columns mirror those on `session_metadata`. The per-issue token budget
(`agent.max_tokens`) sums `total_tokens` here; the other three are recorded for parity and
future use. The `session_metadata` write cadence changed to throttled-incremental during a
running session (at most one write per issue per two seconds, on the orchestrator event loop)
so the `cost_budget` tool can read in-flight spend before the session's `run_history` row
exists.

`tokens_measured` is not mirrored onto `session_metadata`, and does not need to be. The
throttled in-flight write is gated on a usage-bearing event, so while a session runs its row
comes into existence exactly when that session has reported a measurement, and the presence of
a row whose `session_id` matches the live session is itself the measurement signal the
`cost_budget` tool reads. A running session with no matching row is what makes that tool's
`used_tokens_complete` false, on the same footing as a completed run with
`tokens_measured = 0`. The unconditional write at session exit is outside that window: by then
the run's own measurement state has already been recorded on its `run_history` row.

**`session_metadata`**: last known session metadata per issue (for observability and debug)

| Column              | Type    | Notes                             |
| ------------------- | ------- | --------------------------------- |
| `issue_id`          | TEXT PK | Tracker-internal issue ID         |
| `session_id`        | TEXT    | Last session ID                   |
| `agent_pid`         | TEXT    | Last known agent PID, may be null |
| `input_tokens`      | INTEGER | Accumulated input tokens          |
| `output_tokens`     | INTEGER | Accumulated output tokens         |
| `total_tokens`      | INTEGER | Accumulated total tokens          |
| `cache_read_tokens` | INTEGER | Accumulated cache-read tokens (migration 002) |
| `model_name`        | TEXT    | Last reported LLM model identifier (migration 002) |
| `api_request_count` | INTEGER | Number of API round-trips observed (migration 002) |
| `updated_at`        | TEXT    | ISO-8601 timestamp of last update |

**`aggregate_metrics`**: global token and runtime totals

| Column              | Type    | Notes                             |
| ------------------- | ------- | --------------------------------- |
| `key`               | TEXT PK | Metric key (e.g., `agent_totals`) |
| `input_tokens`      | INTEGER |                                   |
| `output_tokens`     | INTEGER |                                   |
| `total_tokens`      | INTEGER |                                   |
| `cache_read_tokens` | INTEGER | Cumulative cache-read tokens (migration 002) |
| `seconds_running`   | REAL    | Cumulative runtime seconds        |
| `updated_at`        | TEXT    | ISO-8601 timestamp                |

**`reaction_fingerprints`**: cross-restart reaction deduplication (migration 008)

| Column        | Type    | Notes                                                         |
| ------------- | ------- | ------------------------------------------------------------- |
| `issue_id`    | TEXT    | Tracker-internal issue ID (composite PK with `kind`)          |
| `kind`        | TEXT    | Reaction kind; one row per issue per kind                     |
| `fingerprint` | TEXT    | The observed value this kind deduplicates on                  |
| `dispatched`  | INTEGER | `1` when the action for this fingerprint has been performed   |
| `updated_at`  | TEXT    | ISO-8601 timestamp                                            |

Primary key: `(issue_id, kind)`. Every reaction kind that deduplicates across restarts owns a row
here under its own `kind` discriminator, so one issue may hold several rows at once without the
kinds interfering. Upserts reset `dispatched` to `0` when the fingerprint value changes, which
re-arms the latch for exactly one further action.

What the fingerprint holds is defined by the owning kind, not by this table: a digest of the
comment set last acted on, the git ref last checked, a journal high-water mark, or a merge commit
identifier, whichever value identifies "the same observation" for that kind. The column is opaque
text and this schema attaches no meaning to it beyond equality.

`dispatched` likewise means "the action this kind performs has been performed for this
fingerprint", and the row's lifecycle after that point is the owning kind's to define. Most kinds
treat the row as spent. The merge-completion kind (§11G.4) instead retains a dispatched row rather
than deleting it, because deleting it would let the next poll observe the same merge as new.

**`handoff_absence_resets`**: end of a consecutive handoff-absence sequence (migration 013)

| Column         | Type    | Notes                                                              |
| -------------- | ------- | ------------------------------------------------------------------ |
| `issue_id`     | TEXT PK | Tracker-internal issue ID                                          |
| `reset_run_id` | INTEGER | Highest `run_history.id` for the issue at the reset; `0` when none |
| `updated_at`   | TEXT    | ISO-8601 timestamp                                                 |

One row per issue, written only when a run's evidence verdict is `work observed`. The
consecutive-absence count (§14.2) is the number of the issue's absence-marked `run_history` rows
with an `id` above `reset_run_id`, so the count returns to zero the moment a work-observed run is
recorded and cannot be restored by a later handoff-write failure. Outcomes that carry no verdict
never write here, which is what keeps a blocked soft stop, a reaction run, an undeterminable verdict
or a run under `handoff_evidence: off` from resetting a sequence they say nothing about.

The reset point is read from `run_history` inside the write, so a work-observed run whose own
history row could not be persisted still clears the absences recorded before it. This table holds no
verdict history: it is a per-issue position that is overwritten, and deleting a row only restores
the issue's full recorded absence sequence. A work-observed run is no longer the table's only
writer: releasing a parked issue whose park reason is the consecutive-absence ceiling (below) also
writes a reset here, so the loop does not immediately re-derive the exhausted count and park the
issue again on the tick that released it.

**`parked_issues`**: issues held out of primary dispatch until a human acts (migration 014)

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

One row per parked issue, holding current state rather than history: the row is deleted the
moment the park is lifted, not retained as a record of a past park. `parked_state` is nullable in
meaning though not in type: an empty string means no tracker state was observed at park time,
which only a park taken from the retry lane produces, since that lane parks ahead of its own
tracker fetch by design (§14.2). `label_applied` has exactly one writer: the orchestrator's
release-rule observation of the label on a later fetch of the issue, never the outcome of the
label write itself.

### 19.3 Migration Strategy

- Migrations are numbered sequentially and applied in order at startup.
- Applied migrations are tracked in a `schema_migrations` table.
- Migrations are additive (new columns/tables) where possible; destructive migrations require
  explicit versioning.
