---
name: git-commit
description: >
  Use when asked to commit, save, or persist changes to Git.
  Handles atomic commits, branch safety, Conventional Commits format,
  and project style matching. Do NOT use for pushing, creating PRs,
  or branch management beyond safety checks.
metadata:
  version: 2026-04-12
---

# Git Commit

## Workflow

### Step 1: Identify changes and group atomically

```bash
git status --short
git diff
git diff --cached
```

Each commit = one logical change. Split unrelated changes into separate commits.

| Situation                          | Commits   |
| ---------------------------------- | --------- |
| New service + its tests            | 1 commit  |
| New feature + unrelated config fix | 2 commits |
| Multiple files for one feature     | 1 commit  |

- If user says "commit all" — group into logical atomic commits
- If ambiguous — ask which files and grouping

### Step 2: Check branch safety (BLOCKING)

```bash
git branch --show-current
```

Protected branches: `main`, `master`, `develop`, `release/*`, `hotfix/*`.

**STOP if on a protected branch.** Do not commit. Do not proceed to Step 3.
Instead:

1. Inform the user: "Cannot commit to `<branch>` — it is a protected branch."
2. Create a feature branch: `git checkout -b <type>/<kebab-description>`
3. Only then continue to Step 3.

If on a feature branch: proceed.

#### Branch naming convention

Format: `<type>/<kebab-case-description>`

| Type       | Use Case           | Example                            |
| ---------- | ------------------ | ---------------------------------- |
| `feat`     | New feature        | `feat/bill-reminders`              |
| `fix`      | Bug fix            | `fix/null-amount-validation`       |
| `refactor` | Code restructuring | `refactor/extract-payment-service` |
| `chore`    | Maintenance tasks  | `chore/update-dependencies`        |
| `docs`     | Documentation      | `docs/api-reference`               |
| `test`     | Test additions     | `test/payment-service-coverage`    |

### Step 3: Match project commit style

```bash
git log --format="%s" -20
```

Identify vocabulary, detail level, scope patterns. Mimic the project's
phrasing while following Conventional Commits format.

See `references/commit-format.md` for type table, rules, and anti-patterns.

### Step 4: Stage and commit

```bash
git add <files>
git commit -m "<type>[scope]: <description>"
```

For multi-line messages:

```bash
git commit -m "<subject>" -m "<body>"
```

Subject line: imperative mood, under 72 chars, no period, English only.
Body (if needed): wrap at 72 chars, explain what and why.

Do not reference `docs/architecture.md`, `docs/decisions/`, section numbers,
or ADR numbers in commit messages. Those belong in specs and plans,
not in the git history.

### Step 5: Verify

```bash
git log --oneline -1
git show --stat HEAD
```

Report: commit hash, files changed, insertions/deletions.

## Error Recovery

| Error                 | Fix                                                               |
| --------------------- | ----------------------------------------------------------------- |
| "nothing to commit"   | Check `git status`, verify files have changes                     |
| Pre-commit hook fails | Read the error, fix the issue, create a NEW commit (do not amend) |
| Wrong files committed | `git reset --soft HEAD~1`, re-stage correctly, commit again       |

## Handoff

If the user also asked to create a PR, invoke the `creating-pr` skill after
committing. Do not hand-roll `gh pr create` — the skill has a required template.
