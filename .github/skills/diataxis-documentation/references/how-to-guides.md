# Writing How-to Guides

A how-to guide is a set of directions that guide a competent reader through a real-world problem to a specific result. The reader already knows what they want to achieve. They are at work, not studying. They need a reliable path, not a lesson.

Think of a recipe. It assumes you can chop, saut&eacute;, and measure. It tells you what to combine and in what order. It does not teach you knife skills or explain the Maillard reaction. A recipe that stops to explain basic technique is a bad recipe.

## Core Principles

### 1. Solve a Real Problem, Not a Machinery Question

How-to guides address human needs and projects, not button-by-button tool descriptions.

- **Good:** "How to configure automatic reconnection with exponential backoff"
- **Bad:** "To deploy your database, select the correct options from the dropdown and click Deploy"

The first addresses a real architectural need. The second describes UI mechanics that belong in a reference or are self-evident to a competent user.

### 2. Name It Precisely

The title is a contract. It tells the reader exactly what problem this guide solves. Ambiguous names waste everyone's time.

- **Good:** "How to integrate application performance monitoring with Prometheus"
- **Bad:** "Application performance monitoring" (is this a concept? a reference page? a tutorial?)
- **Bad:** "Monitoring guide" (what kind of monitoring? what outcome?)

### 3. Assume Competence

The reader knows the domain. They know what a database is, what an API key does, what deployment means. They came here because this *specific* task in *this specific system* is new to them. Do not teach basics.

- **Good:** "Add the middleware to your pipeline:" (shows the code)
- **Bad:** "A middleware is a function that runs between the request and response cycle. It can modify requests, responses, or terminate the cycle early. In our system, middleware is configured in the pipeline. Let's add our middleware:" (three sentences of teaching before the action)

### 4. Focus Exclusively on Action

Every sentence should either instruct the reader to do something or provide information directly required for the next action. No digressions, no tangents, no "interesting background."

If the reader would benefit from understanding *why*, link to an Explanation doc. Do not embed the explanation.

### 5. Allow Branching

Real problems are not always linear. A how-to guide can fork:

```markdown
## Configure authentication

### If using API keys

Generate a key in the dashboard and add it to your config:
...

### If using OAuth

Register your application and configure the redirect URI:
...
```

This respects the reader's actual situation rather than forcing them through irrelevant steps.

### 6. Practical Completeness Over Exhaustive Completeness

Cover the most common path thoroughly. Mention the most important edge cases. Do not catalog every possible option -- that is what Reference is for.

- **Good:** "For most deployments, use the default connection pool size. If you serve more than 10,000 concurrent requests, see the [connection pool reference](../reference/connection-pool.md) for tuning options."
- **Bad:** A table of 47 configuration flags with descriptions of each (this is Reference, not a guide).

## Structure Template

```markdown
# How to [achieve specific outcome]

[1-2 sentences: what this guide helps you do and when you'd need it.
Optional: prerequisites as a short list.]

## [First major action]

[Imperative instructions. Code blocks. Expected results where helpful.]

## [Second major action]

...

## [Verification / confirmation step]

[How the reader confirms success. A command to run, output to check,
or behavior to observe.]

## Troubleshooting

[2-3 most common failure modes with solutions. Keep brief -- link to
deeper diagnostics if needed.]
```

## Voice and Tone

- **Second person, imperative mood.** "Configure the endpoint" not "We will now configure the endpoint."
- **Direct and efficient.** Respect the reader's time. They are at work.
- **Confident.** "Add the following to your config:" not "You might want to consider adding..."
- **Conditional where reality demands it.** "If your cluster runs version 3.x, use flag `--legacy`. For version 4.x, this flag is not needed."

### Characteristic Phrases

- "This guide shows you how to..."
- "Add the following to..."
- "Run the command:"
- "If you need X, do Y."
- "Verify that the change took effect:"
- "For more options, see [reference link]."

## Anti-Patterns

| Anti-Pattern | Why It Fails |
|---|---|
| Teaching in a guide | The reader knows the domain. Explaining fundamentals wastes their time and signals the doc doesn't respect their expertise. |
| Describing UI mechanics | "Click the blue button labeled Deploy" is not a how-to guide. If the UI is self-explanatory, the guide is unnecessary. If it is not, the problem is the UI. |
| Exhaustive option coverage | Listing every flag and parameter turns a guide into a reference page. Cover the common path; link to reference for the rest. |
| No verification step | The reader has no way to confirm success. They leave uncertain whether they completed the task correctly. |
| Ambiguous titles | A reader scanning a docs sidebar cannot distinguish "Configuration" from "Settings Guide" from "Setup." Be specific: "How to configure rate limiting for the public API." |
| Linear-only structure | Real problems branch. Forcing every reader through every step, including irrelevant ones, creates friction and errors. |

## Boundary Discipline

**Drifting into Tutorial territory?** If you find yourself walking through basics step-by-step with "now let's..." language, you are teaching. Tutorials guide a learner through an exercise. How-to guides give a practitioner directions to solve their problem.

The clearest test: **does the reader choose to be here, or were they sent here to learn?** A tutorial reader is assigned (by a course, a getting-started flow). A how-to reader arrives voluntarily with a specific goal.

**Drifting into Reference?** If you are listing parameters, methods, or return types without connecting them to the task at hand, you are writing reference material. Link to it instead.

**Drifting into Explanation?** If you are writing "the reason this works is..." -- stop. That is valuable content, but it belongs in an Explanation doc, linked from here.
