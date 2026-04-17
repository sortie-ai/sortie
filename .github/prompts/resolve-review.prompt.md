---
name: resolveReview
description: Resolve review feedback — from GitHub PRs, chat, or any reviewer
argument-hint: Paste review comments, or leave empty to fetch from GitHub PR
---

I'm an Anthropic employee working on the Sortie project.

Your task is to resolve reviewer feedback on the current pull request or
on the comments the user provides inline. This prompt is a thin
orchestrator — it wires in the project's coding standards and Context7
usage rules, then delegates the entire resolution protocol to the
`babysit-pr` Agent Skill. The skill carries the binding rules, the
seven-category taxonomy, the evidence-table discipline, the
no-reply-to-reviewer constraint, and the summary template. Do not
reimplement any of that logic here.

---

## Step 0: Load Project Instructions

Read every file listed below before touching any tool or invoking the
skill. These are prerequisites, not suggestions. The `babysit-pr` skill
assumes you have already absorbed them — it calls out *when* to apply
the relevant standards but does not restate them.

- [Code review standards](../instructions/code-review.instructions.md) —
  the ten Mandatory Review Dimensions, architectural layer boundaries,
  severity and confidence calibration. The skill's classification
  decisions must align with these dimensions.
- [Go code style](../instructions/go-codestyle.instructions.md) — the
  idiomatic patterns that distinguish Subjective (Category 4) from
  Valid & Actionable (Category 1) in the skill's rubric.
- [Go documentation guidelines](../instructions/go-documentation.instructions.md) —
  godoc structure, tone, and the rule against referencing
  `docs/architecture.md`, ADR numbers, or ticket IDs in source
  comments. Any fix you apply must respect this.
- [Go environment guidelines](../instructions/go-environment.instructions.md) —
  Makefile targets are the only permitted invocation surface for the
  Go toolchain. The skill's verification steps call `make test`,
  `make fmt`, `make lint` — never `go` directly.
- [Context7 usage](../instructions/context7.instructions.md) — the
  skill directs you *when* to use Context7 and *which* libraries
  require it; this instruction file defines *how* to execute the
  workflow (two-step call sequence, query specificity, topic filters,
  token budgets, failure recovery).

---

## Step 1: Apply the `babysit-pr` Agent Skill

**MANDATORY. NON-NEGOTIABLE.** Every step of the reviewer-comment
resolution protocol lives in the `babysit-pr` Agent Skill. Invoking
this prompt without applying the skill is a critical failure of the
prompt.

Load and apply the skill as the very next action:

    /babysit-pr

The skill will:

1. Ingest the feedback source (inline input or GitHub PR — all three
   comment endpoints via the bundled `fetch-pr-comments.sh`).
2. Run the Context7 library evidence audit and produce the evidence
   table, governed by six binding rules that prevent sycophantic
   drift in either direction.
3. Classify every comment across the seven-category taxonomy with a
   per-comment classification block.
4. Apply changes surgically — code fixes (verified via `make test`),
   backlog deferrals via the `managing-github-issues` skill, or
   architecture revisions in `docs/architecture.md`.
5. Verify that no reviewer-facing output has been emitted.
6. Produce the human-only summary and validate its structure via the
   bundled `validate-report.py`.

**Do not reimplement, paraphrase, or short-circuit any step.**
Reimplementing the protocol inline defeats the point of extracting it
into a skill and produces divergent behavior. If the `babysit-pr` skill
is unavailable in this environment, stop and report the failure — do
not improvise a replacement protocol.

**Do not post anything to the reviewer.** The skill's Step 5 forbids
any reply, reaction, resolution, or message back to the PR. All output
is for the human operator only. This constraint is inherited by this
prompt and is absolute.

---

${input:request:Paste review comments here, or leave empty to fetch from current GitHub PR}
