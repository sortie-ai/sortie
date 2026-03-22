# Sortie Roadmap

High-level milestones and tasks for building Sortie from zero to a self-hosting orchestration
service. Each task is atomic, independently verifiable, and sized for a single agent session.

## Milestone 0: Project Scaffold

Establish the Go project structure, tooling, and development conventions before writing any
business logic. Every subsequent task assumes this foundation exists.

- [x] 0.1 Research Go project layout conventions (standard-layout, cmd/internal/pkg patterns)
      and select the structure for Sortie. Document the decision in a short comment in go.mod
      or a dedicated section in CONTRIBUTING.md.
      **Verify:** `make build` succeeds with an empty main package.

- [x] 0.2 Initialize Go module (`go mod init`), create `cmd/sortie/main.go` with a minimal
      `main()` that prints version and exits. Set up the directory skeleton per the chosen
      layout.
      **Verify:** `go run ./cmd/sortie` prints version string and exits 0.

- [x] 0.3 Configure linting and formatting: add `golangci-lint` config (`.golangci.yml`),
      create a `Makefile` with targets `fmt`, `lint`, `test`, `build`. Ensure `make lint`
      passes on the empty project.
      **Verify:** `make lint` and `make fmt` exit 0 with no warnings.

- [x] 0.4 Set up CI: create `.github/workflows/ci.yml` that runs `make lint` and `make test`
      on push and PR. Use a Go matrix build (latest stable Go version).
      **Verify:** push to GitHub triggers CI and all jobs pass.

- [x] 0.5 Add a `CLAUDE.md` (or `AGENTS.md`) context file for coding agents. Include: build
      commands, test commands, project structure overview, naming conventions, and architectural
      boundaries that agents must not violate.
      **Verify:** an agent reading the file can answer "how do I build and test this project"
      without additional context.

- [x] 0.6 Research and write ADR-0004: Workflow file format. Evaluate YAML front matter in
      Markdown (current spec) vs TOML front matter vs pure YAML vs separate config + prompt
      files. Consider: single-file UX for workflow authors, parsing complexity, ecosystem
      familiarity (TOML gaining traction in Go/Rust ecosystems, YAML dominant in DevOps),
      front matter delimiter conventions, and how prompt body is separated from config.
      Document the decision in `docs/decisions/0004-workflow-file-format.md`.
      **Verify:** ADR exists with status `accepted`, covers at least 3 alternatives, and
      `docs/decisions/README.md` index is updated.

- [x] 0.7 Research and write ADR-0005: Prompt template engine. Evaluate Go `text/template`
      with `missingkey=error` vs `pongo2` (Jinja2-like) vs Handlebars via Go library vs
      simple string interpolation. Consider: this is user-facing API surface — workflow
      authors write templates, and changing the engine breaks all existing workflows.
      Trade-offs: stdlib simplicity and zero dependencies vs richer filters/syntax, agent
      generation quality for each syntax, error message clarity on template mistakes.
      Document the decision in `docs/decisions/0005-prompt-template-engine.md`.
      **Verify:** ADR exists with status `accepted`, covers at least 3 alternatives, and
      `docs/decisions/README.md` index is updated.

- [x] 0.8 Set up structured logging with `slog`: configure default logger with key=value
      output, define helper to create sub-loggers with `issue_id`, `issue_identifier`, and
      `session_id` context fields. This foundation is used by all subsequent milestones.
      **Verify:** unit test creates a logger with context fields, writes a message, confirms
      structured output contains the expected keys.

## Milestone 1: Configuration Layer

Parse `WORKFLOW.md`, expose typed config, and support dynamic reload. No orchestration logic
yet - just the ability to read, validate, and watch the workflow file.

- [x] 1.1 Select and add parsing libraries to `go.mod` based on ADR-0004 (workflow file format)
      and ADR-0005 (template engine) decisions. If YAML was chosen: evaluate `gopkg.in/yaml.v3`
      vs `github.com/goccy/go-yaml`. If TOML: evaluate `github.com/BurntSushi/toml` vs
      `github.com/pelletier/go-toml/v2`. For the template engine, add the library selected in
      ADR-0005.
      **Verify:** `go mod tidy` succeeds, dependencies resolve.

- [x] 1.2 Implement the workflow loader: read a file, split YAML front matter from Markdown
      body, parse front matter into a map, return `{config, prompt_template}`. Handle error
      cases: missing file, invalid YAML, non-map front matter.
      **Verify:** unit tests cover happy path, missing file, bad YAML, non-map YAML.

- [x] 1.3 Implement the typed config layer: define Go structs for all config sections
      (`tracker`, `polling`, `workspace`, `hooks`, `agent`). Apply defaults. Resolve `$VAR`
      environment indirection and `~` path expansion. Validate required fields. Reference
      architecture Section 6.4 for the complete field list. Key `agent` fields that must be
      present: `kind` (default `claude-code`), `command`, `turn_timeout_ms` (default 3600000),
      `read_timeout_ms` (default 5000), `stall_timeout_ms` (default 300000),
      `max_concurrent_agents` (default 10), `max_turns` (default 20),
      `max_retry_backoff_ms` (default 300000), `max_concurrent_agents_by_state` (default `{}`).
      Normalize per-state concurrency map keys to lowercase and ignore invalid values.
      **Verify:** unit tests cover defaults, env resolution, path expansion, validation errors,
      per-state concurrency map normalization.

- [x] 1.4 Implement prompt template rendering using `text/template` with strict mode (no
      undefined variables). Accept `issue`, `attempt`, and `run` as template inputs. The `run`
      object contains `turn_number` (integer), `max_turns` (integer), and `is_continuation`
      (boolean). Test with a sample template that exercises all variables.
      **Verify:** unit tests cover successful render, unknown variable error, nested field
      access (labels, blockers), and `run` fields (`turn_number`, `is_continuation`).

- [x] 1.5 Implement the turn prompt builder: a function that returns the full rendered task
      prompt on the first turn (`turn_number == 1`, `is_continuation == false`) and a shorter
      continuation guidance prompt on subsequent turns (`is_continuation == true`). The
      continuation prompt should not resend the original task prompt that is already present
      in the agent's thread history. See architecture Sections 7.1 and 12.3.
      **Verify:** unit test renders a first-turn prompt and a continuation prompt for the same
      issue, confirms the continuation prompt is shorter and does not duplicate the task body.

- [x] 1.6 Implement filesystem watcher for `WORKFLOW.md`. On change, re-read and re-apply
      config. On invalid reload, keep last known good config and log an error. Expose a
      method to get the current effective config.
      **Verify:** integration test modifies a temp WORKFLOW.md file, confirms new config is
      picked up. A second test introduces invalid YAML and confirms the old config is retained.

- [x] 1.7 Implement CLI entry point: accept an optional positional argument for workflow file
      path, default to `./WORKFLOW.md`. Add `--port` flag (stored for later). On missing file,
      print a clear error and exit nonzero.
      **Verify:** `go run ./cmd/sortie /tmp/test-workflow.md` loads the file.
      `go run ./cmd/sortie` without a file in cwd exits with an error message.

## Milestone 2: Persistence Layer

SQLite database for retry queues, run history, session metadata, and aggregate metrics.
No orchestration logic yet - just the storage primitives.

- [x] 2.1 Add `modernc.org/sqlite` to `go.mod` per ADR-0002 (pure-Go SQLite, no CGo).
      Create a minimal integration test that opens an in-memory SQLite database, verifies
      WAL mode can be enabled, and exercises basic CRUD.
      **Verify:** test opens DB, creates a table, inserts a row, reads it back.

- [x] 2.2 Implement schema migration runner: numbered migrations applied in order, tracked in
      a `schema_migrations` table. Implement the initial migration that creates the four core
      tables from the architecture doc (Section 19.2): `retry_entries`, `run_history`,
      `session_metadata`, `aggregate_metrics`.
      **Verify:** unit test applies migrations to a fresh DB, confirms all tables exist with
      correct columns.

- [x] 2.3 Implement CRUD operations for `retry_entries`: save, load all, delete by issue_id.
      **Verify:** unit tests for save, load, delete, and idempotent save (upsert).

- [x] 2.4 Implement CRUD operations for `run_history`: append a completed run, query by
      issue_id, query recent runs with pagination.
      **Verify:** unit tests for append, query by issue, and pagination.

- [x] 2.5 Implement CRUD operations for `session_metadata` and `aggregate_metrics`: upsert
      session metadata, read/write aggregate metrics (including `seconds_running`).
      **Verify:** unit tests for each operation.

- [x] 2.6 Implement startup recovery: load persisted retry entries, reconstruct timers from
      `due_at_ms` timestamps, return a list of entries with computed remaining delays.
      **Verify:** unit test creates retry entries with past and future `due_at_ms`, confirms
      the loader returns correct remaining delays (past entries get delay 0).

## Milestone 3: Domain Model and Tracker Adapter Interface

Define the normalized issue model, the tracker adapter interface, implement the adapter
registry, and implement the first adapter (Jira). No orchestration logic yet - just the
ability to talk to a tracker.

- [x] 3.1 Define the normalized `Issue` struct with all fields from architecture Section 4.1.1.
      Define the `TrackerAdapter` interface with the five required operations (Section 11.1).
      Place these in `internal/domain/` or equivalent.
      **Verify:** code compiles, interfaces are importable from other packages.

- [x] 3.2 Implement the adapter registry: a map of `kind` string to adapter constructor
      function, with `Register` and `Get` methods. This registry is shared by tracker adapters
      and agent adapters (or one registry per dimension). The orchestrator uses this to look up
      adapters by `tracker.kind` and `agent.kind` from config. See architecture Section 6.3
      (dispatch preflight validation requires adapters to be registered and available).
      **Verify:** unit test registers a mock adapter, retrieves it by kind, confirms unknown
      kind returns an error.

- [x] 3.3 Implement a file-based tracker adapter for development and testing. Reads issues
      from a JSON or YAML file on disk. Supports all five adapter operations against the file
      contents. Register it in the adapter registry under kind `file`.
      **Verify:** unit tests with a fixture file containing sample issues. Tests cover
      candidate fetch, state refresh, terminal fetch, single issue fetch, comments.

- [x] 3.4 Research Jira REST API: authentication methods (API token, OAuth, PAT), relevant
      endpoints (search, issue, comments, transitions), pagination model, rate limits.
      Document findings in a short `docs/jira-adapter-notes.md`.
      **Verify:** document exists with endpoint references and auth requirements.

- [x] 3.5 Implement Jira tracker adapter: all five required operations — candidate issue fetch
      using JQL, issue state refresh by ID batch, terminal state fetch by states, single issue
      fetch with comments, and comment fetch by issue ID. Normalize Jira responses to the
      `Issue` model. Register in the adapter registry under kind `jira`.
      **Verify:** unit tests with HTTP response fixtures (recorded or hand-crafted JSON).
      Tests cover normalization, pagination, error mapping to generic categories (Section 11.4).

- [x] 3.6 Implement real Jira integration test (guarded by env var `SORTIE_JIRA_TEST=1` and
      credentials). Fetch real issues from a test project, confirm normalization produces valid
      Issue structs.
      **Verify:** `SORTIE_JIRA_TEST=1 make test PKG=./internal/tracker/jira/... RUN=Integration`
      passes against a real Jira instance. Skipped cleanly when env var is absent.

## Milestone 4: Agent Adapter Interface and Claude Code Adapter

Define the agent adapter interface and implement the first adapter (Claude Code). No
orchestration logic yet - just the ability to launch an agent, run a turn, and receive events.

- [x] 4.1 Define the `AgentAdapter` interface with `StartSession`, `RunTurn`, `StopSession`,
      and an optional `EventStream` channel method (Section 10.1). Define the normalized event
      types from architecture Section 10.3, including the `token_usage` event with
      `{input_tokens, output_tokens, total_tokens}`. Place these in `internal/domain/` or
      equivalent.
      **Verify:** code compiles, interfaces are importable.

- [x] 4.2 Research Claude Code CLI: available flags, subprocess behavior, stdio output format,
      session lifecycle, how to detect turn completion and failures. Document findings in
      `docs/claude-code-adapter-notes.md`.
      **Verify:** document exists with CLI reference and observed behavior.

- [x] 4.3 Implement a mock agent adapter for testing. Simulates session start, emits canned
      events on `RunTurn` (including token_usage events), supports configurable success/failure
      outcomes. Register in the adapter registry under kind `mock`.
      **Verify:** unit tests demonstrate the mock adapter satisfying the interface contract.

- [x] 4.4 Implement Claude Code agent adapter: subprocess launch, stdio reading, event parsing,
      session lifecycle (start, turn, stop). Normalize Claude Code output to the standard event
      types. Register in the adapter registry under kind `claude-code`.
      **Verify:** unit tests with captured Claude Code output fixtures. Tests cover event
      parsing, timeout handling, subprocess cleanup.

- [x] 4.5 Implement real Claude Code integration test (guarded by env var
      `SORTIE_CLAUDE_TEST=1`). Launch Claude Code with a trivial prompt in a temp workspace,
      confirm session starts, a turn completes, and events are received.
      **Verify:** `SORTIE_CLAUDE_TEST=1 make test PKG=./internal/agent/claude/... RUN=Integration`
      passes. Skipped cleanly when env var is absent.

## Milestone 5: Workspace Manager

Workspace creation, reuse, path safety, and hook execution. No orchestration logic yet -
just the ability to prepare and clean workspaces.

- [x] 5.1 Implement workspace path computation: sanitize issue identifier to workspace key,
      join with workspace root, validate containment (path must be under root, no symlink
      escape).
      **Verify:** unit tests cover sanitization, containment check, symlink rejection.

- [x] 5.2 Implement workspace creation and reuse: create directory if missing, reuse if exists,
      return hard error if path exists but is not a directory. Use atomic os.Mkdir for
      reliable `created_now` flag. Track `created_now` flag.
      **Verify:** unit tests with temp directories covering create, reuse, non-directory
      conflict error, and atomic CreatedNow correctness.

- [x] 5.3 Implement hook execution: run a shell script with workspace as cwd, enforce timeout,
      set environment variables (`SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`,
      `SORTIE_WORKSPACE`, `SORTIE_ATTEMPT`), capture and truncate output.
      **Verify:** unit tests run a trivial hook script, confirm env vars are set, confirm
      timeout kills the hook, confirm output truncation.

- [x] 5.4 Restrict hook subprocess environment: inherit only `PATH`, `HOME`, `SHELL`, and
      `SORTIE_*` variables instead of the full parent process environment. This prevents
      accidental leakage of secrets (e.g., `JIRA_API_TOKEN`, cloud credentials) into hook
      scripts that may log or forward their environment.
      **Verify:** unit test confirms hook process receives `SORTIE_*` and `PATH` but not an
      unrelated variable set in the parent environment.

- [x] 5.5 Implement workspace lifecycle orchestration: `after_create` on new, `before_run`
      before agent, `after_run` after agent, `before_remove` before cleanup. Enforce failure
      semantics (fatal vs. ignored per hook).
      **Verify:** integration test exercises the full lifecycle with a temp workspace and
      script hooks that write marker files.

- [x] 5.6 Implement workspace cleanup function for terminal issues: given a list of issue
      identifiers, remove matching workspace directories (with `before_remove` hook). This
      function is a reusable primitive called by the orchestrator during startup cleanup
      (Section 8.6) and active-run reconciliation (Section 8.5).
      **Verify:** unit test creates temp workspaces, marks some as terminal, confirms cleanup
      removes only terminal workspaces.

## Milestone 6: Orchestrator Core

The polling loop, dispatch, reconciliation, retry, and state machine. This is the central
component. Uses mock adapters for tracker and agent - no real external calls.

- [x] 6.1 Implement the orchestrator state struct: running map, claimed set, retry attempts,
      completed set, agent totals (including `seconds_running`), agent rate limits. Implement
      slot availability calculation (global and per-state). See architecture Section 4.1.8.
      **Verify:** unit tests for slot math with various running/claimed combinations.

- [x] 6.2 Implement candidate selection and dispatch sorting: priority ascending, created_at
      oldest first, identifier tiebreaker. Implement eligibility checks (active state, not
      claimed, not running, slots available, blocker rule). See architecture Section 8.2.
      **Verify:** unit tests with various issue sets confirm correct sort order and
      eligibility filtering.

- [x] 6.3 Implement the dispatch function (Section 16.4): claim issue, spawn worker goroutine,
      add to running map with initial session fields (all token counters at zero, timestamps,
      retry_attempt). Handle spawn failure by scheduling retry. Clear any existing retry entry
      for the issue on successful spawn.
      **Verify:** unit tests confirm issue is claimed, running entry is created with correct
      initial fields, and spawn failure triggers retry scheduling.

- [x] 6.4 Implement the worker attempt function (Section 16.5): the goroutine spawned by
      dispatch. Sequence: create/reuse workspace, run `before_run` hook, start agent session,
      loop up to `agent.max_turns` turns. On each turn: build the turn-appropriate prompt
      (full prompt on turn 1 via task 1.4, continuation prompt on turns 2+ via task 1.5),
      call `RunTurn`, relay agent events to the orchestrator. After each successful turn,
      re-check tracker issue state — if no longer active or max turns reached, break. On
      exit (normal or error), stop session and run `after_run` hook (best-effort). Report
      exit reason to the orchestrator.
      **Verify:** integration test with mock tracker and mock agent confirms: (a) multi-turn
      loop runs 3 turns when tracker stays active and agent succeeds, (b) loop stops early
      when tracker state becomes terminal after turn 1, (c) loop stops at max_turns, (d)
      agent failure on turn 2 reports error and runs after_run hook.

- [x] 6.5 Implement agent event handling: when the worker relays agent update events to the
      orchestrator, update live session fields in the running map entry (`session_id`,
      `agent_pid`, `last_agent_event`, `last_agent_timestamp`, `last_agent_message`,
      `turn_count`). For `token_usage` events, compute deltas relative to
      `last_reported_*_tokens` to avoid double-counting, then add deltas to both the
      per-session counters and the global `agent_totals`. Track the latest rate-limit payload
      in `agent_rate_limits`. See architecture Sections 7.3 and 13.5.
      **Verify:** unit test sends a sequence of agent events (session_started, token_usage x3,
      turn_completed), confirms running entry fields are updated correctly, token deltas are
      accumulated without double-counting, and agent_totals reflect the sum.

- [x] 6.6 Add retry semantics to error categories: extend `TrackerErrorKind` and
      `AgentErrorKind` with a helper that returns whether a given error kind is retryable
      and its recommended backoff strategy (exponential or non-retryable).
      For example: `tracker_transport_error` is retryable with exponential backoff,
      `tracker_auth_error` is non-retryable, `turn_timeout` is retryable. The worker exit
      handler (6.7) uses this to decide between continuation retry, backoff retry, or
      giving up.
      **Verify:** unit tests confirm each error kind returns the expected retry semantics.
      Table-driven test covers all defined kinds.

- [x] 6.7 Implement worker exit handling (Section 16.6): normal exit adds runtime seconds to
      `agent_totals` and `aggregate_metrics` (SQLite), persists completed run to `run_history`,
      schedules continuation retry (attempt 1, 1s delay). Abnormal exit does the same but
      schedules exponential backoff retry (`min(10000 * 2^(attempt-1), max_retry_backoff_ms)`).
      **Verify:** unit tests for both exit paths, confirm correct retry delays, runtime seconds
      accounting, and SQLite persistence.

- [x] 6.8 Implement retry timer handling (Section 16.6): on timer fire, re-fetch active
      candidates, find issue by ID, check eligibility. If not found, release claim. If found
      and eligible and slots available, dispatch. If found but no slots, requeue with error
      `no available orchestrator slots`. If found but no longer active, release claim.
      **Verify:** unit tests with mock tracker returning various states on retry.

- [x] 6.9 Implement reconciliation (Section 16.3): stall detection (elapsed >
      stall_timeout_ms; skipped if stall_timeout_ms <= 0), tracker state refresh for all
      running issue IDs (terminal -> stop + cleanup workspace via 5.6, active -> update
      in-memory issue snapshot, other -> stop without cleanup). Handle refresh failure by
      keeping workers running and retrying next tick.
      **Verify:** unit tests for each reconciliation outcome including refresh failure and
      disabled stall detection.

- [x] 6.10 Implement dispatch preflight validation (Section 6.3): before each dispatch cycle,
      validate that the workflow can be loaded/parsed, `tracker.kind` is present and the
      adapter is registered, `tracker.api_key` is present after `$` resolution,
      `tracker.project` is present when required, `agent.command` is present when `agent.kind`
      requires a local command, and the agent adapter is registered. On startup, validation
      failure fails startup. Per-tick, validation failure skips dispatch but keeps
      reconciliation active.
      **Verify:** unit tests for each validation check: missing tracker.kind, unresolved
      api_key, unregistered adapter kind, missing agent.command. Integration test confirms
      dispatch is skipped but reconciliation runs when validation fails mid-operation.

- [x] 6.11 Implement the poll loop (Section 16.2): tick scheduling, reconciliation before
      dispatch, preflight validation before dispatch, fetch candidates, sort, dispatch until
      slots exhausted, notify observers. Wire everything together with mock adapters.
      **Optimization note:** `ShouldDispatch` rebuilds `stateSet` maps on each call; the
      dispatch loop should build them once before iterating candidates.
      **Verify:** integration test runs the orchestrator with mock tracker (returns 3 issues)
      and mock agent (completes after 1 turn). Confirm all 3 issues are dispatched, run, and
      completed. Confirm retry on simulated failure.

- [x] 6.12 Implement the full startup sequence (Section 16.1): open/create SQLite DB, run
      schema migrations, configure logging, start workflow file watcher, load persisted retry
      entries from SQLite and reconstruct timers, run dispatch preflight validation (fail
      startup on error), enumerate existing workspace directories and query tracker for their
      states to clean terminal workspaces (Section 8.6 — uses the cleanup function from 5.6),
      schedule the first tick with delay 0, enter event loop.
      **Verify:** integration test with mock tracker and mock agent starts the full
      orchestrator, confirms: DB is created, retry entries from a previous run are loaded,
      terminal workspaces are cleaned, and the first poll tick fires immediately.

- [x] 6.13 Implement dynamic config reload integration: when WORKFLOW.md changes, the
      orchestrator picks up new polling interval, concurrency limits, active/terminal states,
      hook timeouts, agent settings, and prompt template for future runs. In-flight sessions
      are not restarted.
      **Verify:** integration test modifies WORKFLOW.md while orchestrator is running, confirms
      behavior changes (e.g., new polling interval takes effect, new concurrency limit is
      respected).

- [ ] 6.14 Make `tracker.api_key` preflight check conditional via `AdapterMeta.RequiresAPIKey`.
      Add a `RequiresAPIKey bool` field to `registry.AdapterMeta`. Update the Jira adapter's
      `RegisterWithMeta` call to set `RequiresAPIKey: true`. Update the file adapter to use
      `RegisterWithMeta` with `RequiresAPIKey: false`. Change preflight Check 3 to skip the
      `tracker.api_key` validation when the tracker's metadata does not require it. Amend
      architecture.md Section 6.3 to make `tracker.api_key` conditional ("when required by
      the selected tracker adapter"), consistent with `tracker.project` and `agent.command`.
      **Verify:** unit test confirms preflight passes with an empty `tracker.api_key` when the
      adapter's `RequiresAPIKey` is false, and fails when `RequiresAPIKey` is true.

- [ ] 6.15 Make the database path configurable: add an optional `db_path` field to the
      top-level config (default: `.sortie.db` next to WORKFLOW.md). Resolve `$VAR`
      environment indirection and `~` expansion. Update `cmd/sortie/main.go` to use
      the configured path instead of the hardcoded
      `filepath.Join(filepath.Dir(path), ".sortie.db")`. This allows operators to place
      the database on a separate volume or shared filesystem.
      **Verify:** unit test confirms `db_path` is resolved from config with default
      falling back to workflow-adjacent `.sortie.db`. Integration test confirms a custom
      `db_path` is used when specified.

- [ ] 6.16 Fix workspace cleanup to use actual path instead of reconstructing from config.
      `HandleWorkerExit` currently passes `params.WorkspaceRoot` (from fresh config) to
      `workspace.Cleanup`, which calls `ComputePath(root, identifier)` to reconstruct the
      workspace path. If `workspace.root` changes at runtime between worker spawn and exit,
      cleanup targets the wrong directory and the actual workspace becomes orphaned. Store
      the workspace path in `RunningEntry.WorkspacePath` at dispatch time (populated from
      `WorkerResult.WorkspacePath` via the first agent event or passed through dispatch),
      and use it directly in the `PendingCleanup` code path instead of reconstructing from
      `params.WorkspaceRoot` + `entry.Identifier`.
      **Verify:** unit test changes `workspace.root` between dispatch and worker exit with
      `PendingCleanup=true`, confirms cleanup removes the directory at the original path
      (not the new root). Existing cleanup tests continue to pass.

- [ ] 6.17 Guard reconciliation against semantically invalid config on preflight
      failure. When `ReloadWorkflow` succeeds but preflight fails for a
      different reason (missing `tracker.kind`, unregistered adapter, etc.),
      `Config()` returns the newly loaded config which may have empty or
      incorrect `active_states`/`terminal_states`. Reconciliation running with
      these values can cancel workers as "non-active, non-terminal". Either
      keep a pre-reload config snapshot for reconciliation when preflight
      fails, or prevent the workflow manager from promoting a config whose
      state lists are empty.
      **Verify:** unit test loads a config with empty `active_states` that
      passes YAML parsing but fails preflight for a different check, confirms
      reconciliation does not cancel a running worker whose tracker state is
      still in the previous config's active set.

## Milestone 7: End-to-End with Real Adapters

Connect real Jira and real Claude Code adapters to the orchestrator. This is the first time
the system does real work.

- [ ] 7.1 Wire the Jira adapter and Claude Code adapter into the orchestrator startup via the
      adapter registry. Adapter selection uses `tracker.kind` and `agent.kind` from config.
      Confirm the registry-based lookup works end-to-end.
      **Verify:** `go run ./cmd/sortie ./WORKFLOW.md` starts, connects to Jira, and polls
      for issues (with a valid WORKFLOW.md and credentials).

- [ ] 7.2 Write the `WORKFLOW.md` syntax reference (`docs/workflow-reference.md`): a formal
      configuration reference covering file format (front matter + prompt body parsing rules),
      field-by-field specification for every config section (`tracker`, `polling`, `workspace`,
      `hooks`, `agent`, extensions, etc.) with types, defaults, validation rules, dynamic reload
      behavior, and examples. Include prompt template variable reference (Go `text/template`
      syntax, `issue`/`attempt`/`run` inputs, first-turn vs continuation semantics), hook
      lifecycle reference (env vars, failure semantics, inline vs file path), adapter-specific
      configuration sections (per `tracker.kind` and `agent.kind`), error reference (all
      parse/validation errors with causes and fixes), and complete annotated examples (minimal,
      production Jira+Claude Code). Derive all content strictly from `docs/architecture.md`
      Sections 5, 6, 9.4, and 10. This document is the authoritative user-facing reference
      for workflow authors and must be accurate enough to guide task 7.3 (sample `WORKFLOW.md`).
      **Verify:** document covers every field from architecture Section 6.4, every hook from
      Section 5.3.4, every template variable from Section 5.4, and every error from Section 5.5.
      A reviewer can write a valid WORKFLOW.md using only this reference.

- [ ] 7.3 Create a sample `WORKFLOW.md` for testing: configure Jira project, workspace root,
      a simple after_create hook (e.g., `git clone`), and a minimal prompt template.
      **Verify:** the sample file passes config validation when loaded by Sortie.

- [ ] 7.4 Run the first real end-to-end test: create a test issue in Jira, start Sortie,
      confirm it dispatches the issue, Claude Code runs a turn, and the run completes.
      **Verify:** Jira issue shows evidence of agent activity (comment or state change).
      Run history is persisted in SQLite.

- [ ] 7.5 Test failure and retry: create an issue that will cause Claude Code to fail (e.g.,
      invalid workspace), confirm Sortie retries with exponential backoff.
      **Verify:** SQLite run_history shows multiple attempts with increasing delays.

- [ ] 7.6 Test reconciliation: start Sortie with a running issue, move the issue to Done in
      Jira, confirm Sortie stops the agent and cleans the workspace.
      **Verify:** workspace directory is removed after reconciliation.

- [ ] 7.7 Evaluate agent event channel buffer sizing under sustained load. The current buffer
      `max(maxConc*16, 256)` may overflow when many concurrent agents emit high-frequency
      token_usage events, causing silent drops and understated per-session token totals.
      Measure event throughput under 10+ concurrent Claude Code sessions and tune the buffer
      multiplier or introduce a blocking send with timeout for token_usage events.
      **Verify:** run a sustained multi-agent workload, confirm no `agent event channel full`
      warnings in logs, and per-session token totals match agent-reported cumulative totals.

- [ ] 7.8 Evaluate `workerExitCh` buffer sizing when `max_concurrent_agents`
      increases at runtime. The channel is sized `max(maxConc*2, 64)` at
      startup. If concurrency is raised far above the initial value via
      dynamic config reload, `OnExit` performs a blocking send that could
      stall worker goroutines when the buffer fills and the event loop is
      temporarily busy. Consider using a non-blocking send with a fallback
      queue, or sizing the buffer based on a hard upper bound independent of
      the initial config value.
      **Verify:** unit test creates an orchestrator with initial
      `max_concurrent_agents=2`, increases it to a value exceeding the exit
      channel buffer, exits all workers simultaneously, and confirms no
      goroutine blocks indefinitely on the exit send.

## Milestone 8: Observability

Observability surfaces. The system should be monitorable by operators after this milestone.
Basic structured logging was set up in task 0.8; this milestone decides the observability
model (ADR-0007), enhances logging, and implements the chosen surfaces.

- [ ] 8.1 Research and write ADR-0007: Observability model. Evaluate embedded HTTP server
      with JSON API + HTML dashboard (current spec) vs Prometheus `/metrics` endpoint
      consumed by external Grafana vs structured logs only (consumed by log aggregation) vs
      Unix socket + reverse proxy. Consider: the "single binary, zero infrastructure" deployment
      model vs integration with existing monitoring stacks (most Go production services use
      Prometheus). The embedded dashboard optimizes for zero-dependency operation but diverges
      from industry convention. Document the decision in
      `docs/decisions/0007-observability-model.md`.
      **Verify:** ADR exists with status `accepted`, covers at least 3 alternatives, and
      `docs/decisions/README.md` index is updated.

- [ ] 8.2 Audit and enhance structured logging across all modules: confirm `issue_id`,
      `issue_identifier`, and `session_id` context fields are present on all relevant log
      lines (dispatch, retry, reconciliation, worker lifecycle, agent events). Add any
      missing context. Confirm key=value format is consistent.
      **Verify:** run the orchestrator with mock adapters, grep logs for structured fields,
      confirm they are present and consistent across all lifecycle events.

- [ ] 8.3 Implement the runtime snapshot function (Section 13.3): return running sessions
      (including `turn_count` per row), retry queue, agent totals (`input_tokens`,
      `output_tokens`, `total_tokens`, `seconds_running` computed as cumulative ended-session
      time plus active-session elapsed time from `started_at`), and rate limits.
      **Verify:** unit test populates orchestrator state, calls snapshot, confirms all
      fields are populated including computed `seconds_running`.

- [ ] 8.4 Implement the JSON API server (Section 13.7.2): `GET /api/v1/state`,
      `GET /api/v1/<identifier>` (404 for unknown issues), `POST /api/v1/refresh` (202
      Accepted). Use Go `net/http` and `encoding/json`. Bind to loopback by default. Enable
      via `--port` flag (overrides `server.port` from WORKFLOW.md). Return `405` for
      unsupported methods. Use `{"error":{"code":"...","message":"..."}}` envelope for errors.
      **Verify:** integration test starts the HTTP server, calls each endpoint, validates
      response shapes against the architecture doc (Section 13.7.2).

- [ ] 8.5 Implement the HTML dashboard (Section 13.7.1): server-rendered page at `/` showing
      running sessions, retry queue, token totals, runtime seconds, recent events. Use Go
      `html/template`. Add auto-refresh via SSE or meta-refresh.
      **Verify:** start Sortie with `--port 8080`, open `http://localhost:8080` in a browser,
      confirm the dashboard renders current state.

## Milestone 9: Self-Hosting (Sortie Builds Sortie)

At this point, Sortie has enough functionality to orchestrate its own development. Create
Jira issues for remaining work, point Sortie at its own repository, and let agents implement
features.

- [ ] 9.1 Create a production `WORKFLOW.md` for the Sortie repository itself. Define the
      prompt template, hooks (git clone, go mod download, make lint), and agent config.
      **Verify:** Sortie starts and polls the Sortie Jira project.

- [ ] 9.2 Create 3-5 small Jira issues for real improvements (e.g., "add graceful shutdown",
      "add request logging middleware", "add --version flag"). Start Sortie and observe it
      dispatching agents to work on these issues.
      **Verify:** at least one issue results in a working PR or code change.

- [ ] 9.3 Iterate on the WORKFLOW.md prompt based on observed agent behavior. Improve
      instructions for continuation turns, error recovery, and coding conventions.
      **Verify:** subsequent agent runs produce higher quality output than initial runs.

## Milestone 10: Hardening and Production Readiness

Polish for public release. Security, documentation, graceful shutdown, and operational
tooling.

- [ ] 10.1 Implement graceful shutdown: on SIGTERM/SIGINT, stop accepting new dispatches,
      wait for running agents to complete (with timeout), close SQLite cleanly.
      **Verify:** send SIGTERM to running Sortie, confirm it shuts down without data loss.

- [ ] 10.2 Implement the `tracker_api` client-side tool (Section 10.4): expose tracker API
      access to agents during sessions, scoped to the configured project. Advertise the tool
      during session startup. Return structured results: `success=true` on API success,
      `success=false` with preserved response body on API errors, `success=false` with error
      payload on transport/auth/input failures. Unsupported tool names return failure without
      stalling. See architecture Section 10.4 for full contract.
      **Verify:** integration test with mock tracker confirms tool is advertised, successful
      query returns data, API error preserves body, and tool is scoped to configured project.

- [ ] 10.3 Implement `.sortie/status` workspace file reading (Section 21): after each turn
      completes, read `.sortie/status` from the workspace root. If value is `blocked` or
      `needs-human-review`, do not schedule continuation retries until the issue state changes
      in the tracker. Unknown or absent values are ignored. This is advisory only and does not
      affect core orchestration correctness.
      **Verify:** integration test with mock agent that writes `.sortie/status` with `blocked`
      confirms no continuation retry is scheduled. A second test with an absent file confirms
      normal continuation behavior.

- [ ] 10.4 Research and write ADR-0008: Workspace cleanup policy. Evaluate time-based TTL
      expiration (delete workspaces older than N days) vs run-count-based retention (keep
      last N workspaces) vs manual-only cleanup vs disk-pressure-triggered eviction.
      Consider: the current design cleans workspaces only on terminal state transitions
      (Sections 8.5, 8.6), which leaves orphaned workspaces when issues are deleted or
      the orchestrator is stopped before reconciliation. Document the decision in
      `docs/decisions/0008-workspace-cleanup-policy.md`.
      **Verify:** ADR exists with status `accepted`, covers at least 3 alternatives, and
      `docs/decisions/README.md` index is updated.

- [ ] 10.5 Implement workspace TTL cleanup: on startup and periodically (e.g., once per hour),
      scan workspace root for directories older than the configured retention period. Remove
      directories that have no corresponding active or retrying issue in orchestrator state.
      Respect `before_remove` hook. Make the retention period configurable via a
      `workspace.retention_days` field (default: no automatic cleanup for backward
      compatibility).
      **Verify:** unit test creates old workspace directories, runs cleanup, confirms only
      orphaned directories beyond retention age are removed. Directories with active issues
      are preserved.

- [ ] 10.6 Write CONTRIBUTING.md: build instructions, test instructions, code conventions,
      PR process, architecture overview reference.
      **Verify:** a new contributor can follow the guide to build, test, and submit a change.

- [ ] 10.7 Write SECURITY.md: trust model, secret handling, workspace isolation guarantees,
      prompt injection risks, harness hardening guidance. Cover all items from architecture
      Section 15 (trust boundary, filesystem safety, secret handling, hook safety, harness
      hardening).
      **Verify:** document covers all items from architecture Section 15.

- [x] 10.8 Add release automation: GoReleaser config for building cross-platform static
      binaries, GitHub Actions `workflow_dispatch` release workflow that runs lint, unit
      tests, and Jira integration tests before creating a tag and publishing a release.
      **Verify:** "Run workflow" button in GitHub Actions with version input `0.1.0`
      produces release artifacts on GitHub after all tests pass.

- [ ] 10.9 Optimize retry timer candidate fetch: `HandleRetryTimer` calls
      `FetchCandidateIssues` (full paginated sweep) to validate a single issue. At scale
      (hundreds of active issues, multiple concurrent retries) this becomes expensive.
      Replace with `FetchIssueByID` + active-state check to reduce to a single API call
      per retry timer event. Requires verifying that the state check produces the same
      eligibility result as candidate-set membership.
      **Verify:** benchmark or integration test confirms single-issue fetch path.

- [ ] 10.10 Add SBOM generation to release pipeline: install `syft` via `anchore/sbom-action`
      in the release workflow, re-enable the `sboms` section in `.goreleaser.yaml` to produce
      SPDX JSON manifests for each archive artifact.
      **Verify:** dry run release produces `*.sbom.json` files alongside each archive in
      the `dist/` directory.

- [ ] 10.11 Finalize `docs/workflow-reference.md`: update the reference written in task 7.2
      to reflect all features implemented through Milestones 7–10 — including `tracker_api`
      tool extension (10.2), `.sortie/status` file (10.3), workspace TTL cleanup and
      `workspace.retention_days` (10.5), `sortie validate` subcommand, and any adapter-specific
      configuration discovered during end-to-end testing. Add a migration/changelog section
      noting any schema changes since the initial draft. Ensure every field, hook, template
      variable, error code, and adapter extension documented in the architecture spec has a
      corresponding entry. This is the production-quality reference that README.md (10.12)
      will link to as the definitive WORKFLOW.md documentation.
      **Verify:** the reference covers 100% of config fields from architecture Section 6.4,
      all extensions added in Milestones 8–10, and includes at least three complete annotated
      examples (minimal, production Jira+Claude Code, self-hosting). A new user can write a
      valid WORKFLOW.md using only this document.

- [ ] 10.12 Review and finalize README.md: add installation instructions, quick start guide,
      and configuration reference now that the software exists.
      **Verify:** a new user can follow the README to install and run Sortie against their
      own Jira project.

- [ ] 10.13 Prepare 1.0.0 release: update CHANGELOG.md to replace the pre-1.0 notice with
      standard Semantic Versioning adherence, remove the "not yet ready for use" note from
      README.md, and tag the first stable release.
      **Verify:** CHANGELOG.md references SemVer, README.md has no development-only
      disclaimers, and the 1.0.0 release is published.

- [ ] 10.14 Propagate session ID through the retry chain: add `SessionID` to
      `RetryEntry`, `ScheduleRetryParams`, and `persistence.RetryEntry` (schema
      migration). Populate from `WorkerResult.SessionID` in `HandleWorkerExit`,
      read in `HandleRetryTimer`, and pass to `makeWorkerFn(entry.SessionID)` so
      continuation retries can resume the same agent session when the adapter
      supports it (e.g., Claude Code `--resume`). This is an optimization — the
      architecture spec does not require session resume across retry boundaries
      (Section 10.2 covers intra-worker session reuse only).
      **Verify:** unit test confirms session ID survives a full retry round-trip:
      worker exit → schedule retry → timer fire → new worker receives the original
      session ID. Integration test with mock agent confirms `--resume` flag is
      passed when session ID is present.
