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

### 15.6 Workspace Content as Agent Configuration

Some agent runtimes read configuration out of the working directory they are handed, and start
helper processes from what they find there. Sortie creates a workspace, checks repository content
into it, and gives that directory to the agent as its working directory, so repository content
reaches the runtime's configuration loader. Content that arrives with a checkout can therefore
shape how the agent runs, and on a runtime that launches processes from configuration it can
execute on the host under the account Sortie runs as.

Processes an agent runtime starts on its own behalf are not confined by that agent's sandbox
setting. The sandbox governs commands the agent executes; it does not govern the transports and
helper processes the runtime launches to serve the session. Nor does it govern whether workspace
configuration is read at all. A runtime that gates that reading on a recorded trust decision
consults the sandbox only when deciding whether to record trust for a path it has not seen
before, so a restrictive sandbox prevents the decision rather than the loading. On a path that
has never been trusted it therefore does protect: no decision is recorded, so nothing project
scoped is read and no helper declared there is started. A path already carrying that decision
keeps loading its configuration, and keeps starting the processes that configuration declares,
whatever the sandbox says on the run that reaches it. The approval policy
makes no difference to any of this. Tightening the sandbox setting therefore does not close the
exposure, which is the limit of the hardening guidance above.

Sortie derives a workspace path from the issue identifier, so runs for the same unit of work
share a path. A trust decision recorded against that path outlives both the run and the workspace
content that earned it, so the first permissive run for an issue arms every later run at the same
path.

The workspace Sortie creates and hands over is the boundary that matters, and two operator-facing
controls act on it. Give the agent runtime a configuration home scoped to the run, so whatever it
records while running dies with the run instead of accumulating in the operator's own
configuration; this is demonstrated workable rather than theoretical. Control what lands in the
checkout, because the exposure is bounded by who can place a file in the checked-out tree: a
workflow that checks out contributor-supplied refs widens it well beyond one that builds only the
default branch.

Sortie launches a local agent with the orchestrator's environment, so any credential present
there is readable by an agent running with approvals disabled in a write-capable sandbox. Whether
a tracker credential is present depends on how it is supplied. A value named indirectly through
an environment variable, or supplied by an environment override, sits in the orchestrator's
environment and is inherited. A value written literally into workflow configuration does not, and
reaches the tracker client without passing through the environment, although it then sits in the
configuration file instead.

Dispatching a run to a remote host bounds this differently. Only an explicitly constructed set of
variables crosses with the command, so the remote agent inherits that set rather than the
orchestrator's environment, and the exposure is the size of that set.

The sandbox does not help in either case. It governs filesystem and network reach, not what a
process reads from the environment it was started with.

Which runtimes read workspace configuration, which files they key on, and how any of this differs
between their releases are adapter concerns and belong in the adapter notes.
