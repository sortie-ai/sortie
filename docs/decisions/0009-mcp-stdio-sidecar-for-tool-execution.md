---
status: accepted
date: 2026-03-31
decision-makers: Serghei Iakovlev
---

# Use MCP stdio sidecar for agent tool execution

## Context and Problem Statement

The `AgentTool` interface and `ToolRegistry` exist (Section 10.4.2–10.4.3) but tools are
prompt-advertised only: agents see tool documentation in their prompts but cannot call tools
at runtime. Section 10.4.3 explicitly marks the runtime execution channel as undesigned.
Issue #224 requires a concrete transport mechanism so registered tools become callable during
agent sessions.

## Decision Drivers

1. **Agent agnosticism.** The execution channel MUST work with any MCP-compatible agent
   runtime (Claude Code, Copilot CLI, future adapters) without adapter-specific integration
   code in the orchestrator core.
2. **Single-binary constraint.** The solution MUST NOT introduce additional binaries,
   shared libraries, or runtime dependencies. Per ADR-0001, the output is one
   statically-linked Go binary.
3. **Session isolation.** Each agent session MUST have its own execution channel instance
   scoped to the session's issue, workspace, and credentials. Tool calls from one session
   MUST NOT leak state to another.
4. **Lifecycle simplicity.** The execution channel's lifecycle MUST be tied to the agent
   process, with no orphan processes or stuck goroutines. The worker MUST NOT manage the
   MCP server process directly.
5. **Consistency with architecture.** The channel MUST integrate with the existing
   `ToolRegistry` for tool discovery and dispatch, and coexist with the file-based
   `.sortie/status` advisory channel (Section 10.4.6) without interference.

## Considered Options

- MCP stdio sidecar launched by the agent runtime via config file
- HTTP-based tool server on a localhost port
- In-process tool dispatch via agent adapter callback hooks

## Decision Outcome

Chosen option: **MCP stdio sidecar launched by the agent runtime via config file**, because
it satisfies all five drivers with the lowest implementation and operational complexity.

The worker generates a temporary `mcp-config.json` in the workspace directory and passes its
path to the agent via the `--mcp-config` flag (Claude Code) or `--additional-mcp-config`
(Copilot CLI). The agent runtime reads this configuration and spawns `sortie mcp-server` as
its own child process — the worker does not manage the MCP server lifecycle directly. This is
the standard MCP stdio transport model: the agent runtime owns the server process.

The MCP server communicates with the agent over stdio using JSON-RPC per the Model Context
Protocol specification. It handles `tools/list` (returns registered tools from the
`ToolRegistry`) and `tools/call` (delegates to the matching `AgentTool.Execute` implementation
with session context).

The MCP server receives configuration through three channels, split by kind:

- **Per-session context** (issue ID, workspace path, database path, session ID) reaches the
  server through the `env` field in `mcp-config.json`. The worker writes these values at
  config generation time. These are non-secret session metadata — safe to persist in the
  workspace directory.
- **Tracker credentials** reach the server through inherited environment variables. The
  orchestrator's process environment contains these values (set by the operator). The agent
  subprocess inherits them via `os.Environ()`, and the MCP server inherits them from the
  agent runtime, which spawns it as a child process.
- **Tracker configuration** (kind, endpoint, project, state mappings, query filter) reaches
  the server through the `--workflow` CLI argument. The subcommand parses the WORKFLOW.md
  file to extract tracker config and construct the `TrackerAdapter` — see
  "TrackerAdapter bootstrapping in the MCP server" below.

The MCP server expects the following environment variables:

| Variable | Purpose | Channel | Required |
|---|---|---|---|
| `SORTIE_ISSUE_ID` | Scopes tool calls to the current issue | Config `env` field | Yes |
| `SORTIE_ISSUE_IDENTIFIER` | Human-readable issue key; used by `tracker_api` for project scoping via identifier prefix | Config `env` field | Yes |
| `SORTIE_WORKSPACE` | Workspace root path for file-relative operations | Config `env` field | Yes |
| `SORTIE_DB_PATH` | SQLite database path (read-only access for Tier 1 tools) | Config `env` field | Yes |
| `SORTIE_SESSION_ID` | Session identifier for Tier 1 run-history queries | Config `env` field | Yes |
| Tracker credentials | Adapter-specific (e.g., GitHub token, Jira API key) | Inherited env | Tier 2 tools only |

`SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`, and `SORTIE_WORKSPACE` are existing hook
environment variables (Section 5.3.4). `SORTIE_DB_PATH` and `SORTIE_SESSION_ID` are new
variables introduced by this ADR — the hook environment does not need them because hooks
do not query the database or correlate with agent sessions. The MCP server needs
`SORTIE_DB_PATH` to open a read-only SQLite connection for Tier 1 tools, and
`SORTIE_SESSION_ID` to scope run-history queries to the current session.

Per-session variables (the first five rows) are computed by the worker at config generation
time and written into the `env` field of `mcp-config.json`. They do not exist in the
orchestrator's process environment — multiple workers run concurrently with different values
for each session. The config file `env` field is the correct delivery mechanism: the agent
runtime reads it and sets these variables on the MCP server child process at spawn time.

Tracker credentials are whichever variables the configured `TrackerAdapter` requires (e.g.,
`GITHUB_TOKEN`, Jira API key). These are set by the operator in the orchestrator's process
environment. The agent subprocess inherits them via `os.Environ()`, and the MCP server
inherits them from the agent — both the Claude Code and Copilot CLI adapters set
`cmd.Env = os.Environ()`, establishing this inheritance chain.

Tracker configuration is workflow-level and stable across sessions — it belongs in the
WORKFLOW.md file, not in the config `env` field. This three-way separation keeps each
channel appropriately scoped: session metadata in the config `env` field, credentials in
inherited environment, tracker structure in the workflow file.

The `mcp-config.json` file written to disk uses the standard MCP configuration format:

```json
{
  "mcpServers": {
    "sortie-tools": {
      "type": "stdio",
      "command": "/usr/local/bin/sortie",
      "args": ["mcp-server", "--workflow", "/absolute/path/to/WORKFLOW.md"],
      "env": {
        "SORTIE_ISSUE_ID": "abc-123",
        "SORTIE_ISSUE_IDENTIFIER": "PROJ-42",
        "SORTIE_WORKSPACE": "/tmp/sortie_workspaces/PROJ-42",
        "SORTIE_DB_PATH": "/var/lib/sortie/sortie.db",
        "SORTIE_SESSION_ID": "sess-7f3a"
      }
    }
  }
}
```

The `command` field MUST contain the absolute path to the running `sortie` binary, resolved
via `os.Executable()` at config generation time. This eliminates a PATH dependency — the
agent runtime spawns the MCP server as a child process using this path directly, without
requiring `sortie` to be on the agent's PATH.

The `--workflow` argument MUST contain the absolute path to the resolved WORKFLOW.md file.
The worker already resolves this path at startup (Section 5.1); it passes the same resolved
path to the `args` array. Relative paths are prohibited because the agent runtime sets the
MCP server's working directory to the workspace root, which may differ from the
orchestrator's cwd.

The file MUST NOT contain credential values. The `env` field contains per-session context
variables (non-secret metadata) — never inline secret values such as API keys or tokens.
Credentials reach the MCP server through inherited environment variables, not through the
config file. This ensures that `mcp-config.json` is safe to persist in the workspace
directory without leaking credentials.

### MCP config merging with operator-provided servers

Operators MAY declare custom MCP servers via the `mcp_config` passthrough field in
WORKFLOW.md (e.g., `claude-code.mcp_config` or `copilot-cli.mcp_config`). Claude Code
accepts only one `--mcp-config` flag, and Copilot CLI accepts only one
`--additional-mcp-config` value. If the worker generates its own config file and the
operator also specifies one, one silently overwrites the other.

The worker MUST merge both configurations into a single file:

1. If the operator's `mcp_config` is set, the worker reads and parses the referenced JSON
   file. If the file is unreadable or contains invalid JSON, the worker fails the attempt
   with a validation error.
2. The worker adds the `sortie-tools` server entry to the parsed `mcpServers` object.
3. If the operator's config already contains a server named `sortie-tools`, the worker
   fails the attempt with a validation error — name collisions indicate a
   misconfiguration that must be resolved by the operator.
4. The worker writes the merged config to `mcp-config.json` in the workspace directory and
   passes its path to the agent via the adapter's MCP config flag.

When the operator does not specify `mcp_config`, the worker writes a file containing only
the `sortie-tools` entry, as shown in the JSON example above.

The merge operates at the `mcpServers` object level only. The worker does not interpret,
validate, or modify the operator's server entries — it preserves them verbatim. This keeps
the merge logic minimal and avoids the worker needing knowledge of arbitrary MCP server
configurations.

The MCP server opens SQLite with `?mode=ro` in the DSN, enforcing read-only access at the
driver level. This is sufficient for Tier 1 tools that query run history — the orchestrator's
main process remains the single writer (per ADR-0002: SQLite in WAL mode, single-writer
concurrency).
Tier 2 tools (e.g., `tracker_api`) delegate to tracker HTTP APIs via the `TrackerAdapter`
interface and do not access the database. The server exits cleanly when stdin closes, which
happens automatically when the agent runtime terminates the server's stdio pipe.

### TrackerAdapter bootstrapping in the MCP server

The MCP server is a separate process — it does not inherit Go objects from the worker. To
run Tier 2 tools it must construct its own `TrackerAdapter` instance. The subcommand
accomplishes this by replaying the same construction chain used by the main process:

1. Parse the WORKFLOW.md file specified by `--workflow` to extract `config.TrackerConfig`.
2. Resolve the tracker constructor from the adapter registry
   (`registry.Trackers.Get(kind)`).
3. Build the config map and construct the `TrackerAdapter`. Credentials resolve from
   inherited environment variables through the same `config.EnvOverride` mechanism used by
   the main process — the MCP server inherits the orchestrator's process environment via
   the `os.Environ()` → agent → MCP server chain.
4. Register `trackerapi.New(adapter, project)` in a local `ToolRegistry` only if
   `project` is non-empty — the same guard the main process uses.

The tool registration decision is identical to the main process:

| Condition | Outcome |
|---|---|
| Tracker section absent from WORKFLOW.md | No tracker adapter constructed. Tier 2 tools not registered. MCP server starts with Tier 1 tools only. |
| Tracker section present, `project` empty | Tracker adapter constructed (available for the orchestrator's own use), but `tracker_api` not registered — the tool requires a project to scope queries. |
| Tracker section present, `project` set, credentials missing | Adapter constructor returns an error. MCP server exits with a non-zero code. The agent runtime surfaces the failure. |
| Tracker section present, `project` set, credentials valid | `tracker_api` registered. Full tool set available. |

No new flags or environment variables control tool registration. The subcommand derives the
decision entirely from the WORKFLOW.md configuration and inherited environment — the same
inputs the main process uses. This guarantees that `tools/list` in the MCP server returns
the same tool set the main process advertises in prompts.

The subcommand reuses the `config`, `registry`, and adapter packages. No tracker-specific
logic exists in the MCP server implementation (`internal/tool/mcpserver/`) itself — it
receives a populated `ToolRegistry` from the `cmd/sortie/` wiring layer, just as the main
process wires the orchestrator's `WorkerDeps.ToolRegistry`.

If `--workflow` is omitted, the subcommand exits with a usage error. The worker always
provides this flag — omission indicates a manual invocation outside the intended lifecycle.

Entry point: `sortie mcp-server` subcommand in the existing binary, maintaining the
single-binary constraint per ADR-0001. The subcommand handler lives in `cmd/sortie/` (wiring
only). The MCP server implementation — JSON-RPC stdio transport, `tools/list`, and
`tools/call` dispatch — lives in `internal/tool/mcpserver/`. This separates the transport
layer from tool implementations (`internal/tool/trackerapi/` and future tool packages) while
keeping the entire tool subsystem under the `internal/tool/` tree per Section 10.4.

### Relationship to prompt-time tool advertisement

The existing `buildToolAdvertisement` path (Section 10.4.3) appends tool documentation to the
first-turn prompt. This ADR does not remove it. With MCP active, agents receive tool
information through two channels:

- **Prompt text** — human-readable Markdown description of each tool's name, purpose, and
  input schema. Present in the conversation history. Serves as a fallback for agents without
  MCP client support and as a readable reference the agent can consult across turns.
- **MCP `tools/list`** — machine-readable JSON Schema used by the agent runtime for actual
  tool dispatch.

Both channels draw from the same `ToolRegistry`, so the tool set is always consistent.
Redundancy is intentional, following the same defense-in-depth rationale as the tool / status
file separation (Section 10.4.6): if MCP is unavailable, the agent still knows the tools
exist; if the prompt is truncated, MCP `tools/list` is authoritative.

### Graceful degradation: MCP server crash

If `sortie mcp-server` crashes or exits unexpectedly mid-session, the agent runtime
detects the broken stdio pipe and returns a JSON-RPC error for any subsequent `tools/call`
request. The worker does not detect or handle this event directly — it has no visibility
into the MCP server process, which is the agent runtime's child (per decision driver #4).

This is by design, for three reasons:

1. **The worker cannot observe the crash.** The MCP server's exit status is delivered to
   the agent runtime (its parent process), not to the worker. Adding detection would
   require the worker to monitor a process it did not spawn, violating the lifecycle
   boundary established in this ADR.

2. **Existing error paths handle the outcome.** The agent sees tool call failures and
   decides how to proceed — retry, work around the missing tool, or exit with a failure
   code. If the agent exits non-zero, the worker maps the exit code to an error category
   per Section 10.5, and the orchestrator retries the attempt per its retry policy
   (Section 10.6 step 5). No MCP-specific error category is needed.

3. **Defense in depth provides resilience.** Per Section 10.4.6, the tool channel and file
   channel are independent. If the MCP execution channel becomes unavailable, the agent
   still has prompt-time tool documentation and the `.sortie/status` advisory file channel.
   Neither channel is a single point of failure for the other.

Adding MCP-server-specific crash detection to the worker would violate the agent-agnostic
principle (decision driver #1): the worker would need knowledge of the MCP server's process
identity and failure semantics, coupling it to the sidecar transport choice.

### Considered Options in Detail

**HTTP-based tool server.** The worker starts an HTTP server on localhost and passes the URL
to the agent for tool calls. Common objections to this approach — port collisions, firewall
restrictions, network latency — are individually solvable: binding to port 0 yields an
OS-assigned port, localhost traffic bypasses firewall rules, and local TCP round-trip times
are comparable to stdio pipe latency. The decisive disadvantages are structural:

1. **Lifecycle inversion.** With the stdio sidecar, the agent runtime spawns the MCP server
   as its child — when the agent exits, the pipe closes and the server terminates
   automatically. With HTTP, the worker owns the server: it must start the server before
   the agent, keep it running for the session, and shut it down after the agent exits or
   crashes. This violates decision driver #4 (lifecycle simplicity) and requires explicit
   teardown on every exit path — normal completion, agent crash, orchestrator shutdown — to
   avoid orphaned servers.
2. **Transport compatibility.** MCP defines stdio and Streamable HTTP transports. Stdio is
   universally supported by MCP-compatible agent runtimes: Claude Code and Copilot CLI both
   accept standard MCP config files with stdio server entries. HTTP-based MCP transport
   (SSE, Streamable HTTP) has narrower and less consistent runtime support. Choosing HTTP
   would limit the set of compatible agents without compensating benefit.
3. **Session isolation overhead.** Stdio achieves session isolation structurally — each
   agent session gets its own pipe to its own MCP server instance. With HTTP, the worker
   must allocate a distinct port per concurrent session and prevent cross-session tool calls
   via port-to-session mapping. Solvable, but adds coordination that stdio eliminates
   entirely.

**In-process tool dispatch via agent adapter callback hooks.** Tools execute inside the
orchestrator process without a separate server. The adapter intercepts the agent's
conversational stdout to detect tool-call requests, dispatches them to the `ToolRegistry`,
and injects results back into the agent's stdin.

This approach bypasses MCP entirely. The standard MCP config mechanism requires either
spawning a child process (stdio transport) or connecting to an HTTP endpoint (Streamable HTTP
transport) — neither can target a goroutine within the orchestrator process. Without MCP as
the interposition layer, each adapter must:

- Parse agent output to detect tool-call intent. Agents express this differently — Claude
  Code uses structured JSON tool-use blocks, other runtimes may use different formats.
  Detection logic is format-specific and fragile.
- Extract structured tool input from the detected request format.
- Format tool results in whatever syntax the agent expects to receive.
- Manage control flow: pause reading agent output, execute the tool, inject the result,
  resume reading.

Each adapter becomes a bespoke tool-call interceptor, breaking the agent-agnostic goal
(driver #1). The `ToolRegistry` dispatch is shared, but detection, parsing, and response
framing are entirely adapter-specific.

Beyond the agent-agnostic violation, two further problems apply:

- **Process isolation lost.** A tool that panics crashes the orchestrator — not just the
  current session but all concurrent sessions sharing the process. A slow `tracker_api`
  HTTP call blocks the adapter's event loop, preventing subsequent agent output from being
  read until the tool returns. Goroutine-based concurrency within the adapter mitigates
  blocking but introduces synchronization complexity between concurrent tool execution and
  the agent's serial stdio stream.
- **Layer violation.** The architecture places agent communication (Section 10) and the tool
  subsystem (Section 10.4) in separate packages with distinct responsibilities. In-process
  dispatch merges them: the adapter becomes both communication handler and tool dispatcher,
  complicating testing and violating the single-responsibility boundary.

## Consequences

### Positive

- Any MCP-compatible agent works without adapter-specific tool integration code.
- Process isolation: a tool crash or timeout does not affect the orchestrator process.
- The `mcp-config.json` file is a standard MCP artifact; agents that support `--mcp-config`
  require zero custom wiring.
- Stdio transport eliminates lifecycle management complexity — no port allocation, no
  explicit shutdown, no orphan cleanup.

### Negative

- One additional process per active agent session. The agent runtime manages the MCP server
  as its child process, so the worker's process tree is unchanged. However, the host runs
  N MCP server processes alongside N agent processes during concurrent sessions.
- Session context passing via config file environment variables limits the information
  available to the MCP server at startup. Complex session metadata requires serialization
  to a file or database query rather than direct in-memory access.

### Scope limitation: SSH remote execution

This ADR covers local execution only — the agent and MCP server run on the same host as the
orchestrator. The SSH worker extension (Appendix A) introduces three requirements that this
design does not address:

1. **Binary availability.** The `sortie` binary must be installed on each remote host so the
   agent runtime can spawn `sortie mcp-server`. Appendix A already requires "coding-agent
   executable" on the remote host; the MCP server extends this to include the `sortie` binary.
2. **Config file delivery.** The worker generates `mcp-config.json` locally, but in SSH mode
   `workspace.root` is interpreted on the remote host. The config file must be written to the
   remote workspace before the agent launches.
3. **Environment variable forwarding.** The current SSH transport (`sshutil.BuildSSHArgs`)
   constructs a remote shell command without `SendEnv` or explicit variable forwarding.
   Per-session context is delivered via the config file `env` field (handled by config file
   delivery above), but tracker credentials in the orchestrator's process environment do
   not automatically reach the remote agent or its MCP server child.

These are solvable (e.g., write config via SSH before agent launch, prepend `env VAR=val` to
the remote command), but the design belongs in a follow-up specific to SSH tool execution —
not in this ADR.
