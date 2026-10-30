## Appendix A. SSH Worker Extension (Optional)

This appendix describes a common extension profile in which Sortie keeps one central orchestrator but executes worker runs on one or more remote hosts over SSH.

### A.1 Execution Model

- The orchestrator remains the single source of truth for polling, claims, retries, and reconciliation.
- `worker.ssh_hosts` provides the candidate SSH destinations for remote execution.
- Each worker run is assigned to one host at a time, and that host becomes part of the run's effective execution identity along with the issue workspace.
- The workspace path names a directory on both hosts. The orchestrator creates and validates it on its own host (path sanitization, no traversal), runs hooks there, and keeps Sortie's session state under it. The agent works in the same absolute path on the remote host, which hooks prepare, and the orchestrator trusts the remote host to enforce filesystem permissions.
- The coding-agent is launched over SSH stdio instead of as a local subprocess, so the orchestrator still owns the session lifecycle even though commands execute remotely.
- The remote shell supervises the coding-agent for as long as the SSH connection lives. sshd sends no signal to a command it runs without a terminal when the connection ends, and a runtime waiting on its model reads and writes nothing that would tell it. So once the connection is gone, the remote shell sends SIGTERM to the process group sshd created for the session, waits for the agent up to `agent.stop_grace_ms` rounded up to whole seconds, and then sends SIGKILL. Every stop of a remote launch (a cancelled turn, a read or stall timeout, a session stop, shutdown) ends the local ssh client, and that disconnect starts this sequence on the remote host; the remote shell checks for it once per second. Every launch turns SSH connection sharing off, because a shared connection outlives the client that ended and the remote shell would never see it close. Nothing in the agent's process group outlives it, and a process the agent moved to another process group is not reached. A turn Sortie stopped is not over when its local ssh client ends: the turn waits, on a second connection, until every process that descends from the remote agent has exited, which a lock the agent tree holds reports without a poll. That wait never outlasts the remote shell's own bound, one poll interval plus the grace, so an agent that never exits ends the turn through the same SIGTERM and SIGKILL sequence. A host without the standard `flock` utility, or that cannot be reached, gets no wait.
- Continuation turns inside one worker lifetime should stay on the same host and workspace.
- A remote host should satisfy the same basic contract as a local worker environment: reachable POSIX-compatible login shell with the standard `sleep` utility, writable workspace root, coding-agent executable, and any required auth or repository prerequisites; a launch that carries an environment variable additionally requires the standard `dd` utility on the remote host, and a launch that delivers Sortie's tools additionally requires remote port forwarding to be allowed for the session's key and connection.
- Orchestrator environment variables reach a remote agent only when `worker.ssh_pass_env` names them, the agent kind declares them as its runtime's own credential variables, or a conversion that governs the session's kind carries them from the retired kind it replaced, and only when `worker.ssh_disallow_pass_env` does not name them. A carried variable arrives on the SSH session's standard input, ahead of the agent command, never as part of a process argument. A kind that computes a setting of its own for its runtime sends that setting on the same carrier, named by neither list; the names a conversion carries join the declared set and behave exactly as the retired kind's did, and the session captures them at dispatch, so a running session keeps the names it started with; a variable the session withholds (§10.7) is never carried, even when `worker.ssh_pass_env` names it, and the remote shell unsets it before the agent command runs; everything else the agent's environment holds comes from the host itself. A kind whose startup protocol carries a credential exchange of its own can still send a credential from the orchestrator in that exchange, where it reaches the runtime as a protocol message and never as an environment variable. A launch that delivers Sortie's tools carries more variables the same way, named by neither list: the per-session bearer that authorizes the tool server, and the values of the headers an operator HTTP tool server declares; an `agent-client-protocol` runtime receives these values inside its session-creation request instead. A session start opens one extra connection that probes the forward before the first prompt.
- Remote tool execution: Sortie's tool server stays on the orchestrator host and reaches the agent through a reverse forward on the launch's own SSH connection. A forward the remote host refuses leaves the session without Sortie's tools, declared in its opening notice and never failed. Operator HTTP tool servers reach the agent; operator stdio tool servers are withheld and declared.

### A.2 Scheduling Notes

- SSH hosts may be treated as a pool for dispatch.
- Implementations may prefer the previously used host on retries when that host is still available.
- `worker.max_concurrent_agents_per_host` is an optional shared per-host cap across configured SSH hosts.
- When all SSH hosts are at capacity, dispatch should wait rather than silently falling back to a different execution mode.
- Implementations may fail over to another host when the original host is unavailable before work has meaningfully started.
- Once a run has already produced side effects, a transparent rerun on another host should be treated as a new attempt, not as invisible failover.

### A.3 Problems to Consider

- Remote environment drift:
  - Each host needs the expected shell environment, coding-agent executable, auth, and repository prerequisites.
- Workspace locality:
  - Workspaces are usually host-local, so moving an issue to a different host is typically a cold restart unless shared storage exists.
- Path and command safety:
  - Remote path resolution, shell quoting, and workspace-boundary checks matter more once execution crosses a machine boundary.
- Startup and failover semantics:
  - Implementations should distinguish host-connectivity/startup failures from in-workspace agent failures so the same ticket is not accidentally re-executed on multiple hosts.
- Host health and saturation:
  - A dead or overloaded host should reduce available capacity, not cause duplicate execution or an accidental fallback to local work.
- Cleanup and observability:
  - Operators need to know which host owns a run, where its workspace lives, and whether cleanup happened on the right machine.
