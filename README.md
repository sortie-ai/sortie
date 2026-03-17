# Sortie

Sortie is an orchestration service that turns issue tracker tickets into autonomous coding
agent sessions. It polls for work, creates isolated per-issue workspaces, dispatches agents,
and manages their lifecycle through retries, reconciliation, and observability.

Engineers manage work at the ticket level. Agents handle implementation.

> **Note:** Sortie is under active development and is not yet ready for use.

## Why "Sortie"

A *sortie* is a military and aviation term for a single mission executed autonomously. The
metaphor is precise: the orchestrator dispatches agents on missions (issues), each with an
isolated workspace, a defined objective, and an expected return. The name is short, two
syllables, pronounceable across languages, and does not conflict with existing projects in
this domain.

## How It Works

1. Polls an issue tracker for tickets in active states.
2. Creates an isolated workspace for each ticket.
3. Renders a prompt from the ticket data and a workflow template.
4. Launches a coding agent session inside the workspace.
5. Monitors execution: stall detection, timeout enforcement, state reconciliation.
6. Retries failed runs with exponential backoff.
7. Cleans up workspaces when tickets reach terminal states.

The workflow definition (prompt template and runtime configuration) lives in a single
`WORKFLOW.md` file that is version-controlled alongside the target repository. Changes
to the workflow are detected and applied without restart.

## Architecture

Sortie is a single Go binary. It uses SQLite for persistent state (retry queues, session
metadata, run history) and communicates with coding agents over stdio. The orchestrator
is the single authority for all scheduling decisions; there is no external job queue or
distributed coordination.

Issue trackers and coding agents are integrated through adapter interfaces. Adding support
for a new tracker or agent is an additive change: implement the interface in a new package.

The initial implementation targets Jira and Claude Code. See
[docs/decisions/](docs/decisions/) for detailed rationale on technology choices.

## Prior Art

Sortie's architecture is informed by [OpenAI Symphony](https://github.com/openai/symphony),
a spec-first orchestration framework with an Elixir reference implementation. Key
differences:

- **Go instead of Elixir** for deployment simplicity and broader contributor accessibility.
- **SQLite persistence** instead of in-memory state, surviving process restarts.
- **Pluggable trackers and agents** instead of Linear-only and Codex-only integration.
- **Claude Code as primary agent** instead of Codex app-server.

## License

[Apache License 2.0](LICENSE)
