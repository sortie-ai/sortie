---
name: changelog-maintenance
description: >
  Use when asked to update the changelog, document version changes, prepare
  a release, or add entries for recent work. Handles CHANGELOG.md updates
  following Keep a Changelog format and Semantic Versioning. Do NOT use for
  committing (use git-commit) or creating release notes outside CHANGELOG.md.
metadata:
  version: 2026-04-12
---

# Changelog Maintenance

Sortie's changelog speaks to operators and integrators who deploy and configure
the service. Every entry must answer: "Does this change affect someone who
installs, upgrades, configures, or integrates with Sortie?" If not, omit it.

Authoritative references:

- [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/)
- [Common Changelog](https://common-changelog.org/)
- [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)

## When to use

- Adding entries for new features, fixes, or breaking changes.
- Preparing a release: moving Unreleased entries under a versioned heading.
- Creating CHANGELOG.md from scratch when it does not exist.

## Workflow

### Step 1: Read the current changelog

```bash
cat CHANGELOG.md
```

If the file does not exist, create it with the preamble from Step 4.

### Step 2: Gather changes

**The merged PR is the atomic unit of a changelog entry — not the commit.**
A single PR often contains the feature commit, follow-up fixes, review
feedback, test additions, and docs updates. These are one logical change and
produce one changelog bullet. Never split a PR's commits into separate entries.

#### 2a: Identify the version boundary

```bash
# Find the last tag and its commit
git tag --sort=-version:refname | head -5
git log --oneline -1 "$(git describe --tags --abbrev=0 2>/dev/null)"
```

#### 2b: Determine the release window and list merged PRs

First, find the date of the last tag — this is the start of the release window:

```bash
# Date of the last tag (use as the window start)
git log -1 --format="%ai" "$(git describe --tags --abbrev=0 2>/dev/null)"
```

**Primary: milestone-based** (when milestones are set):

```bash
# PRs in a specific milestone (e.g., M10)
gh pr list --state merged --limit 100 \
  --json number,title,mergedAt,milestone,labels \
  --jq '.[] | select(.milestone != null and (.milestone.title | startswith("M10")))
        | "\(.number)\t\(.mergedAt | split("T")[0])\t\(.title)"' \
  | sort -t$'\t' -k2
```

**Fallback: date-based** (when milestones are not set, replace YYYY-MM-DD with the tag date):

```bash
gh pr list --state merged --limit 200 \
  --json number,title,mergedAt,labels \
  --jq '.[] | select(.mergedAt >= "YYYY-MM-DDT00:00:00Z")
        | "\(.number)\t\(.mergedAt | split("T")[0])\t\(.title)"' \
  | sort -t$'\t' -k2
```

When preparing a new release, the window is: tag date (exclusive) → today.

#### 2c: Inspect individual PRs when needed

```bash
# PR title, body (scope/intent), and constituent commits
gh pr view <NUMBER> --json title,body --jq '"\(.title)\n\(.body)"' | head -40
gh pr view <NUMBER> --json commits --jq '.commits[].messageHeadline'

# Extract linked issues from the PR body (all GitHub closing keywords, case-insensitive;
# handles optional markdown bold, optional colon, cross-repo owner/repo#N, multiple per line)
gh pr view <NUMBER> --json body --jq '.body' \
  | grep -ioE '\*{0,2}(close[ds]?|fix(e[ds])?|resolve[ds]?|related|part of)[^:#*]*:?\*{0,2}[[:space:]]+([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#[0-9]+'
```

Use the PR body's **Scope & Context** section to understand the user-facing
impact. Note which issues the PR closes or references — these become part of
the reference block in the changelog entry. Do not rely on `git log --oneline`
— it shows commits, not logical changes.

If the user describes changes verbally, use that as the primary source.

### Step 3: Filter — decide what belongs

The changelog records **notable changes to the distributed software**. A change
is notable when it alters what a consumer of Sortie can observe: new
capabilities, changed behavior, fixed bugs, security patches, removed features,
or deprecation notices.

Apply the following filter to every commit or change before writing an entry.

**ALWAYS include:**

| Signal                                                     | Why it matters to consumers           |
| ---------------------------------------------------------- | ------------------------------------- |
| New user-facing feature (CLI flag, adapter, config option) | Operators discover new capabilities   |
| Changed behavior of existing feature                       | Operators must adjust usage           |
| Bug fix for incorrect behavior                             | Operators know issues are resolved    |
| Security or vulnerability fix                              | Operators must act on upgrades        |
| Deprecation of public interface                            | Operators prepare for removal         |
| Removal of feature or public interface                     | Operators must adapt before upgrading |
| Performance improvement with measurable impact             | Operators benefit from upgrading      |
| New or changed persistence schema (migration)              | Operators plan upgrade procedures     |
| Changed CLI flags, env vars, or config file format         | Operators must update deployment config |

**NEVER include — these are noise, not signal:**

| Noise                                                     | Why it does not belong                |
| --------------------------------------------------------- | ------------------------------------- |
| Internal variable/function/type renames                   | No observable effect on consumers     |
| Code formatting, whitespace, linting fixes                | No observable effect on consumers     |
| Test-only changes (new tests, test refactors)             | Not shipped to consumers              |
| CI/CD pipeline changes (workflows, actions)               | Not shipped to consumers              |
| Dotfile changes (`.gitignore`, `.github/*`, `CODEOWNERS`) | Not shipped to consumers              |
| Documentation-only changes (README, AGENTS.md, comments)  | Not shipped to consumers              |
| Merge commits                                             | Infrastructure artifact, not a change |
| Internal refactoring with no behavior change              | No observable effect on consumers     |
| Dev-only dependency bumps                                 | Not shipped to consumers              |
| Project scaffolding and repo housekeeping                 | Not shipped to consumers              |

**Edge cases — include only when the threshold is met:**

| Change                         | Include when...                                                  | Omit when...                            |
| ------------------------------ | ---------------------------------------------------------------- | --------------------------------------- |
| Dependency bump                | Major version, security fix, or changed behavior                 | Routine patch/minor with no user impact |
| Refactoring                    | It changes observable performance, error messages, or log output | Purely internal restructuring           |
| New internal module/package    | It introduces a new adapter or public API surface                | It reorganizes existing code            |
| ADR or architecture doc update | It records a decision that changes system behavior               | It clarifies existing behavior          |

When in doubt, ask: "If I were an operator reading this before upgrading, would
I need to know this?" If the answer is no, leave it out.

### Step 4: Classify each change

Place every surviving entry under exactly one category:

| Category       | When to use                                                                  |
| -------------- | ---------------------------------------------------------------------------- |
| **Added**      | New user-facing capability: CLI command, adapter, config option, API surface |
| **Changed**    | Existing behavior altered in a way consumers can observe                     |
| **Deprecated** | Still works but scheduled for removal in a future version                    |
| **Removed**    | Previously available feature or interface deleted                            |
| **Fixed**      | Bug fix — incorrect behavior corrected                                       |
| **Security**   | Vulnerability patch, dependency CVE fix                                      |

Writing rules:

- **One bullet per logical change between releases.** A logical change is
  everything the operator or integrator observes as a single unit of value.
  It may span multiple PRs and commits if they all deliver, refine, or fix
  the same capability within the release window.
- **Fold within-release churn.** If a feature is introduced in one PR and
  then corrected, polished, or adjusted in subsequent PRs before the release
  ships, all of that work produces **one** changelog entry describing the
  final state. From the operator's perspective there was no intermediate
  broken state — only the delivered result. Do not list each PR separately
  when they collectively form one observable change.
- **Fold sub-fixes into the feature entry.** If a PR introduces a feature and
  also fixes a bug discovered during its implementation (e.g., a race
  condition found while adding env overrides), describe the fix as part of
  the feature bullet — do not create a separate Fixed entry. Only create a
  standalone Fixed entry when a PR's sole purpose is a bug fix independent of
  any in-progress feature.
- Link every PR and issue parenthetically at the end of the bullet using full
  GitHub URLs — plain `#NNN` is not clickable in rendered markdown. Determine
  the repo URL from the comparison links at the bottom of CHANGELOG.md or via
  `git remote get-url origin`.
  - Issue: `[#NNN](https://github.com/OWNER/REPO/issues/NNN)`
  - PR: `[#NNN](https://github.com/OWNER/REPO/pull/NNN)`
  - When both an issue and its implementing PR are available, use issue.
  - If only PR is available, link the PR.
  - When multiple issues/PRs are explicitly linked, list all of them.
  - Multi-reference format — one reference per line inside the parens:
    ```
    ([#398](https://github.com/OWNER/REPO/issues/398),
    [#403](https://github.com/OWNER/REPO/issues/389))
    ```
  - Single reference stays on the same line: `([#403](https://github.com/OWNER/REPO/pull/403))`.
- Start each bullet with what changed, not with "Fixed" or "Added" (the heading
  already says that).
- Be specific: "`coroutine 'main' was never awaited` bug after async migration"
  not "Fixed async bug".
- Identify the subsystem when it helps locate the change: "Jira adapter:",
  "SQLite store:", "CLI:".
- Reference types or functions in backticks when they help the reader.
- Do not copy git commit messages verbatim — rewrite for a human reader.

### Step 5: Write the entry

Format (Keep a Changelog 1.1.0):

```markdown
# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Description of new capability.

## [X.Y.Z] - YYYY-MM-DD

### Fixed

- Description of what was broken and how it was fixed.

[Unreleased]: https://github.com/OWNER/REPO/compare/vX.Y.Z...HEAD
[X.Y.Z]: https://github.com/OWNER/REPO/compare/vA.B.C...vX.Y.Z
```

Structural rules:

- Reverse chronological order (newest first).
- `[Unreleased]` section always present at the top.
- Dates in ISO 8601 (`YYYY-MM-DD`).
- Comparison links at the bottom for every version.
- Empty categories are omitted (no `### Removed` if nothing was removed).

### Step 6: Determine the version bump

When cutting a release, choose the version number:

| Bump      | Trigger                                               |
| --------- | ----------------------------------------------------- |
| **Major** | Breaking API/CLI change, removed public functionality |
| **Minor** | New feature, backward-compatible behavior change      |
| **Patch** | Bug fix, security patch                               |

To cut a release:

1. Replace `## [Unreleased]` with `## [X.Y.Z] - YYYY-MM-DD`.
2. Add a fresh empty `## [Unreleased]` section above it.
3. Update the comparison links at the bottom.

### Step 7: Verify

- [ ] Every entry passes the filter from Step 3 (no noise).
- [ ] Newest version is at the top.
- [ ] Every version has a date (except Unreleased).
- [ ] Bottom links are correct and complete.
- [ ] No empty category headings.
- [ ] No git-log copy-paste — entries are human-readable.
- [ ] Entries identify the subsystem where helpful.

## Error Recovery

| Problem                    | Fix                                                        |
| -------------------------- | ---------------------------------------------------------- |
| Missing comparison links   | Reconstruct from `git tag --sort=-version:refname`         |
| Duplicate entries          | Deduplicate, keep the more descriptive version             |
| Entry under wrong category | Move it; if ambiguous, prefer Changed over Added           |
| No tags in repository      | Use commit SHAs in comparison links as a temporary measure |
| Noise entry slipped in     | Remove it — a leaner changelog is more trustworthy         |

## Anti-Patterns

| Anti-pattern | Why it's wrong | Correct approach |
| --- | --- | --- |
| One entry per commit | Commits are implementation steps, not logical changes. A 6-commit PR produces one changelog bullet. | Use `gh pr list` to enumerate PRs; write one bullet per logical change. |
| One entry per PR when multiple PRs deliver the same feature | Between releases, a feature PR plus its follow-up fix PRs are one observable change. Listing each separately implies the operator saw an intermediate broken state they never did. | Group all within-release PRs that touch the same capability into a single bullet describing the final result. |
| Separate "Fixed" entry for a sub-fix within a feature PR | Inflates the changelog and obscures that the fix was part of the feature delivery. | Fold the fix into the feature's Added bullet (e.g., "…with race-safe access" instead of a separate Fixed entry). |
| Using `git log --oneline` as the primary source | Produces commit-level noise: test commits, review feedback, merge commits, formatting fixes. | Query merged PRs via `gh pr list --state merged` filtered by milestone or date range since the last git tag. |
| Plain `#NNN` references | Not clickable in rendered markdown — readers must manually construct the URL to navigate to the change. | Use `[#NNN](https://github.com/OWNER/REPO/pull/NNN)` for PRs and `[#NNN](https://github.com/OWNER/REPO/issues/NNN)` for issues. |
| Omitting issue links | PR numbers alone lose the problem context; readers must search to understand what was fixed. | When a PR closes or references an issue, include both the issue link and the PR link. When multiple issues or PRs are explicitly linked, list all of them. |
