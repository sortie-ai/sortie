# Sortie

[![CI](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml/badge.svg)](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/sortie-ai/sortie/graph/badge.svg?token=K2TPXBCbvb)](https://codecov.io/gh/sortie-ai/sortie)

Sortie turns issue tracker tickets into autonomous coding agent sessions.
Engineers manage work at the ticket level. Agents handle implementation.
Single binary, zero dependencies, SQLite persistence.

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
  kind: github
  api_key: $GITHUB_TOKEN
  project: acme/billing-api
  query_filter: "label:agent-ready"
  active_states: [todo, in-progress]
  in_progress_state: in-progress
  terminal_states: [done, wontfix]

agent:
  kind: claude-code
  max_turns: 10
  max_sessions: 3
  max_concurrent_agents: 4

workspace:
  root: ~/workspace/billing-api

hooks:
  after_create: |
    git clone --depth 1 git@github.com:acme/billing-api.git .
  before_run: |
    git fetch origin main
    git checkout -B "sortie/$SORTIE_ISSUE_IDENTIFIER" origin/main
  after_run: |
    git add -A && git diff --cached --quiet || \
      git commit -m "sortie($SORTIE_ISSUE_IDENTIFIER): automated changes"
    git push origin "sortie/$SORTIE_ISSUE_IDENTIFIER"
---

You are a senior Go engineer working on the billing-api service.

## {{ .issue.identifier }}: {{ .issue.title }}

{{ .issue.description }}

{{ if .run.is_continuation }}
Resuming work — review workspace state before continuing.
{{ end }}
{{ if .attempt }}
Retry attempt {{ .attempt }}. Check the workspace for partial work.
{{ end }}
```

Set `GITHUB_TOKEN` to a fine-grained PAT with **Issues: Read and write** permission
scoped to the target repository. States are mapped to GitHub labels — create labels
matching your `active_states` and `terminal_states` before starting Sortie. The
`query_filter` scopes polling to issues with a specific label so Sortie only picks up
work you explicitly mark as ready. See the
[GitHub adapter reference](https://docs.sortie-ai.com/reference/adapter-github/) for
full configuration details.

Sortie watches this file, polls for matching issues, creates an isolated workspace
for each, and launches Claude Code with the rendered prompt. It handles the rest:
stall detection, timeout enforcement, retries with backoff, state reconciliation
with the tracker, and workspace cleanup when issues reach terminal states. Changes
to the workflow are applied without restart.

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

Supported trackers: GitHub Issues and Jira. Supported agents: Claude Code. See
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
