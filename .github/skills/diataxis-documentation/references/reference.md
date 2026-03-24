# Writing Reference Documentation

Reference documentation is the austere, authoritative description of the machinery. Users consult it -- they do not read it. They arrive mid-task, needing one precise fact: a function signature, a parameter's allowed values, a configuration key's default. They need truth and certainty, firm ground on which to stand while they work.

Think of a nutritional label on food packaging. Standard format. Reliable facts. Consistent structure. No recipes, no opinions, no marketing. Information so serious it should be governed by law.

## Core Principles

### 1. Describe -- Nothing More

Reference documentation has exactly one job: accurate, neutral description of what exists and how it behaves. It states facts. It does not instruct, persuade, teach, or opine.

- **Good:** "`timeout` (int) -- Maximum wait time in milliseconds. Default: 30000. Must be positive."
- **Bad:** "`timeout` (int) -- You should set this to a reasonable value. We recommend 30000ms for most use cases, though if you're dealing with slow networks you might want to increase it."

The first is reference. The second has drifted into how-to territory (recommendation) and explanation (reasoning about networks).

### 2. Mirror the Machinery's Structure

The documentation's organization must follow the product's structure, not an author's narrative preference. If the code has three modules with five functions each, the reference documents three modules with five functions each. The reader expects to find documentation where the product puts things.

- API reference follows the API's URL structure
- CLI reference follows the command hierarchy
- Configuration reference follows the config file's structure
- Type reference follows the type system's organization

### 3. Be Relentlessly Consistent

Reference material is useful precisely when it is predictable. Every function, every parameter, every configuration key follows the same pattern. If one function entry shows `Parameters`, `Returns`, `Raises`, and `Examples` -- every function entry shows those same sections in that same order, even when some are empty.

Consistency lets readers locate information by position, not by reading. A reader who has looked up three functions knows exactly where to find the return type of the fourth.

### 4. Be Complete

Reference is the one type where completeness is non-negotiable. Every public API endpoint. Every configuration option. Every CLI flag. Omission in reference is a lie -- the reader trusts that what they see is what exists. If something is missing, they will assume it does not exist and make incorrect decisions.

### 5. Provide Examples (Sparingly)

Short, illustrative examples clarify descriptions without displacing them. An example shows how a parameter's description manifests concretely:

```markdown
### `retry_policy` (string)

Controls retry behavior for failed requests.

Allowed values: `"none"`, `"linear"`, `"exponential"`

Default: `"exponential"`

Example:
    client = Client(retry_policy="linear")
```

The example does not explain retry strategies. It shows the parameter in use. Nothing more.

## Structure Patterns

### For APIs / Functions / Methods

```markdown
## `function_name`

[One sentence: what this function does.]

**Parameters:**

| Name | Type | Required | Default | Description |
|------|------|----------|---------|-------------|
| `param` | `string` | Yes | -- | [What it is. Constraints.] |

**Returns:** `Type` -- [What is returned.]

**Raises:**
- `ErrorType` -- [When this error occurs.]

**Example:**

    result = function_name("value")
```

### For Configuration Options

```markdown
## `config.section.key`

[What this option controls.]

- **Type:** `string`
- **Default:** `"value"`
- **Allowed values:** `"a"`, `"b"`, `"c"`
- **Environment variable:** `APP_SECTION_KEY`
- **Since:** v2.3.0
```

### For CLI Commands

```markdown
## `command subcommand`

[What this command does.]

**Usage:** `tool command subcommand [flags] <required-arg> [optional-arg]`

**Arguments:**

| Argument | Required | Description |
|----------|----------|-------------|
| `required-arg` | Yes | [What it is.] |

**Flags:**

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | [What it does.] |
```

## Voice and Tone

- **Third person, present tense.** "Returns the connection status" not "You can get the connection status by calling..."
- **Austere and neutral.** No personality. No warmth. No encouragement. Precision is the tone.
- **Declarative statements.** "The default value is 30." Not "The default value is typically 30" (is it or isn't it?).
- **Imperative for rules.** "Must be a positive integer." "Must not exceed 4096 bytes."

### Characteristic Phrases

- "Returns..."
- "Accepts..."
- "Default: ..."
- "Must be..."
- "Raises `X` when..."
- "Since version..."
- "See also: [related reference]"
- "Deprecated since v3.0. Use `alternative` instead."

## Anti-Patterns

| Anti-Pattern | Why It Fails |
|---|---|
| Instructional creep | "To configure this, go to Settings and click..." is a how-to guide disguised as reference. Reference describes what the setting does, not how to navigate to it. |
| Opinions and recommendations | "We recommend setting this to 50" belongs in a how-to guide. Reference states the default, the range, and the behavior. |
| Inconsistent format | If some functions have "Examples" sections and others don't, the reader cannot trust the structure. Always use the same template. |
| Narrative flow between entries | Reference entries are independent. A reader who jumps to entry #47 should not need context from entry #1. No "as mentioned above." |
| Auto-generated without curation | Raw API docs from code comments are a starting point, not a finished product. They typically lack examples, omit behavioral nuance, and have inconsistent descriptions. |
| Mixing reference with explanation | "The connection pool maintains a set of reusable connections because creating TCP connections is expensive..." -- the "because" clause is explanation. Cut it. If needed, link to an Explanation doc. |

## Boundary Discipline

**Drifting into How-to Guide?** If you are writing steps ("First, configure X. Then, add Y to Z."), that is procedural content. Reference describes X, Y, and Z individually. A how-to guide connects them into a workflow.

**Drifting into Explanation?** If you are writing "because," "the reason is," or "this exists to solve..." -- that is explanation. Reference states *what*. Explanation provides *why*.

**Drifting into Tutorial?** If your examples are growing into a guided walkthrough with sequential steps building on each other, that is a tutorial. Reference examples are isolated, self-contained illustrations of a single element.

## The Completeness Standard

Reference is the only Diataxis type where exhaustive completeness is an explicit requirement. For the other three types, practical utility outweighs completeness. For reference, missing information is a defect. A user who consults reference and finds no entry for an existing feature has been failed.

Maintain reference as a living mirror of the product. When the product changes, reference changes in the same commit.
