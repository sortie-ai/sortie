## 1. Problem Statement

Sortie is a long-running automation service that continuously reads work from an issue tracker,
creates an isolated workspace for each issue, and runs a coding agent session for that issue inside
the workspace.

The service solves four operational problems:

- It turns issue execution into a repeatable daemon workflow instead of manual scripts.
- It isolates agent execution in per-issue workspaces so agent commands run only inside per-issue
  workspace directories.
- It keeps the workflow policy in-repo (`WORKFLOW.md`) so teams version the agent prompt and runtime
  settings with their code.
- It provides enough observability to operate and debug multiple concurrent agent runs.

Sortie documents its trust and safety posture explicitly. It does not mandate a single approval,
sandbox, or operator-confirmation policy; some deployments may target trusted environments with a
high-trust configuration, while others may require stricter approvals or sandboxing.

Important boundary:

- Sortie is a scheduler/runner and tracker reader.
- Ticket writes (state transitions, comments, PR links) are typically performed by the coding agent
  using tools available in the workflow/runtime environment.
- A successful run may end at a workflow-defined handoff state (for example `Human Review`), not
  necessarily `Done`.

