---
name: resolveReview
description: Resolve review feedback — from GitHub PRs, chat, or any reviewer
argument-hint: Paste review comments, or leave empty to fetch from GitHub PR
---

Analyze review feedback and resolve each comment with principled judgment. Your objective is exceptional quality — apply changes that genuinely improve the work, and respectfully decline those that do not.

## Step 1: Determine Feedback Source and Scope

Examine the input provided by the user.

**Source A — Inline input.** The user pasted or typed review comments directly. Use these comments as-is. Do not fetch from GitHub.

**Source B — GitHub PR.** The user provided a PR number, URL, or the input is empty and a PR exists on the current branch. Fetch all comments:

```bash
PR_NUMBER=$(gh pr view --json number --jq '.number')
gh api "repos/{owner}/{repo}/pulls/${PR_NUMBER}/comments" --paginate
gh api "repos/{owner}/{repo}/pulls/${PR_NUMBER}/reviews" --paginate
gh pr view "$PR_NUMBER" --json comments --jq '.comments'
```

**Scope detection.** Classify the feedback domain by examining what the comments reference:

| Signal | Domain | Handling Agent |
|---|---|---|
| Comments reference source files, function names, test failures, code style | **Code** | Apply changes directly (you are acting as the Coder) |
| Comments reference architecture, design decisions, specifications, data models, system diagrams | **Architecture** | Revise specification documents (you are acting as the Architect) |
| Comments reference both code and architecture | **Mixed** | Separate into two groups; resolve each in its own domain |

## Step 2: Classify Each Comment

For **every** comment, determine its category:

| Category | Description | Action |
|---|---|---|
| **Valid & Actionable** | Real bug, security flaw, performance issue, design gap, readability improvement, or idiomatic best practice that aligns with the project philosophy. | **Apply the fix.** |
| **Valid — Deferred to Roadmap** | Genuine improvement, but out of scope for this change: a design trade-off acceptable at current scale, an optimization that matters only at higher load, a feature that depends on unfinished milestones, or a refactor better done as a dedicated task. | **Triage into TODO.md** (see Step 3b). |
| **Valid but Already Addressed** | Concern was correct at the time but has since been resolved. | **Skip with explanation.** |
| **Subjective / Stylistic** | Neither better nor worse — merely different. Does not align with existing conventions. | **Skip with explanation.** |
| **Incorrect or Counterproductive** | Would introduce a bug, degrade quality, violate architecture, break conventions, or reduce clarity. | **Reject with rationale.** |
| **Outdated / Stale** | References content that no longer exists in the current state. | **Skip with explanation.** |
| **Needs Discussion** | You disagree but cannot definitively disprove the concern. | **Flag for human decision.** |

Before accepting or rejecting: quote the reviewer comment and the artifact it references. Reason through: (a) what is the reviewer asking, (b) is it technically correct, (c) does it align with conventions, (d) would it improve or degrade quality.

## Step 3: Apply Changes

### For code-domain comments (Valid & Actionable):
1. Locate the exact file and line range.
2. Implement the change surgically — modify only what is necessary.
3. Verify: `make test` must pass.
4. If the suggestion is directionally correct but the proposed implementation is suboptimal, implement a **better version** that addresses the underlying concern.

### For deferred comments — Roadmap Triage (Step 3b)

Not every valid concern belongs in the current change. When you classify a comment as **Valid — Deferred to Roadmap**, decide whether it earns a place in [TODO.md](../../TODO.md). Apply these filters in order:

1. **Architecture conflict gate.** Read the relevant section of [architecture.md](../../docs/architecture.md). If the suggestion contradicts the spec's design intent — not just its current implementation — it does not belong in the roadmap. Explain why in the summary and stop here.
2. **Redundancy check.** Scan TODO.md for an existing task that already covers this concern, even partially. If one exists, note the task number in the summary and stop here — do not create duplicates.
3. **Roadmap horizon test.** Would this realistically matter before the last defined milestone ships? If it is aspirational work beyond the current roadmap's scope, mention it in the summary as a future consideration but do not modify TODO.md.

If the concern passes all three filters, add it to TODO.md:

- **Placement.** Find the milestone whose theme most closely relates to the concern. Insert the task after the last existing task in that milestone, using the next sequential number (e.g., if the last task is `6.13`, the new one is `6.14`). If no milestone fits, append a new milestone after the last one — number it sequentially and give it a concise theme title.
- **Task format.** Follow the existing convention: `- [ ] N.N <description>\n      **Verify:** <how to confirm completion>`. The description must be self-contained — a future agent reading only the task line must understand what to do.
- **Do not renumber** existing tasks. Append only.

### For architecture-domain comments (Valid & Actionable):
1. Locate the exact section in the specification document.
2. Revise the specification to address the concern.
3. Verify internal consistency — ensure the change does not contradict other sections.
4. If the revision has downstream implications for existing code, enumerate them.

## Step 4: Produce Summary

```markdown
## Review Resolution Summary

### Source
{GitHub PR #N / Inline feedback / Mixed}

### Applied (N comments)
- [Comment by @reviewer, location] — What was changed and why.

### Deferred to Roadmap (N comments)
- [Comment by @reviewer, location] — Why deferred, and where it landed: `TODO.md task N.N` / existing task N.N / not added (reason).

### Skipped — Already Addressed (N comments)
- [Comment by @reviewer, location] — Why it no longer applies.

### Skipped — Subjective (N comments)
- [Comment by @reviewer, location] — The stylistic trade-off.

### Rejected (N comments)
- [Comment by @reviewer, location] — Technical rationale.

### Needs Discussion (N comments)
- [Comment by @reviewer, location] — The open question and both sides of the argument.

### Stale / Outdated (N comments)
- [Comment by @reviewer, location] — Why it no longer applies.
```

## Guiding Principles

1. **Quality is paramount.** Never apply a change that makes the work worse, regardless of who suggested it.
2. **Respect the project's philosophy.** Changes must be consistent with established conventions. Reject suggestions that contradict architectural patterns defined in [architecture.md](../../docs/architecture.md).
3. **Think like a maintainer, not a people-pleaser.** The goal is not to mark every comment as "resolved." The goal is to ship correct, maintainable work.
4. **Be thorough but surgical.** Apply the minimum change that fully addresses the concern.
5. **Explain every decision.** Document your reasoning for every apply, skip, or reject.
6. **Err on the side of correctness.** When in doubt, investigate deeper before deciding.
7. **Defer wisely, not reflexively.** "Not now" is a valid answer only when you can articulate where and when. A deferred concern with no roadmap home is a concern ignored.

## Constraints

- Do NOT fabricate review comments. Work only with comments from the identified source.
- Do NOT apply changes that break existing tests or introduce type errors.
- Do NOT blindly follow suggestions that reduce readability, performance, or safety.
- Preserve the project's existing code style and architectural patterns.
- When rejecting, your rationale must be technical and specific — never dismissive.

Apply standards from: [Code review standards](../instructions/code-review.instructions.md), [Go documentation guidelines](../instructions/go-documentation.instructions.md), and [Go environment guidelines](../instructions/go-environment.instructions.md)

${input:request:Paste review comments here, or leave empty to fetch from current GitHub PR}
