# Agent2Orchestrator (A2O) Protocol

A specification for out-of-band advisory signaling between autonomous coding agents
and the Sortie orchestration service via filesystem sentinel files.

**Version:** 1.0 \
**Status:** Normative \
**Audience:** Implementers of orchestrator readers, agent adapter writers, workflow authors, and
coding agent runtimes seeking to participate in Sortie-managed execution.

---

## Abstract

When an orchestration service manages the lifecycle of autonomous coding agent sessions, a
fundamental information asymmetry arises: the orchestrator controls dispatch, retry, and
termination, yet it lacks direct visibility into the agent's internal assessment of task
feasibility. Without an explicit feedback channel, the orchestrator must infer agent progress
solely from external tracker state, leading to wasteful continuation retry cycles that consume
compute resources and API quota without advancing the task.

This document specifies a file-based advisory protocol that resolves this asymmetry. The protocol
enables any coding agent — regardless of runtime, SDK availability, or tool-calling
capability — to transmit a structured signal to the orchestrator by writing a single
plain-text file to the workspace filesystem.

The design is informed by analysis of six candidate signaling mechanisms evaluated against the
constraints of agent-agnostic orchestration in a fragmented ecosystem where five or more major
coding agent platforms maintain distinct capability profiles and configuration cultures
[4, 6, 8]. The file-based approach was selected for its universal compatibility, fail-safe
degradation properties, and alignment with established Unix IPC patterns [11].

## 1. Introduction

### 1.1 Problem statement

Sortie is a long-running orchestration service that continuously reads work items from an issue
tracker, creates isolated per-issue workspaces, and executes coding agent sessions within those
workspaces. After each agent turn completes, the orchestrator must decide whether to continue
with another turn, schedule a retry, or release the work item. This decision currently relies on
two signals: the exit status of the agent process and the issue's state in the external tracker.

Neither signal captures the case where an agent has completed its turn normally but has determined
that further automated work is futile — for example, when required credentials are missing, a
human architectural decision is needed, or the task specification is ambiguous. In such cases, the
orchestrator schedules continuation retries that the agent will repeatedly fail, burning tokens and
API capacity. Hassan et al. [5] identify this as a manifestation of the "speed vs. trust" gap in
agentic software engineering: agents produce output at high velocity, but a significant fraction
is not merge-ready, and the orchestration layer lacks the signal fidelity to distinguish
productive from futile execution.

### 1.2 Requirements

The feedback channel must satisfy the following constraints, derived from Sortie's architectural
principles (architecture Section 1, Section 2):

1. **Agent-agnostic.** The mechanism must not depend on any specific agent runtime's capabilities.
   Any process capable of writing a file to disk must be able to participate. This rules out
   mechanisms that require MCP client support, A2A protocol stacks, or tool-calling infrastructure.

2. **Fail-safe.** Every failure mode must degrade to normal orchestrator behavior. A missing file,
   a corrupted file, an unrecognized value, or a read error must all result in the orchestrator
   proceeding as if no signal were present.

3. **Advisory, not authoritative.** The signal informs the orchestrator's decision but does not
   control it. The orchestrator retains full authority over dispatch, retry, and termination. An
   agent cannot commandeer orchestrator control flow by writing to this file.

4. **Zero dependencies.** No SDK, no network stack, no runtime library. The protocol must be
   implementable with a single shell command.

5. **Forward-compatible.** New signal values may be added in future versions without breaking
   existing orchestrators. Existing values never change meaning and are never removed.

6. **Inspectable.** An operator must be able to determine the current signal state with standard
   Unix tools (`cat`, `ls`).

### 1.3 Scope

This document specifies:

- The canonical file path, format, and parsing rules for the status file.
- The recognized vocabulary in version 1 and the orchestrator's behavioral response to each value.
- Read timing, write responsibility, cleanup obligations, and idempotency guarantees.
- The relationship to tracker-mediated state transitions and handoff mechanisms.
- Auto-injection of protocol instructions into agent prompts.
- Versioning and extensibility rules.

This document does not specify:

- The internal implementation of the orchestrator's retry or dispatch logic (see architecture
  Sections 7, 8, and 16).
- The mechanism by which coding agents decide to write a status signal. That is an agent-internal
  concern.
- Future extensions to the `.sortie/` namespace beyond the `status` file.

### 1.4 Relationship to existing protocols

The protocol occupies a distinct niche in the 2025–2026 landscape of agent interoperability
standards. The Model Context Protocol (MCP) standardizes vertical agent-to-tool integration via
JSON-RPC 2.0, with over 10,000 deployed servers and 97 million monthly SDK downloads as of early
2026 [1]. The Agent-to-Agent protocol (A2A) addresses horizontal coordination between peer agents,
with 150+ participating organizations by version 0.3 [2, 3]. The Agent Communication Protocol
(ACP) from IBM and the Linux Foundation standardizes message formats for cross-platform agent
communication [9]. The Agent Network Protocol (ANP) focuses on decentralized agent discovery in
distributed networks [9].

None of these protocols addresses the specific use case of **out-of-band advisory signaling from
a managed worker process to its supervising orchestrator**. MCP solves "agent calls tool." A2A
solves "agent delegates to agent." The Sortie agent protocol solves "subordinate process advises
supervisor of feasibility assessment through an asynchronous side-channel." The closest analogy in
classical systems programming is the Unix sentinel file pattern [11] and Kubernetes exec-based
health probes, both of which use file existence or contents as a polling-compatible signal
mechanism.

### 1.5 Notation and conventions

The key words "MUST", "MUST NOT", "SHOULD", "SHOULD NOT", and "MAY" in this document are to be
interpreted as described in RFC 2119.

*Orchestrator* refers to the Sortie orchestration service. *Agent* refers to any coding agent
process executing within a Sortie-managed workspace. *Turn* refers to a single invocation of the
agent adapter's `RunTurn` operation. *Worker* refers to the orchestrator-side goroutine that
manages a sequence of turns for one issue.

## 2. Protocol specification

### 2.1 File path

The status file path is:

```
<workspace_root>/<sanitized_issue_identifier>/.sortie/status
```

Where:

- `<workspace_root>` is the configured `workspace.root` value (architecture Section 5.3.3).
- `<sanitized_issue_identifier>` is the issue identifier sanitized to the character class
  `[A-Za-z0-9._-]` (architecture Section 9.6, Invariant 3).
- `.sortie/` is a reserved directory namespace within the per-issue workspace.
- `status` is the canonical filename.

The `.sortie/` directory is not created by the orchestrator. The agent creates it as needed.

Relative to the agent's working directory (which MUST equal the per-issue workspace path per
architecture Section 9.6, Invariant 1), the file path is:

```
.sortie/status
```

### 2.2 File format

The status file is a UTF-8 encoded plain-text file. The parsing algorithm is:

1. Read the file contents as a byte sequence.
2. Split on the newline character (`0x0A`).
3. Take the first line.
4. Trim leading and trailing ASCII whitespace (`0x09`, `0x0A`, `0x0D`, `0x20`).
5. The resulting string is the **status token**.

Lines after the first are reserved for future use and MUST be ignored by version 1 readers. This
reservation establishes an upgrade path to multi-line formats (e.g., structured context) without
breaking backward compatibility.

The status token is case-sensitive. Implementations MUST NOT normalize case.

### 2.3 Recognized values (version 1)

Version 1 defines two status tokens:

#### 2.3.1 `blocked`

The agent has determined that it cannot make further meaningful progress on the task without
external intervention. Typical causes include: missing credentials or environment prerequisites,
ambiguous task specifications requiring human clarification, dependencies on work outside the
agent's scope, or repeated failures on the same operation.

**Orchestrator behavior:** The orchestrator treats this as a **soft stop**. It completes the
current turn normally, then breaks the turn loop — no further continuation turns execute within
the current worker run. On worker exit, the orchestrator MUST NOT schedule a continuation retry
(architecture Section 8.4). The claim on the issue is released. If the issue subsequently returns
to an active state (e.g., after a human updates it), normal dispatch eligibility resumes.

To clarify the two distinct suppression effects:

- **Continuation turns** (turns 2..N within a single worker run): the turn loop breaks
  immediately after reading the recognized status token. The worker does not proceed to the next
  turn.
- **Continuation retries** (new worker runs scheduled after a normal worker exit): the
  post-exit retry handler skips scheduling. The issue is not re-dispatched until its tracker
  state changes.

#### 2.3.2 `needs-human-review`

The agent has completed its work and determined that the results require human review before
further automated action is appropriate. Typical causes include: a pull request is ready for
review, architectural decisions are embedded in the output that require validation, or the agent
has low confidence in its solution.

**Orchestrator behavior:** Like `blocked`, this value triggers a soft stop: the turn loop breaks,
continuation retries are suppressed, and the issue claim is released. Unlike `blocked`, when
`tracker.handoff_state` is configured (architecture Section 5.3.1) and the issue is in an active
tracker state, the orchestrator performs the handoff transition before releasing the claim. If the
handoff transition fails (network error, permission denied, nil adapter), the orchestrator logs a
warning and releases the claim without scheduling a retry.

This distinction reflects the semantic difference between the two values: `blocked` means "I
cannot proceed" (no completed work to hand off), while `needs-human-review` means "work is
complete, ready for review" (completed work should be visible in the tracker via handoff).

#### 2.3.3 Common orchestrator response

For both recognized values, the orchestrator:

1. Completes the current turn normally (does not abort mid-turn).
2. Breaks the turn loop (no further continuation turns in this worker run).
3. Exits the worker run with a normal exit status.
4. Does NOT schedule a continuation retry (new worker run).
5. Releases the issue claim.
6. Logs the status token value at `info` level with the issue identifier.

Additionally, for `needs-human-review` only: when `tracker.handoff_state` is configured and the
issue is in an active tracker state, the orchestrator attempts the handoff transition between
steps 3 and 5. See Section 2.3.2 for failure handling.

The issue becomes eligible for re-dispatch only when a subsequent tracker poll detects a
state change.

#### 2.3.4 Last-state-wins semantics

The orchestrator reads the status file once, after the turn completes (Section 3.1). The file's
contents at the moment of that read determine the signal. If the agent writes `blocked` early in
the turn and later deletes the file or overwrites it with an empty string before the turn ends,
the orchestrator sees the final state (absent or empty) and proceeds with default behavior.
Conversely, if the agent writes `blocked` as its last action in the turn, that value is what the
orchestrator reads.

### 2.4 Absent file

If the `.sortie/status` file does not exist, the orchestrator proceeds with its default behavior:
continuation retries are scheduled according to the standard algorithm (architecture Section 8.4).

File absence is the expected state during normal productive execution. The protocol follows the
Kubernetes health probe principle: absence of a failure signal is treated as healthy [see
Section 3.3].

### 2.5 Unrecognized values

If the status token is present but does not match any value defined in Section 2.3, the
orchestrator MUST ignore it and proceed as if the file were absent. A warning-level log entry
SHOULD be emitted with the unrecognized value and issue identifier.

This rule is the foundation of the protocol's forward compatibility. Newer agents may write
values that older orchestrators do not understand; the safe default is to ignore the unknown
and continue.

### 2.6 Read errors

If the orchestrator encounters any error while reading the status file (permission denied,
I/O error), it MUST:

1. Log a warning with the error details and issue identifier.
2. Treat the file as absent (proceed with default behavior).

Read errors MUST NOT cause the worker run to fail. The protocol is advisory; its unavailability
does not affect core orchestration correctness.

Non-UTF-8 or binary content is not a read error. The parsing algorithm (Section 2.2) operates on
raw bytes and does not validate encoding. Binary content that survives the first-line split
produces a token that will not match any recognized value and is handled as an unrecognized token
(Section 2.5).

## 3. Operational semantics

### 3.1 Read timing

The orchestrator reads the status file **after each completed turn**, before making the
continuation-turn or retry decision. The read occurs in the worker goroutine, within the turn
loop described in architecture Section 16.5.

The read-after-turn timing eliminates race conditions between agent writes and orchestrator
reads: the agent's turn has completed and the agent process is no longer writing before the
orchestrator reads the file.

#### 3.1.1 Placement relative to tracker state refresh

The status file read is placed **before** the tracker state refresh. This ordering is deliberate:

1. The status file represents the agent's self-assessment: "I cannot make progress." This signal
   is available immediately after the turn completes, with no network call.
2. The tracker state refresh requires a network round-trip to the issue tracker API. Performing
   the status file check first avoids a wasted API call when the agent has already signaled that
   further work is futile.
3. If the agent writes `blocked` and the issue is simultaneously in a terminal tracker state,
   the soft-stop path (no continuation retry, release claim) is the correct outcome for both
   conditions. No information is lost by short-circuiting before the tracker refresh.

The tracker state refresh remains authoritative for detecting external state changes (e.g., a
human moved the issue to "Done" while the agent was running). That check executes on every turn
where the status file does not trigger a soft stop.

Pseudo-code placement within the worker turn loop (extending architecture Section 16.5):

```
while true:
    prompt = build_turn_prompt(...)
    turn_result = agent_adapter.run_turn(session, prompt, issue, on_message)

    if turn_result failed:
        stop_session(); fail_worker("agent turn error")

    // --- STATUS FILE READ POINT ---
    status = read_sortie_status(workspace.path)
    if status in ["blocked", "needs-human-review"]:
        log_info("agent signaled status", issue.id, status)
        stop_session()
        run_hook_best_effort("after_run", workspace.path)
        exit_normal_soft_stop(status)    // breaks turn loop; exit handler differentiates (Section 3.6)
    // --- END STATUS FILE READ ---

    refreshed_issue = tracker.fetch_issue_states_by_ids([issue.id])
    ...
```

### 3.2 Write responsibility

The **agent** is the sole writer of the `.sortie/status` file.

The **orchestrator** MUST NOT write to or modify the `.sortie/status` file during a worker run.
The orchestrator's only file-system operation on this path is the pre-dispatch cleanup
(Section 3.4) and the post-turn read (Section 3.1).

The agent creates the `.sortie/` directory and status file as needed. The write is a simple
file creation or overwrite:

```sh
mkdir -p .sortie && echo "blocked" > .sortie/status
```

This single shell command is the complete agent-side protocol implementation. No SDK, library,
or special tooling is required.

### 3.3 Safe default semantics

The protocol is designed so that every failure mode degrades to normal orchestrator behavior:

| Condition | Orchestrator behavior |
|---|---|
| File does not exist | Normal (continue/retry as usual) |
| File exists, recognized value | Honor the signal (Section 2.3) |
| File exists, unrecognized value | Normal (ignore, warn) |
| File exists, empty after trimming | Normal (empty string is unrecognized) |
| File unreadable (permission, I/O) | Normal (warn, treat as absent) |
| File contains binary/non-UTF-8 data | Normal (first-line parse yields unrecognized token) |
| `.sortie/` directory does not exist | Normal (file does not exist) |

This exhaustive fail-safe property is a deliberate design choice. In a system managing autonomous
agents whose behavior is not fully predictable, every advisory channel must degrade gracefully.
The file-based sentinel is the only mechanism among the six alternatives evaluated (Section 5)
where all failure modes are unconditionally safe.

### 3.4 Workspace cleanup

The orchestrator MUST delete the `.sortie/status` file, if it exists, **before each new
dispatch** to a workspace. This prevents stale status signals from a previous run from affecting
the new dispatch.

The deletion occurs during the worker run initialization, **before** the `before_run` hook
executes and before the first agent turn begins. This ordering ensures that `before_run` hook
scripts may write to `.sortie/status` as a pre-condition gate (e.g., a CI readiness check) without
their output being erased. The cleanup targets only the `status` file; the `.sortie/` directory
itself is left intact.

In the worker lifecycle (architecture Section 16.5), the cleanup slot is:

1. Workspace directory ensured (`workspace_manager.create_for_issue`).
2. **Status file cleanup** (this section).
3. `before_run` hook.
4. Agent session start.
5. First turn.

The cleanup operation MUST apply the same symlink rejection as the read path (Section 7.2):
before deleting, verify via `Lstat` that neither `.sortie/` nor `status` is a symbolic link. If
a symlink is detected, log a warning and skip the deletion — do not follow the link.

If the deletion fails (e.g., the file was already removed, or the `.sortie/` directory does not
exist), the error is logged at debug level and ignored.

```
function pre_dispatch_cleanup(workspace_path):
    status_path = workspace_path / ".sortie" / "status"
    if any component of status_path is a symlink (Lstat check):
        log_warn("symlink detected in .sortie path, skipping cleanup", workspace_path)
        return
    err = remove(status_path)
    if err and err is not "file not found":
        log_debug("status file cleanup failed", workspace_path, err)
```

### 3.5 Idempotency

The re-dispatch and re-block cycle is an expected operational pattern:

1. Agent writes `blocked` to `.sortie/status`.
2. Orchestrator reads the signal, exits normally, releases claim.
3. A human updates the issue in the tracker (adds information, changes state).
4. Next tracker poll detects the issue is active and eligible.
5. Orchestrator dispatches a new worker run for the issue.
6. Pre-dispatch cleanup (Section 3.4) removes the stale `.sortie/status` file.
7. Agent begins fresh work. If still blocked, it writes `blocked` again.

The polling interval provides natural rate limiting for this cycle. No additional idempotency
mechanism is required.

### 3.6 Interaction with tracker handoff state

The `.sortie/status` file protocol and the `tracker.handoff_state` configuration
(architecture Section 5.3.1) are **complementary mechanisms** that interact during the worker
exit phase. The status file value determines whether the handoff transition fires:

| `.sortie/status` value | Worker exit | Handoff transition | Continuation retry |
|---|---|---|---|
| `needs-human-review` | Normal | Performed (if configured and issue is active) | Suppressed |
| `blocked` | Normal | Skipped | Suppressed |
| absent or unrecognized | Normal | Performed (if configured and issue is active) | Depends on handoff result |
| (any) | Error | Skipped | Standard error retry |

The semantic distinction drives the difference: `blocked` means the agent cannot proceed, so
there is no completed work to hand off. `needs-human-review` means the agent completed its work
and the issue should move to a review state in the tracker.

Both values suppress continuation retries and release the issue claim. The handoff transition is
the only behavioral divergence between them.

When a `handoff_state` is configured and the agent writes `blocked`, the orchestrator skips the
handoff transition entirely. The issue remains in its current tracker state. This is correct:
blocked work should not advance in the tracker.

When a `handoff_state` is configured and the agent writes `needs-human-review`, the orchestrator
attempts the handoff transition. On success, the issue moves to the configured handoff state. On
failure (network error, permission denied, nil adapter), the orchestrator logs a warning and
releases the claim without scheduling a retry.

## 4. Prompt integration

### 4.1 Auto-injection of protocol instructions

The orchestrator SHOULD inject a brief protocol description into the agent's prompt so that the
agent is aware of the signaling mechanism. This injection:

- Applies to the **first turn only** of each worker run.
- Is appended **after** the workflow template has been rendered and **after** any other
  first-turn suffixes (e.g., tool advertisement). The status protocol instructions appear last
  in the composed prompt.
- Is not applied to continuation turns (the instruction is already in the agent's conversation
  history from turn 1).

The ordering of first-turn suffixes is:

1. Rendered workflow template.
2. Tool advertisement (if a tool registry is configured).
3. Status protocol instructions (this section).

This ordering places the protocol instructions at the end, closest to the agent's point of
attention, and avoids interleaving with tool documentation.

The injected text SHOULD be concise and imperative. It tells the agent what the file is, when to
write it, and the exact command to use. It does not explain the orchestrator's internal logic.

Example injection text:

```
If you determine that you cannot make further progress on this task without human
intervention, or if your work is complete and requires human review, signal the
orchestrator by running:

    mkdir -p .sortie && echo "blocked" > .sortie/status

Use "blocked" when you cannot proceed. Use "needs-human-review" when your work is
complete and awaiting review. Do not write this file during normal productive work.
```

### 4.2 Prompt injection safety

The auto-injected text is a fixed string controlled by the orchestrator, not derived from
untrusted input (issue descriptions, comments, or labels). It therefore does not introduce
prompt injection risk beyond what already exists in the workflow template.

Workflow authors MAY include their own `.sortie/status` instructions in the workflow template.
If both the workflow template and the auto-injection contain protocol instructions, the
auto-injected text appears after the template text. Duplicate instructions are harmless; they
reinforce the signal.

## 5. Design rationale

This section summarizes the architectural alternatives evaluated during the design of this
protocol and the reasoning behind the selected approach.

### 5.1 Alternatives considered

Six signaling mechanisms were evaluated against the requirements in Section 1.2:

**Alternative 1: Tracker-mediated signaling.** The agent uses tracker API tools to transition
the issue to a handoff state; the orchestrator detects the change on the next poll cycle. This
is the approach used by Symphony (OpenAI) with Linear [15] and GitHub Copilot Agent Mode with the
GitHub API. It was rejected because it requires tool-calling capability in the agent runtime,
couples the signaling mechanism to a specific tracker, and conflates the advisory signal ("I am
blocked") with an authoritative state transition in the tracker.

**Alternative 2: MCP server side-channel.** The orchestrator hosts an MCP server exposing a
`set_status` tool; the agent calls it via JSON-RPC. Adoption analysis suggests that advanced MCP
mechanisms beyond basic tool calls remain "shallowly adopted," with 1–2 artifacts per repository
in a study of 2,926 GitHub projects [4]. This alternative was rejected because it requires MCP
client support in the agent runtime, violates the agent-agnostic constraint, and introduces MCP
server lifecycle management complexity.

**Alternative 3: A2A protocol integration.** The orchestrator implements a Google A2A server;
the agent sends task status updates via A2A. While the protocol has reached 150+ participating
organizations [2, 3], it was designed for inter-agent peer communication, not for worker-to-
supervisor advisory signaling. Current coding agent runtimes (Claude Code, Codex CLI, Copilot
CLI) are not A2A clients. The implementation cost (HTTP server, JSON-RPC stack, SSE streaming)
constitutes massive overengineering for a single-token advisory signal.

**Alternative 4: Unix domain socket / named pipe.** The orchestrator opens a socket in the
workspace; the agent writes a message to it. Rejected due to platform dependence (UDS on
Linux/macOS, named pipes on Windows), lack of persistence (data lost if the orchestrator is not
listening), and the requirement for agents to connect to a socket rather than merely write a file.

**Alternative 5: Environment variable / return code.** The agent sets an exit code or environment
variable. Rejected because LLM-based agents cannot control process exit codes, environment
variables do not cross process boundaries (child cannot set parent's environment), and the
mechanism offers no persistence.

### 5.2 Decision matrix

| Criterion | Tracker | MCP | File | A2A | Socket | Exit code |
|---|---|---|---|---|---|---|
| Agent-agnostic | No | No | **Yes** | No | No | No |
| Zero dependencies | No | No | **Yes** | No | No | Yes |
| Fail-safe default | Partial | No | **Yes** | No | No | Partial |
| Persistence | Yes | No | **Yes** | Yes | No | No |
| Inspectability | Medium | Low | **High** | Medium | Low | Low |
| Forward compatible | Tracker-dep. | Schema ver. | **Ignore-unknown** | Spec ver. | Protocol-dep. | Fixed |
| Implementation cost | Low | Medium | **Low** | High | Medium | Low |

The file-based sentinel is the only mechanism that satisfies all six requirements from
Section 1.2 simultaneously.

### 5.3 Precedent in systems engineering

The file-based sentinel pattern has extensive precedent in production systems:

- **Unix PID files.** Programs write their process identifier to a well-known path (typically
  `/var/run/<name>.pid`) so that other processes can discover and signal them. Raymond [11]
  describes this as a standard filesystem-based IPC pattern.

- **Makefile sentinel targets.** Build systems use empty marker files to track completion of
  multi-step build phases, avoiding redundant work on subsequent invocations.

- **Kubernetes exec probes.** The `kubelet` determines container health by executing a command
  that checks for the existence of a file (e.g., `/tmp/healthy`). The probe model is polling-
  based, fail-safe on absence, and requires zero infrastructure beyond the filesystem.

- **Agent orchestration systems.** OpenCode uses `.orchestra/tasks/{id}.done` and
  `.orchestra/tasks/{id}.error` as per-task sentinel files for agent-to-orchestrator signaling.

### 5.4 Theoretical grounding

The protocol's design aligns with several principles from the distributed systems and agent
orchestration literature:

**Loose coupling.** Galster et al. [4] find that in a study of 2,926 GitHub repositories,
context files (Markdown documents) dominate as the primary configuration mechanism for agentic
coding tools, while "advanced mechanisms such as Skills and Subagents are only shallowly adopted."
This confirms that the lowest-common-denominator approach (plain-text files) achieves maximum
adoption in a fragmented ecosystem.

**Observable signaling.** The "Codified Context" framework [7] describes a three-tier
infrastructure for AI agents in complex codebases, where routing between tiers occurs through
observable signals. The `.sortie/status` file serves as an observable signal at the orchestration
tier, consistent with this architectural pattern.

**Minimal orchestration complexity.** Google's agent white paper (2025) argues that orchestration
should begin with a perception-reasoning-action loop of minimal complexity. The protocol adheres
to this principle: one file, one token, read-after-turn.

**Traceability.** Wang et al.'s work on OpenHands demonstrates that explicit event-stream
mechanisms provide full traceability of agent actions. The status file is a minimal instance
of the same principle: an explicit, logged, inspectable signal that becomes part of the run
history.

## 6. Versioning and extensibility

### 6.1 Version identification

This document specifies version 1 of the protocol. The protocol version is implicit in the set
of recognized status tokens. There is no explicit version field in the file format.

### 6.2 Evolution rules

The following rules govern protocol evolution:

1. **New values MAY be added** in future versions. Each new value must be accompanied by a
   specification of the orchestrator's behavioral response.

2. **Existing values MUST NOT change meaning.** The semantics of `blocked` and
   `needs-human-review` as defined in Section 2.3 are permanent.

3. **Values MUST NOT be removed.** An orchestrator must recognize all values from all previous
   protocol versions.

4. **Unrecognized values are always ignored.** This is the core forward-compatibility mechanism.
   A version-1 orchestrator encountering a version-2 token degrades gracefully to default
   behavior.

5. **The file format (Section 2.2) is fixed.** Future versions may assign semantics to lines
   beyond the first, but the first-line parsing algorithm does not change.

### 6.3 Reserved namespace

The `.sortie/` directory within the workspace is reserved for orchestrator-agent communication
files. Future extensions may define additional files in this namespace (e.g., `.sortie/metrics`,
`.sortie/log`). Each new file requires its own specification addendum.

### 6.4 Multi-line extension path

Version 1 ignores all lines after the first. A future version may define a structured format
for additional lines:

```
blocked
reason: missing API key STRIPE_SECRET_KEY
context: checked env, checked .env, checked vault
```

Version 1 orchestrators encountering this file will correctly parse the first line as `blocked`
and ignore the remaining lines. This provides a non-breaking upgrade path to richer signaling
without changing the wire format.

## 7. Security considerations

### 7.1 Agent control boundary

The protocol is advisory by design. An agent cannot force the orchestrator to take any specific
action by writing to `.sortie/status`. The orchestrator's behavioral response to recognized
values (suppress continuation retries) is a deliberate choice by the orchestrator, not a command
from the agent.

A malicious or malfunctioning agent that writes `blocked` to every workspace will cause the
orchestrator to stop retrying those issues. This is the correct behavior: an agent that claims
to be blocked should not be retried. The operational remedy is the same as for any malfunctioning
agent: investigate via logs and observability, fix the agent configuration, and re-dispatch
manually.

### 7.2 Path containment

The `.sortie/status` file resides within the per-issue workspace directory, which is itself
contained under `workspace.root` (architecture Section 9.6). The orchestrator reads only this
specific path. No user-controlled input influences the path construction beyond the issue
identifier, which is sanitized to `[A-Za-z0-9._-]`.

The orchestrator MUST NOT follow symbolic links when reading the status file. A symlink at the
status file path, the `.sortie/` directory, or any intermediate component MUST be treated as a
read error (Section 2.6): log a warning, treat as absent.

This requirement extends the workspace safety model (architecture Section 9.6) into the
`.sortie/` namespace. Existing workspace path validation covers the workspace root and per-issue
directory; the symlink check here adds coverage for files created by the agent inside the
workspace. Implementation requires `Lstat` checks on the path components leading to the status
file.

### 7.3 Denial of service

An agent that rapidly creates and overwrites the status file cannot degrade orchestrator
performance because the orchestrator reads the file at most once per turn, and turn frequency
is bounded by `agent.turn_timeout_ms`. No filesystem watch or event-driven mechanism is employed;
the read is strictly poll-based at the turn boundary.

### 7.4 Information leakage

The status file contains a single token from a fixed vocabulary. It does not carry secrets,
credentials, or sensitive data. The token is logged by the orchestrator; this logging does not
create an information leakage vector.

## 8. Implementation guidance

### 8.1 Orchestrator reader (Go)

The orchestrator reader is a function called within the worker turn loop. It MUST:

1. Construct the status file path from the workspace path.
2. Validate that the constructed path is contained within the workspace root.
3. Attempt to read the file. On any error (including "not found"), return an empty status.
4. Parse the first line per Section 2.2.
5. Return the status token to the caller.

The caller (worker turn loop) compares the token against recognized values.

### 8.2 Agent writer

Any agent runtime that wishes to participate in the protocol MUST:

1. Create the `.sortie/` directory if it does not exist.
2. Write the status token followed by a newline to `.sortie/status`.
3. Use atomic write semantics where available (write to temporary file, rename). This is
   recommended but not required, because the orchestrator reads only after the turn completes,
   at which point the agent is no longer writing.

Minimal implementation:

```sh
mkdir -p .sortie && echo "blocked" > .sortie/status
```

### 8.3 Operator inspection

Operators can inspect the current status signal for any workspace:

```sh
cat /path/to/workspace/.sortie/status
```

A missing file or empty output indicates no advisory signal (normal operation). The presence
of `blocked` or `needs-human-review` indicates the agent's last assessment.

## 9. Conformance

An implementation conforms to this specification if it satisfies all of the following:

1. The orchestrator reads `.sortie/status` after each completed turn, before the retry decision.
2. The orchestrator recognizes `blocked` and `needs-human-review` per Section 2.3.
3. The orchestrator ignores unrecognized values per Section 2.5.
4. The orchestrator handles read errors per Section 2.6.
5. The orchestrator deletes `.sortie/status` before each new dispatch per Section 3.4.
6. The orchestrator never writes to or modifies `.sortie/status` during a worker run.
7. The status file does not trigger tracker state transitions per Section 3.6.
8. Symbolic links at any path component are treated as read errors per Section 7.2.
9. Pre-dispatch cleanup applies the same symlink rejection as reads per Section 3.4.

## References

[1] Bridging Protocol and Production: Design Patterns for Deploying AI Agents with MCP.
    arXiv:2603.13417, 2026.

[2] Agent2Agent Protocol Specification. a2a-protocol.org, v0.2.5–v0.3, 2025.

[3] Google Cloud Blog. "Agent2Agent protocol is getting an upgrade." July 2025.

[4] Galster, M. et al. "Configuring Agentic AI Coding Tools: An Exploratory Study."
    arXiv:2602.14690, 2026.

[5] Hassan, A. E. et al. "Agentic Software Engineering: Foundational Pillars and a
    Research Roadmap." arXiv:2509.06216, 2025.

[6] Santos, E. et al. "Decoding the Configuration of AI Coding Agents: Insights from
    Claude Code Projects." arXiv:2511.09268, 2025.

[7] "Codified Context: Infrastructure for AI Agents in a Complex Codebase."
    arXiv:2602.20478, 2026.

[8] Chatlatanagulchai, W. et al. "On the Use of Agentic Coding Manifests: An Empirical
    Study of Claude Code." arXiv:2509.14744, 2025.

[9] Ehtesham, A. et al. "A Survey of Agent Interoperability Protocols: MCP, ACP, A2A,
    and ANP." arXiv, 2025.

[10] Anbiaee, S. et al. "Security Threat Modeling for Emerging AI-Agent Protocols."
     arXiv:2602.11327, 2026.

[11] Raymond, E. S. *The Art of Unix Programming.* Ch. 7: Multiprogramming, Taxonomy of
     Unix IPC Methods. 2003.

[12] "Four Design Patterns for Event-Driven Multi-Agent Systems." Confluent / InfoWorld,
     January 2025.

[13] Orogat, L. et al. "MAFBench: A Unified Benchmark for Multi-Agent Frameworks."
     arXiv, 2026.

[14] "A Comprehensive Empirical Evaluation of Agent Frameworks on Code-centric Software
     Engineering Tasks." arXiv:2511.00872, 2025.

[15] Osmani, A. "Conductors to Orchestrators: The Future of Agentic Coding."
     O'Reilly Radar / addyosmani.com, 2025–2026.

[16] "A Practical Guide for Designing, Developing, and Deploying Production-Grade
     Agentic AI Workflows." arXiv:2512.08769, 2025.

[17] Anthropic. "2026 Agentic Coding Trends Report." resources.anthropic.com, 2026.

## Acknowledgments

The protocol design was informed by operational experience with Symphony (OpenAI), analysis of
the MCP and A2A protocol ecosystems, and the empirical studies of agentic coding tool
configuration by Galster et al. [4], Santos et al. [6], and Chatlatanagulchai et al. [8].
