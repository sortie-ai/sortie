---
tracker:
  kind: jira
  endpoint: $JIRA_ENDPOINT
  api_key: $JIRA_API_KEY
  project: SORT
  active_states:
    - backlog
    - selected for development
    - in progress
  terminal_states:
    - done
    - cancelled

polling:
  interval_ms: 30000

workspace:
  root: ~/sortie-workspaces

hooks:
  after_create: |
    git clone "$REPO_URL" . 2>/dev/null || true
    go mod download
  before_run: |
    git fetch origin main
    git reset --hard origin/main
  after_run: |
    make fmt
  timeout_ms: 120000

agent:
  kind: claude-code
  command: claude
  max_turns: 10
  max_concurrent_agents: 3
  turn_timeout_ms: 3600000
  stall_timeout_ms: 300000
  max_retry_backoff_ms: 300000
---

You are a Go engineer working on the Sortie project.

## Task

{{ .issue.title }}

{{ .issue.description }}

## Context

- Project: Sortie — a spec-first orchestration service written in Go.
- Architecture doc at `docs/architecture.md` is the authoritative specification.
- Run `make lint` and `make test` before finishing.

## Labels

{{- range .issue.labels }}

- {{ . }}
  {{- end }}

{{- if .issue.blockers }}

## Blockers

{{- range .issue.blockers }}

- {{ .identifier }}: {{ .summary }}
  {{- end }}
  {{- end }}

{{- if .run.is_continuation }}

## Continuation

This is turn {{ .run.turn_number }} of {{ .run.max_turns }}.
Review your previous work and continue where you left off.
Do not repeat completed steps.
{{- end }}
