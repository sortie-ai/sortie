# Quality and Style Guide

This guide applies to all four Diataxis types. Load it alongside the type-specific reference for every documentation task.

## Two Dimensions of Quality

### Functional Quality (Measurable)

These are independent, objective, and non-negotiable. Failure in any one dimension is a defect.

| Dimension | Test |
|---|---|
| **Accuracy** | Every fact is verifiably correct. Code examples run. Commands produce stated output. |
| **Completeness** | Nothing is missing that a reader in this type's context would need. (For reference: exhaustive. For how-to: practical. For tutorial: sufficient to complete the exercise. For explanation: sufficient to understand the subject.) |
| **Consistency** | Terminology, formatting, structure patterns, and voice are uniform throughout. |
| **Currentness** | Reflects the actual state of the product. No references to removed features or deprecated behavior without explicit labels. |
| **Precision** | No ambiguous statements. "The timeout is 30 seconds" not "the timeout is relatively short." |

### Deep Quality (Judged, Not Measured)

These qualities are interdependent and subjective. They cannot be checked with a linter. They require taste.

- **Flow.** The reader moves through the document without friction. Each section follows naturally from the previous one. There is a sense of momentum.
- **Fit.** The document feels shaped for its reader. A tutorial feels supportive. A reference feels authoritative. A how-to guide feels efficient. An explanation feels illuminating.
- **Anticipation.** The document answers questions before the reader asks them. "You might wonder why we don't just..." appears right when the reader is wondering exactly that.
- **Economy.** Every sentence earns its place. Nothing is present for padding, decoration, or the author's self-expression.

Deep quality depends on functional quality. A beautifully written document with incorrect facts is worse than a dry document with correct facts. Get the facts right first. Then make it beautiful.

## Writing Principles

### Write With Narrative Purpose

Technical documentation is not a data dump. Even a reference page has a structural narrative: the reader arrives, locates what they need, confirms it, and leaves. A tutorial has an explicit narrative: a journey from "I don't know how" to "I just did it."

For every document, know the reader's story:
- **Where are they coming from?** (What triggered them to open this doc?)
- **What do they need right now?** (The answer determines everything you write.)
- **Where are they going next?** (Link them forward.)

### Start Concrete, Then Generalize

This is the "concreteness fading" principle from learning science research. The optimal sequence is:

1. **Concrete:** A specific, tangible, runnable example that solves a real problem
2. **Structural:** A simplified view that strips away non-essential details, showing the pattern
3. **Abstract:** The general principle that applies across use cases

Readers who see only abstractions cannot apply them. Readers who see only concrete examples cannot generalize. The progression from specific to general is how understanding actually forms.

- **Good:** Show a working code example first. Then explain what each part does. Then show the general pattern.
- **Bad:** Start with the type signature, then the abstract pattern, then maybe an example at the bottom.

### Reduce Cognitive Load

Three types of cognitive load (from John Sweller's research):

- **Intrinsic load:** The inherent complexity of the subject. You can sequence it but not eliminate it.
- **Extraneous load:** Mental effort wasted on bad presentation. **Eliminate this ruthlessly.**
- **Germane load:** The productive effort of building mental models. **Maximize this.**

Concrete techniques:

| Principle | Technique |
|---|---|
| One concept per section | Never introduce two new ideas in the same paragraph |
| Consistent structure | Repeat the same section pattern (Overview, Instructions, Verification) so readers navigate by position, not by reading |
| No forward references | Never say "as we'll see later." If the reader needs it now, provide it now. |
| Re-state across distance | If a code example references a concept from 5 sections ago, remind the reader in a brief phrase. Do not make them scroll back. |
| Use whitespace | Short paragraphs. Bulleted lists. Generous spacing. Dense walls of text are extraneous load. |

### Defeat the Curse of Knowledge

Once you understand something, you literally cannot imagine what it was like not to understand it. This is the most destructive force in documentation.

**Symptoms:**
- Unexplained acronyms ("Configure the IAM role")
- Missing "why" ("Just add the middleware" -- why? what does it do?)
- Skipped prerequisites ("Set up the webhook" -- the reader has never created a webhook)
- Minimizing language ("Simply run the migration" -- there is nothing simple about it for someone who has never done it)

**Countermeasures:**
- Search every document for "just," "simply," "obviously," "of course," "easy," and "straightforward." Every occurrence likely hides a skipped explanation. Replace or remove each one.
- Write for your past self six months ago.
- Ask: "What would I have needed to know before this sentence made sense?"

### Use Progressive Disclosure

Layer complexity rather than presenting it all at once. Two levels maximum (research shows usability drops sharply beyond two):

1. **Essential path:** The default, most common approach. Presented directly.
2. **Details:** Advanced options, edge cases, alternative approaches. Presented in clearly labeled sections the reader can skip.

Do not hide essential information. Do not force advanced readers through beginner content. Let each reader find their level.

## Voice and Tone

Sound like a knowledgeable friend who understands what the reader wants to do. Be conversational without being colloquial. Be confident without being arrogant. Let personality show, but remember the primary purpose is delivering information to someone who may be in a hurry.

**The tone spectrum (calibrate to this):**

| Too informal | Right | Too formal |
|---|---|---|
| "Dude! This API is totally awesome!" | "This API lets you collect data about user preferences." | "The API may enable acquisition of information pertaining to user preferences." |
| "Then, BOOM, garbage-collect and you're golden." | "To clean up, call the `collectGarbage` method." | "Task completion requires executing an automated memory management function." |

### Core Voice Rules

- **Use contractions.** "Don't", "isn't", "you'll", "can't" sound warmer than their expanded forms. Technical docs aren't legal briefs.
- **Use "you" for the reader.** "You configure the endpoint" not "The user configures the endpoint." Exception: tutorials use "we" (tutor and learner together).
- **State things directly.** "This method returns an error" not "I think this method might return an error." No "I believe", "it seems", "sort of", "kind of".
- **Use positive phrasing.** Say what something *is*, not what it *isn't*. "The cache persists for 24 hours" not "The cache doesn't expire immediately."
- **Skip politeness markers in procedures.** "Click View" not "Please click View." "See the reference" not "Please see the reference."

### Banned Words

When you encounter these words, replace or remove them. The replacement column shows what to use instead.

**Minimizing language (remove or replace):**

| Banned | Replacement | Why |
|---|---|---|
| "simply", "just", "easily" | remove | Skips explanations and makes struggling readers feel inadequate |
| "merely", "straightforward" | remove | Same as above |
| "trivially", "obviously", "of course", "naturally" | remove | If it were obvious, it wouldn't need documentation |
| "quick", "quickly" (in procedures) | remove or give actual time | "Takes about 2 minutes" is honest; "quickly" is a lie for someone stuck |

**Vague and inflated language:**

| Banned | Replacement |
|---|---|
| "leverage" | "use" |
| "utilize" | "use" |
| "facilitate" | "help" or "enable" |
| "commence" | "start" |
| "implement" (as verb for user actions) | "set up", "add", "create" |
| "robust" (as decoration) | "strong" or remove; be specific about what makes it strong |
| "seamless" | "automatic" or describe the actual behavior |
| "performant" | "fast" or give benchmarks |
| "numerous" | "many" or give the actual count |
| "sufficient" | "enough" |
| "remainder" | "rest" |
| "individual" (as noun) | "person" |
| "initial" | "first" |
| "assistance" | "help" |
| "attempt" | "try" |
| "referred to as" | "called" |
| "pretty", "quite", "rather", "really", "very" | remove; they weaken the sentence they try to strengthen |
| "a bit", "a little" | remove or be specific |
| "thing" | name the actual thing |

**AI-generated filler (remove completely):**

| Category | Words/Phrases |
|---|---|
| Verbs | "delve", "harness", "underscore", "illuminate", "bolster", "streamline", "navigate" (metaphorical), "resonate", "showcase", "embark" |
| Nouns | "tapestry", "realm", "beacon", "landscape", "journey" (metaphorical), "paradigm shift", "game-changer" |
| Adjectives | "multifaceted", "cutting-edge", "meticulous", "revolutionary", "innovative", "disruptive", "mission-critical", "battle-tested", "comprehensive" (as decoration) |
| Openers | "In today's ever-evolving...", "It's important to note that...", "Let's dive into...", "In this article, we will..." |
| Closers | "In summary", "In conclusion", "In essence", "Hope this helps!" |
| Filler responses | "Certainly!", "Great question!", "Absolutely!", "You're right!" |

**Hedging (use only when uncertainty is genuine):**

| Banned | Fix |
|---|---|
| "It could be argued that perhaps..." | Commit to the argument or cut the sentence |
| "may potentially" | Pick one: "may" or "potentially" |
| "It might be the case that..." | State the fact or state the uncertainty plainly |
| "There are various ways to..." | Name them or recommend one |

**Banned phrases:**

- "best practices" -> "proven approaches"
- "blazing fast" / "lightning fast" -> give actual numbers
- "By developers, for developers" -> remove
- "We can't wait to see what you'll build" -> remove
- "The future of..." -> remove
- "We're excited" -> remove or "We look forward"
- "it's not just [x], it's [y]" -> restructure

### Required Practices

| Practice | Rationale |
|---|---|
| Active voice by default | "The server closes the connection" not "The connection is closed by the server." Shorter, clearer, assigns responsibility. |
| Present tense | "Returns the value" not "Will return the value." The machinery exists now. |
| Specific over vague | "Takes 30 seconds" not "Takes a moment." "Handles up to 10,000 connections" not "Handles many connections." |
| Show, don't describe | A code example communicates more than a paragraph of description. Lead with the example when both are needed. |
| Opinions need reasons | Every recommendation requires a "because." "Use X because Y" not "Use X." |
| Negative examples alongside positive | Show what NOT to do and what goes wrong. This teaches judgment, not only procedure. |
| Back claims with evidence | "Reduces latency by 40%" not "Significantly reduces latency." |
| Use Oxford commas | "Tutorials, how-to guides, and reference" not "Tutorials, how-to guides and reference." |

### Sentence and Paragraph Rhythm

Vary sentence length deliberately. Three long sentences in a row create fatigue. Three short sentences in a row feel robotic. Mix them.

A paragraph develops one idea. New idea, new paragraph. In code-heavy docs, single-sentence paragraphs before code blocks are normal.

Do not let all paragraphs be the same length. Monotonous block rhythm is a telltale sign of mechanically generated text. Some ideas need one sentence. Some need five. Let the content dictate the length.

Do not start consecutive sentences with the same word or phrase. "You can configure... You can also set... You can then verify..." reads like a template. Restructure for variety.

### Avoiding AI-Generated Patterns

These patterns are dead giveaways that text was machine-generated. Readers lose trust when they detect them, even subconsciously.

**Em-dash overuse.** Em-dashes (---) are an AI signature. Models insert them constantly. Replace with commas, parentheses, periods, or semicolons.

- Bad: "The system --- which handles authentication --- is fast"
- Good: "The system, which handles authentication, is fast"
- Bad: "This is important --- it affects performance"
- Good: "This is important. It affects performance."

**Symmetrical structures.** Perfectly parallel paragraphs, lists that go "Firstly... Secondly... Thirdly...", or three-adjective constructions ("robust, scalable, and reliable") signal mechanical generation. Vary your structures.

**Transition word stacking.** "Furthermore", "Additionally", "Moreover" used as paragraph openers in sequence. Real writers vary transitions or drop them entirely when the connection is obvious.

**Uniform paragraph length.** If every paragraph is 3-4 sentences, the rhythm is mechanical. Break the pattern deliberately.

**The read-aloud test.** Read the text aloud, or at least mouth the words. If a sentence sounds awkward when spoken, it probably reads awkwardly too. Not every sentence needs to sound conversational, but none should sound like they were assembled from parts.

## Structural Rules

### Titles

A title is a promise. It tells the reader exactly what they get if they keep reading.

- Make the promise concrete: "How to configure rate limiting for public APIs" not "Rate limiting guide."
- Use sentence case, not title case: "How to configure rate limiting" not "How To Configure Rate Limiting."
- For tutorials: describe what will be built. "Build a real-time dashboard with WebSockets."
- For how-to guides: start with "How to" and name the outcome.
- For reference: mirror the product structure. "CLI reference: deploy command."
- For explanation: frame around the "why." "Why Sortie uses adapter interfaces."

### Headings

- Descriptive, not clever. "Configuring rate limits" not "Keeping the flood gates closed."
- Sentence case, not title case. "Configuring rate limits" not "Configuring Rate Limits."
- Consistently grammatical. If one heading is a gerund phrase ("Configuring X"), all sibling headings are gerund phrases.
- Scannable. A reader who only reads the headings should understand the document's structure and find what they need.

### Code Examples

- Every code example must be runnable as written (or explicitly marked as pseudocode).
- Show the output where relevant. Do not ask the reader to imagine what happens.
- Use realistic values, not `foo`, `bar`, `test123`. Realistic examples teach by osmosis: the reader absorbs naming conventions and typical values while learning the actual concept.
- Highlight the relevant parts when examples are long. Use comments like `// <-- this line is new` or bold the changed section in prose.

### Cross-References

- Link between types explicitly. A tutorial should link to relevant reference and explanation docs. A how-to guide should link to reference for full option lists.
- Use the Diataxis type in the link text: "See the [rate limiting reference](../reference/rate-limiting.md) for all configuration options." This helps the reader predict what they'll find.
- Never say "see above" or "as mentioned earlier." Link to the specific section.

### Punctuation and Formatting

- **Oxford commas.** Always: "tutorials, how-to guides, and reference."
- **Exclamation points.** Use sparingly. Reserve for genuinely exciting moments. Most technical content has zero.
- **Commas with quoted terms.** When listing quoted words, place commas outside the closing quotes (logical style): words like "foo", "bar", and "baz" -- not "foo," "bar," and "baz."
- **Straight quotes only.** Use straight single quotes (') and double quotes ("), not curly/smart quotes.
- **Periods over commas.** When a comma creates a run-on, use a period. Shorter sentences are easier to parse.
- **"But" and "And" can start sentences.** But don't overuse them.

## Validation Checklist

Run this checklist before considering any document complete:

### All Types
- [ ] The compass questions unambiguously identify one type for this document
- [ ] No banned words or phrases appear (check the full banned words table)
- [ ] No minimizing language ("simply," "just," "easily," etc.)
- [ ] Active voice used throughout (passive only for deliberate emphasis)
- [ ] Every code example runs or is marked as pseudocode
- [ ] Cross-references use type-aware link text
- [ ] Paragraph lengths vary (no wall-of-same-size blocks)
- [ ] No consecutive sentences start with the same word
- [ ] Headings use sentence case and are consistently grammatical
- [ ] Title makes a concrete promise about what the reader gets
- [ ] No em-dash overuse (commas, parentheses, or periods instead)
- [ ] No symmetrical list structures ("Firstly... Secondly... Thirdly...")
- [ ] No "please" in procedural instructions
- [ ] Oxford commas used consistently
- [ ] Straight quotes only (no curly/smart quotes)
- [ ] Passes the read-aloud test (no awkward or robotic sentences)
- [ ] The document serves one Diataxis type, no type mixing

### Tutorial-Specific
- [ ] Opens with what will be built, not what will be learned
- [ ] Every step produces a visible result
- [ ] Expected output shown after every command
- [ ] No choices or alternatives offered
- [ ] No explanation longer than one sentence (links out instead)
- [ ] Uses "we" voice consistently

### How-to Guide-Specific
- [ ] Title starts with "How to" and names a specific outcome
- [ ] Assumes competence -- no basic concept explanations
- [ ] Ends with a verification step
- [ ] Includes troubleshooting for common failure modes
- [ ] Uses imperative mood consistently

### Reference-Specific
- [ ] Structure mirrors the product's structure
- [ ] Every entry follows the same template (no missing sections)
- [ ] Complete -- every public element is documented
- [ ] No opinions, recommendations, or "because" clauses
- [ ] Examples illustrate without explaining

### Explanation-Specific
- [ ] Opens with motivation, not definition
- [ ] Contains at least one analogy or concrete connection to something familiar
- [ ] Takes clear positions with supporting reasoning
- [ ] Acknowledges alternatives fairly
- [ ] Contains no procedural steps (links to how-to guides instead)
- [ ] Scoped by a "why" question or explicit boundary statement
