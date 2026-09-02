## 10. Agent Adapter Contract

This section defines the interface contract that all agent adapters must satisfy. Adapter-specific
protocol details (handshake sequences, JSON-RPC framing, stdio vs. HTTP transport) are documented
separately per adapter.

### 10.1 Agent Adapter Interface

An agent adapter must implement the following operations:

- `StartSession(workspace, config) -> Session`
  - Launch or connect to an agent process/service in the given workspace.
  - Returns an opaque session handle.
- `RunTurn(session, prompt, issue, on_event) -> TurnResult`
  - Execute one agent turn with the given prompt.
  - Delivers events to the orchestrator via `on_event` callback (push adapters) or returns
    them in the result (synchronous adapters).
  - Returns when the turn completes (success, failure, or timeout).
- `StopSession(session)`
  - Terminate the agent process/service cleanly.
- `EventStream() -> <event channel>`
  - Optional: adapters that push events asynchronously may expose an event channel.

Built-in agent adapter kinds:

- `claude-code`, `copilot-cli`, `codex`, `opencode`, `kiro`, `mock`, and `agent-client-protocol` are built-in agent adapter kinds.
- `claude-code`, `copilot-cli`, `opencode`, and `kiro` use one local subprocess per turn.
- `codex` uses one persistent local subprocess per session.
- `agent-client-protocol` uses one persistent local subprocess per session, in which a turn is a single open request that ends with a declared stop reason, a third session model beside the two above.
- `mock` launches no process and belongs to none of these three session models.

Built-in adapter summary:

| Kind | Session model | Event surface | Resume and identity | Notable differences |
|---|---|---|---|---|
| `claude-code` | One subprocess per turn | Newline-delimited JSON from `claude -p --output-format stream-json --verbose` | New sessions use `--session-id`; continuation turns use `--resume`; runtime `session_id` may replace the provisional adapter-generated ID | Token usage is normalized from the terminal `result` event's per-model usage, aggregated across models and added to the run total per turn. |
| `copilot-cli` | One subprocess per turn | Newline-delimited JSON from an abbreviated `copilot -p --output-format json -s --autopilot --no-ask-user ...` invocation | Uses `--resume <session_id>` when a session ID is known; falls back to `--continue` when a prior result omitted `sessionId` | The turn's outcome comes from the runtime's own task-completion report rather than from the terminal event's exit code alone; a turn that ends without that report is reported as an incomplete turn with error kind `turn_incomplete`. The adapter always includes `--max-autopilot-continues` and includes --allow-all unless copilot-cli.allowed_tools is set, in which case that allow-list replaces it, while copilot-cli.denied_tools, copilot-cli.available_tools, and copilot-cli.excluded_tools leave it in place; session identity is captured from the terminal `result` event rather than a start event. Token usage is recovered from the runtime's session-state journal after process exit, with the stream's per-message output counts standing in as the in-turn estimate; unavailable in SSH mode. A run whose session-state journal cannot be read, which includes SSH mode, is reported measured on the stream's output-token figures alone: it carries no input or cache-read component, so its recorded total is output-only. It is reported unmeasured only when no assistant message carried the output-token field either. |
| `codex` | One persistent `codex app-server` subprocess per session | JSON-RPC 2.0 over stdio | `ResumeSessionID` maps to `thread/resume`; otherwise the adapter starts a new thread; thread ID is the session ID | Turns are started inside the persistent session with `turn/start`; tool handling is the MCP sidecar the runtime spawns from configuration overrides the adapter supplies on the app-server command line, on a local launch only; approval handling is unchanged and stays part of the app-server protocol. Token usage is normalized from `thread/tokenUsage/updated`, with a per-run baseline subtracted from the thread-cumulative total. |
| `opencode` | One subprocess per turn | Line-delimited JSON from `opencode run --format json --dir <workspace>` | `ResumeSessionID` maps to `--session <session_id>`; the first observed `sessionID` becomes the session ID; a mismatch is `turn_failed` with error kind `response_error` | The adapter maps `opencode.model`, `opencode.agent`, `opencode.variant`, `opencode.thinking`, `opencode.pure`, and `opencode.dangerously_skip_permissions` to CLI flags; parses `step_start`, `text`, `reasoning`, `tool_use`, `step_finish`, and `error`; maps plain-text permission warnings to `notification` and unknown output to `malformed`; recovers final token usage with `opencode export --sanitize <session_id>`, summed across the run's assistant messages; maps logical `error` events to `turn_failed` even when the process exits with status `0`. Tool handling is the same MCP sidecar, delivered through the runtime's inline configuration environment variable, on a local launch only. |
| `kiro` | One subprocess per turn | Plain-text human transcript from `kiro-cli chat --no-interactive`; stdout carries the assistant answer (ANSI-stripped), stderr carries the `▸ Credits: … • Time: …` trailer and warnings | Headless mode does not surface a session ID; `ResumeSessionID` is recorded but continuation relies on `--resume` against the cwd-scoped conversation store keyed by the workspace path | Headless Kiro emits no structured output and no token counts, so the adapter emits no `token_usage` events and leaves `TurnResult.Usage` zero; token-based budget enforcement does not apply; `agent.turn_timeout_ms` is the wall-clock budget bound, and a silent turn is caught first by `agent.stall_timeout_ms`. Every run is reported unmeasured, because the headless runtime reports no token counts. Exit code 0 is ambiguous (success and invalid-credential failure both exit 0): success requires the credits trailer on stderr, and `Authentication failed.` on stderr with empty stdout maps to `turn_failed`. `StartSession` requires `KIRO_API_KEY` and, only in local mode and once per session, runs a `kiro-cli whoami` canary to reject silently invalid keys before any turn; a missing credential would otherwise block headless `chat` indefinitely on an interactive device-login flow, which the credential preflight forecloses and which stall detection, not the turn timeout, would otherwise end. MCP injection has no effect under `KIRO_API_KEY` auth (the backend profile gate disables MCP), so `MCPConfigPath` is ignored and `--require-mcp-startup` is unreachable. Permissions are controlled by `--trust-all-tools` or a `--trust-tools=<names>` allowlist; the two modes are mutually exclusive. The model is pinned per turn with `--model` because the `/model` slash command is unavailable headless. |
| `mock` | Launches no process | Canned events for tests | Configured directly by the test rather than recovered from a runtime | Delivers no tool servers |
| `agent-client-protocol` | One persistent local subprocess per session, in which a turn is a single open request that ends with a declared stop reason. This is a third model beside the one-subprocess-per-turn kinds and the persistent-session kind. | Newline-delimited JSON-RPC over stdio, with the runtime named by `agent.command`, and one closed set of session updates normalized at the boundary. | The session identifier comes from the session-creation response; continuation is attempted only when the agent advertised it, is confirmed by observation with a negative control, and falls back to a new session when the advertisement is absent or the observation fails. | The stable usage notification reports context occupancy rather than spend, so a run under this kind is reported unmeasured with token-based budget enforcement inactive; tool servers are re-expressed into the session-creation request and are not delivered on a remote launch; permission requests are refused inside the protocol by selecting an option of a refusing kind. |

### 10.2 Session Lifecycle

The orchestrator interacts with an agent session as follows:

1. Call `StartSession` before the first turn.
2. Call `RunTurn` for each turn. Continuation turns reuse the same session.
3. Call `StopSession` when the worker run is ending.

The session handle and any session identifiers are adapter-specific. The orchestrator treats
`session_id` as an opaque string.

#### 10.2.1 Handoff-Evidence Ownership

The handoff-evidence verdict belongs to the orchestrator. It is computed from the workspace baseline
and the positive SCM signals the orchestrator already reads, after an otherwise-eligible normal
worker exit. This decision adds no operation, event, result field, or work-classification obligation
to the agent adapter interface.

An adapter's turn disposition (§10.8) and the handoff-evidence verdict answer different questions.
The former normalizes what one runtime reported about a turn; the latter decides whether the
orchestrator observed durable output behind a tracker handoff. A successful adapter result, a
non-zero completed-turn count, token usage, or a tool call is diagnostic context only and cannot by
itself establish either work observed or absence of work.

A future adapter may contribute a positive work signal. Such a signal can only change an absence or
undeterminable case to work observed; it cannot declare absence and does not transfer ownership of
the verdict from the orchestrator. This preserves one monotone verdict across present and future
runtimes.

### 10.3 Normalized Event Types

The orchestrator expects the following event types from any agent adapter. Adapters map their
native protocol events to this normalized set:

- `session_started`: session initialized successfully
- `startup_failed`: session could not be initialized
- `turn_completed`: turn finished successfully
- `turn_failed`: turn finished with failure
- `turn_cancelled`: turn was cancelled
- `turn_ended_with_error`: turn ended due to an error condition
- `turn_input_required`: agent asked for a decision only a person could give, ending the attempt under the shared refusal posture
- `token_usage`: normalized token usage event: `{input_tokens, output_tokens, total_tokens, cache_read_tokens}`. Optional `model` field (string) identifies the LLM model when available. Optional `api_duration_ms` field (int64, milliseconds) carries per-request or per-turn API response wait time when the adapter can measure it.

  Counter definitions: `input_tokens` counts every token sent to the model, including
  prompt-cache reads and prompt-cache writes. `output_tokens` counts every token the model
  generated, including reasoning tokens. `cache_read_tokens` is the subset of `input_tokens`
  served from the prompt cache and MUST NOT be added to any other counter. `total_tokens` is
  `input_tokens + output_tokens`, computed by the adapter: an adapter MUST NOT pass a vendor
  total through, and MUST report `0` for a counter its runtime does not expose. This definition
  supersedes the clause in ADR-0013 (`docs/decisions/0013-agent-cost-budget.md`) that leaves
  `cache_read_tokens` undefined as either included in or disjoint from `total_tokens`.

  Scope: every reported value is cumulative over the run, meaning the agent session the
  orchestrator opened with `StartSession`, across every turn of that session, and excludes
  usage a resumed session accumulated before `StartSession`. The sequence of values an adapter
  reports within one run is monotonically non-decreasing per component.

  Emission rules: an adapter SHOULD emit one `token_usage` event per model API request when it
  can observe request boundaries, and MUST NOT emit more than one `token_usage` event per
  observed request. An adapter MAY carry the run-cumulative snapshot on a turn-finalization
  event (`turn_completed`, `turn_failed`, `turn_cancelled`, `turn_ended_with_error`), which the
  orchestrator accumulates without counting it as an API request. `TurnResult.Usage` MUST carry
  the same run-cumulative snapshot as the turn's last emitted value.

  Measurement semantics: emitting a `token_usage` event asserts that the runtime reported usage
  for the observed request. An all-zero payload asserts a measurement of zero, which is a
  legitimate statement distinct from having no measurement at all. An adapter MUST NOT emit the
  event when the runtime supplied no usage figure for the observed event. This qualifies, rather
  than repeals, the sentence above requiring an adapter to report `0` for a counter its runtime
  does not expose: reporting `0` for one counter inside a measurement is a statement about that
  counter, while emitting no event is a statement about the whole request.

  `TurnResult.UsageMeasured` carries the same assertion at turn granularity: it is true once the
  adapter has observed at least one usage measurement for the session, by the end of that turn,
  and false otherwise. It is monotone: once true for a session, it MUST remain true for every
  later turn of that session. An adapter that can never measure usage reports `UsageMeasured`
  false on every turn and emits no `token_usage` event. An adapter whose ability to measure
  depends on the run, rather than being permanently absent, reports the property per run rather
  than as a static adapter property.
- `tool_result`: a tool call completed. Optional fields: `tool_name` (string), `duration_ms` (int64).
- `notification`: informational message from the agent
- `other_message`: unclassified message
- `malformed`: unparseable or unrecognized message

Each event should include:

- `event` (enum/string)
- `timestamp` (UTC timestamp)
- `agent_pid` (if available)
- optional `usage` map: `{input_tokens, output_tokens, total_tokens, cache_read_tokens}`
- optional `model` string: LLM model identifier when available
- payload fields as needed

Token accounting is normalized at the adapter boundary. The orchestrator receives
`{input_tokens, output_tokens, total_tokens, cache_read_tokens}` directly and does not parse
adapter-specific payload shapes.

### 10.4 Approval, Tools, and User Input Policy

This section covers the approval posture, tool subsystem, and user-input handling for agent
sessions.

#### 10.4.1 Approval policy

Every agent run is unattended: no person is present to grant a permission or answer a question
while a run is in progress. A request for consent to act is refused in the continuable form the
runtime's own protocol offers, where the runtime offers one, so the turn can proceed by another
route; a request addressed to a person ends the attempt with the `turn_input_required` outcome.
The posture is uniform across every agent adapter and is not an operator-configurable setting.

Policy requirements:

- Each deployment enforces the same refusal posture: a permission request is refused rather than
  granted, and a request for human input ends the attempt rather than stalling.
- Approval requests and user-input-required events MUST NOT leave a run stalled indefinitely. A
  configuration value that would let the agent stop and wait for a person is refused before the
  run, rather than satisfied when it arrives mid-turn.

Unsupported tool calls:

- No adapter routes a tool call: every kind with an execution channel routes calls to the same
  MCP sidecar (§10.4.3), and the sidecar answers a name not in the `ToolRegistry` with a JSON-RPC
  error object, code `-32602`, rather than a result. A result carrying `isError` is reserved
  for a tool that was found and whose execution failed.
- The session continues after either answer. This is the channel's behavior, not adapter-level
  behavior; no adapter intercepts or routes a tool call itself.

Ending on a request only a person could answer:

- If the agent asks for something only a person could give, end the run attempt immediately with
  the `turn_input_required` outcome, distinct from a generic turn failure.

#### 10.4.2 Tool interface contract

All tools that Sortie exposes to agents implement the `AgentTool` interface:

- `Name() string`: stable tool identifier used for matching tool call requests to
  implementations. MUST be unique within a `ToolRegistry`.
- `Description() string`: human-readable summary suitable for inclusion in agent prompts and
  MCP `tools/list` responses.
- `InputSchema() json.RawMessage`: JSON Schema describing the tool's expected input. Used for
  MCP tool registration and prompt-based documentation.
- `Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)`: runs the tool
  and returns a structured JSON result. The Go `error` return is reserved for internal failures
  (nil adapter, marshal failure) that indicate programming errors.

Every tool's `Execute` MUST return the uniform result envelope. On success it MUST return
`{"success": true, "data": <payload>}`, where `<payload>` is the tool's success object. On a
domain failure (missing auth, API failures, invalid input, unreadable local state) it MUST return
`{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}`, where `kind` is from the
tool's documented closed set (Section 10.4.5) and `message` is a human-readable detail. A domain
failure is carried in this envelope with a nil Go `error`; the non-nil Go `error` return signals
only an internal marshal failure. All tools marshal both envelopes through one shared helper, so
the two shapes are byte-identical at the top level across tools.

#### 10.4.3 Tool registry

`ToolRegistry` is the central registration point for all agent tools.

Invariants:

- Registration is static at build time. All tools are registered during orchestrator
  initialization, before the first dispatch. No dynamic plugin loading.
- The registry is safe for concurrent reads after construction. Concurrent `Register` + `Get` is
  a data race; callers MUST NOT call `Register` after passing the registry to the orchestrator.
- Duplicate names panic (programming error, not runtime input).
- The registry feeds both the prompt-time tool advertisement and the runtime execution channel
  for an agent kind whose declared disposition delivers one; a kind whose declaration delivers
  none receives neither, which is what keeps the two consistent for every kind. What decides is
  the declaration, not the kind. The execution channel, where one exists, is an MCP stdio sidecar
  exposed by the `sortie mcp-server` subcommand.

#### 10.4.4 Tool tiers

Tools are classified by their dependency profile. The tier determines security posture, test
strategy, and failure characteristics.

**Tier 1, pure orchestrator state.** These tools read from local session state (a workspace
state file or the SQLite database) with zero external calls. They are deterministic and fast.
Beyond internal bugs, their only runtime failure mode is the failure envelope of Section 10.4.2
when the local state they read is missing or unreadable. Their `error.kind` values come from the
closed Tier 1 set `state_unavailable`, `state_malformed`, and `query_failed`: an absent, symlinked,
oversized, or unreadable state file is `state_unavailable`, a present but unparseable state file is
`state_malformed`, and a failed database query is `query_failed`.

- `sortie_status` (Section 10.4.5)
- `workspace_history` (Section 10.4.5)
- `cost_budget` (Section 10.4.5)

**Tier 2, external dependencies.** These tools interact with external services (tracker APIs,
future SCM APIs) through network calls using orchestrator-managed credentials. They are subject
to transport failures, authentication errors, rate limits, and per-tool timeouts.

- `tracker_api` (Section 10.4.5)
- `notify_operator` (Section 10.4.5)

Future tools follow the same classification.

#### 10.4.5 Built-in tools

Sortie registers the built-in tools below in the `ToolRegistry`, subject to each tool's
availability conditions. The MCP server (`sortie mcp-server`, per ADR-0009) registers a tool
only when its required inputs are present in the session environment.

**`tracker_api` (Tier 2)** executes queries and mutations against the configured issue
tracker using the orchestrator's tracker credentials.

Availability: only meaningful when valid tracker auth is configured. When auth is absent, the
tool SHOULD NOT be registered.

Project scoping: the tool is scoped to the configured project. An agent working on project PROJ
MUST NOT be able to query or mutate issues in unrelated projects through this passthrough tool.

Supported operations:

| Operation | Required fields | Description |
|---|---|---|
| `fetch_issue` | `issue_id` | Fetch a single issue by ID |
| `fetch_comments` | `issue_id` | Fetch comments for an issue |
| `search_issues` | (none) | Return issues in configured active states |
| `transition_issue` | `issue_id`, `target_state` | Transition an issue to a target state |

The `TrackerAdapter.CommentIssue` method exists on the adapter interface but is not yet exposed
through `tracker_api`.

Tracker dispatch:

The tool delegates to the configured `TrackerAdapter` implementation. Transport, input shape,
and query semantics are adapter-defined.

Result semantics: the tool returns the uniform envelope of Section 10.4.2. On success it returns
`{"success": true, "data": <payload>}`, where `<payload>` is the per-operation response object. On
a domain failure (API-level error, invalid input, missing auth, or transport failure) it returns
`{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}`. The `error.kind` comes
from the closed set below.

| `error.kind` | Condition |
|---|---|
| `invalid_input` | Input fails schema decode, carries unknown or trailing fields, or omits a required field for the operation |
| `unsupported_operation` | The requested operation is not one of the four supported operations |
| `project_scope_violation` | The target issue is outside the configured project |
| `tracker_transport_error` | The request was canceled or its deadline was exceeded |
| `internal_error` | An unexpected error that the adapter did not classify |
| `domain.TrackerError` kinds | The adapter returned a classified tracker error; its kind passes through verbatim |

The response payload or error envelope is returned as structured JSON that the agent can inspect
in-session.

**`sortie_status` (Tier 1)** returns live session runtime metadata. It reads the
worker-maintained session state file `.sortie/state.json` in the workspace and makes no external
calls.

Availability: registered when the session workspace path is present in the environment
(`SORTIE_WORKSPACE`). The tool takes no input.

Response fields:

| Field | Type | Description |
|---|---|---|
| `turn_number` | integer | Current turn within the session |
| `max_turns` | integer | Configured `agent.max_turns` |
| `turns_remaining` | integer | `max_turns - turn_number`, clamped at 0 |
| `attempt` | integer or null | Retry or continuation attempt number; null on the first run |
| `session_duration_seconds` | number | Wall-clock seconds since the session started |
| `tokens` | object | `input_tokens`, `output_tokens`, `total_tokens`, and `cache_read_tokens` |

On success the tool returns the response object above under `data`, in the envelope
`{"success": true, "data": {...}}` of Section 10.4.2. On failure it returns the failure envelope
`{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}`. An absent, symlinked,
oversized, or unreadable state file yields `error.kind` `state_unavailable`; a present but
unparseable state file, including an invalid `started_at`, yields `state_malformed`.

**`workspace_history` (Tier 1)** returns the most recent completed run attempts for the current
issue. It queries the `run_history` table (Section 19.2) through a read-only SQLite connection
and makes no external calls.

Availability: registered when the database path and issue ID are present in the environment
(`SORTIE_DB_PATH`, `SORTIE_ISSUE_ID`). The tool takes no input.

On success the tool returns, under `data` in the envelope `{"success": true, "data": {...}}` of
Section 10.4.2, a JSON object `{issue_id, entries}`, where `entries` lists at most the 10 most
recent runs, newest first. Each entry has `attempt`, `agent_adapter`, `started_at`,
`completed_at`, `status` (`succeeded`, `failed`, `cancelled`, `ci_failed`, or `needs_person`), and
`error` (null unless the run failed). The per-entry `error` is the run's own error and is distinct
from the envelope's `error` object.

On failure the tool returns the failure envelope
`{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}` with `error.kind`
`query_failed`. This happens when the history query fails.

**`cost_budget` (Tier 1)** returns cumulative per-issue token spend and the remaining token
budget so an agent can self-regulate before the orchestrator's token ceiling blocks a
re-dispatch. It sums `total_tokens` across the issue's `run_history` rows and adds the running
session's recorded `total_tokens` from `session_metadata`, then makes no external calls. It is
the advisory counterpart to the `agent.max_tokens` enforcement on the dispatch path; both read
the same summed `total_tokens`, so the advisory reading and the refusal agree.

Availability: registered when the database path and issue ID are present in the environment
(`SORTIE_DB_PATH`, `SORTIE_ISSUE_ID`), the same condition as `workspace_history`. The running
session ID arrives through `SORTIE_SESSION_ID`. The tool takes no input.

On success the tool returns, under `data` in the envelope `{"success": true, "data": {...}}` of
Section 10.4.2, a JSON object with the following fields:

- `used_tokens`: cumulative `total_tokens` across the issue's completed sessions, plus the
  running session's recorded `total_tokens` when a session is in flight. The running session's
  spend is added only when the `session_metadata` row's session ID matches the live session ID,
  so a stale earlier session is never counted twice.
- `budget_tokens`: the configured `agent.max_tokens`. `0` means unlimited.
- `remaining_tokens`: `budget_tokens - used_tokens`, floored at `0`; `null` when the budget is
  unlimited, so the agent distinguishes "no limit" from "nothing left".
- `used_sessions`: completed sessions for the issue. The running session is not counted,
  matching how `agent.max_sessions` counts. Continues to count sessions whose spend is unknown,
  because `agent.max_sessions` counts sessions rather than spend.
- `budget_sessions`: the configured `agent.max_sessions`. `0` means unlimited.
- `unmeasured_sessions`: the count of the issue's completed sessions whose coding agent reported
  no token usage, so `used_tokens` excludes them rather than counting them as zero spend.
- `used_tokens_complete`: `false` when `unmeasured_sessions` is greater than zero, or when a
  running session ID was supplied and no matching `session_metadata` row was found for it;
  `true` otherwise. A `false` value tells the agent that `used_tokens` is a lower bound and
  `remaining_tokens` an upper bound, rather than exact figures.

The asymmetry between `used_tokens` (includes the running session) and `used_sessions`
(excludes it) is deliberate: a session is discrete and either finished or not, whereas tokens
accrue continuously, so an advisory reading that ignored in-flight spend would be useless
exactly when the agent consults it. The running session's spend reaches `session_metadata`
through throttled incremental writes during the session and reaches `run_history` only at
session exit, so no window double counts.

On failure the tool returns the failure envelope
`{"success": false, "error": {"kind": "<kind>", "message": "<message>"}}` with `error.kind`
`query_failed`. This happens when the budget query fails.

This result shape supersedes the five-field description of ADR-0013
(`docs/decisions/0013-agent-cost-budget.md`); `unmeasured_sessions` and `used_tokens_complete`
are additions this section now records as current.

**`notify_operator` (Tier 2)** sends a real-time notification to an operator-configured
channel while a session runs. The agent uses it to escalate a decision it should not make
alone, report progress on a long task, or flag a blocker, without terminating the session.
The tool resolves the configured notifier backends and posts to them; it knows nothing about
any specific channel.

Availability: registered only when at least one valid notifier backend is configured in the
`notifications` list (Section 5.3.11). The registration derives from the same workflow file the
main process reads, so the sidecar and the main process agree on the tool set. When the list
is empty or absent, the tool is not registered.

The agent supplies only the message; the system owns the envelope and the agent cannot set
or forge any envelope field. The input schema rejects unknown fields:

| Field | Type | Required | Description |
|---|---|---|---|
| `severity` | string | Yes | One of `info`, `warning`, `critical` |
| `title` | string | Yes | Non-empty short summary |
| `body` | string | Yes | Non-empty notification detail |
| `category` | string | No | One of `decision_needed`, `progress`, `blocked`, `completed`, `other` |

The system-owned envelope carries a generated notification id, an ISO-8601 UTC timestamp, a
source (the hostname by default), the issue id and identifier, the session id, the attempt,
and the dispatch-frozen agent kind. The envelope correlates the notification to a session for
an operator and a machine consumer.

Rate limiting: the tool enforces a per-session cap, `max_per_session`, where `0` selects the
default rather than unlimited. A call past the cap returns `rate_limited` and sends nothing.
An accepted call increments the counter once, after delivery, not once per backend.

Result semantics: on success the tool returns `{"success": true, "data": {"delivered": <int>,
"notification_id": "<id>"}}`, the uniform success envelope of Section 10.4.2. On a domain failure
it returns `{"success": false, "error": {"kind": "...", "message": "..."}}` with `error.kind` in
the closed set below. The Go error return is reserved for an internal marshal failure.

| `error.kind` | Condition |
|---|---|
| `invalid_input` | Input fails schema decode, carries unknown or trailing fields, has an out-of-enum `severity` or `category`, or has an empty `title` or `body` |
| `rate_limited` | The per-session counter has reached `max_per_session` |
| `send_failed` | A backend returned a transport failure, a non-2xx response, or an unparseable response. The message is a redacted category and never echoes the URL, request body, or response body |
| `backend_unavailable` | No backend could be resolved at execution time (defensive; normal operation gates registration on a configured backend) |

#### 10.4.6 Tools vs. agent-authored files

The tool subsystem (this section) and the `.sortie/status` file protocol (Section 21) are
independent communication channels between agents and the orchestrator. They address different
concerns, operate on different transports, and have deliberately different failure
characteristics. This separation is a design choice, not an implementation accident.

**Communication patterns.**

| Property | Agent tools | `.sortie/status` file |
|---|---|---|
| Direction | Agent <-> Orchestrator (request-response) | Agent -> Orchestrator (one-way advisory) |
| Transport | Tool call (MCP stdio sidecar) | Filesystem sentinel file |
| Timing | Synchronous, during a turn | Asynchronous, read after turn completes |
| Purpose | Data access (tracker queries, orchestrator state) | Control flow (retry suppression, soft stop) |
| Failure mode | Tool call fails; agent receives error and continues | File absent or unreadable; orchestrator proceeds normally |
| Agent requirement | MCP client or equivalent tool-calling capability | Write a file to disk (`echo "blocked" > .sortie/status`) |

**Why two channels exist.** The channels serve orthogonal roles:

- Tools are the **data plane**: the agent requests information or performs a mutation and
  receives a structured result within the same turn. The agent needs the response to continue
  its work.
- The `.sortie/status` file is the **control plane**: the agent advises the orchestrator about
  task feasibility after the turn completes. The orchestrator uses this signal to suppress
  continuation retries. No response flows back to the agent.

The `notify_operator` tool (Section 10.4.5) is a third direction that the data-plane and
control-plane framing above does not cover: agent to operator. Most tools target the
orchestrator and return data the agent consumes to continue its turn. `notify_operator` is a
tool by transport (it travels the MCP execution channel and returns a delivery result), but its
recipient is a human on a configured channel, not the orchestrator, and it changes no
orchestrator state. The three patterns are distinct: read or mutate orchestrator-held data
(`tracker_api`, `cost_budget`, `sortie_status`, `workspace_history`), advise the orchestrator
about feasibility after the turn (`.sortie/status`), and reach the operator in real time during
the turn (`notify_operator`). `notify_operator` does not suppress retries, perform a handoff, or
release a claim; an agent that needs to alter orchestrator control flow still writes
`.sortie/status`.

Collapsing both into a single MCP-based channel was evaluated and rejected during the A2O
protocol design (see `docs/agent-to-orchestrator-protocol.md`, Section 5.1, Alternative 2).
The MCP approach fails the agent-agnostic requirement: an agent without MCP client support
cannot send the control signal. The file-based channel satisfies all six A2O requirements
(agent-agnostic, fail-safe, advisory, zero-dependency, forward-compatible, inspectable)
simultaneously; no tool-call-based mechanism achieves this.

**Coexistence.** An agent MAY use both channels in the same session. Typical sequence:

1. Agent calls a tool (e.g., `tracker_api.fetch_issue`) to gather context.
2. Agent determines the task requires a human architectural decision.
3. Agent writes `mkdir -p .sortie && echo "blocked" > .sortie/status`.
4. Turn completes; orchestrator reads the status file, suppresses retries, and, where the
   dispatch drives issue state, parks the issue and applies the parking label.

The two channels do not interact. A tool call cannot write to `.sortie/status` on behalf of
the agent, and the `.sortie/status` file cannot trigger tool execution. The orchestrator
processes them at different points in the worker lifecycle: tool calls during the turn (via the
execution channel), status file after the turn (Section 21.1, read timing per
`agent-to-orchestrator-protocol.md` Section 3.1).

**Defense in depth.** The independence of the two channels provides resilience. If the MCP
execution channel is unavailable (sidecar crash, agent lacks MCP support),
the file-based advisory signal still functions. If the filesystem is read-only or the workspace
is on a remote host with restricted write access, tool calls still function. Neither channel is
a single point of failure for the other.

#### 10.4.7 Notifier adapter family

Operator notifications are an adapter family, the same shape as the tracker, agent, CI, and
SCM families. The `notify_operator` tool (Section 10.4.5) is a thin Tier 2 wrapper over this
family; a new channel is a new package behind the existing interface, not a tool rewrite.

The family has the following parts:

- **The `domain.Notifier` interface** exposes one method, `Send(ctx, Notification) error`. A
  single method keeps every backend interchangeable and lets any producer reuse the family.
  An implementation applies a per-call timeout and never logs the endpoint URL, the request
  body, or the response body.
- **The normalized `domain.Notification`** has two layers. The envelope is system-owned and
  carries the notification id, timestamp, source, issue id and identifier, session id,
  attempt, and dispatch-frozen agent kind. The message is agent-supplied and carries
  `severity`, `title`, `body`, and an optional `category`. The value is self-contained:
  every field a backend needs rides in it, with no dependency on producer-only state, so a
  future orchestrator producer can fill the envelope without an interface change.
- **The `registry.Notifiers` registry** maps a `kind` string to a constructor. Backend
  packages register in `init()`; the sidecar resolves backends by `kind` at runtime. This
  mirrors `registry.SCMAdapters` exactly.
- **The backend packages** are one per `kind`. v1 ships `webhook` (posts
  the notification as a JSON object using generic field names) and `slack` (posts a
  Slack-shaped body with a `text` field). Each builds on the shared HTTP client with its
  configured endpoint as the base URL, applies a mandatory per-call timeout, and classifies
  its own transport and non-2xx errors into a category that omits the URL and payload.

The backend packages obey the adapter-family boundary rules: no cross-adapter imports, no
importing the orchestrator, normalization to the domain type at the boundary, and generic
`notifier_*` vocabulary in the core, never `slack_*`.

Backends register via the notifier registry using `init()` functions:

```go
func init() {
    registry.Notifiers.Register("webhook", newNotifier)
}
```

The `NotifierConstructor` signature is:

```go
type NotifierConstructor func(config map[string]any) (domain.Notifier, error)
```

The `config` parameter receives the per-backend fields from the matching `notifications`
list entry (Section 5.3.11), with `$VAR` references already resolved. A constructor rejects a missing required
field or a secret that resolved to the empty string, which surfaces as a fatal sidecar
startup error rather than a notification posted nowhere.

### 10.5 Timeouts and Error Mapping

Timeouts:

- `agent.read_timeout_ms`: request/response timeout during startup and sync requests
- `agent.turn_timeout_ms`: enforced by orchestrator based on wall-clock duration
- `agent.stall_timeout_ms`: enforced by orchestrator based on event inactivity

Error mapping (recommended normalized categories):

- `agent_not_found`
- `invalid_workspace_cwd`
- `response_timeout`
- `turn_timeout`
- `port_exit`
- `response_error`
- `turn_failed`
- `turn_incomplete`
- `turn_cancelled`
- `turn_input_required`
- `turn_refused`
- `turn_outcome_unknown`

### 10.6 Agent Runner Contract

The `Agent Runner` wraps workspace + prompt + agent adapter.

Behavior:

1. Create/reuse workspace for issue.
2. Build prompt from workflow template.
3. Start agent session via adapter.
4. Relay agent events to orchestrator.
5. On any error, fail the worker attempt (the orchestrator will retry).

Note:

- Workspaces are intentionally preserved after successful runs.

### 10.7 Local Subprocess Launch Contract

This subsection applies only to adapters that launch a local subprocess (e.g., Claude Code,
Copilot CLI, OpenCode CLI, Kiro CLI, and the Codex app-server). HTTP-based and remote
adapters define their own connection semantics.

When `agent.kind` requires a local subprocess:

- Command: `agent.command`
- Invocation:
  - `agent.command` is split on whitespace. The first token is resolved to an executable path
    using the host's `PATH` lookup rules: a token containing a path separator is used as given,
    and any other token is searched for on `PATH`, which yields an absolute path. The remaining
    tokens become a fixed argument prefix inserted before any per-turn arguments (for example,
    `codex app-server` resolves `codex` and yields `app-server` as a prefix argument).
  - POSIX and Windows: the adapter execs the resolved binary directly with that argument vector.
    No shell is involved in local invocation. On Windows the subprocess additionally receives
    `CREATE_NEW_PROCESS_GROUP` so it can be signaled independently of the orchestrator.
  - When the worker runs remotely over SSH, `agent.command` is instead passed through unsplit and
    unresolved as the command the remote shell executes; a shell is involved only on that path.
    See [Appendix A. SSH Worker Extension (Optional)](27-appendix-a-ssh-worker-extension.md) for
    the remote execution model.
- Working directory: workspace path
- Stdout/stderr: separate streams

Process group isolation:

- The adapter MUST place the subprocess in its own process group before starting it.
  - POSIX: `Setpgid = true` (new process group at fork time).
  - Windows: `CREATE_NEW_PROCESS_GROUP` creation flag, followed by Job Object assignment after
    process start. The Job Object is configured with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so
    the entire process tree is terminated if the orchestrator crashes.

Graceful shutdown sequence:

- On context cancellation or `StopSession`, the adapter sends a platform-appropriate graceful
  shutdown signal to the process group:
  - POSIX: `SIGTERM` to the process group (`kill(-pgid, SIGTERM)`).
  - Windows: `CTRL_BREAK_EVENT` via `GenerateConsoleCtrlEvent` to the process group.
- After a grace period (default 5 seconds), force-terminate the process tree:
  - POSIX: `SIGKILL` to the process group.
  - Windows: `TerminateJobObject` to kill all processes in the Job Object.
- After `cmd.Wait()` returns, a best-effort force kill is sent to the process group to reap any
  children that survived the graceful signal.

Standard-error drain before reap:

- The adapter waits for its subprocess's standard-error reader to finish before reaping the
  process, because reaping closes the pipe's read end and races a reader still consuming
  buffered output. That wait is bounded, so a descendant that inherits the standard-error
  handle and outlives the direct child cannot withhold the reap, the process-group termination,
  or the publication of the turn's outcome.
- Reaping the process and terminating its group are what release such a reader in the ordinary
  case, so the turn keeps the standard-error output the reader had already collected.
- When neither release ends the wait, the turn's collected standard-error output is replaced by
  a marker, and an adapter whose success evidence lives on standard error reports the turn
  failed rather than succeeded.

Recommended additional process settings:

- Max line size: 10 MB (for safe buffering)

### 10.8 Turn Disposition

Every adapter must decide, at the end of each turn, whether the turn completed, failed, or was
cancelled, and pair that decision with the returned error. This decision follows one shared rule
so that a new adapter inherits it rather than re-deriving it.

The rule is evaluated as an ordered table. The first matching row decides the turn:

1. The runtime reported the turn cancelled (an orchestrator-initiated cancellation): `turn_cancelled`.
   This row matches on the orchestrator's own cancellation rather than on the word the runtime
   uses: a runtime report that the turn completed, arriving after the orchestrator cancelled the
   turn, does not displace this row.
2. The adapter recognized a request only a person could answer that the turn cannot continue
   past: `turn_input_required`. It outranks every failure row below, so such a request is
   never reported as a generic turn failure, and it ranks under cancellation, so a shutdown
   already in progress still reports itself.
3. The runtime reported the turn failed, or the adapter itself ended the turn on its own
   determination (a protocol violation, a timeout waiting for the first response, or a transport
   failure): `turn_failed`, with the error kind the runtime or the adapter supplied.
4. The runtime reported the turn succeeded: `turn_completed`. A positive report from the runtime is
   authoritative and is never second-guessed by counting output.
5. The runtime ended the turn without the task-completion report its protocol defines: `turn_failed`.
6. The runtime reported no outcome at all, and the adapter observed no process exit for the turn (a
   persistent session with no per-turn exit): `turn_failed`.
7. The runtime reported no outcome, a process exit was observed, and the exit code is non-zero:
   `turn_failed`.
8. The runtime reported no outcome, the process exited zero, and the adapter found no evidence the
   model produced anything this turn: `turn_failed`. Exit code zero is never by itself a success
   signal; an adapter with nothing positive to offer reports a failed turn.
9. The runtime reported no outcome, the process exited zero, and the adapter found evidence the
   model produced something this turn: `turn_completed`.

Extracting the runtime's own report and the per-turn work evidence from a vendor's wire format is
adapter-local, and each adapter's protocol document states how it does so. Turning that evidence
into a disposition is not adapter-local: every adapter obtains its turn disposition from the one
shared rule above, and an adapter whose runtime reports something the rule cannot express extends
the rule itself, under review, rather than deciding the case in a private branch.

Two adapters cannot supply the rule's per-turn work evidence in the same currency as the others,
and diverge from the rule's letter while obeying its intent:

- Headless Kiro reports no token counts at all, so its zero-work guard is expressed in a different
  currency: the credits trailer on stderr is the positive success signal, and its absence with a
  zero exit code is the adapter's only evidence of nothing produced.
- Codex's persistent per-session subprocess has no per-turn process exit to observe, so the rule's
  zero-work row (row 8 above) is structurally unreachable for it: an absent turn-completion report
  is already a failure on its own terms.

`turn_ended_with_error` remains a documented normalized event type (Section 10.3), reserved for a
future adapter whose runtime genuinely distinguishes a transport-class failure from every other
failure, even though no built-in adapter emits it today.
