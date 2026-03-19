---
tracker:
  kind: jira
  endpoint: $JIRA_ENDPOINT
  api_key: $JIRA_API_KEY
  project: PLATFORM
  query_filter: "component = 'billing-api' AND labels = 'agent-ready'"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
    - Won't Do

polling:
  interval_ms: 45000

workspace:
  root: ~/workspaces/billing-api

hooks:
  after_create: |
    git clone --depth 1 git@github.com:acme/billing-api.git .
    go mod download
    cp .env.template .env
  before_run: |
    git fetch origin main
    git checkout -B "sortie/$SORTIE_ISSUE_IDENTIFIER" origin/main
  after_run: |
    make fmt
    make lint --fix 2>/dev/null || true
    git add -A
    git diff --cached --quiet || git commit -m "sortie($SORTIE_ISSUE_IDENTIFIER): automated changes"
  before_remove: |
    git push origin --delete "sortie/$SORTIE_ISSUE_IDENTIFIER" 2>/dev/null || true
  timeout_ms: 120000

agent:
  kind: claude-code
  command: claude --dangerously-skip-permissions
  max_turns: 15
  max_concurrent_agents: 4
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state:
    in progress: 3
    to do: 1

server:
  port: 8642
---

You are a senior Go engineer working on the billing-api service at Acme Corp.

## Your task

**{{ .issue.identifier }}**: {{ .issue.title }}

{{ if .issue.description }}{{ .issue.description }}{{ end }}

## Project context

billing-api is a Go service that handles subscription management, invoice generation,
and payment processing via Stripe. It uses:

- Go 1.22, Chi router, sqlc for database access, PostgreSQL
- `internal/` layout: `handler/`, `service/`, `repository/`, `model/`, `middleware/`
- OpenAPI spec at `api/openapi.yaml` - handler signatures must match the spec
- Database migrations in `migrations/` using golang-migrate

Key files to read before making changes:

- `AGENTS.md` for build commands and architectural boundaries
- `docs/architecture.md` for service design and data flow
- `api/openapi.yaml` if the task touches HTTP endpoints

## Rules

1. Run `make lint` and `make test` before finishing. All checks must pass.
2. Do not modify `go.mod` unless the task explicitly requires a new dependency.
3. Do not change database migration files that are already applied (numbered below the
   latest in `migrations/`). Create new migration files for schema changes.
4. Write table-driven tests. Aim for edge cases, not just the happy path.
5. If you encounter a problem outside the scope of this task, note it in a
   `.sortie/status` file as `blocked` and stop.

{{ if .issue.url }}

## Reference

Ticket: {{ .issue.url }}
{{ end }}

{{ if .issue.labels }}

## Labels

{{ .issue.labels | join ", " }}
{{ end }}

{{ if .issue.parent }}

## Parent issue

{{ .issue.parent.identifier }}
{{ end }}

{{ if .issue.blocked_by }}

## Blockers

The following issues block this task. If any are unresolved, focus on preparation
work that does not depend on the blocker (tests, scaffolding, documentation).

{{ range .issue.blocked_by }}- **{{ .identifier }}**{{ if .state }} ({{ .state }}){{ end }}
{{ end }}
{{ end }}

{{ if .issue.comments }}

## Discussion history

Review these comments for context and prior decisions:

{{ range .issue.comments }}> **{{ .author }}** ({{ .created_at }}):

> {{ .body }}

{{ end }}
{{ end }}

{{ if .attempt }}

## Retry context

This is retry attempt {{ .attempt }}. A previous run failed. Check the workspace for
partial work. Do not start from scratch - identify what failed and fix the specific
issue. Run `git log --oneline -5` to see recent commits from prior attempts.
{{ end }}

{{ if .run.is_continuation }}

## Continuation guidance

This is turn {{ .run.turn_number }} of {{ .run.max_turns }}.

Review your previous actions in this session. Do not repeat completed steps.
Focus on:

1. Verifying that prior changes compile and pass tests.
2. Addressing any remaining work from the task description.
3. Running the full test suite before finishing.
   {{ end }}
