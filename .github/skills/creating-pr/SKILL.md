---
name: creating-pr
description: >
  Use when asked to create a pull request, open a PR, or submit changes
  for review. Handles branch verification, change analysis, title and
  description generation, and gh pr create. Do NOT use for committing
  (use git-commit), pushing without PR, or reviewing existing PRs.
---

# Creating a Pull Request

## Workflow

### Step 1: Verify branch state

```bash
CURRENT=$(git branch --show-current)
DEFAULT=$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name')
```

- If `$CURRENT` equals `$DEFAULT` or is `develop`/`release/*`/`hotfix/*`:
  inform user they are on a protected branch, cannot create PR from here
- If uncommitted changes exist: commit first (use git-commit skill)
- If branch not pushed: `git push -u origin $CURRENT`

### Step 2: Analyze changes

```bash
DEFAULT=$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name')
git log --format="%s%n%b" "$DEFAULT..HEAD"
git diff --name-only "$DEFAULT..HEAD"
git diff --stat "$DEFAULT..HEAD"
```

From the diff and commits, identify:

- **Type**: primary change type (feat, fix, refactor, chore, perf)
- **Intent**: business/technical goal (1-2 sentences)
- **Entry point**: most critical changed file for reviewer
- **Sensitive areas**: files needing extra scrutiny (auth, payments, data)
- **Breaking changes**: `!` in commits or BREAKING CHANGE footer
- **Migrations**: database or schema changes

### Step 3: Generate title

Conventional Commits format: `<type>[scope]: <description>`

- Imperative mood, under 72 chars, no period, English only
- Match the project's commit style (check `git log --format="%s" -20`)

### Step 4: Generate description

Use the template from `assets/pull_request_template.md`. Three sections:

1. **Scope & Context** — Type, Intent, Related Issues
2. **Reviewer Guide** — Complexity (Low/Medium/High), Entry Point, Sensitive Areas
3. **Risk Assessment** — Breaking Changes, Migrations/State

Formatting rules:

- No fluff intros ("This PR updates...")
- Filenames in backticks: \`path/to/file.ts\`
- Use " - " (hyphen), not "—" (em-dash)
- All sections required, sub-sections only when relevant data exists

Do not reference `docs/architecture.md`, `docs/decisions/`, section numbers,
ADR numbers, or TODO IDs in pull request descriptions. Those belong in specs and
plans, not in the git history.

Complexity guide:

| Level  | Criteria                                              |
| ------ | ----------------------------------------------------- |
| Low    | Single file, config, docs, simple fix                 |
| Medium | Multiple related files, new feature with tests        |
| High   | Cross-cutting, migrations, breaking changes, security |

### Step 5: Create PR

```bash
DEFAULT=$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name')
gh pr create \
  --title '<title>' \
  --body '<description>' \
  --base "$DEFAULT"
```

For drafts, add `--draft`.

MANDATORY: Use single quotes for `--body` to avoid shell interpolation. **NEVER** use double quotes, which can cause variables or special characters in the description to be misinterpreted by the shell.

### Step 6: Verify

```bash
gh pr view --web
```

Report: PR number, URL, title, base/head branches.

## Error Recovery

| Error                         | Fix                                     |
| ----------------------------- | --------------------------------------- |
| "pull request already exists" | `gh pr view` to see existing            |
| "no commits between"          | Verify branch has commits ahead of base |
| Auth failure                  | `gh auth login --web`                   |
