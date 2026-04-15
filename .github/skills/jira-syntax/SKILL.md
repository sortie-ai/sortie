---
name: jira-syntax
description: >
  Use when writing Jira issue descriptions, comments, or work logs.
  Also use when converting Markdown to Jira wiki markup, when the user
  says "format for Jira", "Jira markup", "wiki notation", or asks to
  create, update, or validate Jira ticket content. Handles bug report
  and feature request templates.
  Do NOT use for Jira API operations, JQL queries, or workflow transitions.
metadata:
  author: airSlate Inc.
  version: "1.0"
  category: project-management
---

# Jira wiki markup

Jira uses its own wiki notation that differs from Markdown in every formatting construct. Agents default to Markdown. Mixing the two produces broken rendering in Jira. This skill ensures correct output.

## Markdown-to-Jira conversion table

These substitutions are mandatory when targeting Jira:

| Concept | Jira wiki markup | NOT Markdown |
|---------|-----------------|--------------|
| Heading | `h2. Title` | `## Title` |
| Bold | `*bold*` | `**bold**` |
| Italic | `_italic_` | `*italic*` |
| Inline code | `{{code}}` | `` `code` `` |
| Code block | `{code:java}...{code}` | ` ```java ``` ` |
| Link | `[text\|url]` | `[text](url)` |
| Issue link | `[PROJ-123]` | n/a |
| User mention | `[~username]` | `@username` |
| Bullet list | `* item` | `- item` |
| Numbered list | `# item` | `1. item` |
| Table header | `\|\|Header\|\|` | `\|Header\|` |

Read `references/syntax-reference.md` for the complete notation including panels, colors, nested lists, and special blocks.

## Conversion examples

**Example 1** (bug comment with code):

Input (Markdown):
```
## Root cause

The `processData()` function throws when **input is null**.

- Missing null check on line 45
- Related: see issue #123
```

Output (Jira wiki markup):
```
h2. Root cause

The {{processData()}} function throws when *input is null*.

* Missing null check on line 45
* Related: see [PROJ-123]
```

**Example 2** (status update with table):

Input (Markdown):
```
### Sprint progress

| Task | Status | Owner |
|------|--------|-------|
| Auth module | Done | @john |
| API docs | In progress | @jane |
```

Output (Jira wiki markup):
```
h3. Sprint progress

||Task||Status||Owner||
|Auth module|{color:green}Done{color}|[~john]|
|API docs|{color:yellow}In progress{color}|[~jane]|
```

## Workflow

### Step 1: Determine delivery method and content type

**Delivery method — choose one path and follow it exclusively:**

- **Atlassian MCP tool** (`createJiraIssue`, `editJiraIssue`) — write in Markdown. The MCP server uses the v3 API and converts the `description` field from Markdown to ADF internally. Skip Steps 2 and 3, go directly to Step 4.
- **Jira wiki editor or REST API v2** (`text/wiki` content type) — write in Jira wiki markup. Continue with Steps 2–4.

**Content type — choose the appropriate template:**

- **Bug report** — read `assets/bug-report.md`
- **Feature request** — read `assets/feature-request.md`
- **Free-form content** — write directly using the conversion table and examples above

### Step 2: Write content in Jira wiki markup

Apply these rules:

1. Use `h2.` for top-level sections, `h3.` for subsections. Always include a space after the period because `h2.Title` fails to render.
2. Use `*` for bullet lists and `#` for numbered lists. Nest with repeated symbols (`**`, `##`, `#*`), not indentation.
3. Wrap code in `{code:language}...{code}`. Always specify the language because bare `{code}` blocks lose syntax highlighting.
4. Use `{{text}}` for inline code, paths, and UI element names.
5. Use `[text|url]` for labeled links. Use `[PROJ-123]` for issue references, `[~username]` for mentions.
6. Use `||` for table headers and `|` for data cells. Keep column counts consistent per row.
7. Use `{panel:title=X}...{panel}` for callout boxes. Use `{color:red}text{color}` for colored text.
8. Close every block macro (`{code}`, `{panel}`, `{color}`, `{quote}`, `{expand}`, `{noformat}`).

### Step 3: Validate

Run the validation script against the content file:

```bash
sh scripts/validate-jira-syntax.sh <file>
```

The script checks for Markdown syntax used where Jira markup is expected, unclosed block macros, headings missing the space after the period, and code blocks without a language identifier.

If the script is unavailable, verify manually:

- [ ] No Markdown headings (`##`), bold (`**`), or backtick code blocks
- [ ] All `{code}`, `{panel}`, `{color}` blocks are closed
- [ ] Headings have a space after the period (`h2. Title`)
- [ ] Code blocks specify a language (`{code:python}`)
- [ ] Links use pipe syntax (`[label|url]`)
- [ ] Lists use `*` for bullets and `#` for numbers

### Step 4: Submit

Pass the validated content to the Jira API or paste it into the Jira editor. If an MCP tool or jira-communication skill is available, use it for submission. This skill handles syntax only.
