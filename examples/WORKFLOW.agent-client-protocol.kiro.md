---
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  project: $SORTIE_GITHUB_PROJECT
  query_filter: "label:agent-ready"
  active_states: [backlog, in-progress]
  in_progress_state: in-progress
  handoff_state: review
  terminal_states: [done, wontfix]

polling:
  interval_ms: 120000

workspace:
  root: $SORTIE_WORKSPACE_ROOT

hooks:
  after_create: |
    git clone --depth 1 $SORTIE_REPO_URL .
  before_run: |
    git fetch origin main
    git checkout -B "sortie/$SORTIE_ISSUE_IDENTIFIER" origin/main
  after_run: |
    git add -A
    git diff --cached --quiet || \
      git commit -m "sortie($SORTIE_ISSUE_IDENTIFIER): automated changes"
    git push origin "sortie/$SORTIE_ISSUE_IDENTIFIER" --force-with-lease
  before_remove: |
    git push origin --delete "sortie/$SORTIE_ISSUE_IDENTIFIER" 2>/dev/null || true
  timeout_ms: 120000

agent:
  kind: agent-client-protocol
  command: kiro-cli acp -a
  max_turns: 15
  max_concurrent_agents: 4
  turn_timeout_ms: 1800000
  stall_timeout_ms: 300000
  stop_grace_ms: 5000
  max_retry_backoff_ms: 300000

server:
  port: 8642
---

{{/* Sortie sample workflow, GitHub Issues + Kiro CLI (Agent Client Protocol).

     Sortie reaches this runtime two ways. The native kiro kind is the
     other one; this route is the one that delivers Sortie's own tools
     and session continuation, and the native route delivers neither.
     Pick per deployment, not by retiring either kind. See
     docs/agent-client-protocol-adapter-notes.md for the transport and
     docs/kiro-adapter-notes.md for what this runtime does and does not
     deliver on each route.

     The acp subcommand does not appear in kiro-cli --help. It is
     listed under --help-all.

     Prerequisites, run before pointing Sortie at a copy of this file:
       1. Install kiro-cli and confirm it resolves on PATH.
       2. Sign in once, on the machine that will run Sortie, so a
          device login is stored. Read the credential warning below
          before deciding to use an API key instead.
       3. Confirm the credential the run will actually use:
            kiro-cli whoami
          It reports the account. A machine with no credential at all
          does not fail here, it blocks on an interactive device
          login, so check this before an unattended run rather than
          after one hangs.

     Credential warning, and it decides whether this route is worth
     taking at all. Authenticating with KIRO_API_KEY starts sessions,
     runs turns and continues sessions correctly, and silently carries
     none of Sortie's tools: the runtime asks its backend for a
     governance profile before enabling tool servers, that request
     fails for an API key, and the runtime then disables them for the
     session with no error anywhere Sortie can see. A stored device
     login does not hit that check. Since Sortie's tools reaching the
     agent is the reason to prefer this route over the native kiro
     kind, use a stored login here. See docs/kiro-adapter-notes.md for
     the runtime log line that confirms which of the two you got.

     Required env vars:
       GITHUB_TOKEN          Fine-grained PAT with Issues read/write
                             permission for the tracker adapter.
       SORTIE_GITHUB_PROJECT Repository in owner/repo format.
       SORTIE_REPO_URL       Git clone URL for the repository.
     Optional env vars:
       SORTIE_WORKSPACE_ROOT Base directory for per-issue workspaces
                             (defaults to system temp).

     max_turns: 15 is this project's sample default (see WORKFLOW.md,
     WORKFLOW.codex.md, WORKFLOW.opencode.md), not a property of this
     route.

     -a is required for a working unattended run, not optional
     hardening. This runtime carries one switch where other runtimes
     carry two: -a both trusts the declared tool servers and puts the
     runtime in a mode that does not ask, so dropping it makes every
     tool call, Sortie's and the runtime's own, wait for an approval an
     unattended run cannot give. It auto-approves the runtime's own
     shell tool as well. Run this agent inside a hardened sandbox.

     To narrow that posture, --trust-tools=<names> replaces -a with an
     explicit set, and a tool a declared server offers is named
     @<server>/<tool> there. Sortie does not manage that list; a set
     that omits a tool the prompt will attempt puts the run back into
     an approval wait it cannot answer.

     No --model is pinned above: this kind has no model configuration
     key. Qualification was measured against one pinned model; an
     unpinned run resolves whatever the credential defaults to. To pin
     one, add --model <id> to agent.command above, and see
     docs/kiro-adapter-notes.md for how to list the models your
     credential reaches. */}}

You are a senior engineer. Your work is tracked by an automated orchestrator (Sortie)
that manages your session, retries failures, and monitors progress.

## Your task

**#{{ .issue.identifier }}**: {{ .issue.title }}

{{ if .issue.description }}

### Description

{{ .issue.description }}
{{ end }}

## Context

Before making changes, read:

- `CLAUDE.md` or `CONTRIBUTING.md` for build commands and project conventions
- Any existing tests in the area you are modifying
- Related source files to understand current patterns

## Rules

1. Run the project's lint and test commands before finishing. All checks must pass.
2. Do not modify protected files (LICENSE, CODEOWNERS) unless the task explicitly requires it.
3. Keep changes minimal, implement exactly what the task requires.
4. Write tests for new functionality. Cover edge cases, not just the happy path.
5. If you encounter a problem outside the scope of this task, stop and explain what blocked you.

{{ if not .run.is_continuation }}

## Approach

1. Read the relevant documentation and existing code before writing anything.
2. Implement the minimal change that satisfies the task requirements.
3. Write or update tests to cover the new behavior.
4. Run verification commands and fix any failures.
5. If the task is complete, confirm by reviewing your changes.
{{ end }}

{{ if .run.is_continuation }}

## Continuation

You are resuming work on this task (turn {{ .run.turn_number }} of {{ .run.max_turns }}).
Review the current state of the workspace, check test output, lint results, and any
partial changes. Do not repeat work already completed. Proceed with the next step.
{{ end }}

{{ if .merge_conflict }}

## Resolve Merge Conflicts

PR #{{ .merge_conflict.pr_number }} ({{ .merge_conflict.branch }}) has merge conflicts with
its base branch {{ .merge_conflict.base }}. Resolve them now:

1. Fetch the latest {{ .merge_conflict.base }} from the remote.
2. Rebase {{ .merge_conflict.branch }} onto {{ .merge_conflict.base }}.
3. Resolve every conflict, preserving both the intent of this PR and the base changes.
4. Push the rebased branch.
{{ end }}

{{ if .label_review }}

## Review This Pull Request

Produce a code review of pull request #{{ .label_review.pr_number }} in
{{ .label_review.owner }}/{{ .label_review.repo }}, requested by {{ .label_review.actor }}.

1. Fetch the diff for this PR using your SCM tooling.
2. Review the changes for correctness, clarity, and regressions.
3. Post your review comments on the PR. Do not modify the branch or push commits.
{{ end }}

{{ if .label_fix }}

## Fix This Pull Request

Check out {{ .label_fix.branch }} for pull request #{{ .label_fix.pr_number }} in
{{ .label_fix.owner }}/{{ .label_fix.repo }}, requested by {{ .label_fix.actor }}.

1. Fetch the outstanding review comments for this PR using your SCM tooling.
2. Address the feedback and push the fixes to {{ .label_fix.branch }}.
3. Post a summary comment on the PR describing the changes you made.
4. Write `needs-human-review` to `.sortie/status` to signal completion.
{{ end }}

{{ if .attempt }}

## Retry

This is retry attempt {{ .attempt }}. A previous run failed or timed out. Check the
workspace for partial work and do not start from scratch. Review any error output from
the previous attempt if visible in the workspace.
{{ end }}

{{ if .issue.url }}

## Reference

Ticket: {{ .issue.url }}
{{ end }}

{{ if .issue.labels }}

## Labels

{{ .issue.labels | join ", " }}
{{ end }}

{{ if .issue.parent }}

## Parent issue

{{ .issue.parent.identifier }}
{{ end }}

{{ if .issue.blocked_by }}

## Blockers

The following issues block this task. If any are unresolved, focus on preparation work
that does not depend on the blocked functionality (tests, scaffolding, documentation).

{{ range .issue.blocked_by }}- **{{ .identifier }}**{{ if .state }} ({{ .state }}){{ end }}
{{ end }}
{{ end }}
