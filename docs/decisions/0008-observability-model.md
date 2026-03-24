---
status: accepted
date: 2026-03-24
decision-makers: Serghei Iakovlev
---

# Use Embedded Dashboard with Prometheus Metrics for Observability

## Context and Problem Statement

Sortie is a long-running orchestration daemon that dispatches coding agent sessions, manages
retry queues, and reconciles tracker state. Operators need visibility into what the system
is doing: which issues are being worked, how many tokens have been consumed, whether agents
are stalled, and what errors have occurred. The architecture document (Section 13) specifies
structured logging, a runtime snapshot interface, an optional human-readable status surface,
and an embedded HTTP server with a JSON API and HTML dashboard. However, the architecture
intentionally leaves the observability *model* open — it prescribes the data that must be
observable (Section 13.3) but not the telemetry exposition strategy.

The Go ecosystem has consolidated around two dominant observability paradigms:

1. **Prometheus pull-based metrics** — the de facto standard for Go services in production.
   Nearly every Go service of consequence exposes a `/metrics` endpoint scraped by
   Prometheus. The `prometheus/client_golang` library has 26,000+ importers on pkg.go.dev.
   Grafana dashboards, alerting rules, and operational playbooks are built on PromQL.

2. **OpenTelemetry (OTel)** — the CNCF's vendor-neutral telemetry framework, converging
   traces, metrics, and logs into a single SDK. OTel Go SDK reached metrics stability in
   2024 and has seen rapid adoption. OTel can export to Prometheus (via `prometheus` exporter
   bridge), OTLP endpoints, and dozens of backends. It is the trajectory of the industry.

Sortie's deployment model is distinctive: a single binary with zero infrastructure
dependencies, often run on a developer's laptop or a single SSH host. This creates tension
between the "zero-dependency dashboard" model (embedded HTTP server with self-contained HTML)
and the "integrate with existing monitoring" model (emit standard telemetry consumed by
external tools). The decision must resolve this tension.

## Decision Drivers

1. **Single binary, zero infrastructure.** Sortie deploys as one binary with no external
   servers. The observability model must work out of the box without Prometheus, Grafana,
   or any collector — `sortie WORKFLOW.md` must produce useful visibility immediately.

2. **Industry convention compatibility.** Most Go production environments use Prometheus
   and Grafana. An observability model that ignores this reality forces operators to build
   custom integrations from scratch, or worse, to grep structured logs and transform them
   into metrics manually.

3. **Operational debuggability.** When an agent is stuck, an operator needs answers in
   seconds — not after configuring a monitoring stack. The system must support "glance at
   the dashboard" and "curl an endpoint" workflows without ceremony.

4. **Low implementation and maintenance cost.** The observability layer is not the product.
   It must be simple to implement, simple to extend, and must not introduce coupling between
   metric definitions and core orchestration logic.

5. **Token economics visibility.** LLM token consumption is the primary cost driver.
   Operators need real-time and historical visibility into input/output/total tokens,
   segmented by issue, at both snapshot and aggregate granularity.

6. **Forward compatibility.** The choice should not paint the project into a corner. As the
   ecosystem evolves (OTel adoption, Prometheus 3.x with OTLP native support), the model
   should accommodate future enhancements without breaking changes.

## Considered Options

- **Embedded HTTP server with JSON API + HTML dashboard** — the architecture Section 13.7
  spec: bespoke observability surface with `curl`/browser access, no standard telemetry
  format, no integration with external monitoring systems.
- **Prometheus `/metrics` endpoint** — standard `/metrics` endpoint via
  `prometheus/client_golang` scraped by existing Prometheus infrastructure.
- **Structured logs only** — emit all observability as structured `log/slog` key=value
  lines consumed by log aggregation (Loki, Elasticsearch, CloudWatch).
- **Unix socket + reverse proxy** — JSON API and metrics over a Unix domain socket;
  operators configure a reverse proxy for external access.
- **OpenTelemetry SDK native** — OTel Go SDK (`go.opentelemetry.io/otel`) as the primary
  instrumentation layer, exporting via OTLP to an OTel Collector.

## Decision Outcome

Chosen option: **Embedded JSON API + HTML dashboard as the primary observability surface,
combined with a Prometheus `/metrics` endpoint as an opt-in integration point**, because
this layered model preserves zero-infrastructure operation (drivers 1, 3) while enabling
standard production monitoring integration (driver 2) without over-engineering (driver 4).
Structured logs are retained as the always-on baseline. Unix socket and OpenTelemetry SDK
are deferred.

This is a **layered observability model** with three tiers:

### Tier 1: Structured Logs (Always On)

Structured `log/slog` output remains the foundational observability layer. Every dispatch,
retry, reconciliation, worker lifecycle event, and error emits a structured log line with
`issue_id`, `issue_identifier`, and `session_id` context fields (per architecture Section
13.1). This tier requires zero configuration and zero dependencies.

Structured logs are the safety net: when the HTTP server is disabled, when Prometheus is not
scraping, when the dashboard is not open — logs are always there. They are the audit trail,
the forensic record, and the first place an operator looks when something goes wrong.

### Tier 2: Embedded JSON API + HTML Dashboard (Opt-In via `--port` or `server.port`)

The embedded HTTP server specified in architecture Section 13.7 provides:

- **JSON API** (`/api/v1/*`) for programmatic access to runtime state, per-issue detail, and
  operational triggers.
- **HTML Dashboard** (`/`) for browser-based monitoring with zero external tools.

This tier is enabled when a port is configured and serves Sortie-specific data that has no
standard metric equivalent: workspace paths, agent event messages, retry queue reasons,
`.sortie/status` values, run history context, and per-issue debugging detail. It satisfies
the architecture's runtime snapshot requirement (Section 13.3) and the human-readable status
surface requirement (Section 13.4).

The JSON API shapes follow architecture Section 13.7.2 exactly.

### Tier 3: Prometheus `/metrics` Endpoint (Opt-In, Co-located with HTTP Server)

When the HTTP server is enabled, Sortie exposes a standard Prometheus metrics endpoint at
`/metrics`. This endpoint is served by `prometheus/client_golang`'s `promhttp.Handler()` from
a dedicated `prometheus.Registry` (not the global default, to avoid polluting metrics with
unrelated collectors).

The `/metrics` endpoint is available at the same address and port as the JSON API and
dashboard. No separate port, no separate configuration. If `--port 8080` enables the
HTTP server, then `/`, `/api/v1/*`, and `/metrics` are all served on port 8080.

#### Metric Definitions

All metrics use the `sortie_` prefix per Prometheus naming conventions. Metrics expose
operational counters and gauges derived from orchestrator state; they do not duplicate the
rich contextual data available in the JSON API.

**Gauges (point-in-time state):**

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `sortie_sessions_running` | Gauge | — | Number of currently running agent sessions |
| `sortie_sessions_retrying` | Gauge | — | Number of issues in the retry queue |
| `sortie_slots_available` | Gauge | — | Remaining dispatch slots (`max_concurrent - running - claimed`) |
| `sortie_active_sessions_elapsed_seconds` | Gauge | — | Sum of wall-clock elapsed seconds across all running sessions (computed at scrape time from `started_at`) |

**Counters (monotonically increasing):**

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `sortie_tokens_total` | Counter | `type={input,output}` | Cumulative LLM tokens consumed |
| `sortie_agent_runtime_seconds_total` | Counter | — | Cumulative agent runtime in seconds |
| `sortie_dispatches_total` | Counter | `outcome={success,error}` | Dispatch attempts and their outcomes |
| `sortie_worker_exits_total` | Counter | `exit_type={normal,error,cancelled}` | Worker exit events by type |
| `sortie_retries_total` | Counter | `trigger={error,continuation,timer}` | Retry scheduling events by trigger |
| `sortie_reconciliation_actions_total` | Counter | `action={stop,cleanup,keep}` | Reconciliation outcomes per issue |
| `sortie_poll_cycles_total` | Counter | `result={success,error,skipped}` | Poll tick outcomes |
| `sortie_tracker_requests_total` | Counter | `operation={fetch_candidates,fetch_issue,fetch_comments,transition},result={success,error}` | Tracker adapter API calls |
| `sortie_handoff_transitions_total` | Counter | `result={success,error,skipped}` | Handoff state transition outcomes |

**Histograms (distributions):**

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `sortie_poll_duration_seconds` | Histogram | — | Time per complete poll cycle (fetch + dispatch) |
| `sortie_worker_duration_seconds` | Histogram | `exit_type={normal,error,cancelled}` | Wall-clock time per worker session |

Histogram bucket selection is an implementation detail deferred to the implementing task.
The two histograms have vastly different expected ranges: poll cycles are O(seconds) while
worker sessions are O(minutes) to O(hours). Implementations should use tuned buckets
(e.g., `prometheus.ExponentialBuckets`) appropriate to each histogram's expected range
rather than the library defaults.

**Info (static metadata):**

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `sortie_build_info` | Gauge (always 1) | `version`, `go_version` | Build metadata for target identification |

#### Metric Instrumentation Boundaries

Metric instrumentation follows the architecture's layer model (Section 3.2):

- **Coordination Layer** (orchestrator) owns all gauges and most counters: session counts,
  slot availability, dispatch outcomes, worker exits, retry scheduling, reconciliation
  actions, poll cycles, and handoff transitions.
- **Integration Layer** (adapters) owns `sortie_tracker_requests_total`: each tracker adapter
  increments the counter on API call completion without the orchestrator knowing
  tracker-specific details.
- **Observability Layer** (server package) owns the HTTP server, mux routing, Prometheus
  handler registration, and the concrete `prometheus/client_golang` implementation. It does
  not own any metric definitions — it provides the implementation and serves the endpoint.

To preserve layer boundaries, the orchestrator and adapters must not import
`prometheus/client_golang` directly. Instead, a `Metrics` interface is defined in the
coordination layer (or `internal/domain`) with methods corresponding to the metric
operations above (e.g., `IncDispatch(outcome)`, `SetRunningSessions(count)`,
`ObservePollDuration(seconds)`). The Observability layer provides a Prometheus-backed
implementation that registers collectors on a dedicated `prometheus.Registry` — not the
global default. A no-op implementation is provided for when the HTTP server is disabled
and for use in unit tests. This ensures metrics are testable in isolation and the
Coordination layer has no transitive dependency on the Prometheus library.

The Prometheus HTTP handler must use `promhttp.HandlerFor(registry, opts)` — not the
default `promhttp.Handler()` — to serve from the dedicated registry. To retain scrape
self-instrumentation (`promhttp_metric_handler_*` metrics), configure
`promhttp.HandlerOpts` to register on the same dedicated registry; otherwise these
metrics silently land on the global default and are lost.

#### Cardinality Discipline

The metric set is deliberately narrow. Sortie's concurrency is O(10) agents, not O(10,000)
microservice endpoints. High-cardinality labels (`issue_id`, `issue_identifier`) are
intentionally absent from Prometheus metrics — they belong in the JSON API and structured
logs. Prometheus metrics answer aggregate questions ("how many sessions are running?", "what
is the token burn rate?"). The JSON API answers specific questions ("what is issue MT-649
doing right now?").

This discipline ensures the `/metrics` endpoint remains cheap to scrape and store, even if
Sortie orchestrates hundreds of issues over time.

### Why This Combination

The three tiers serve distinct operator personas and deployment contexts:

| Persona | Context | Primary Tier |
|---------|---------|-------------|
| Solo developer | Laptop, no monitoring stack | Tier 2 (dashboard + JSON API) |
| Platform engineer | Existing Prometheus + Grafana | Tier 3 (/metrics) |
| On-call engineer | Incident investigation | Tier 1 (logs) + Tier 2 (JSON API) |
| CI/automation | Scripted health checks | Tier 2 (JSON API `curl`) |

No single tier satisfies all personas. The combination provides:

- **Zero-infrastructure baseline** (Tiers 1 + 2): `sortie --port 8080 WORKFLOW.md` gives
  full visibility with no external tools.
- **Production integration** (Tier 3): operators with existing monitoring stacks add Sortie
  as a standard Prometheus scrape target — one line in `prometheus.yml`.
- **Defense in depth**: if the HTTP server is misconfigured or the port is blocked, structured
  logs still capture everything.

### Considered Options in Detail

**Embedded HTTP server with JSON API + HTML dashboard.** The architecture Section 13.7 spec:
`GET /api/v1/state`, `GET /api/v1/<identifier>`, `POST /api/v1/refresh`, and `/` dashboard
via `html/template`. Pros: zero dependencies, full control over Sortie-specific data
presentation (retry queue delays, turn counts, `.sortie/status`), aligns with single-binary
deployment. Cons: diverges from Go ecosystem conventions, no alerting integration (operators
cannot define PromQL alerts), no historical time-series queries, custom dashboard maintenance
burden, and bespoke JSON shapes require custom clients. This option is chosen as the primary
surface because the Sortie-specific context it provides has no standard metric equivalent.

**Prometheus `/metrics` endpoint.** Standard Prometheus exposition via
`prometheus/client_golang`. Pros: industry standard (26,000+ importers, CNCF #1 monitoring
tool), alerting via PromQL for free, historical queries via Prometheus/Grafana, pure Go
library (~3 MB, no CGo), interoperates with Alertmanager/Thanos/Cortex/Mimir, and
forward-compatible with OTel via Prometheus 3.x OTLP ingestion. Cons: requires a running
Prometheus instance (violates zero-infrastructure for operators without one), lacks rich
Sortie-specific context (workspace paths, agent event messages), no built-in dashboard. This
option is chosen as the opt-in integration point for production monitoring stacks.

**Structured logs only.** All observability as structured `log/slog` key=value lines consumed
by log aggregation (Elasticsearch/Loki/Splunk/CloudWatch). Pros: zero new dependencies,
works with any aggregation tool, captures full event context. Cons: log-to-metric pipelines
are fragile and silently break on log format changes, no standardized metric schema (no
reusable dashboards or alert rules), no point-in-time state snapshots, query latency for
aggregation questions, and the architecture Section 13.3 runtime snapshot requirement
cannot be satisfied by logs alone. Retained as the always-on Tier 1 baseline but
insufficient as the sole observability surface.

**Unix socket + reverse proxy.** JSON API and metrics over a Unix domain socket. Pros: no TCP
port binding, avoids port conflicts, socket permissions provide host-level access control.
Cons: requires reverse proxy configuration for remote access (contradicts zero-infrastructure
principle), non-standard Prometheus service discovery, less convenient debugging (`curl
--unix-socket` vs `curl http://localhost:8080/`). Deferred — this is a transport concern,
not a model concern; it can be offered as a future listener option without affecting metric
definitions, API shapes, or dashboard implementation.

**OpenTelemetry SDK native.** OTel Go SDK as the primary instrumentation layer, exporting via
OTLP. Pros: vendor-neutral (one API, many backends), traces provide causal context across
the dispatch lifecycle, CNCF graduated project with stable Go metrics SDK, OTel Prometheus
exporter bridge enables backward compatibility. Cons: heavyweight dependency tree (would
triple Sortie's direct dependency count from 3 to 13+), requires an OTel Collector for full
value (the Prometheus bridge produces identical output to `prometheus/client_golang` with
more code and abstraction), distributed tracing solves a problem that does not exist for a
single-process daemon with O(10) concurrent workers, and the OTel slog bridge is
experimental. Deferred, not rejected — the Prometheus metric names and types defined here
are designed to be portable; migration to OTel is mechanical when ecosystem maturity and
deployment complexity justify it.

## New Dependency

This decision introduces one new direct dependency:

**`github.com/prometheus/client_golang`** (Apache 2.0 license)

- Pure Go, no CGo. Compatible with the single-binary deployment model (ADR-0001).
- Adds ~3 MB to the compiled binary (measured with `go build -ldflags="-s -w"`).
- Stable API (v1.x) with a 10-year track record. Backward-compatible releases.
- 26,000+ importers on pkg.go.dev — the most widely used metrics library in the Go
  ecosystem.
- Transitive dependencies are minimal and well-audited: `prometheus/common`,
  `prometheus/client_model`, `protobuf` (for metric serialization), `golang.org/x/sys`.
- The library self-instruments: `promhttp.Handler()` adds `promhttp_metric_handler_*`
  metrics automatically.

This is the first non-trivial third-party dependency beyond the existing three. The
justification is proportional: Prometheus metrics are the standard interface between Go
services and production monitoring infrastructure. Building a custom metrics exposition
format would be more code, more maintenance, and less useful.

## Spec Sections Requiring Update

1. **Section 13.4** — Clarify that the "optional human-readable status surface" is the
   HTML dashboard at `/` when the HTTP server is enabled.
2. **Section 13.7** — Add `/metrics` to the HTTP server route list. Document that it
   serves Prometheus exposition format via `prometheus/client_golang`.
3. **Section 3.3** — Add `prometheus/client_golang` to the external dependencies list
   (conditional: only linked when the HTTP server is enabled; but in practice, always
   linked in the binary).
4. **Section 18.2** — Add "Prometheus `/metrics` endpoint exposes defined gauges,
   counters, and histograms when the HTTP server is enabled" to the recommended
   extensions checklist.

## Consequences

### Positive

- **Zero-infrastructure operation preserved.** Operators without a monitoring stack get
  full visibility via structured logs, the JSON API, and the HTML dashboard. No external
  tools required.
- **Production integration enabled.** Operators with Prometheus and Grafana add Sortie
  as a scrape target with one line of configuration. Alerting, dashboards, and long-term
  storage work out of the box with standard tools.
- **Community-shareable monitoring assets.** The defined metric names and labels enable
  publishing a reference Grafana dashboard JSON and Prometheus alert rules that work for
  any Sortie deployment. This is impossible with bespoke JSON APIs or structured logs.
- **Forward-compatible.** The metric definitions are stable. Migration to OTel SDK or
  Prometheus 3.x OTLP ingestion is mechanical and non-breaking.
- **Low implementation cost.** Prometheus metric instrumentation is 50–100 lines of metric
  definitions + registration, plus one `promhttp.Handler()` route. The bulk of the
  implementation effort remains in the JSON API and HTML dashboard, which are already
  specified.
- **Testable in isolation.** A dedicated `prometheus.Registry` (not the global default)
  means metrics can be asserted in unit tests without global state pollution.

### Negative

- **One new dependency.** `prometheus/client_golang` adds to the dependency tree. Mitigated
  by: the library is pure Go, stable, widely used, and well-maintained by a CNCF project.
- **Binary size increase.** ~3 MB increase from the Prometheus library and protobuf
  serialization. Negligible for a daemon binary.
- **Prometheus is pull-based.** If Sortie runs behind a firewall or NAT where Prometheus
  cannot reach it, the `/metrics` endpoint is unreachable. This is a known limitation for
  the primary target audience (developer laptops, SSH hosts behind NAT). Mitigated by:
  structured logs and the JSON API (Tiers 1 + 2) remain fully functional without
  Prometheus reachability. For push-based metric export, the concrete path forward is
  Prometheus Agent mode with `remote_write` or an OTel Collector sidecar — both scrape
  the local `/metrics` endpoint and forward to a remote backend. Pushgateway is not
  recommended for long-running daemons due to stale-metric semantics on shutdown.
- **Two metric "sources of truth."** The JSON API returns computed aggregates (e.g.,
  `agent_totals.seconds_running` includes active-session elapsed time). Prometheus counters
  like `sortie_agent_runtime_seconds_total` are monotonically increasing and only increment
  on session end — `rate()` over the counter will show 0 during long-running sessions when
  no sessions have completed within the query window. To close this gap, the gauge
  `sortie_active_sessions_elapsed_seconds` provides a scrape-time snapshot of total elapsed
  time across running sessions, enabling operators to detect active work even when no
  sessions have recently completed. The JSON API remains the authoritative source for
  per-issue elapsed time detail; Prometheus provides aggregate operational signals.

### Neutral

- **OTel is deferred, not rejected.** This decision does not preclude adopting OTel in the
  future. The metric names, types, and semantics are designed to be portable across
  instrumentation libraries. When OTel Go SDK maturity, ecosystem adoption, and Sortie's
  deployment complexity justify the migration, the change is additive.
- **Unix socket listener is deferred.** It can be added as a transport option without
  changing the observability model.
- **Distributed tracing is out of scope.** Sortie is a single-process daemon with O(10)
  concurrent workers. If future extensions (SSH worker dispatch, multi-instance
  coordination) introduce cross-process communication, tracing can be revisited.
