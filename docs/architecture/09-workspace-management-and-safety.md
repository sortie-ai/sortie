## 9. Workspace Management and Safety

### 9.1 Workspace Layout

Workspace root:

- `workspace.root` (normalized path; the current config layer expands path-like values and preserves
  bare relative names)

Per-issue workspace path:

- `<workspace.root>/<sanitized_issue_identifier>`

Workspace persistence:

- Workspaces are reused across runs for the same issue.
- Successful runs do not auto-delete workspaces.

Workspace cleanup runs through the following mechanisms, ordered and non-overlapping in intent:

- **The terminal gate** is the primary mechanism. It is always on and unconditional: whenever the
  tracker reports an issue's state as a member of `tracker.terminal_states`, the workspace for that
  issue is removed, whether that gate fires from startup cleanup, active-run reconciliation, or the
  periodic sweep.
- **The age bound** is a backstop for the population the terminal gate can never reach: an issue
  parked in the workflow's handoff state with no external automation advancing it, an issue left
  active after a permanent failure, an issue moved to a state the configuration does not name, or
  an issue deleted from the tracker entirely. It is opt-in and off by default, configured by
  `workspace.retention_days`, and evaluated only by the periodic sweep. A workspace is removable by
  age when its latest recorded activity is older than the configured window. That activity is
  anchored on the later of two timestamps: the most recent `completed_at` recorded in the run
  history for the workspace's identifier, and the `pushed_at` value recorded in the workspace's
  `.sortie/scm.json`. A workspace with neither a parseable completion nor a parseable recorded push
  is retained regardless of how long it has sat on disk: absence of a record is absence of
  evidence, not evidence of age.

The age bound never fires on a workspace the terminal gate would have removed on the same pass,
because the terminal check runs first. It applies only to the periodic sweep, not to startup
cleanup or active-run reconciliation; see the polling and reconciliation material for why.

### 9.2 Workspace Creation and Reuse

Input: `issue.identifier`

Algorithm summary:

1. Sanitize identifier to `workspace_key`.
2. Compute workspace path under workspace root.
3. Ensure the workspace path exists as a directory.
4. Mark `created_now=true` only if the directory was created during this call; otherwise
   `created_now=false`.
5. If `created_now=true`, run `after_create` hook if configured.

Notes:

- This section does not assume any specific repository/VCS workflow.
- Workspace preparation beyond directory creation (for example dependency bootstrap, checkout/sync,
  code generation) is implementation-defined and is typically handled via hooks.

### 9.3 Optional Workspace Population (Implementation-Defined)

Sortie does not require any built-in VCS or repository bootstrap behavior.

Implementations may populate or synchronize the workspace using implementation-defined logic and/or
hooks (for example `after_create` and/or `before_run`).

Failure handling:

- Workspace population/synchronization failures return an error for the current attempt.
- If failure happens while creating a brand-new workspace, implementations may remove the partially
  prepared directory.
- Reused workspaces should not be destructively reset on population failure unless that policy is
  explicitly chosen and documented.

### 9.4 Workspace Hooks

Supported hooks:

- `hooks.after_create`
- `hooks.before_run`
- `hooks.after_run`
- `hooks.before_remove`

Execution contract:

- Execute in a local shell context appropriate to the host OS, with the workspace directory as
  `cwd`.
- On POSIX systems, `sh -c <script>` is the conforming default; `bash -lc <script>` may be used
  when a login shell environment is required.
- On Windows, `cmd.exe /C <script>` is the conforming default. The hook subprocess is created
  suspended and assigned to a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` before it is
  resumed, so no unsupervised execution window exists in which a spawned descendant could start
  before the job assignment takes effect. Timeout-triggered termination therefore kills the
  entire process tree, not just the direct child. Job Object creation is not a precondition for
  running the hook: when it fails, the failure is logged, the hook still runs, and that run has
  no process-tree termination guarantee.
- Hook timeout uses `hooks.timeout_ms`; default: `60000 ms`.
- Log hook start, failures, and timeouts. A failure or timeout record carries the hook's captured
  combined stdout and stderr under `hook_output`; a hook that succeeds with output emits it in a
  debug-level record.
- Output capture retains the last 8 KiB of the combined stream (the tail, where failure
  diagnostics appear), prefixed with a truncation marker when earlier output was dropped.

Failure semantics:

- `after_create` failure or timeout is fatal to workspace creation.
- `before_run` failure or timeout is fatal to the current run attempt.
- `after_run` failure or timeout is logged and ignored.
- `before_remove` failure or timeout is logged and ignored.

Hook environment variables available to all hooks:

- `SORTIE_ISSUE_ID`: tracker-internal issue ID.
- `SORTIE_ISSUE_IDENTIFIER`: human-readable ticket key.
- `SORTIE_WORKSPACE`: absolute path to the per-issue workspace directory.
- `SORTIE_ATTEMPT`: current attempt number.

Hook environment variables available only to `after_run`:

- `SORTIE_SELF_REVIEW_STATUS`: self-review outcome for the current run: `"disabled"`,
  `"passed"`, `"cap_reached"`, or `"error"`. Defaults to `"disabled"` when self-review
  is not configured.
- `SORTIE_SELF_REVIEW_SUMMARY_PATH`: absolute path to `.sortie/review_summary.md` in
  the workspace. Absent when self-review did not run or summary was not written.

#### 9.4.1 Handoff Evidence Baseline

For a run whose frozen `tracker.handoff_evidence` policy is `observed` or `strict`, the
orchestrator captures a workspace baseline at the boundary between preparation and agent launch:
after directory creation/reuse, population, and the applicable pre-run hooks have finished, and
before `StartSession` can begin agent work. Both the full preparation path and the ensure-only path
must reach this boundary. Under `off`, no baseline is captured.

The baseline records the workspace's committed position and enough working-tree state to determine
later whether that state changed. Evidence is a difference from this baseline, not a property of the
tree at exit:

- A moved committed position or a working-tree state that differs from the baseline is work
  observed.
- Workspace SCM metadata naming a pushed commit or pull request for the issue is also work observed,
  even when an earlier run wrote that metadata. Per-issue workspaces are reused, so this positive
  signal prevents a later no-op run from stranding work that was already pushed.
- When no positive signal holds, a successful inspection against a baseline is absence of work
  observed. A missing baseline, a non-version-controlled workspace, a failed inspection, or no
  recorded workspace yields evidence not determinable instead.

No particular source-control command or baseline representation is prescribed. The contract must
distinguish pre-existing uncommitted state from changes made by this run and must recognize a commit
created by `after_run` even when that hook leaves the tree clean.

The `after_run` contract itself is unchanged. Its failure remains logged and ignored, and its exit
status has no vote in the handoff disposition. Any files, commits, pushes, or SCM metadata the hook
produces are visible only through the same workspace evidence rules as other run output.

### 9.5 Workspace SCM metadata (`.sortie/scm.json`)

The `.sortie/scm.json` file is a workspace-level file that carries SCM metadata written by the
agent, a post-push hook, or any process running inside the workspace. The orchestrator reads this
file after a normal worker exit to determine the git ref for CI status queries and the PR identity
for review comment polling. The file is a shared workspace-level SCM metadata contract that CI
feedback, review comment routing, and other features can reuse.

`SCMMetadata` fields:

- `branch` (string, required for CI): the branch name (e.g. `feature/PROJ-42`). If empty or
  absent, the file is treated as missing and CI status queries are skipped.
- `sha` (string, optional): the commit SHA at push time. When present, the orchestrator passes
  this to `CIStatusProvider.FetchCIStatus` instead of the branch name for deterministic results.
- `pushed_at` (string, optional): ISO-8601 timestamp of the push. Startup pending reaction recovery
  uses this timestamp to skip stale SCM activity. When absent, recovery falls back to
  `run_history.completed_at`.
- `pr_number` (integer, optional): the pull request number associated with this branch. Zero or
  absent when no PR has been created. Written by the agent or post-push hook. When positive and
  `owner` and `repo` are non-empty AND `reactions.review_comments.provider` is configured, the
  orchestrator creates a pending `review`-kind reaction on normal worker exit. When positive and
  `owner` and `repo` are non-empty AND `reactions.auto_merge.provider` is configured, the
  orchestrator creates a pending `merge`-kind reaction on normal worker exit. When `pr_number`
  is `0`, both review polling and auto-merge are skipped.
- `owner` (string, optional): the SCM repository owner (e.g. GitHub org or user). Written by
  the agent alongside `pr_number`. Required for review comment polling; when empty, review
  polling is skipped. The `owner` field is the authoritative source of SCM repository identity;
  it is never derived from the tracker project configuration, which may be a Jira key or other
  non-SCM identifier.
- `repo` (string, optional): the SCM repository name. Written by the agent alongside
  `pr_number`. Required for review comment polling; when empty, review polling is skipped.

Safety and parsing rules:

- Maximum file size: 4096 bytes. Oversized files are rejected and logged at warn level.
- Symlink rejection: both `.sortie/` and `.sortie/scm.json` are checked via `Lstat` before
  reading. If either is a symbolic link, the file is rejected and logged at warn level. This
  prevents symlink-based path escape attacks.
- Malformed JSON is rejected and logged at warn level.
- The function never returns an error to the caller; all failure modes degrade gracefully to a
  zero-value metadata struct (CI queries are skipped).

#### 9.5.1 `.sortie/status` lifecycle

A run that enters the self-review phase has the file removed at four points in its lifetime, all
best-effort and all subject to the same `Lstat` symlink rejection described for `.sortie/scm.json`
above: no removal follows a symbolic link at `.sortie/` or at the file itself, and a rejected or
failed removal is logged and does not affect the run.

The first removal happens before each new dispatch to a workspace, so a stale value from a
previous run cannot affect the new one. The second happens during a run, at the moment the
orchestrator acts on a recognized value that admits the run to the self-review phase: the removal
runs immediately before the phase's first review turn, so the phase's own first read does not
observe the value that admitted it. The third and fourth happen inside the phase itself, after
each review turn and after each fix turn, whenever the value read there is recognized. Together
these four removals keep the file stating what the agent has said since the phase last acted on
it, rather than carrying forward a value already acted on.

The read after each completed coding turn, outside the self-review phase, removes nothing: a
recognized value read there is left in the file, and a run that never enters the phase carries
that value through to teardown.

### 9.6 Safety Invariants

This is the most important portability constraint.

Invariant 1: Run the coding agent only in the per-issue workspace path.

- Before launching the coding-agent subprocess, validate:
  - `cwd == workspace_path`

Invariant 2: Workspace path must stay inside workspace root.

- Normalize both paths to absolute.
- Require `workspace_path` to have `workspace_root` as a prefix directory.
- Reject any path outside the workspace root.

Invariant 3: Workspace key is sanitized.

- Only `[A-Za-z0-9._-]` allowed in workspace directory names.
- Replace all other characters with `_`.

Invariant 4: A workspace key held by an entry in the running map or the retry map is never a sweep
candidate, whatever its age, and so is removed by neither the terminal gate nor the age bound on
that pass. These exclusions are absolute; the age bound does not weaken them.

Invariant 4a: A workspace key held by a pending reaction entry is a sweep candidate unless that
entry's kind carries an expiry. A kind whose pending entry is bounded by a TTL pins its workspace
for as long as the entry lives; a kind that polls an external fact for an unbounded time does not,
because pinning it would hold the directory for as long as that poll runs. A reaction kind the
orchestrator does not recognize pins, so an unclassified kind fails toward retention rather than
removal. A kind that does not pin MUST NOT depend on the workspace directory surviving between its
own polls. A kind whose pending entry is configured with an unbounded watch window
(`watch_window_ms: 0`) still pins its workspace for as long as the entry lives; the entry's own
terminal-state release remains the exit that ends the pin. Pinning stays keyed on kind, not on the
configured value.

Invariant 5: `workspace.retention_days` cannot be configured below its floor, and that floor in
days equals the pending-reaction recovery lookback in days. Any workspace the age bound may remove
is one that pending reaction recovery would already have skipped as stale, so removing it cannot
silently break recovery for an issue recovery still regards as live.

Invariant 6: The age bound performs no tracker write, no source-control write, no reaction
fingerprint write, and no creation or deletion of a pending reaction entry. Reaction state is
read-only to the age bound. Every removal it performs routes through the same workspace removal
path as the terminal gate, so key sanitization, containment under the workspace root, and the
`before_remove` hook apply unchanged. The age bound introduces no new way to reach the filesystem.
