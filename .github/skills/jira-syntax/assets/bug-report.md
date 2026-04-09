# Bug report template

Copy the block below and fill in each section. Remove placeholder text in brackets.

```
h2. Bug description

[One-sentence summary of the defect]

h3. Environment
* *Browser:* [e.g. Chrome 120.0]
* *OS:* [e.g. Windows 11]
* *App version:* [e.g. 2.3.1]
* *Environment:* [Production / Staging / Local]

h3. Steps to reproduce
# [Navigate to ...]
# [Perform action ...]
# [Observe unexpected behavior]

h3. Expected behavior
[What should happen]

h3. Actual behavior
[What happens instead]

{panel:title=Error message|bgColor=#FFEBE9}
{code:javascript}
[Paste error output or stack trace here]
{code}
{panel}

h3. Additional context
* *Frequency:* [Always / Sometimes / Rare]
* *User impact:* [Critical / High / Medium / Low]
* *Workaround:* [Yes — describe / No]

h3. Screenshots
[^screenshot.png]

h3. Related issues
* Blocks [PROJ-XXX]
* Related to [PROJ-YYY]

h3. Technical notes
{code:javascript}
// Relevant code snippet
{code}

----
*Reported by:* [~username]
*Date:* YYYY-MM-DD
```

## Checklist

Before submitting, verify:

- [ ] `h2.` for the top heading, `h3.` for subsections
- [ ] Numbered list (`#`) for steps to reproduce
- [ ] `{code:language}` with a language identifier
- [ ] `{panel}` and `{code}` blocks are closed
- [ ] Issue links use `[PROJ-XXX]` format
- [ ] User mentions use `[~username]` format
- [ ] `{{monospace}}` for paths and UI elements
