---
name: babysit-pr
description: >
  Resolve reviewer comments on a pull request or pasted feedback using a
  six-step evidence-grounded protocol. Use when asked to resolve review
  feedback, address reviewer comments, process PR comments, triage
  review feedback, apply review suggestions, handle code review
  feedback, decide which review comments to accept or reject, or
  babysit a PR through its review lifecycle. The protocol verifies
  every external-library claim with Context7, classifies each comment
  across seven categories, applies changes surgically or defers them to
  the GitHub Issues backlog, and emits a summary intended strictly for
  the human operator. The skill NEVER posts replies, reactions, or
  messages back to the reviewer. Do NOT use for authoring a new code
  review (use reviewImpl), for security scans, or for opening a new PR.
metadata:
  version: 2026-04-17
---

# Babysit PR — Reviewer Comment Resolution Protocol

Apply changes that genuinely improve the work. Respectfully decline those
that do not. Every accept, reject, defer, or skip is backed by documented
evidence: Context7 lookups, `docs/architecture.md`, or an explicit logical
argument grounded in the code. The goal is not to mark every comment
resolved; the goal is to ship correct, maintainable work.

This skill carries the protocol. It assumes the agent has already been
briefed on project-specific coding standards, Go conventions, and
Context7 mechanics at the prompt level. This skill supplies the protocol
and its rules; the prompt supplies the standards.

## When to use

| User intent                                                     | Apply? |
|-----------------------------------------------------------------|--------|
| "resolve the review" / "address this feedback"                  | Yes    |
| "process the comments on PR #42"                                | Yes    |
| "apply these review comments" (comments pasted inline)          | Yes    |
| "triage the reviewer feedback" / "decide what to fix"           | Yes    |
| "open a PR" / "create a review" / "run a security scan"         | No     |
| "refactor this file" / "write a new test"                       | No     |

## Prerequisites

Before executing any step, confirm:

1. The `gh` CLI is available and authenticated (required for Source B).
2. You know the Context7 workflow (two calls: `resolve-library-id`, then
   `query-docs`). This skill defines *when* to use it and *which*
   libraries require it; the prompt-level instructions define *how*.
3. The `managing-github-issues` Agent Skill is available for backlog
   triage in Step 4b. Do not hand-roll `gh issue create` — the skill
   carries the mandatory taxonomy lookup and body templates.

## Workflow

Copy this checklist into your response and mark items as you complete
them. Do not skip gates. Each gate exists to prevent a specific failure
mode documented in the protocol's rules.

- [ ] Step 1 — Ingest feedback and classify domain
- [ ] Step 2 — Context7 evidence audit (triage, execute, tabulate, bind)
- [ ] Step 3 — Classify every comment with a per-comment block
- [ ] Step 4 — Apply changes (code, backlog defer, or architecture)
- [ ] Step 5 — Verify no reviewer-facing output was emitted
- [ ] Step 6 — Produce the human-only summary

### Step 1 — Ingest feedback and classify domain

Examine the input the user provided.

**Source A — Inline input.** The user pasted or typed review comments.
Use them as-is. Do not fetch anything from GitHub.

**Source B — GitHub PR.** The user provided a PR number or URL, or the
input is empty and a PR exists on the current branch. Run the fetch
script to collect every kind of comment:

```bash
bash .github/skills/babysit-pr/scripts/fetch-pr-comments.sh [PR_NUMBER]
```

If the script is unavailable, run the three commands it wraps — missing
any of them silently drops a class of comments:

```bash
PR=$(gh pr view --json number --jq '.number')
gh api "repos/{owner}/{repo}/pulls/${PR}/comments" --paginate
gh api "repos/{owner}/{repo}/pulls/${PR}/reviews"  --paginate
gh pr view "$PR" --json comments --jq '.comments'
```

Classify the feedback domain from what the comments reference:

| Signal                                                    | Domain       | Role      |
|-----------------------------------------------------------|--------------|-----------|
| Source files, function names, test failures, Go style     | Code         | Coder     |
| Architecture spec, state machine, adapter contract, ADR   | Architecture | Architect |
| Both                                                      | Mixed        | Split the comments into two groups; resolve each in its own domain |

### Step 2 — Context7 evidence audit

**MANDATORY.** Complete every sub-step before assigning any
classification to any comment. There are no exceptions.

A reviewer asserting that a library behaves a certain way is making a
verifiable, falsifiable claim. Context7 is the verification mechanism.
Accepting or rejecting on unchecked library assumptions is the proximate
cause of both false approvals and false rejections. This step prevents
both failure modes.

#### 2a. Triage — which comments require Context7

For each collected comment, answer: *does this comment reference, either
explicitly or implicitly, the behavior, API surface, correct usage
pattern, or known limitations of an external library?*

If yes, mark the comment **[C7-REQUIRED]** in your internal analysis.

The tag is a working annotation for Steps 2b–2c and Step 3 reasoning
only. It MUST NEVER appear in GitHub comments, PR replies, issue
bodies, the Step 6 summary, or any other output visible outside your
own reasoning.

For the list of libraries that require Context7 validation in this
project — with the exact `resolve-library-id` argument for each — and
for the categories of comments that do NOT require Context7 (Go stdlib,
logic errors, architectural violations, style), read
[references/context7-triage.md](references/context7-triage.md).

The default posture is cautious: **when in doubt, run Context7.** A
false positive (running it when not strictly necessary) costs one tool
call. A false negative (skipping it when needed) costs a wrong
classification and a defensible-looking mistake.

#### 2b. Execute the Context7 workflow

For every **[C7-REQUIRED]** comment, run the two-step Context7 workflow
per the project's Context7 usage instructions. Follow those instructions
for query phrasing, topic filtering, token budgets, and failure
recovery.

When the library is not indexed and you fall back to an authoritative
source (the library's official docs, `pkg.go.dev` for Go packages, or
the library's GitHub README), record `[FALLBACK: web]` in the evidence
table. The finding is still treated as authoritative; only the
logistics differ.

#### 2c. Library Evidence Table

Build this table completely before proceeding to Step 3. Every
**[C7-REQUIRED]** comment gets exactly one row. The table is evidence,
not interpretation — classification comes in Step 3.

Use [assets/evidence-table-template.md](assets/evidence-table-template.md)
as the structural template. It contains a blank skeleton, filled
example rows calibrated for this project's Go stack, and column
discipline notes.

#### 2d. Binding rules

These rules govern every classification in Step 3. They are not
guidelines; they are gates. They exist to counteract the well-documented
tendency of language models to drift toward agreeing with whoever spoke
last — a drift that is the proximate cause of both sycophantic
acceptance of wrong suggestions and sycophantic rejection of correct
ones when the reviewer's tone becomes uncertain.

1. **Refuted library claim ⇒ not Valid.** A comment whose library claim
   Context7 refutes CANNOT be classified as Valid. It is Incorrect or
   Counterproductive, regardless of the reviewer's seniority, the
   certainty of their tone, or any perceived social pressure to agree.

2. **Confirmed library claim ⇒ not Subjective.** A comment whose
   library claim Context7 confirms has an objective basis. Classify it
   on correctness and scope grounds, never as Subjective.

3. **Confirmed but out of scope ⇒ Deferred, not Subjective.** The
   confirmation is real; only the timing is wrong. Route it to Step 4b.

4. **Ambiguous result ⇒ Needs Discussion.** If Context7 returns
   ambiguous, version-conflicting, or contradictory results, classify
   the comment as Needs Discussion. Document the exact ambiguity in the
   Step 6 summary — which claims conflict, and across which versions.

5. **Skipped [C7-REQUIRED] comment ⇒ may not classify.** If you did
   not run Context7 for a [C7-REQUIRED] comment, you may not classify
   it. Stop, return to Step 2b, and run it.

6. **Context7 vs `docs/architecture.md` ⇒ architecture wins.**
   Context7 describes what a library *can* do; the architecture
   document specifies what this project *will* do. When they conflict,
   the project's specification wins.

### Step 3 — Classify every comment

With the Library Evidence Table complete, classify every comment. Write
the per-comment classification block verbatim before assigning a
category. Forcing yourself through each field catches comments that
seem clear but turn out to depend on an unverified library claim or an
unstated architectural assumption.

Use [assets/classification-block-template.md](assets/classification-block-template.md)
as the structural template. It contains the blank block, a filled
example, and discipline notes.

The seven categories:

| Category                          | Action                        |
|-----------------------------------|-------------------------------|
| Valid & Actionable                | Apply the fix (Step 4a / 4c)  |
| Valid — Deferred to Backlog       | GitHub Issues triage (Step 4b)|
| Valid but Already Addressed       | Skip with explanation         |
| Subjective / Stylistic            | Skip with explanation         |
| Incorrect or Counterproductive    | Reject with rationale         |
| Outdated / Stale                  | Skip with explanation         |
| Needs Discussion                  | Flag for human decision       |

For precise criteria, worked examples, the borderline-case decision
rubric, and how Context7 verdicts map to each category, read
[references/classification-categories.md](references/classification-categories.md).

### Step 4 — Apply changes

#### 4a. Code-domain comments (Valid & Actionable)

1. Locate the exact file and line range.
2. **Before writing any fix that uses an external library API,** run
   Context7 for the *implementation* — not just for the classification.
   Verify the exact method signature, parameter types, and return
   shape against current documentation. The reviewer may be correct in
   direction but wrong in the specific API call they suggested.
3. Implement the change surgically. Modify only what is necessary.
4. Verify using Makefile targets only — never invoke `go` directly:
   `make fmt && make lint && make test` must pass. When scoped testing
   is faster, use `make test PKG=./internal/<pkg>` or `make test RUN=<TestName>`.
5. If the suggestion is directionally correct but the proposed
   implementation is suboptimal, implement a **better version** that
   addresses the underlying concern. Document the divergence in the
   Step 6 summary.

#### 4b. Deferred comments — GitHub Issues triage

Not every valid concern belongs in the current change. When you classify
a comment as **Valid — Deferred to Backlog**, decide whether it earns a
tracked GitHub issue.

**MANDATORY.** Load and apply the `managing-github-issues` Agent Skill
before creating any issue. That skill carries the mandatory taxonomy
lookup (`get_taxonomy.sh`), duplicate detection via `gh issue list
--search`, label/milestone/issue-type conventions, and the body
templates for each issue type. Do not hand-roll `gh issue create`.

Apply the three triage gates in order before invoking the create
operation:

1. **Architecture conflict gate.** Read the relevant section of
   `docs/architecture.md`. If the suggestion contradicts the spec's
   design intent — not merely its current implementation — it does not
   belong in the backlog. Explain why in the Step 6 summary and stop.

2. **Duplicate check.** Search open issues for existing work that
   covers this concern, even partially. If a matching issue exists,
   note the issue number (e.g., `#142`) in the summary and stop. Do
   not create duplicates.

3. **Scope test.** Would this realistically matter within the scope of
   open milestones? If it is aspirational work beyond the current
   roadmap's horizon, mention it in the summary as a future
   consideration and stop.

If the concern passes all three gates, invoke the
`managing-github-issues` skill to create the issue: it handles
taxonomy-aware label selection, the body template for the chosen issue
type, and the GraphQL issue-type assignment after creation.

#### 4c. Architecture-domain comments (Valid & Actionable)

1. Locate the exact section in `docs/architecture.md`.
2. Revise the specification to address the concern.
3. Verify internal consistency — the change must not contradict other
   sections, the state machine diagrams, the adapter contract, or
   accepted ADRs in `docs/decisions/`.
4. If the revision has downstream implications for existing code
   (e.g., a state transition was renamed, a validation rule tightened),
   enumerate them in the Step 6 summary so the human operator can
   schedule follow-up code work.

Never modify accepted ADRs in `docs/decisions/` without explicit
instruction from the user — per the project's Ask-first boundary.

### Step 5 — Verify no reviewer-facing output

**You are FORBIDDEN from posting any comment, reply, or message to the
reviewer under any circumstances.**

This prohibition is absolute and has no exceptions:

- Do NOT post reply comments to any review comment, inline or otherwise.
- Do NOT post issue-level comments on the PR conversation tab.
- Do NOT share your analysis, reasoning, plans, or rationale with the
  reviewer.
- Do NOT explain why you accepted or rejected a suggestion.
- Do NOT evaluate or react to the quality of the review.
- Do NOT use `gh api` or any GitHub CLI command that writes to the PR
  (comments, reviews, reactions, resolutions, marking-as-outdated).

Before producing the Step 6 summary, confirm you have not executed any
of the forbidden operations. All reasoning, all evidence, all decisions
belong in the summary for the human operator — not in the PR thread.

### Step 6 — Produce the summary

Write the summary using [assets/summary-template.md](assets/summary-template.md).
The template has one section per category plus the source header and
the Context7 evidence log.

If the validator script is available, run it on the summary file:

```bash
python3 .github/skills/babysit-pr/scripts/validate-report.py <summary-file>
```

Fix any reported violations and re-run until it reports PASS.

If the script is unavailable, verify manually using this checklist:

- [ ] Source header present (PR #N / Inline feedback / Mixed).
- [ ] Context7 Evidence Log table has one row per [C7-REQUIRED] comment.
- [ ] All seven category sections are present, including empty ones.
- [ ] No `[C7-REQUIRED]` tags appear anywhere in the summary.
- [ ] Every populated entry names the file:line changed.
- [ ] Every "Deferred" entry names a GitHub outcome (#N created /
      #N existing / not added with reason).
- [ ] Every "Rejected" entry cites specific Context7 or architecture
      evidence.
- [ ] Every "Needs Discussion" entry names the open question and both
      sides.

## Constraints

- **NEVER post any comment, reply, or message to the reviewer.** This
  is the single highest-priority rule. Skill output is for the human
  operator only.
- Do NOT fabricate review comments. Work only with comments from the
  identified source.
- Do NOT apply changes that break existing tests or introduce lint
  errors. `make test` and `make lint` must pass after changes.
- Do NOT invoke `go` directly — use Makefile targets (`make fmt`,
  `make lint`, `make test`, `make build`) for every verification step.
- Do NOT act on a suggestion about library behavior without first
  running Context7 for any [C7-REQUIRED] comment, regardless of how
  confident you feel.
- Do NOT classify a [C7-REQUIRED] comment before Step 2b completes for
  that comment.
- Do NOT skip Context7 because you feel confident about the API.
  Confidence is the proximate cause of hallucination. Certainty is
  earned from documentation, not recalled from training data.
- Do NOT reference `docs/architecture.md`, `docs/decisions/`, section
  numbers, ADR numbers, or ticket IDs in any source-code comment
  (godoc or inline) — those belong in specs and plans, not source.
- Do NOT use CGo or introduce libraries that require a C toolchain
  (e.g., `mattn/go-sqlite3`). The project ships a single statically
  linked binary; only `modernc.org/sqlite` is permitted.
- Preserve the project's existing code style, adapter boundaries, and
  architectural layer hierarchy.
- When rejecting, your rationale must be technical and specific — never
  dismissive. Cite Context7 findings when they support the rejection.

### Scope boundaries

The skill's responsibility ends at deciding what to do with each
comment and applying the minimum change that resolves it. Adjacent
actions belong to other skills or to the human operator.

- Do NOT commit or push. Leave the working tree modified; the
  `git-commit` and `create-pr` Agent Skills handle git state.
- Do NOT alter PR metadata — status, labels, assignees, reviewers,
  milestone, or branch protection settings.
- Do NOT close, merge, approve, or reopen the PR.
- Do NOT resolve, delete, or rename review threads. Thread state
  belongs to the PR author.
- Do NOT switch branches or modify files outside the paths named in a
  comment classified Valid & Actionable.

## Guiding principles

1. **Library claims are falsifiable.** A reviewer asserting an API
   behavior is making a verifiable claim. Context7 verifies it.
   Accepting or rejecting without verifying is the root cause of both
   false approvals and false rejections.
2. **Quality over harmony.** Never apply a change that makes the work
   worse, regardless of who suggested it or how confidently.
3. **Architecture wins over library capability.** When
   `docs/architecture.md` and Context7 conflict, architecture wins.
4. **Spec-first, code-second.** For architecture-domain feedback, the
   spec is the source of truth; code follows. Revising code without
   revising the spec is drift.
5. **Think like a maintainer, not a people-pleaser.** The goal is not
   to mark every comment resolved. The goal is to ship correct,
   maintainable work.
6. **Be thorough but surgical.** Apply the minimum change that fully
   addresses the concern. Every changed line must trace to a
   classified comment.
7. **Every decision needs evidence.** Document reasoning, source, and
   conclusion for every apply, skip, or reject. Assertions without
   citations are opinions.
8. **Defer wisely, not reflexively.** "Not now" is valid only when you
   can articulate where and when. A deferred concern without a roadmap
   home is a concern ignored.
