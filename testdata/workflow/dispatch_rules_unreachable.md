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
    - name: catch-all
      template: ./prompts/bug.md

    - name: bug
      match:
        labels: ["bug"]
      template: ./prompts/bug.md
---
You are an agent assigned to {{ .issue.identifier }}: {{ .issue.title }}.
