# Documentation Index

## Architecture

| Document | Description |
| --- | --- |
| [architecture.md](architecture.md) | Index page for the architecture specification; the per-section files below are the source of truth. |
| [01-problem-statement](architecture/01-problem-statement.md) | What Sortie automates and the orchestrator/agent split of responsibility. |
| [02-goals-and-non-goals](architecture/02-goals-and-non-goals.md) | In-scope goals and explicit non-goals of the system. |
| [03-system-overview](architecture/03-system-overview.md) | Main components, the six abstraction layers, and their dependency rule. |
| [04-core-domain-model](architecture/04-core-domain-model.md) | Domain entities, their fields, and identifier normalization rules. |
| [05-workflow-specification](architecture/05-workflow-specification.md) | `WORKFLOW.md` discovery, front-matter schema, and prompt-template contract. |
| [06-configuration-specification](architecture/06-configuration-specification.md) | Config source precedence, dynamic reload, preflight validation, and cheat sheet. |
| [07-orchestration-state-machine](architecture/07-orchestration-state-machine.md) | Issue orchestration states, run-attempt lifecycle, and recovery rules. |
| [08-polling-scheduling-and-reconciliation](architecture/08-polling-scheduling-and-reconciliation.md) | Poll loop, candidate selection, concurrency caps, retries, and reconciliation. |
| [09-workspace-management-and-safety](architecture/09-workspace-management-and-safety.md) | Workspace layout, retention, hooks, `.sortie` artifacts, and safety invariants. |
| [10-agent-adapter-contract](architecture/10-agent-adapter-contract.md) | Agent adapter interface, session lifecycle, event types, and error mapping. |
| [11-issue-tracker-integration-contract](architecture/11-issue-tracker-integration-contract.md) | Required tracker operations, normalization, error contract, and write boundary. |
| [12-ci-feedback-contract](architecture/12-ci-feedback-contract.md) | `CIStatusProvider` interface and CI feedback integration in the reconcile loop. |
| [13-pr-review-comment-feedback-contract](architecture/13-pr-review-comment-feedback-contract.md) | SCM read interface for human review comments, fingerprinting, and debounce. |
| [14-auto-merge-reaction-contract](architecture/14-auto-merge-reaction-contract.md) | SCM write surface, merge preconditions, and token-scope preflight. |
| [15-bot-review-comment-feedback-contract](architecture/15-bot-review-comment-feedback-contract.md) | Bot-author classification and non-debounced bot review dispatch. |
| [16-merge-conflict-reaction-contract](architecture/16-merge-conflict-reaction-contract.md) | Conflict detection, episodic retry, and escalation for merge conflicts. |
| [17-prompt-construction-and-context-assembly](architecture/17-prompt-construction-and-context-assembly.md) | Template inputs, rendering rules, and retry/continuation semantics. |
| [18-logging-status-and-observability](architecture/18-logging-status-and-observability.md) | Logging conventions, runtime snapshot, metrics, and the HTTP status surface. |
| [19-failure-model-and-recovery-strategy](architecture/19-failure-model-and-recovery-strategy.md) | Failure classes, restart recovery, and operator intervention points. |
| [20-security-and-operational-safety](architecture/20-security-and-operational-safety.md) | Trust boundaries, filesystem safety, and secret handling. |
| [21-reference-algorithms](architecture/21-reference-algorithms.md) | Pseudocode for startup, the poll tick, dispatch, and worker retry handling. |
| [22-test-and-validation-matrix](architecture/22-test-and-validation-matrix.md) | Expected test coverage per area and the integration profile. |
| [23-implementation-checklist](architecture/23-implementation-checklist.md) | Definition of done: conformance requirements and operational validation. |
| [24-persistence-schema](architecture/24-persistence-schema.md) | SQLite schema, table definitions, and migration strategy. |
| [25-webhook-support](architecture/25-webhook-support.md) | Webhook-ingress design held as a future extension. |
| [26-agent-authored-workspace-files](architecture/26-agent-authored-workspace-files.md) | `.sortie/status` and `.sortie/review_verdict.json` artifacts left by the agent. |
| [27-appendix-a-ssh-worker-extension](architecture/27-appendix-a-ssh-worker-extension.md) | Optional remote-execution model for workers over SSH. |
| [28-label-command-and-read-only-review-contract](architecture/28-label-command-and-read-only-review-contract.md) | PR label-command detection and the read-only review / read-write fix dispatches. |
| [29-merge-completion-reaction-contract](architecture/29-merge-completion-reaction-contract.md) | Post-merge continuation trigger, review wait, and terminal-target selection. |

## Architecture Decision Records

MADR-format records in [decisions/](decisions/); [decisions/README.md](decisions/README.md) holds the status table.

| ADR | Decision |
| --- | --- |
| [0001](decisions/0001-use-go-as-core-runtime.md) | Use Go as core runtime. |
| [0002](decisions/0002-use-sqlite-for-persistence.md) | Use SQLite for persistence. |
| [0003](decisions/0003-adapter-based-integration.md) | Use adapter interfaces for integration extensibility. |
| [0004](decisions/0004-workflow-file-format.md) | Use YAML front matter for workflow files. |
| [0005](decisions/0005-prompt-template-engine.md) | Use Go `text/template` for prompt rendering. |
| [0006](decisions/0006-use-fsnotify-for-file-watching.md) | Use `fsnotify` for filesystem event watching. |
| [0007](decisions/0007-handoff-state-and-tracker-writes.md) | Use handoff state transitions to signal agent completion. |
| [0008](decisions/0008-observability-model.md) | Use an embedded dashboard with Prometheus metrics for observability. |
| [0009](decisions/0009-mcp-stdio-sidecar-for-tool-execution.md) | Use an MCP stdio sidecar for agent tool execution. |
| [0010](decisions/0010-keep-tracker-adapter-unified.md) | Keep `TrackerAdapter` as a unified interface. |
| [0011](decisions/0011-dispatch-rule-configuration.md) | Use first-match-wins dispatch rules in `WORKFLOW.md` front matter. |
| [0012](decisions/0012-auto-merge-reaction.md) | Extend `SCMAdapter` with write methods for auto-merge reactions. |
| [0013](decisions/0013-agent-cost-budget.md) | Use cumulative per-issue token counts for the agent cost budget. |
| [0014](decisions/0014-operator-notifications.md) | Use an adapter family for operator notifications. |
| [0015](decisions/0015-pr-label-command-detection.md) | Detect PR label commands by polling the label-event journal. |
| [0016](decisions/0016-place-forge-integrations-in-one-package-per-forge.md) | Place forge integrations in one package per forge. |
| [0017](decisions/0017-close-tracker-issue-on-managed-pr-merge.md) | Close the tracker issue when a managed pull request merges. |
| [0018](decisions/0018-bound-workspace-retention-by-age.md) | Bound workspace retention by age independently of tracker state. |
| [0019](decisions/0019-keep-usage-data-on-the-host.md) | Keep usage data on the host and aggregate across instances by pull. |
| [0020](decisions/0020-withhold-handoff-on-observed-absence-of-work.md) | Withhold the handoff transition only when absence of work is observed. |
| [0021](decisions/0021-run-self-review-before-ending-on-the-completion-signal.md) | Run self-review before ending the run on the completion signal. |
| [0022](decisions/0022-release-a-parked-issue-on-a-human-gesture.md) | Release a parked issue on a human gesture in the tracker. |
| [0023](decisions/0023-scope-the-ci-verdict-to-the-current-head.md) | Scope the CI verdict to the pull request's current head. |
| [0024](decisions/0024-start-a-new-feedback-epoch-when-the-head-moves.md) | Start a new feedback epoch when the pull request head moves. |
| [0025](decisions/0025-refuse-agent-requests-only-a-human-could-answer.md) | Refuse agent requests that only a human could answer. |
| [0026](decisions/0026-re-read-issue-state-before-recording-absence-failure.md) | Re-read the issue state before recording an absence failure. |
| [0027](decisions/0027-give-the-consecutive-absence-ceiling-its-own-setting.md) | Give the consecutive-absence ceiling its own setting. |
| [0028](decisions/0028-let-the-agent-declare-that-nothing-needed-changing.md) | Let the agent declare that nothing needed changing. |
| [0029](decisions/0029-adopt-agent-client-protocol-as-a-generic-agent-transport.md) | Adopt the Agent Client Protocol as a single generic agent transport. |

## Agent Adapters

| Document | Description |
| --- | --- |
| [agent-adapter-concepts.md](agent-adapter-concepts.md) | Mental model of agent adapters: kinds, package boundaries, and the ACP qualification. |
| [agent-adapter-howto.md](agent-adapter-howto.md) | Ordered procedure for adding a new adapter kind or refreshing an existing one. |
| [claude-code-adapter-notes.md](claude-code-adapter-notes.md) | Working notes for the Claude Code adapter: model mismatches and known traps. |
| [codex-adapter-notes.md](codex-adapter-notes.md) | Working notes for the Codex adapter: process model and trust/approval collisions. |
| [copilot-adapter-notes.md](copilot-adapter-notes.md) | Working notes for the Copilot CLI adapter: session/cost model and hard-to-diagnose failures. |
| [opencode-adapter-notes.md](opencode-adapter-notes.md) | Working notes for the OpenCode adapter: why it skips the shared subprocess skeleton. |
| [kiro-adapter-notes.md](kiro-adapter-notes.md) | Working notes on Kiro CLI over both routes: the native kind and the generic ACP adapter. |
| [gemini-adapter-notes.md](gemini-adapter-notes.md) | Working notes on Gemini CLI via the generic ACP adapter and its token accounting. |
| [agent-client-protocol-adapter-notes.md](agent-client-protocol-adapter-notes.md) | Working notes for the generic ACP adapter: pinned schema artifact and runtime selection. |

## Tracker and SCM Adapters

| Document | Description |
| --- | --- |
| [jira-adapter-notes.md](jira-adapter-notes.md) | Working notes for the Jira tracker adapter: decisions and failure modes. |
| [linear-adapter-notes.md](linear-adapter-notes.md) | Working notes for the Linear tracker adapter: GraphQL costs and model mismatches. |
| [github-adapter-notes.md](github-adapter-notes.md) | Working notes for the GitHub SCM adapter: design decisions and traps. |
| [gitlab-adapter-notes.md](gitlab-adapter-notes.md) | Working notes for the GitLab SCM adapter: design decisions and traps. |
| [gitea-adapter-notes.md](gitea-adapter-notes.md) | Working notes for the Gitea SCM adapter: design decisions and traps. |

## Specifications and Reference

| Document | Description |
| --- | --- |
| [workflow-reference.md](workflow-reference.md) | Authoritative user-facing `WORKFLOW.md` syntax reference for workflow authors. |
| [file-based-tasks-spec.md](file-based-tasks-spec.md) | Informational RFC for the file-based tasks specification. |
| [agent-to-orchestrator-protocol.md](agent-to-orchestrator-protocol.md) | A2O protocol spec: out-of-band agent-to-orchestrator signaling via sentinel files. |
