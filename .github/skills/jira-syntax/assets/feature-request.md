# Feature request template

Copy the block below and fill in each section. Remove placeholder text in brackets.

```
h2. Feature overview

[One-paragraph summary of the feature]

h3. Business value
* *User impact:* [Who benefits and how]
* *Business goal:* [Strategic objective this supports]
* *Priority justification:* [Why now]

h3. User stories

h4. As a [user type]
* I want to [action]
* So that [benefit]

h3. Acceptance criteria
# [Specific, testable criterion]
# [Specific, testable criterion]
# [Specific, testable criterion]

h3. Requirements

h4. Must have
* [Requirement]
* [Requirement]

h4. Should have
* [Requirement]

h4. Could have
* [Requirement]

h3. Non-functional requirements
* *Performance:* [Response time, throughput]
* *Security:* [Auth, data protection]
* *Scalability:* [Load targets]

h3. Technical considerations
{code:python}
# Architecture notes or pseudocode
{code}

h3. Dependencies
* Requires [PROJ-XXX] — [reason]
* Impacts [PROJ-YYY] — [coordination needed]

h3. Open questions
* [Question needing clarification]
* [Decision required]

h3. Success metrics
||Metric||Target||Measurement||
|[Metric name]|[Target value]|[How measured]|

----
*Requested by:* [~username]
*Stakeholders:* [~pm], [~designer], [~engineer]
```

## Checklist

Before submitting, verify:

- [ ] `h2.` for the top heading, `h3.`/`h4.` for subsections
- [ ] Numbered list (`#`) for acceptance criteria
- [ ] Bullet lists (`*`) for requirements
- [ ] `{code:language}` with a language identifier
- [ ] Table headers use `||` (double pipe)
- [ ] Issue links use `[PROJ-XXX]` format
- [ ] User mentions use `[~username]` format
