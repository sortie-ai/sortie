# Sortie

[![CI](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml/badge.svg)](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/sortie-ai/sortie/graph/badge.svg?token=K2TPXBCbvb)](https://codecov.io/gh/sortie-ai/sortie)

Sortie turns issue tracker tickets into autonomous coding agent sessions.
Engineers manage work at the ticket level. Agents handle implementation.
Single binary, zero dependencies, SQLite persistence.

> Sortie is in active development. The core system is implemented: tracker adapters
> (Jira, file-based), agent adapter (Claude Code), workspace management, persistence,
> orchestrator (dispatch, retry, reconciliation, multi-turn sessions), and observability
> (JSON API, HTML dashboard, Prometheus metrics, health endpoints). Current work:
> self-hosting, hardening, and documentation.
> See [project roadmap](https://github.com/orgs/sortie-ai/projects/1) for current status.

## The Problem

Coding agents can handle routine engineering tasks: bug fixes, dependency updates, test
coverage, build features. But running them at scale requires infrastructure that doesn't
exist yet: isolated workspaces, retry logic, state reconciliation, tracker integration,
cost tracking. Teams build this ad-hoc, poorly, and differently each time.

Sortie is that infrastructure.

## How It Works

Define your `WORKFLOW.md` in a single file alongside the target repository:

```markdown
---
tracker:
  kind: jira
  project: PLATFORM
  query_filter: "component = 'billing-api' AND labels = 'agent-ready'"
  active_states: [To Do, In Progress]
  handoff_state: Human Review
  terminal_states: [Done, Won't Do]

agent:
  kind: claude-code
  max_concurrent_agents: 4
  max_concurrent_agents_by_state:
    in progress: 3
    to do: 1

hooks:
  after_create: |
    git clone git@github.com:acme/billing-api.git .
    go mod download
  before_run: |
    git checkout -B "sortie/$SORTIE_ISSUE_IDENTIFIER" origin/main
  after_run: |
    git add -A && git diff --cached --quiet || \
      git commit -m "sortie($SORTIE_ISSUE_IDENTIFIER): automated changes"
---

You are a senior Go engineer working on the billing-api service.

## {{ .issue.identifier }}: {{ .issue.title }}

{{ .issue.description }}

{{ if .attempt }}
This is retry attempt {{ .attempt }}. Check the workspace for partial work
from the previous run. Do not start from scratch.
{{ end }}
```

Sortie watches this file, polls Jira for matching issues, creates an isolated
workspace for each, and launches Claude Code with the rendered prompt. It handles
the rest: stall detection, timeout enforcement, retries with backoff, state
reconciliation with the tracker, and workspace cleanup when issues reach terminal
states. Changes to the workflow are applied without restart.

See [examples/WORKFLOW.md](examples/WORKFLOW.md?plain=1) for a complete example with
all hooks, continuation guidance, and blocker handling.

## Architecture

Sortie is a single Go binary. It uses SQLite for persistent state (retry queues, session
metadata, run history) and communicates with coding agents over stdio. The orchestrator
is the single authority for all scheduling decisions; there is no external job queue or
distributed coordination. For full architectural details, see
[docs/architecture.md](docs/architecture.md).

Issue trackers and coding agents are integrated through adapter interfaces. Adding support
for a new tracker or agent is an additive change: implement the interface in a new package.

The initial implementation targets Jira and Claude Code. See
[docs/decisions/](docs/decisions/) for detailed rationale on technology choices.

## Documentation

Full configuration reference, CLI usage, and getting started guide:
[docs.sortie-ai.com](https://docs.sortie-ai.com)

## Prior Art

Sortie's architecture is informed by [OpenAI Symphony](https://github.com/openai/symphony),
a spec-first orchestration framework with an Elixir reference implementation. Sortie diverges
in language (Go for deployment simplicity), persistence (SQLite instead of in-memory state),
extensibility (pluggable adapters for any tracker or agent, not hardcoded to Linear and Codex),
and completion signaling (orchestrator-managed handoff transitions instead of relying solely on
agent-initiated tracker writes).

## Why "Sortie"

A _sortie_ is a military and aviation term for a single mission executed autonomously. The
metaphor is precise: the orchestrator dispatches agents on missions (issues), each with an
isolated workspace, a defined objective, and an expected return. The name is short, two
syllables, pronounceable across languages, and does not conflict with existing projects in
this domain.

## Roadmap

See our [project board](https://github.com/orgs/sortie-ai/projects/1) for current status and priorities.

## License

[Apache License 2.0](LICENSE)
