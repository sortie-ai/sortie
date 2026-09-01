# Architecture Decision Records

This directory contains architecturally significant decisions for Sortie, documented as
[Markdown Architectural Decision Records (MADR)](https://adr.github.io/madr/).

## Decisions

| ADR                                                  | Title                                                             | Status   |
| ---------------------------------------------------- | ----------------------------------------------------------------- | -------- |
| [0001](0001-use-go-as-core-runtime.md)               | Use Go as core runtime                                            | Accepted |
| [0002](0002-use-sqlite-for-persistence.md)           | Use SQLite for persistence                                        | Accepted |
| [0003](0003-adapter-based-integration.md)            | Use adapter interfaces for integration extensibility              | Accepted |
| [0004](0004-workflow-file-format.md)                 | Use YAML Front Matter for Workflow Files                          | Accepted |
| [0005](0005-prompt-template-engine.md)               | Use Go `text/template` for Prompt Rendering                       | Accepted |
| [0006](0006-use-fsnotify-for-file-watching.md)       | Use `fsnotify` for Filesystem Event Watching                      | Accepted |
| [0007](0007-handoff-state-and-tracker-writes.md)     | Use Handoff State Transitions to Signal Agent Completion          | Accepted |
| [0008](0008-observability-model.md)                  | Use Embedded Dashboard with Prometheus Metrics for Observability  | Accepted |
| [0009](0009-mcp-stdio-sidecar-for-tool-execution.md) | Use MCP stdio sidecar for agent tool execution                    | Accepted |
| [0010](0010-keep-tracker-adapter-unified.md)         | Keep TrackerAdapter as a Unified Interface                        | Accepted |
| [0011](0011-dispatch-rule-configuration.md)          | Use First-Match-Wins Dispatch Rules in `WORKFLOW.md` Front Matter | Accepted |
| [0012](0012-auto-merge-reaction.md)                  | Extend `SCMAdapter` with Write Methods for Auto-Merge Reactions   | Accepted |
| [0013](0013-agent-cost-budget.md)                    | Use cumulative per-issue token counts for the agent cost budget   | Accepted |
| [0014](0014-operator-notifications.md)               | Use an adapter family for operator notifications                  | Accepted |
| [0015](0015-pr-label-command-detection.md)           | Detect PR Label Commands by Polling the Label-Event Journal       | Accepted |
| [0016](0016-place-forge-integrations-in-one-package-per-forge.md) | Place Forge Integrations in One Package per Forge    | Accepted |
| [0017](0017-close-tracker-issue-on-managed-pr-merge.md) | Close the Tracker Issue When a Managed Pull Request Merges | Accepted |
| [0018](0018-bound-workspace-retention-by-age.md)        | Bound Workspace Retention by Age Independently of Tracker State | Accepted |
| [0019](0019-keep-usage-data-on-the-host.md)             | Keep Usage Data on the Host and Aggregate Across Instances by Pull | Accepted |
| [0020](0020-withhold-handoff-on-observed-absence-of-work.md) | Withhold the Handoff Transition Only When Absence of Work Is Observed | Accepted |
| [0021](0021-run-self-review-before-ending-on-the-completion-signal.md) | Run Self-Review Before Ending the Run on the Completion Signal | Accepted |
| [0022](0022-release-a-parked-issue-on-a-human-gesture.md) | Release a Parked Issue on a Human Gesture in the Tracker | Accepted |
| [0023](0023-scope-the-ci-verdict-to-the-current-head.md) | Scope the CI Verdict to the Pull Request's Current Head | Accepted |
| [0024](0024-start-a-new-feedback-epoch-when-the-head-moves.md) | Start a New Feedback Epoch When the Pull Request Head Moves | Accepted |
| [0025](0025-refuse-agent-requests-only-a-human-could-answer.md) | Refuse Agent Requests That Only a Human Could Answer | Accepted |
| [0026](0026-re-read-issue-state-before-recording-absence-failure.md) | Re-Read the Issue State Before Recording an Absence Failure | Accepted |
| [0027](0027-give-the-consecutive-absence-ceiling-its-own-setting.md) | Give the Consecutive-Absence Ceiling Its Own Setting | Accepted |
| [0028](0028-let-the-agent-declare-that-nothing-needed-changing.md) | Let the Agent Declare That Nothing Needed Changing | Accepted |
| [0029](0029-adopt-agent-client-protocol-as-a-generic-agent-transport.md) | Adopt the Agent Client Protocol as a Single Generic Agent Transport | Accepted |
