---
name: diataxis-documentation
description: "Create, edit, and validate technical documentation using the Diataxis framework. Use when writing tutorials, how-to guides, reference docs, or explanations. Use when reviewing or auditing existing documentation for structural correctness. Use when deciding what type of document to write. Also use when the user mentions Diataxis, documentation quality, documentation types, or asks to write 'deep dive' articles, onboarding guides, API docs, or architectural explanations. Do NOT use for code comments, commit messages, changelogs, or README generation."
---

# Diataxis Documentation Framework

Diataxis (from Greek *dia* "across" + *taxis* "arrangement") organizes documentation around **what the reader needs**, not what the author knows. It was created by Daniele Procida and adopted by Django, Canonical, Cloudflare, Gatsby, and hundreds of other projects.

## The Two Axes

Documentation sits on two independent dimensions:

```
                      PRACTICAL
                       (action)
                          |
            Tutorials     |     How-to Guides
           learning by    |    achieving a specific
              doing       |        result
                          |
  ACQUISITION --------- NEED ---------- APPLICATION
   (study)                |               (work)
                          |
           Explanation    |     Reference
          understanding   |    looking up exact
             why          |      information
                          |
                     THEORETICAL
                     (cognition)
```

**Axis 1 -- Action vs. Cognition.** Does this content serve what the reader *does*, or what the reader *understands*?

**Axis 2 -- Acquisition vs. Application.** Is the reader *learning* something new, or *applying* what they already know?

These axes are independent. Conflating them is the root cause of most documentation failures.

## The Compass -- Identify the Right Type

When writing or reviewing any piece of documentation, ask two questions:

| Question | Answer A | Answer B |
|----------|----------|----------|
| Is this about **doing** or **understanding**? | Doing (practical) | Understanding (theoretical) |
| Is the reader **learning** or **working**? | Learning (acquisition) | Working (application) |

| Doing + Learning | **Tutorial** |
|---|---|
| **Doing + Working** | **How-to Guide** |
| **Understanding + Working** | **Reference** |
| **Understanding + Learning** | **Explanation** |

Use this compass at every scale: a single paragraph, a section, an entire document. If writing feels difficult or forced, you are probably in the wrong quadrant. Stop. Re-orient with the compass.

## The Four Types at a Glance

| | Tutorial | How-to Guide | Reference | Explanation |
|---|---|---|---|---|
| **Purpose** | Enable learning through doing | Solve a specific real-world problem | Describe the machinery accurately | Build conceptual understanding |
| **Reader's state** | Beginner, anxious, needs confidence | Competent, busy, needs a solution | Working, needs a precise fact | Curious, reflective, needs the "why" |
| **Reader asks** | "Teach me to..." | "How do I...?" | "What exactly is...?" | "Why does...?" |
| **Writing voice** | Guiding, warm, first-person plural | Direct, imperative, task-focused | Austere, neutral, factual | Discursive, opinionated, reflective |
| **Analogy** | Teaching a child to cook | A recipe | A nutritional label | Harold McGee's *On Food and Cooking* |

## Workflow

### Creating New Documentation

1. **Orient with the compass.** Ask the two questions. Determine the type before writing a single word.
2. **Load the type-specific guide.** Read the appropriate reference file:
   - Tutorial: `references/tutorials.md`
   - How-to Guide: `references/how-to-guides.md`
   - Reference: `references/reference.md`
   - Explanation: `references/explanation.md`
3. **Read the style guide.** For voice, tone, and anti-pattern guidance, read `references/quality-and-style.md`. This is mandatory for all types.
4. **Write the document** following the loaded guidance.
5. **Validate.** Run the boundary check below before considering the document complete.

### Editing Existing Documentation

1. Read the existing document in full.
2. Classify it using the compass. If it mixes types, note which sections belong to which type.
3. Load the relevant reference files for each identified type.
4. Restructure or rewrite to achieve type purity. If a document genuinely serves two types, split it into two documents with cross-links rather than blending them.

### Reviewing / Auditing Documentation

1. Read the document.
2. Classify with the compass.
3. Check for boundary violations (see below).
4. Load `references/quality-and-style.md` for the quality checklist.
5. Report findings with specific line-level citations.

## Boundary Check -- The Five Violations

These are the most common failures. Check every document against them:

| Violation | Symptom | Fix |
|---|---|---|
| **Tutorial contains explanation** | Paragraphs of theory interrupt the hands-on flow | Move to a linked Explanation doc. Keep only one-sentence context in the tutorial. |
| **How-to guide teaches** | Explains concepts the reader should already know | Assume competence. Link to a tutorial or explanation for prerequisites. |
| **Reference contains instructions** | "To do X, click Y" appears in what should be a description | Move procedural content to a how-to guide. Reference describes; it does not instruct. |
| **Explanation contains procedures** | Step-by-step instructions appear in a conceptual article | Move procedures to a how-to guide. Explanation discusses; it does not direct. |
| **Any type uses wrong voice** | A tutorial sounds clinical; a reference sounds chatty | Re-read the voice guidance for that type. Rewrite to match. |

## When Content Resists Classification

Some content genuinely spans quadrants. This is not a failure of the framework -- it signals that the content should be **split**, not shoehorned.

- A "Getting Started" page that both teaches and solves a problem? Split into a Tutorial (the learning path) and a How-to Guide (the quick setup for experienced users).
- An API page that describes parameters but also explains design rationale? Split into Reference (the parameters) and Explanation (the design rationale), cross-linked.

Diataxis does not require four top-level directories. It requires that any single document serves one purpose clearly. Organization can be flat, nested, or topic-based -- the constraint is on content purity, not filesystem layout.
