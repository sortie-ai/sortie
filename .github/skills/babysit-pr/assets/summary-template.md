# Step 6 Summary — Template

This is the template for the final summary the skill produces for the
human operator. All reasoning, all evidence, all decisions belong in
this document. Nothing goes to the reviewer.

## Template

Copy this block verbatim. Fill every section. Write `_(none)_` as the
body of any section with zero comments so the human operator can see
the category was considered.

```markdown
## Review Resolution Summary

### Source
{GitHub PR #N / Inline feedback / Mixed}

### Context7 Evidence Log

| # | Comment | Library | Finding | Verdict |
|---|---|---|---|---|
| … | … | … | … | REVIEWER CORRECT / REVIEWER INCORRECT / AMBIGUOUS / FALLBACK: web / N/A |

### Applied (N comments)

- **[@reviewer, file:line]** — What was changed and why. C7 validation: {row # or N/A}.

### Deferred to Backlog (N comments)

- **[@reviewer, file:line]** — Why deferred. GitHub outcome: #N (created) / #N (existing) / not added ({reason}).

### Skipped — Already Addressed (N comments)

- **[@reviewer, file:line]** — Why it no longer applies (commit, PR, or branch that resolved it).

### Skipped — Subjective (N comments)

- **[@reviewer, file:line]** — The stylistic trade-off. C7 validation: N/A — no library API claim.

### Rejected (N comments)

- **[@reviewer, file:line]** — Technical rationale. C7 evidence: {finding, library, version} — or architectural citation ({constraint, e.g., "single-writer invariant", "layer boundary", "CGo ban"}).

### Needs Discussion (N comments)

- **[@reviewer, file:line]** — The open question and both sides. C7 status: {ambiguous / not indexed / version conflict / spec-undetermined}.

### Stale / Outdated (N comments)

- **[@reviewer, file:line]** — What changed and where the referenced entity went.
```

## Section rules

- **All seven category sections MUST appear** in every summary, even
  those with zero comments. An empty section uses `_(none)_` as its
  body so the human operator can see the category was evaluated.
- **File:line citations are mandatory** for every entry except
  comments explicitly classified as "general feedback" in Step 3.
- **Internal tags like `[C7-REQUIRED]` MUST NOT appear.** They are
  working annotations for Steps 2 and 3 only. A tag leaked into the
  summary is a protocol violation.
- **No reviewer-facing language.** Write as a maintainer reporting to
  themselves. Avoid phrasing like "we should tell the reviewer that…"
  — the reviewer never reads this.
- **GitHub outcomes must be specific.** Use the exact form
  `#42 (created)`, `#17 (existing)`, or
  `not added (architecture conflict)` / `not added (out of roadmap
  horizon)`. Never just "issue created."
- **Evidence log is complete.** Every `[C7-REQUIRED]` comment from
  Step 2a must have a row. Use N/A in the Verdict column only for
  comments that turned out not to make a library claim after all.
- **Rejected entries carry specific evidence.** "Context7 disagreed"
  is insufficient. State the finding, the library, and the version
  the finding applies to — or name the architectural invariant the
  suggestion would violate.
