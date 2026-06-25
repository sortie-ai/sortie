## 18. Implementation Checklist (Definition of Done)

Use the same validation profiles as Section 17:

- Section 18.1 = `Core Conformance`
- Section 18.2 = `Extension Conformance`
- Section 18.3 = `Real Integration Profile`

### 18.1 Required for Conformance

- Workflow path selection supports explicit runtime path and cwd default
- `WORKFLOW.md` loader with YAML front matter + prompt body split
- Typed config layer with defaults and `$` resolution
- Dynamic `WORKFLOW.md` watch/reload/re-apply for config and prompt
- Polling orchestrator with single-authority mutable state
- Issue tracker client with candidate fetch + state refresh + terminal fetch
- Tracker adapter interface with at least one implementation (Jira)
- Agent adapter interface with at least one implementation (Claude Code)
- Workspace manager with sanitized per-issue workspaces
- Workspace lifecycle hooks (`after_create`, `before_run`, `after_run`, `before_remove`)
- Hook timeout config (`hooks.timeout_ms`, default `60000`)
- Hook environment variables (`SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`, `SORTIE_WORKSPACE`,
  `SORTIE_ATTEMPT`)
- SQLite persistence layer with schema migrations
- Startup recovery from persisted state (retry timers reconstructed from SQLite `due_at`)
- Agent launch command config (`agent.command`, adapter-defined default)
- Strict prompt rendering with `issue`, `attempt`, and `run` variables
- Exponential retry queue with continuation retries after normal exit
- Configurable retry backoff cap (`agent.max_retry_backoff_ms`, default 5m)
- Reconciliation that stops runs on terminal/non-active tracker states
- Workspace cleanup for terminal issues (startup sweep + active transition)
- Structured logs with `issue_id`, `issue_identifier`, and `session_id`
- Operator-visible observability (structured logs; optional snapshot/status surface)

### 18.2 Recommended Extensions (Not Required for Conformance)

- HTTP server honors CLI `--port` over `server.port`, uses a safe default bind host, and exposes
  the baseline endpoints/error semantics in Section 13.7 if shipped.
- Prometheus `/metrics` endpoint exposes defined gauges, counters, and histograms when the HTTP
  server is enabled (Section 13.7.3). Backed by `github.com/prometheus/client_golang` with a
  dedicated registry; no external Prometheus server required.
- Agent tool subsystem: `ToolRegistry` populated with the built-in tools per Section 10.4
  (`tracker_api`, `sortie_status`, `workspace_history`). The runtime execution channel is an
  MCP stdio sidecar (per ADR-0009).
- Make observability settings configurable in workflow front matter without prescribing UI
  implementation details.
- First-class tracker write APIs (comments/state transitions) in the orchestrator, supplementing
  agent tool-based mutations.

### 18.3 Operational Validation Before Production (Recommended)

- Run the `Real Integration Profile` from Section 17.8 with valid credentials and network access.
- Verify hook execution and workflow path resolution on the target host OS/shell environment.
- If the HTTP server is shipped, verify the configured port behavior and loopback/default bind
  expectations on the target environment.

