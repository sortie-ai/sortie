---
tracker:
  kind: file
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done

file:
  path: examples/issues.json

polling:
  interval_ms: 30000

workspace:
  root: /tmp/sortie-test-workspaces

agent:
  kind: mock
  max_turns: 3
  max_concurrent_agents: 2
---

{{/* Offline validation workflow using file tracker and mock agent.
     Run from repo root: go run ./cmd/sortie examples/WORKFLOW.test.md */}}
**{{ .issue.identifier }}**: {{ .issue.title }}

{{ if .issue.description }}{{ .issue.description }}{{ end }}

{{ if .run.is_continuation }}Continue working on the task. Review current state and proceed.{{ end }}
