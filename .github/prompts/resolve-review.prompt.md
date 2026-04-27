---
name: resolveReview
description: Resolve review feedback — from GitHub PRs, chat, or any reviewer
argument-hint: Paste review comments, or leave empty to fetch from GitHub PR
---

Your task is to resolve reviewer feedback on the current pull request or on the comments the user provides inline. This prompt is a thin orchestrator — it wires in the project's coding standards and Context7 usage rules, then delegates the entire resolution protocol to the `babysit-pr` Agent Skill. The skill carries the binding rules, the seven-category taxonomy, the evidence-table discipline, the no-reply-to-reviewer constraint, and the summary template. Do not reimplement any of that logic here.

---

## Step 1: Load Project Instructions

Read every file listed below before touching any tool or invoking the skill. These are prerequisites, not suggestions. The `babysit-pr` skill assumes you have already absorbed them — it calls out *when* to apply the relevant standards but does not restate them.

- [Code review standards](../instructions/code-review.instructions.md) — the ten Mandatory Review Dimensions, architectural layer boundaries, severity and confidence calibration. The skill's classification decisions must align with these dimensions.
- [Go code style](../instructions/go-codestyle.instructions.md) — the idiomatic patterns that distinguish Subjective (Category 4) from Valid & Actionable (Category 1) in the skill's rubric.
- [Go documentation guidelines](../instructions/go-documentation.instructions.md) — `docs/architecture.md`, ADR numbers, or ticket IDs in source  comments. Any fix you apply must respect this.
- Context7 usage — the skill directs you *when* to use Context7 and *which* libraries require it; this instruction file defines *how* to execute the workflow (two-step call sequence, query specificity, topic filters, token budgets, failure recovery).

---

## Step 2: Apply the `babysit-pr` Agent Skill

**MANDATORY. NON-NEGOTIABLE.** Every step of the reviewer-comment resolution protocol lives in the `babysit-pr` Agent Skill. Invoking this prompt without applying the skill is a critical failure of the prompt.

Load and apply the skill as the very next action.

**Do not reimplement, paraphrase, or short-circuit any step.** Reimplementing the protocol inline defeats the point of extracting it into a skill and produces divergent behavior. If the `babysit-pr` skill is unavailable in this environment, stop and report the failure — do not improvise a replacement protocol.

**Do not post anything to the reviewer.** The skill's Step 5 forbids any reply, reaction, resolution, or message back to the PR. All output is for the human operator only. This constraint is inherited by this prompt and is absolute.

---

${input:request:Paste review comments here, or leave empty to fetch from current GitHub PR}
