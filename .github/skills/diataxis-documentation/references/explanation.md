# Writing Explanation Documentation

Explanation is the most undervalued and most intellectually demanding type of documentation. It is a discursive treatment of a subject that permits reflection. The reader is not at the keyboard solving a problem -- they are stepping back to *understand*. They might be on a commute, in a hammock, or between tasks. They are building the mental model that makes everything else -- tutorials, how-to guides, reference -- click into place.

Think of Harold McGee's *On Food and Cooking*. It contains no recipes. It is not instructional. It is not reference material. It places its subject in the context of history, society, science, and technology. It explains *why we do what we do in the kitchen*. You do not read it while cooking. You read it between cooking sessions. And it fundamentally changes how you think about your craft.

The word *explanation* comes from Latin *explanare* -- to unfold. You are unfolding what is hidden in the folds.

## Core Principles

### 1. Make Connections

Explanation thrives on connections -- between concepts within the system, between the system and the wider world, between the current design and its alternatives. Isolated facts are reference. Connected understanding is explanation.

- **Good:** "Sortie's workspace isolation is analogous to Docker's filesystem namespaces, but operates at the orchestrator level rather than the OS level. This means you get path containment without the overhead of container creation -- a deliberate trade-off that favors agent startup speed over the stronger isolation guarantees containers provide."
- **Bad:** "Workspaces are isolated from each other."

The first connects to something the reader likely knows (Docker), names the trade-off explicitly, and explains the design rationale. The second states a fact that belongs in reference.

### 2. Provide Context That No Other Type Can

Why does this feature exist? What problem did it solve? What alternatives were considered? What constraints shaped the design? This is the context that tutorials cannot provide (too much theory), how-to guides ignore (irrelevant to the task), and reference cannot express (too opinionated).

- **Good:** "The decision to use SQLite rather than PostgreSQL was driven by the single-binary deployment model. An orchestration tool that requires a running database server creates a dependency chain that contradicts the project's zero-dependency philosophy. SQLite embeds into the binary. The trade-off is write concurrency -- a constraint that the architecture addresses through careful transaction scoping."
- **Bad:** "Sortie uses SQLite for persistence."

### 3. Offer Opinions and Perspectives

Explanation is the one Diataxis type where authorial voice and judgment are not just permitted but *necessary*. All human understanding contains perspective. Explanation that refuses to take a position is not neutral -- it is hollow.

- **Good:** "Event sourcing is a better fit for this domain than CRUD, because the orchestrator's core question is always 'what happened?' rather than 'what is the current state?' The audit trail is not a nice-to-have -- it is the primary data structure."
- **Bad:** "Some teams use event sourcing while others prefer CRUD. Both approaches have their advantages." (This says nothing. The reader is no wiser.)

Take positions. Support them with reasoning. Acknowledge alternatives respectfully. This is how understanding is built.

### 4. Use Analogies

Analogies are the most powerful tool in explanatory writing. They bridge the gap between what the reader already understands and what they are trying to understand. An analogy that lands makes a paragraph of explanation unnecessary.

- **Good:** "The adapter pattern in Sortie works like a universal power adapter. The orchestrator core is the appliance -- it has a fixed plug shape (the Go interface). Each integration (Jira, Claude Code) is a different country with a different socket. The adapter translates between them. You never rewire the appliance; you swap the adapter."
- **Good:** "A state machine is to a workflow what a grammar is to a language. The grammar does not tell you what sentences to write. It tells you which sentences are valid."

Analogies work best when they share structural similarity with the concept, not just surface similarity. A good analogy illuminates the *relationships* between parts, not just the parts themselves.

### 5. Take the Higher Perspective

How-to guides work at eye level: "do this, then this." Reference works in close-up: "this parameter accepts these values." Explanation works from above: zoomed out, seeing the whole landscape, understanding how pieces relate.

This means explanation can -- and should -- discuss:
- **History:** Why does this design exist? What preceded it?
- **Alternatives:** What else could have been built? Why wasn't it?
- **Trade-offs:** What was gained and lost by this choice?
- **Future direction:** Where is this heading, and why?
- **Connections to the wider world:** How does this relate to industry patterns, research, or other systems the reader may know?

## Structure Template

```markdown
# [Topic title -- implicitly "About [topic]"]

[Opening paragraph that frames the subject and explains why it matters.
Not a summary -- a motivation. Why should the reader care?]

## [Aspect or angle 1]

[Discursive treatment. Analogies, reasoning, context. Multiple
paragraphs are normal and expected here.]

## [Aspect or angle 2]

...

## [Design decisions / trade-offs / alternatives]

[Why things are the way they are. What was considered and rejected.
What constraints shaped the design.]

## [Connections to wider context]

[How this relates to broader patterns, industry standards, or other
parts of the system.]

## Further reading

- [Link to relevant tutorial for hands-on practice]
- [Link to reference for precise specifications]
- [Link to external resources that deepen understanding]
```

Note: Explanation articles are less rigidly structured than the other three types. The section headings above are suggestions. The actual structure should follow the natural shape of the subject. Some explanations are best organized chronologically (the history of a decision). Others work best as a series of contrasts (comparing approaches). Others as a progressive deepening (from simple model to full model). Let the subject dictate the structure.

## Voice and Tone

- **First or third person, depending on what feels natural.** "We chose SQLite because..." or "The system uses SQLite because..." Both work. Pick one per article and stay consistent.
- **Discursive and reflective.** This is the most "written" type of documentation. Vary sentence length. Build paragraphs. Let ideas develop across multiple sentences.
- **Opinionated but fair.** State your position clearly, acknowledge opposing views genuinely. "Some teams prefer X for legitimate reasons, including [reason]. In this context, Y is the stronger choice because [reason]."
- **Unhurried but not meandering.** The reader is here to understand, not to be entertained. Do not pad with filler. Every paragraph should advance understanding.

### Characteristic Phrases

- "The reason for X is..."
- "This is analogous to..."
- "The trade-off is..."
- "An alternative approach would be... but..."
- "Historically, this originated from..."
- "The key insight is..."
- "To understand why, consider..."
- "This matters because..."

## Anti-Patterns

| Anti-Pattern | Why It Fails |
|---|---|
| Refusing to take a position | "There are many approaches" without evaluating any of them is not explanation. It is evasion. The reader came for understanding, which requires judgment. |
| Embedding procedures | "To configure this, follow these steps..." is a how-to guide that has wandered into explanation. Link to the guide instead. |
| Listing without connecting | A bullet list of facts is reference, not explanation. Explanation connects facts into understanding. |
| Explaining everything from scratch | Not every explanation starts at zero. Know your reader. An explanation of retry strategies for a senior engineer does not need to define what a network request is. |
| Opening with definitions | "X is defined as..." is reference-style writing. Explanation opens with motivation: "When a system needs to handle intermittent failures gracefully..." |
| Excessive hedging | "It could be argued that perhaps in some cases..." -- commit to a position or cut the sentence. |
| Closing with generic platitudes | "In conclusion, X is a powerful tool with many applications." This adds no understanding. End with the strongest insight or a forward-looking implication. |

## The Open-Endedness Challenge

Explanation is the hardest type to scope. Tutorials have a clear start and end (the exercise). How-to guides have a clear goal (the task). Reference has a clear boundary (the machinery). Explanation could go on forever.

**Two techniques for scoping:**

1. **The "why" question.** Frame every explanation as answering a specific "why" question: "Why does Sortie use adapter interfaces instead of direct integrations?" This gives the article a clear center of gravity.

2. **Draw a border.** Explicitly state what the article covers and what it does not: "This article explains the state machine design. For the persistence layer that stores state transitions, see [link]."

## Boundary Discipline

**Drifting into Tutorial?** If you catch yourself writing "now let's try..." or "create a file called..." -- you have shifted from understanding to doing. Explanation discusses; it does not direct.

**Drifting into How-to Guide?** If you are writing sequential instructions ("First, configure X. Then, add Y."), those belong in a how-to guide. Explanation may *mention* that X needs configuring, but does not walk through the steps.

**Drifting into Reference?** If you are describing a function's parameters or a config key's allowed values, that is reference material. Explanation may discuss *why* a function exists and what design problem it solves, but the parameter list belongs elsewhere.

## Why Explanation Matters

It is tempting to treat explanation as optional -- the dessert after the "real" documentation. This is wrong. Without explanation, practitioners' knowledge remains "loose and fragmented and fragile." They know *what* to do (from how-to guides) and *what exists* (from reference), but not *why* things are the way they are. Their practice becomes "anxious" -- they follow steps without understanding the principles behind them, unable to adapt when reality diverges from the documented path.

Explanation is not a luxury. It is the connective tissue that makes all other documentation meaningful.
