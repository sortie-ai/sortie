---
status: accepted
date: 2026-10-17
decision-makers: Serghei Iakovlev
---

# Supply a Built-In Prompt for Every Reaction Run

## Context and Problem Statement

The pull request reactions `reactions.ci_failure`, `reactions.review_comments`, `reactions.bot_review`, `reactions.merge_conflicts`, and the review and fix commands of `reactions.label_commands` start agent runs whose instructions depend on reaction data. The data reaches the prompt template under a continuation variable: `ci_failure`, `review_comments`, `bot_review_comments`, `merge_conflict`, `label_review`, or `label_fix`. Only the operator's template supplied the instructions. A template without a branch for the variable sent the normal work prompt, which a fix command ran against a checkout that can push. The variable reached only the first turn, so later turns rendered the work prompt even when the branch existed, and the shipped examples appended their reaction blocks to the work prompt. The only guard, a load-time text search of the `WORKFLOW.md` body for the review and fix variables, warned on correct workflows that route through rule templates and stayed silent on broken ones.

## Decision Drivers

1. **Safety.** No reaction run receives the normal work prompt because Sortie lost track of the reaction, on any turn.
2. **No false warnings.** `sortie validate` stays silent on a correctly configured workflow.
3. **Completeness by construction.** A reaction cannot be added without its prompt.
4. **Operator control.** A workflow that writes its own branch for a reaction keeps it.
5. **Decide on the template that runs.** A reaction can render any template the workflow loads.

## Considered Options

- A built-in prompt per continuation variable, chosen at render time
- A load-time branch check over every template a reaction can render
- A built-in reaction block appended to the operator's render
- Parse-tree analysis for a branch on the variable

## Decision Outcome

Chosen option: **A built-in prompt per continuation variable, chosen at render time**, because it is the only option that meets every driver.

Sortie ships one prompt per continuation variable in the registry that declares the variable, so no variable exists without its prompt. On every turn of a reaction run Sortie renders the template with the variable and without it. A non-blank render that differs from the render without the variable is the operator's branch and is sent, a reaction block inside the work prompt included; otherwise Sortie sends its built-in prompt, rendered with the same data. The variable stays set on every turn, and `.run.is_continuation` marks the later turns. A built-in first turn asks for the completion signal Sortie describes on every first turn; a built-in later turn sends the agent back to the first turn's instructions and adds none. The built-in prompts name no agent runtime and no forge. The shipped examples keep their reaction branches and the work prompt in one exclusive chain. The `reactions.label_commands.review_branch_missing` and `reactions.label_commands.fix_branch_missing` advisories are removed.

### Considered Options in Detail

**Load-time branch check.** It removes the false warning but keeps a warning the operator must act on, relies on a text search that a word in prose satisfies, and leaves the later-turn defect in place.

**Appended block.** Sortie appending its reaction instructions to the operator's render leaves the work instructions in front of the agent, the failure this decision removes.

**Parse-tree analysis.** Finding `if` nodes on the variable misses branches reached through partials or composed conditions and adds a second model of template semantics. Comparing two renders observes what the template produces.

## Consequences

### Positive

- A template that does not react to the variable gives the reaction run Sortie's prompt for that reaction on every turn, never the work prompt.
- `sortie validate` has no reaction-branch warning to get wrong.

### Negative

- The built-in prompts are product text Sortie maintains; an operator changes one only by writing a branch.
- A reaction block inside the work prompt sends the work prompt with it on every turn; a workflow copied from the earlier examples keeps that until it is restructured into an exclusive chain.
- A branch whose render does not change with the variable, or renders only whitespace, is ignored.
- Continuation text outside the variable's branch gives way to Sortie's continuation text on a reaction run's later turns.
- A reaction run renders its template with and without the variable on every turn.
