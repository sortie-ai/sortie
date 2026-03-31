---
name: managing-github-issues
description: >
  Create, edit, search, close, and triage GitHub issues for this project using
  gh CLI. Use when asked to file a bug, request a feature, create a task, report
  a problem, search the backlog, triage issues, or manage the issue tracker.
  Also use when the user says "create an issue", "file a bug", "open a ticket",
  "add to backlog", "search issues", "close issue", or mentions GitHub Issues
  in any task-management context. Handles label/milestone assignment, duplicate
  detection, and project board integration. Do NOT use for pull requests (use
  creating-pr) or changelog entries (use changelog-maintenance).
---

# Managing GitHub Issues

Manage the Sortie project backlog via `gh` CLI. Every issue is public — it
must be professional, self-contained, and actionable without prior context.

## Discover taxonomy (run first)

Before any create/edit/triage operation, fetch the live project taxonomy so
labels, milestones, and project board names are current:

```bash
bash .github/skills/managing-github-issues/scripts/get_taxonomy.sh
```

The script outputs issue types (with their GraphQL `node_id`s), labels grouped
by prefix, open milestones with issue counts, and the project board name. Use
this output — not memorized values — for `--label`, `--milestone`, `--project`
flags and the issue type `node_id` used in the post-creation GraphQL call.

If the script is unavailable, run these individually:

```bash
gh api "/orgs/{owner}/issue-types" --jq '.[] | "\(.node_id)\t\(.name)\t\(.description)"'
gh label list --limit 50
gh api repos/{owner}/{repo}/milestones --paginate \
  -q '.[] | select(.state=="open") | "\(.title)  (open: \(.open_issues), closed: \(.closed_issues))"'
gh project list --owner sortie-ai --limit 5
```

## Issue type conventions

GitHub Issue Types (organization-level) classify the kind of work. Every issue
must have exactly one type. Available types: Task, Bug, Feature, Docs,
Research, Refactor, Test.

`gh issue create` does **NOT** support a `--type` flag. Set the issue type
after creation via GraphQL using the `node_id` from the taxonomy output (see
"Composing the command" below). The taxonomy script lists available types with
their `node_id`s — use its output, not memorized values.

## Label conventions

Labels use a `prefix:name` convention (Kubernetes/CNCF style).
Type classification is handled by Issue Types (above), not labels.

- **`area:` prefix** — at least one per issue. Identifies which subsystem is
  affected (orchestrator, persistence, server, etc.).
- **`status:` prefix** — do not apply at creation. Set when work state changes.
- **Other labels** (`enterprise`, `good first issue`, `help wanted`) — apply
  when relevant.

Pick labels from the live taxonomy output, not from memory. Labels may be
added or renamed between sessions.

## Milestone conventions

Every issue must belong to a milestone — it is the primary organizational
axis. The user may refer to milestones by shorthand ("M10", "milestone 10").
Match the shorthand against the full titles from the taxonomy output.

If the user omits the milestone, ask. Do not guess.

## Before creating an issue

### Duplicate check (BLOCKING)

```bash
gh issue list --search "<keywords>" --state all --limit 20 \
  --json number,title,state,labels,milestone \
  --jq '.[] | "#\(.number) [\(.state)] \(.title)"'
```

- **Exact duplicate:** stop, report the existing issue number.
- **Partial overlap:** mention the related issue, ask whether to proceed.
- **No match:** proceed.

## Create

### Body rules

- **Language:** English.
- **Tone:** Professional, concise, no filler.
- **Privacy:** No usernames, API keys, internal URLs, or personal information.
- **Self-contained:** A stranger must understand the issue from the body alone.
- **Bugs describe problems, not solutions.** The implementer chooses the fix.
- **Verification:** Every issue states how to confirm it is resolved.
- **No hard wrapping.** Write each paragraph as a single line — let GitHub
  handle word wrap at render time. Hard line breaks mid-sentence produce
  ragged diffs and noisy reflows when text is edited later.

### Body templates by issue type

#### Bug

```markdown
## Summary

One-paragraph description of what is broken and how it deviates from expected
behavior.

## Root Cause (observed)

What investigation has revealed so far. Include relevant code paths, log
output, or state transitions. Only include this section if the root cause is
known or strongly suspected.

## Symptoms

Bullet list of observable effects:
- What the user or operator sees
- Error messages, incorrect output, missing data
- Which components are affected

## Steps to Reproduce

1. Numbered steps to trigger the bug
2. Include config, commands, or setup needed
3. State the actual vs expected result

## Verification

The bug is fixed when:
- Specific observable condition that proves correctness
- Test that should pass
```

#### Feature

```markdown
One-paragraph description of the capability and its motivation (why this
matters, what it unblocks).

[Detail paragraphs as needed: behavior, config fields, CLI flags, defaults,
edge cases. Reference related issues with #NNN.]

Verify: concrete command, test, or observable outcome that proves the feature
works. Include default-behavior-preserved check if applicable.
```

#### Research

```markdown
One-paragraph framing of the question or uncertainty to resolve.

Options to evaluate:
- Option A with brief rationale
- Option B with brief rationale
- Option C with brief rationale

Priority: [high|medium|low] with brief justification.

Verify: deliverable that proves the research is complete (ADR, design doc,
prototype, benchmark results).
```

#### Docs

```markdown
One-paragraph description of what documentation is missing, outdated, or
incorrect.

[Specifics: which pages, sections, or examples need attention.]

Verify: documentation is published/merged and covers the stated gap.
```

#### Refactor / Test

```markdown
One-paragraph description of what to improve and why (readability, coverage,
performance, maintainability).

[Specifics: which packages, files, or patterns are affected.]

Verify: concrete condition (test passes, coverage threshold, lint clean,
benchmark result).
```

### Title conventions

- Imperative mood, capitalize first word: "Add X", "Fix Y", "Implement Z"
- Backtick-wrap code identifiers: `` Add `--host` flag for HTTP bind address ``
- Under 80 characters, no trailing period
- No type prefix in the title — issue types handle categorization

### Composing the command

Use single quotes for `--body` to prevent shell interpolation. Escape
literal single quotes in the body with `'\''`.

`gh issue create` does **NOT** support `--type`. Create the issue first, then set
the type via GraphQL using the `node_id` from the taxonomy output.

```bash
# Step 1: create the issue
ISSUE_URL=$(gh issue create \
  --title '<title>' \
  --body '<body>' \
  --label '<area-label>' \
  --milestone '<full milestone title from taxonomy>' \
  --project '<project name from taxonomy>')

ISSUE_NUMBER=$(echo "$ISSUE_URL" | grep -oE '[0-9]+$')

# Step 2: set the issue type (node_id from taxonomy output)
ISSUE_NODE_ID=$(gh api "repos/{owner}/{repo}/issues/${ISSUE_NUMBER}" --jq '.node_id')
gh api graphql -f query="
mutation {
  updateIssue(input: {
    id: \"${ISSUE_NODE_ID}\"
    issueTypeId: \"<type node_id from taxonomy>\"
  }) {
    issue { number title }
  }
}"
```

### Batch creation

When creating multiple related issues:

1. Present all planned issues as a numbered list (title, issue type, area
   labels, milestone) before creating any.
2. Wait for user confirmation.
3. Create sequentially. Report each issue number after creation.
4. Print a summary table when done:

```
| # | Title | Type | Labels | Milestone |
|---|-------|------|--------|-----------|
```

## Search

```bash
# By keyword
gh issue list --search "<query>" --state all --limit 30

# By label
gh issue list --label "<label>" --state open

# By milestone (use full title from taxonomy)
gh issue list --milestone "<full title>" --state open

# Combined filters
gh issue list --label "<label>" --milestone "<full title>"

# Detailed view
gh issue view <number>
```

## Edit

```bash
gh issue edit <number> --title '<new title>'
gh issue edit <number> --add-label '<label>'
gh issue edit <number> --remove-label '<label>'
gh issue edit <number> --milestone '<full milestone title>'
gh issue edit <number> --body '<new body>'
gh issue edit <number> --add-project '<project name>'
```

Confirm destructive edits (body replacement, milestone change) with the user
before executing.

## Close

```bash
# Completed
gh issue close <number> --reason completed --comment 'Resolved in #<PR>'

# Not planned
gh issue close <number> --reason "not planned" --comment '<reason>'
```

Always include `--comment` with a reason. Reference the resolving PR when
applicable.

## Triage

When triaging a finding into the backlog:

1. **Extract the concern** from context (code, logs, review, conversation).
2. **Duplicate check** (see above).
3. **Classify** — choose issue type and area labels from the taxonomy output.
4. **Place in milestone** — match the milestone whose theme fits the concern.
   Read milestone titles from taxonomy output to pick correctly.
5. **Draft the issue** following the body template for the chosen type.
6. **Present for review** — show the full `gh issue create` command before
   executing.

## Quality checklist

Before executing `gh issue create`, verify:

- [ ] Taxonomy was fetched this session (issue types, labels, milestones are current)
- [ ] Duplicate check was performed
- [ ] Title: imperative, under 80 chars, no trailing period
- [ ] Issue type set via GraphQL `updateIssue` with `node_id` from taxonomy
- [ ] At least one `area:` label from taxonomy
- [ ] Milestone set using full title from taxonomy
- [ ] Body matches the template for its issue type
- [ ] Verification criteria present and testable
- [ ] No private information (usernames, keys, internal URLs)
- [ ] No solution prescribed in bug reports
- [ ] Related issues referenced with `#NNN` where applicable
- [ ] `--project` flag uses project name from taxonomy

## Error recovery

| Error | Recovery |
|---|---|
| Issue type not found | Re-run taxonomy script; use exact `node_id` from output |
| Milestone not found | Re-run taxonomy script; use exact title from output |
| Label not found | Re-run taxonomy script; label may have been renamed |
| Project not found | `gh project list --owner sortie-ai` to verify name |
| HTTP 403 | `gh auth refresh -s project` to add project scope |
| HTTP 422 | Check for duplicate title or missing required fields |

## Handoff

- If issues need immediate implementation → user may invoke Planner or Coder.
