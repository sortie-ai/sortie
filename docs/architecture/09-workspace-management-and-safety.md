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
- On Windows, `cmd.exe /C <script>` is the conforming default. The hook subprocess is assigned to
  a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so that timeout-triggered termination
  kills the entire process tree, not just the direct child.
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

- `SORTIE_ISSUE_ID` — tracker-internal issue ID.
- `SORTIE_ISSUE_IDENTIFIER` — human-readable ticket key.
- `SORTIE_WORKSPACE` — absolute path to the per-issue workspace directory.
- `SORTIE_ATTEMPT` — current attempt number.

Hook environment variables available only to `after_run`:

- `SORTIE_SELF_REVIEW_STATUS` — self-review outcome for the current run: `"disabled"`,
  `"passed"`, `"cap_reached"`, or `"error"`. Defaults to `"disabled"` when self-review
  is not configured.
- `SORTIE_SELF_REVIEW_SUMMARY_PATH` — absolute path to `.sortie/review_summary.md` in
  the workspace. Absent when self-review did not run or summary was not written.

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
  polling is skipped. The `owner` field is the authoritative source of SCM repository identity —
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

