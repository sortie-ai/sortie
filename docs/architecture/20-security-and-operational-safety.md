## 15. Security and Operational Safety

### 15.1 Trust Boundary Assumption

Each deployment defines its own trust boundary.

Operational safety requirements:

- Deployments should state clearly whether they are intended for trusted environments, more
  restrictive environments, or both.
- Deployments should state clearly whether they rely on auto-approved actions, operator approvals,
  stricter sandboxing, or some combination of those controls.
- Workspace isolation and path validation are important baseline controls, but they are not a
  substitute for whatever approval and sandbox policy a deployment chooses.

### 15.2 Filesystem Safety Requirements

Mandatory:

- Workspace path must remain under configured workspace root.
- Coding-agent cwd must be the per-issue workspace path for the current run.
- Workspace directory names must use sanitized identifiers.

Recommended additional hardening:

- Run under a dedicated OS user.
- Restrict workspace root permissions.
- Mount workspace root on a dedicated volume if possible.

### 15.3 Secret Handling

- Support `$VAR` indirection in workflow config.
- Do not log API tokens or secret env values.
- Validate presence of secrets without printing them.

### 15.4 Hook Script Safety

Workspace hooks are arbitrary shell scripts from `WORKFLOW.md`.

Implications:

- Hooks are fully trusted configuration.
- Hooks run inside the workspace directory.
- Hook output should be truncated in logs.
- Hook timeouts are required to avoid hanging the orchestrator.

### 15.5 Harness Hardening Guidance

Running coding agents against repositories, issue trackers, and other inputs that may contain
sensitive data or externally-controlled content can be dangerous. A permissive deployment can lead
to data leaks, destructive mutations, or full machine compromise if the agent is induced to execute
harmful commands or use overly-powerful integrations.

Deployments should explicitly evaluate their own risk profile and harden the execution harness
where appropriate. Sortie does not mandate a single hardening posture, but deployments should not
assume that tracker data, repository contents, prompt inputs, or tool arguments are fully
trustworthy just because they originate inside a normal workflow.

Prompt injection risk: issue descriptions, comments, and labels are untrusted input that flows
directly into agent prompts. Workflow prompts should include defensive instructions to reduce the
risk of injected content manipulating agent behavior.

Possible hardening measures include:

- Tightening agent approval and sandbox settings instead of running with a maximally permissive
  configuration.
- Adding external isolation layers such as OS/container/VM sandboxing, network restrictions, or
  separate credentials beyond the built-in agent policy controls.
- Using `tracker.query_filter` to restrict which issues are eligible for dispatch
  (e.g., by label, component, epic, or other tracker-native criteria) so untrusted or
  out-of-scope tasks do not automatically reach the agent.
- Scoping the `tracker_api` tool (Section 10.4.5) so it can only read or mutate data inside the
  intended project scope, rather than exposing general tracker access.
- Reducing the set of registered tools, credentials, filesystem paths, and network destinations
  available to the agent to the minimum needed for the workflow.

The correct controls are deployment-specific, but deployments should document them clearly and
treat harness hardening as part of the core safety model rather than an optional afterthought.

