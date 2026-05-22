---
tracker:
  kind: file
  project: dispatch-fixture
  active_states: [todo, in-progress]
  terminal_states: [done]

agent:
  kind: mock
  command: mock
  max_turns: 1

mock:
  reply: noop

dispatch:
  rules:
    - name: tilde-path-rule
      match:
        labels: ["bug"]
      template: ~/templates/custom.md
---
You are an agent assigned to {{ .issue.identifier }}: {{ .issue.title }}.
