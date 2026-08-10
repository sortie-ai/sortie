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
| `status`          | TEXT    | Terminal status (succeeded, failed, etc.) |
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

### 19.3 Migration Strategy

- Migrations are numbered sequentially and applied in order at startup.
- Applied migrations are tracked in a `schema_migrations` table.
- Migrations are additive (new columns/tables) where possible; destructive migrations require
  explicit versioning.

