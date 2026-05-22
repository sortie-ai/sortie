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
    - name: abs-path-rule
      match:
        labels: ["bug"]
      template: /etc/hosts
---
You are an agent assigned to {{ .issue.identifier }}: {{ .issue.title }}.
