## 4. Core Domain Model

### 4.1 Entities

#### 4.1.1 Issue

Normalized issue record used by orchestration, prompt rendering, and observability output.

Fields:

- `id` (string)
  - Stable tracker-internal ID.
- `identifier` (string)
  - Human-readable ticket key (example: `ABC-123`).
- `title` (string)
- `description` (string or null)
- `priority` (integer or null)
  - Lower numbers are higher priority in dispatch sorting.
- `state` (string)
  - Current tracker state name.
- `branch_name` (string or null)
  - Tracker-provided branch metadata if available.
- `url` (string or null)
- `labels` (list of strings)
  - Normalized to lowercase.
- `assignee` (string or null)
  - Assignee identity as provided by the tracker. Used for prompt rendering and observability.
- `issue_type` (string or null)
  - Tracker-defined type (for example Bug, Story, Task, Epic). Used for prompt rendering and
    per-type concurrency limits.
- `parent` (object or null)
  - Parent issue reference for sub-tasks. Relevant for blocking/dependency logic and prompt
    context.
- `comments` (list or null)
  - Comment records containing human feedback, review notes, and prior agent workpad entries.
    Needed for continuation runs where the agent must understand prior communication.
- `blocked_by` (list of blocker refs)
  - Each blocker ref contains:
    - `id` (string or null)
    - `identifier` (string or null)
    - `state` (string or null)
  - If `blocker.state` is null or unknown, treat it as non-terminal (conservative).
- `created_at` (timestamp or null)
- `updated_at` (timestamp or null)

#### 4.1.2 Workflow Definition

Parsed `WORKFLOW.md` payload:

- `config` (map)
  - YAML front matter root object.
- `prompt_template` (string)
  - Markdown body after front matter, trimmed.

#### 4.1.3 Service Config (Typed View)

Typed runtime values derived from `WorkflowDefinition.config` plus environment resolution.

Examples:

- poll interval
- workspace root
- active and terminal issue states
- concurrency limits
- coding-agent executable/args/timeouts
- workspace hooks

#### 4.1.4 Workspace

Filesystem workspace assigned to one issue identifier.

Fields (logical):

- `path` (workspace path; current runtime typically uses absolute paths, but relative roots are
  possible if configured without path separators)
- `workspace_key` (sanitized issue identifier)
- `created_now` (boolean, used to gate `after_create` hook)

#### 4.1.5 Run Attempt

One execution attempt for one issue.

Fields (logical):

- `issue_id`
- `issue_identifier`
- `attempt` (integer or null, `null` for first run, `>=1` for retries/continuation)
- `workspace_path`
- `started_at`
- `completed_at` (timestamp or null)
  - Populated when the attempt finishes. Used for run duration calculation and persistent history.
- `status`
- `agent_adapter` (string)
  - Which agent adapter was used for this attempt. Relevant when multiple agent types are
    configured.
- `error` (optional)

#### 4.1.6 Live Session (Agent Session Metadata)

State tracked while a coding-agent subprocess is running.

Fields:

- `session_id` (string)
  - Opaque string assigned by the agent adapter. For adapters that expose thread/turn concepts,
    the composition rule (e.g., `<thread_id>-<turn_id>`) is adapter-specific, not universal.
- `thread_id` (string)
- `turn_id` (string)
- `agent_pid` (string or null)
- `last_agent_event` (string/enum or null)
- `last_agent_timestamp` (timestamp or null)
- `last_agent_message` (summarized payload)
- `agent_input_tokens` (integer)
- `agent_output_tokens` (integer)
- `agent_total_tokens` (integer)
- `last_reported_input_tokens` (integer)
- `last_reported_output_tokens` (integer)
- `last_reported_total_tokens` (integer)
- `turn_count` (integer)
  - Number of coding-agent turns started within the current worker lifetime.

#### 4.1.7 Retry Entry

Scheduled retry state for an issue.

Fields:

- `issue_id`
- `identifier` (best-effort human ID for status surfaces/logs)
- `attempt` (integer, 1-based for retry queue)
- `due_at_ms` (monotonic clock timestamp)
- `timer_handle` (runtime-specific timer reference; runtime-only, not persisted to SQLite)
- `error` (string or null)

Note: `timer_handle` is a runtime-only field and is not persisted. On restart, pending retries
are reconstructed from the persisted `due_at` timestamps stored in SQLite.

#### 4.1.8 Orchestrator Runtime State

Single authoritative state owned by the orchestrator. The running map and active timers are
in-memory for performance; retry_attempts, completed set, and agent_totals are backed by SQLite
and survive restarts.

Fields:

- `poll_interval_ms` (current effective poll interval)
- `max_concurrent_agents` (current effective global concurrency limit)
- `running` (map `issue_id -> running entry`)
- `claimed` (set of issue IDs reserved/running/retrying)
- `retry_attempts` (map `issue_id -> RetryEntry`)
- `completed` (set of issue IDs; bookkeeping only, not dispatch gating)
- `agent_totals` (aggregate tokens + runtime seconds)
- `agent_rate_limits` (latest rate-limit snapshot from agent events)
- `reaction_attempts` (map `issue_id:kind -> integer`; number of reaction-fix continuations
  dispatched per issue and reaction kind; reset when the issue leaves the running/retry maps;
  runtime-only, not persisted)
- `pending_reactions` (map `issue_id:kind -> PendingReaction`; populated by worker exit on normal
  exits with SCM metadata when a CI status provider or SCM adapter is configured, and reconstructed
  at startup by `RecoverPendingReactions` for eligible handoff-stage runs, including `merge`-kind
  entries; consumed by per-kind reconcile functions during the reconcile tick: `reconcile_ci_status`
  for kind `ci`, `reconcile_review_comments` for kind `review`, `reconcile_auto_merge` for kind
  `merge`; runtime-only, not persisted)
- `auto_merge_preflight_failed` (boolean): sticky after a startup auth-class preflight failure;
  cleared only by a successful one-shot transport-class retry or by an orchestrator restart.
- `auto_merge_preflight_retry_due_at` (timestamp): non-zero only when the startup preflight failed
  with a transport-class error and scheduled a single bounded retry; cleared by the reconcile tick
  that consumes the retry.

### 4.2 Stable Identifiers and Normalization Rules

- `Issue ID`
  - Use for tracker lookups and internal map keys.
- `Issue Identifier`
  - Use for human-readable logs and workspace naming.
- `Workspace Key`
  - Derive from `issue.identifier` by replacing any character not in `[A-Za-z0-9._-]` with `_`.
  - Use the sanitized value for the workspace directory name.
- `Normalized Issue State`
  - Compare states after `lowercase`.
- `Session ID`
  - Opaque string assigned by the agent adapter. Composition is adapter-specific.

