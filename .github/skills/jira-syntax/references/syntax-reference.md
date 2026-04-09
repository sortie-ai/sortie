# Jira wiki markup syntax reference

Complete notation for Jira's wiki renderer. Use this when the quick table in SKILL.md is insufficient.

## Table of contents

- [Text formatting](#text-formatting)
- [Headings](#headings)
- [Lists](#lists)
- [Links](#links)
- [Code](#code)
- [Tables](#tables)
- [Panels and quotes](#panels-and-quotes)
- [Colors](#colors)
- [Special blocks](#special-blocks)
- [Line breaks and rules](#line-breaks-and-rules)
- [Escaping](#escaping)
- [Emoticons](#emoticons)

## Text formatting

| Syntax | Result | Notes |
|--------|--------|-------|
| `*text*` | **bold** | Single asterisk, not double |
| `_text_` | _italic_ | |
| `{{text}}` | `monospace` | For code, paths, UI elements |
| `-text-` | ~~strikethrough~~ | |
| `+text+` | underline | |
| `^text^` | superscript | |
| `~text~` | subscript | |
| `??text??` | citation | |

## Headings

```
h1. Heading 1
h2. Heading 2
h3. Heading 3
h4. Heading 4
h5. Heading 5
h6. Heading 6
```

Rules:
- Space required after the period (`h2. Title`, not `h2.Title`)
- One heading per line
- Use h2 for main sections, h3 for subsections

## Lists

Bullet lists:
```
* Level 1
** Level 2
*** Level 3
```

Numbered lists:
```
# First
## Nested
# Second
```

Mixed lists:
```
# Numbered
#* Nested bullet
#* Another bullet
# Numbered again
```

Rules:
- Space after `*` or `#`
- Nest with repeated symbols (`**`, `##`), not indentation
- Mix types with combined symbols (`#*`, `*#`)

## Links

| Type | Syntax | Example |
|------|--------|---------|
| External URL | `[http://url]` | `[http://example.com]` |
| Labeled link | `[Label\|url]` | `[Google\|http://google.com]` |
| Issue link | `[KEY-123]` | `[PROJ-456]` |
| User mention | `[~username]` | `[~john.doe]` |
| Attachment | `[^filename]` | `[^screenshot.png]` |
| Email | `[mailto:email]` | `[mailto:team@example.com]` |
| Anchor | `{anchor:name}` + `[#name]` | `{anchor:top}` then `[#top]` |

## Code

Inline: `{{monospace text}}`

Block with syntax highlighting:
```
{code:python}
def hello():
    print("Hello")
{code}
```

Block without highlighting:
```
{noformat}
Plain preformatted text
{noformat}
```

Supported languages: `java`, `javascript`, `typescript`, `python`, `sql`, `json`, `xml`, `html`, `css`, `bash`, `shell`, `php`, `ruby`, `go`, `rust`, `c`, `cpp`, `csharp`, and others.

## Tables

```
||Header 1||Header 2||Header 3||
|Cell A1|Cell A2|Cell A3|
|Cell B1|Cell B2|Cell B3|
```

Rules:
- `||` for header cells (double pipe)
- `|` for data cells (single pipe)
- Every row must have the same column count
- No trailing pipe at end of row

Example with formatting:
```
||Feature||Status||Owner||
|Login|{color:green}Done{color}|[~john.doe]|
|2FA|{color:red}Blocked{color}|Unassigned|
```

## Panels and quotes

Panel with title and background:
```
{panel:title=Important|bgColor=#FFFFCE}
Panel content here.
{panel}
```

Panel parameters: `title`, `bgColor`, `borderStyle` (solid/dashed), `borderColor`, `titleBGColor`.

Styled notice panels:
```
{panel:title=Warning|bgColor=#FFEBE9|borderColor=#FF0000}
Warning content.
{panel}

{panel:title=Info|bgColor=#DEEBFF|borderColor=#0052CC}
Informational content.
{panel}

{panel:title=Success|bgColor=#E3FCEF|borderColor=#00875A}
Success content.
{panel}
```

Block quote:
```
{quote}
Quoted text spanning
multiple lines.
{quote}
```

Single-line quote:
```
bq. This is a block quote.
```

## Colors

```
{color:red}Red text{color}
{color:#0052CC}Hex color{color}
```

Named colors: `red`, `blue`, `green`, `yellow`, `orange`, `purple`, `black`, `white`, `gray`.

Always close the `{color}` tag.

## Special blocks

Expand/collapse:
```
{expand:title=Click to expand}
Hidden content.
{expand}
```

## Line breaks and rules

- `\\` forces a line break within a paragraph
- Blank line starts a new paragraph
- `----` draws a horizontal rule (four dashes)

## Escaping

Backslash escapes special characters: `\*`, `\{`, `\[`, `\|`

Special characters:
- `---` renders as em-dash
- `--` renders as en-dash

## Emoticons

| Code | Meaning |
|------|---------|
| `:)` | Happy |
| `:(` | Sad |
| `:D` | Big smile |
| `;)` | Wink |
| `(y)` | Thumbs up |
| `(n)` | Thumbs down |
| `(!)` | Warning |
| `(?)` | Question |
| `(*)` | Star |
