# Writing Tutorials

A tutorial is an experience that takes place under the guidance of a tutor. The learner arrives anxious, unfamiliar, and needing confidence. They leave having *done* something real and wanting to come back. Nearly all responsibility falls on the teacher.

## The Fundamental Rule

**Do not try to teach. Allow learning to take place.**

Give learners things to *do* through which they learn. Trust the process. A tutorial succeeds when the reader acquires skills AND discovers pleasure in the activity. If they don't want to come back, the tutorial failed -- even if they "learned the material."

Think of teaching a child to cook. It doesn't matter what the child makes or how correctly they do it. What matters is that they work alongside you, at their own pace, through shared activity -- and that they want to return to the kitchen.

## Principles

### 1. Show Where They're Going

Open with the destination, not the journey: "In this tutorial, we will build a working chat server that handles multiple connections." Never write "In this tutorial you will learn about..." -- that is presumptuous. You cannot promise what someone will learn. You can promise what they will build.

### 2. Deliver Visible Results Early and Often

Every step must produce something the reader can see, run, or verify. The gap between "do something" and "see something happen" must be short. Rapid, repeated, meaningful results build confidence and momentum.

**Good:** After each step, the reader runs the code and sees output.
**Bad:** Ten steps of configuration before anything runs.

### 3. Maintain a Narrative of the Expected

At every point, the reader should know exactly what to expect. Manage their anxiety explicitly:

- "You should see output similar to:" (show the exact output)
- "After a moment, the server responds with:" (prepare them for a delay)
- "If you see an error like `connection refused`, check that the server from step 2 is still running."

This is not hand-holding. It is the tutor's duty. A learner who gets unexpected results loses confidence in the tutorial, the tutor, and themselves.

### 4. Point Out What to Notice

Observation is an active skill. Close learning loops by drawing attention to outcomes: "Notice that the second request is faster -- the cache from step 3 is working." Don't explain *why* the cache works (that belongs in Explanation). Just point it out.

### 5. Target the Feeling of Doing

Accomplished practitioners experience a flow state: purpose, action, thinking, and result are joined up. The tutorial must cradle this feeling. Each step should feel like purposeful action, not busywork.

### 6. Permit and Encourage Repetition

Repetition is sometimes the only teacher. Design reversible steps so the reader can redo them. Some readers will voluntarily repeat a step just for the satisfaction of seeing it work again. This is a feature, not a waste.

### 7. Ruthlessly Minimize Explanation

A tutorial is not the place for theory. When context is needed, provide exactly one sentence and link out:

- **Good:** "We use HTTPS here because it encrypts traffic. (See [Why HTTPS matters](../explanation/https.md) for details.)"
- **Bad:** Three paragraphs about TLS handshakes, certificate authorities, and cipher suites before the reader has even made their first request.

The reader did not come here to understand. They came here to do.

### 8. Stay Concrete

All learning moves from the concrete and particular toward the general and abstract. Start with specific values, specific files, specific commands. Never open with abstractions:

- **Good:** "Create a file called `server.go` with the following content:"
- **Bad:** "Servers can be configured in many ways depending on your architecture. The most common patterns are..."

### 9. Ignore Options and Alternatives

One path. No diversions. No "you could also use X instead." No flag variations. The reader is learning, not making architectural decisions. Choices overwhelm beginners and break the narrative flow.

If the reader later needs alternatives, they will find them in a How-to Guide.

### 10. Aspire to Perfect Reliability

A tutorial must work for every reader, every time. This is the hardest requirement and the most important. A learner who follows the steps and gets a different result than promised will blame themselves, not the documentation. Test the tutorial on a clean environment. Test it again after every product change.

## Structure Template

```markdown
# [Action-oriented title: "Build a...", "Create a...", "Deploy a..."]

In this tutorial, we will [concrete deliverable]. By the end,
you will have [tangible result the reader can see/run/use].

## Prerequisites

- [Specific tool] version [specific version] installed
- [Specific access] configured
- Completed [previous tutorial, if part of a series]

## Step 1: [Verb phrase describing action]

[One sentence of context if needed.]

[Exact command or action.]

[Expected result, shown literally.]

## Step 2: [Verb phrase]

...

## Step N: [Verb phrase]

...

## What You Built

[2-3 sentences summarizing what was accomplished and what the reader
now has. Link to relevant How-to Guides for next steps and Explanation
docs for deeper understanding.]
```

## Voice and Tone

- **First-person plural:** "We" is the default pronoun. "We are in this together."
- **Warm but purposeful.** Not clinical, not chatty. A calm, encouraging guide.
- **Present tense, active voice.** "Create the file" not "The file should be created."
- **Short sentences during action steps.** "Run the command. Wait for the output. It should show three lines."
- **Reassuring during transitions.** "Now that the server is running, we can connect a client to it."

### Characteristic Phrases

- "In this tutorial, we will..."
- "First, do x. Now, do y. Now that you have done y, do z."
- "You should see something like:"
- "Notice that..."
- "Don't worry about [concept] for now -- we'll explore that in [link]."
- "Let's check that everything is working:"

## Anti-Patterns

| Anti-Pattern | Why It Fails |
|---|---|
| Starting with theory | The reader came to do, not to think. Abstractions before action create cognitive overload. |
| Offering choices | "You can use either X or Y" forces a decision the learner isn't equipped to make. Pick one. |
| Assuming knowledge | Saying "configure the webhook" when the reader has never seen a webhook. |
| Long gaps between visible results | More than 3-4 steps without output erodes confidence. |
| Explaining instead of linking | Theory paragraphs break the narrative. One sentence + link. |
| Skipping expected output | Every command should show what the reader will see. Omitting this is abandoning the learner. |
| "Exercise for the reader" | The reader is here because they cannot yet exercise independently. Never punt. |

## Boundary Discipline

**Drifting into How-to Guide territory?** If you find yourself saying "if you want X, do Y" -- that is conditional, goal-directed advice for a competent user. In a tutorial, there is one path and no conditions.

**Drifting into Explanation?** If you are writing more than two sentences about *why* something works, stop. Link to an Explanation doc.

**Drifting into Reference?** If you are listing all available options for a parameter, stop. The tutorial uses one specific value. The full list belongs in Reference.
