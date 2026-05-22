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
    - name: bug
      match:
        labels: ["bug"]
      template: ./prompts/bug.md

    - name: docs
      match:
        labels: ["docs"]
      template: ./prompts/docs.md
---
You are an agent assigned to {{ .issue.identifier }}: {{ .issue.title }}.

Use this fallback template when no dispatch rule matches.
