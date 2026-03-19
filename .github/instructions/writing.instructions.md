---
name: 'Technical Writing'
description: 'Prose standards for architecture documents, ADRs, TODO tasks and user guides'
applyTo: '**/docs/*.md'
---

# Technical Writing Standards

These rules govern all prose in this project: architecture documents, ADRs, TODO tasks and user guides. They do not govern Go source files (see [Go documentation guidelines](go-documentation.instructions.md) for godoc conventions).

## Core Principles

**State facts. Do not speculate.** Write declarative, present-tense sentences. "The orchestrator serializes all state mutations" — not "The orchestrator might serialize state mutations." Reserve hedging for genuine uncertainty, and name the uncertainty explicitly when you use it.

**Every sentence must earn its place.** If a sentence can be removed without losing information, remove it. Introductions that restate the heading, transitions that connect nothing, and conclusions that summarize the obvious are all candidates for deletion.

**One term per concept.** Choose a name for each domain concept and use it everywhere. Do not alternate between synonyms for variety. If the spec calls it a "workspace key," never call it a "directory name" or "folder identifier" in the same document.

**Active voice by default.** "The adapter normalizes labels" — not "Labels are normalized by the adapter." Passive voice is acceptable when the actor is irrelevant or unknown: "Errors are logged to stderr."

**Write for the reader who will implement this.** Assume the audience is a Go engineer familiar with concurrency, SQL, and system design. Do not explain goroutines, channels, or `context.Context` from first principles. Do explain project-specific invariants, non-obvious constraints, and decisions that deviate from common practice.

## Requirements Language

Use RFC 2119 keywords in specifications and architecture documents. Capitalize them when they express a binding requirement.

| Keyword | Meaning |
|---|---|
| MUST / SHALL | Absolute requirement. Violation is a bug. |
| MUST NOT / SHALL NOT | Absolute prohibition. |
| SHOULD | Recommended. Deviation requires documented justification. |
| MAY | Truly optional. |

Do not use these keywords in casual prose, PR descriptions, or comments. Reserve them for normative statements in specs and architecture docs.

Every requirement MUST be testable. If you cannot describe how to verify a requirement, rewrite it until you can.

## Document Structure

### Headings

Use sentence case. Capitalize the first word and proper nouns only.

```
Good: "Workspace path containment"
Bad:  "Workspace Path Containment"
Bad:  "WORKSPACE PATH CONTAINMENT"
```

Headings describe content, not category. "Retry backoff algorithm" — not "Details" or "Additional information."

Do not skip heading levels. H1 appears once per document. Sections use H2, subsections H3. Rarely go deeper than H4.

### When to use each format

**Prose paragraphs** — for rationale, context, and narrative explanation of how components interact. Use when relationships between ideas matter more than individual items.

**Bulleted lists** — for unordered collections of related items, constraints, or properties. Use when each item is independent and the reader needs to scan.

**Numbered lists** — for sequential steps where order matters. Use for procedures, algorithms, and workflows.

**Tables** — for structured reference data with consistent attributes across rows. Use when the reader needs to compare or look up values. Do not use tables for prose that happens to have two columns.

**Code blocks** — for anything the reader might execute, copy, or treat as literal syntax. Pseudocode, SQL schemas, CLI commands, Go snippets.

**Mermaid diagrams** — for system context, sequence flows, and state machines where visual structure communicates relationships that prose cannot.

### Cross-references

Reference architecture doc sections by number: "per Section 7.3." Reference ADRs by filename: "per ADR-0002." Reference code by package path: "`internal/persistence`." Do not use vague pointers like "as discussed above" or "see below."

## Specification Writing

Specifications are contracts between architect and implementer. Ambiguity in a spec causes wasted engineering effort.

### Eliminate unmeasurable terms

These words have no testable meaning. Replace them with specific, quantified language or delete them.

| Vague term | Replacement |
|---|---|
| fast, performant | "responds within N ms at P99" or "processes N items/sec" |
| robust, resilient | name the specific failure mode handled |
| flexible, configurable | list the exact configuration options |
| easy, simple, intuitive | remove; describe the interface |
| scalable | "handles N concurrent sessions" |
| secure | name the threat model and the mitigation |
| appropriate, adequate, sufficient | state the threshold |

### Requirement structure

Each requirement stands alone as a complete, verifiable statement.

```
Good: "The workspace key MUST contain only characters matching [A-Za-z0-9._-].
       Characters outside this set MUST be replaced with underscore."

Bad:  "The workspace key should be properly sanitized."
```

### Algorithm descriptions

Use pseudocode blocks matching the style in architecture doc Section 16. Line-oriented, indented for nesting, no English narrative filler between steps.

```
Good:
  function reconcile(state, tracker_snapshot):
    for each issue in state.running:
      if issue not in tracker_snapshot:
        cancel_worker(issue)
        release_claim(issue)

Bad:
  "First, the system iterates through all running issues.
   Then, for each issue, it checks whether the issue still
   exists in the tracker snapshot. If not, it cancels the
   worker and releases the claim."
```

## ADR Writing

Architecture Decision Records are reference documents, not essays. Target 200-500 words.

**Structure:** Context, Decision Drivers, Considered Options, Decision Outcome, Consequences.

**Decision Outcome opens with the choice:** "Chosen option: **X**, because [one-sentence rationale]."

**Trade-off honesty is mandatory.** Acknowledge what the chosen option makes harder. "Go's ecosystem has weaker LLM generation tooling than TypeScript" is honest. "Go is the best choice in every dimension" is not credible.

**Compare concretely.** "Go's goroutines map to OS threads via the runtime scheduler without application-level coordination; Node.js serializes all orchestration on a single event loop thread" — not "Go is better at concurrency."

## Task Writing

Tasks in TODO.md follow a fixed format:

```
- [ ] N.N Brief imperative description.
      **Verify:** measurable completion criterion.
```

**Imperative action verb first:** "Implement," "Add," "Configure," "Research." Not "We need to implement" or "Implementation of."

**Atomic sizing:** one task, one agent session. If a task requires multiple sessions, split it.

**Verification clause is mandatory.** It names a command, a condition, or an observable outcome. "`make test` passes" — not "everything works."

## Inline Comments and Commit Messages

**Comments explain why, not what.** Code shows what. Comments explain non-obvious invariants, safety constraints, workarounds, and intentional deviations.

```go
// Good: Explain a non-obvious constraint
// SetMaxOpenConns(1) enforces single-writer semantics for SQLite WAL mode.
db.SetMaxOpenConns(1)

// Bad: Restate the code
// Set max open connections to 1
db.SetMaxOpenConns(1)
```

**Commit messages** follow [Conventional Commits](commit-messages.instructions.md). Subject line under 72 characters, imperative mood, body explains why.

## PR Descriptions

**Structure:** What problem this solves (1-2 sentences), how it solves it (key changes), what was tested (commands, results), what could break (risk assessment).

Do not narrate the git log. The reviewer can read the diff. Describe the intent and the risk.

## Banned Language

### Words to replace

| Banned | Replacement |
|---|---|
| leverage | use |
| utilize | use |
| facilitate | help, enable |
| implement (as noun) | do, build, write |
| commence | start |
| sufficient | enough |
| numerous | many (or state the count) |
| robust | name the property: durable, crash-safe, retry-tolerant |
| seamless | automatic, transparent |
| performant | fast, or state the metric |
| individual | person |
| remainder | rest |
| attempt (noun) | try |
| assistance | help |

### Phrases to delete

Delete entirely. These add no information.

- "It should be noted that" / "It is important to note that" / "Note that"
- "In order to" (write "To")
- "Due to the fact that" (write "Because")
- "At this point in time" (write "Now")
- "As mentioned above" / "As discussed earlier"
- "For all intents and purposes"
- "At the end of the day"
- "In today's [anything]" / "In the ever-evolving landscape of"
- "Let's dive into" / "Let's explore" / "Let's take a look at"
- "I think" / "I believe" / "We believe"

### LLM-specific patterns to avoid

These patterns signal machine-generated text. Avoid them in all project documents.

**Em-dash overuse.** Em-dashes (—) are an LLM signature. Replace with commas, parentheses, periods, or semicolons. If you must use a dash, prefer a spaced en-dash (" – ").

**Transition word stacking.** Do not use "Furthermore," "Additionally," "Moreover," or "In addition" as paragraph openers. Use "And," "But," "So," or restructure to eliminate the transition.

**Sycophantic openers.** Never begin with "Great question!", "That's a great point!", "Absolutely!", or "I'd be happy to help."

**Formulaic closers.** Never end with "Hope this helps!", "Let me know if you have questions!", "In conclusion," or a paragraph that restates the introduction.

**Hedge stacking.** One hedge per sentence maximum. "This might cause issues" is acceptable. "This could potentially possibly cause issues" is not.

**Symmetric structures.** Do not write lists where every item has identical sentence structure, identical length, and follows a "Firstly... Secondly... Thirdly..." pattern. Vary rhythm.

**Over-bolding.** Bold highlights key terms on first use or critical warnings. Do not bold every other phrase.

## Punctuation

- Use the Oxford comma: "adapters, persistence, and logging."
- Place commas outside closing quotation marks when the comma is not part of the quoted text: words like "foo", "bar", and "baz".
- Use straight quotes and apostrophes, not curly/smart quotes.
- Exclamation marks are reserved for genuine warnings. Do not use for enthusiasm.
- Sentences can start with "But" and "And".

## Language

All project prose is in American English. Use American spelling in all identifiers, comments, docs, and messages: "initialize", "normalize", "behavior", "color".
