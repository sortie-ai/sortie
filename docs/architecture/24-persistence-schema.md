## 19. Persistence Schema

### 19.1 Overview

Sortie uses an embedded SQLite database for durable state. The database file path defaults to
`.sortie.db` in the same directory as `WORKFLOW.md` and can be overridden with the `db_path`
front matter field (see Section 5.3.7). On startup, Sortie opens or creates the database and
runs all pending schema migrations before beginning normal operation.

### 19.2 Tables

**`retry_entries`** — pending retries to be reconstructed on restart

| Column       | Type    | Notes                                              |
| ------------ | ------- | -------------------------------------------------- |
| `issue_id`   | TEXT PK | Tracker-internal issue ID                          |
| `identifier` | TEXT    | Human-readable ticket key                          |
| `attempt`    | INTEGER | Retry attempt number (1-based)                     |
| `due_at_ms`  | INTEGER | Unix epoch milliseconds; used to reconstruct timer |
| `error`      | TEXT    | Last error message, may be null                    |
| `session_id` | TEXT    | Adapter-assigned session ID from previous attempt, may be null |

Note: `timer_handle` is runtime-only and is not stored.

**`run_history`** — completed run attempts

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
| `review_metadata` | TEXT    | JSON-encoded self-review metadata, may be null (migration 007) |
| `input_tokens`      | INTEGER | Accumulated input tokens, 0 for pre-migration rows (migration 011) |
| `output_tokens`     | INTEGER | Accumulated output tokens, 0 for pre-migration rows (migration 011) |
| `total_tokens`      | INTEGER | Accumulated total tokens, 0 for pre-migration rows (migration 011) |
| `cache_read_tokens` | INTEGER | Accumulated cache-read tokens, 0 for pre-migration rows (migration 011) |

The token columns mirror those on `session_metadata`. The per-issue token budget
(`agent.max_tokens`) sums `total_tokens` here; the other three are recorded for parity and
future use. The `session_metadata` write cadence changed to throttled-incremental during a
running session (at most one write per issue per two seconds, on the orchestrator event loop)
so the `cost_budget` tool can read in-flight spend before the session's `run_history` row
exists.

**`session_metadata`** — last known session metadata per issue (for observability and debug)

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

**`aggregate_metrics`** — global token and runtime totals

| Column              | Type    | Notes                             |
| ------------------- | ------- | --------------------------------- |
| `key`               | TEXT PK | Metric key (e.g., `agent_totals`) |
| `input_tokens`      | INTEGER |                                   |
| `output_tokens`     | INTEGER |                                   |
| `total_tokens`      | INTEGER |                                   |
| `cache_read_tokens` | INTEGER | Cumulative cache-read tokens (migration 002) |
| `seconds_running`   | REAL    | Cumulative runtime seconds        |
| `updated_at`        | TEXT    | ISO-8601 timestamp                |

**`reaction_fingerprints`** — cross-restart reaction deduplication (migration 008)

| Column        | Type    | Notes                                                         |
| ------------- | ------- | ------------------------------------------------------------- |
| `issue_id`    | TEXT    | Tracker-internal issue ID (composite PK with `kind`)          |
| `kind`        | TEXT    | Reaction kind (`ci`, `review`)                                |
| `fingerprint` | TEXT    | Deterministic hash of the current reaction state              |
| `dispatched`  | INTEGER | `1` when a fix dispatch has been sent for this fingerprint    |
| `updated_at`  | TEXT    | ISO-8601 timestamp                                            |

Primary key: `(issue_id, kind)`. Upserts reset `dispatched` to `0` when the fingerprint value
changes (new comments detected). Used by CI and review reconcile functions to prevent duplicate
dispatches across restarts.

### 19.3 Migration Strategy

- Migrations are numbered sequentially and applied in order at startup.
- Applied migrations are tracked in a `schema_migrations` table.
- Migrations are additive (new columns/tables) where possible; destructive migrations require
  explicit versioning.

