---
status: accepted
date: 2026-03-17
decision-makers: Serghei Iakovlev
---

# Use YAML Front Matter for Workflow Files

## Context and Problem Statement

Sortie's workflow definition (`WORKFLOW.md`) must encode two distinct payloads in a single
file: structured configuration (tracker settings, polling intervals, agent parameters, hooks)
and a free-form prompt template (Markdown with Go template directives). The file is the
primary authoring surface for workflow operators — it must be easy to read, edit, and version
in Git.

The format must cleanly separate structured config from prose prompt body, parse
unambiguously, and remain familiar to the target audience of DevOps engineers and software
team leads.

## Decision Drivers

1. **Single-file UX.** Workflow authors should maintain one file per workflow, not a config
   file plus a separate prompt file. This simplifies discovery, versioning, and review.
2. **Clean separation.** The boundary between structured config and free-form prompt must be
   unambiguous. Mixing the two leads to parsing fragility and author confusion.
3. **Ecosystem familiarity.** The format should leverage conventions the target audience
   already knows, minimizing the learning curve.
4. **Parsing simplicity.** The parsing rules must be simple enough to implement without a
   full Markdown parser — only a line-oriented delimiter scanner and a YAML decoder — but
   the implementation must be covered by edge-case tests (CRLF line endings, trailing
   whitespace on delimiters, missing closing delimiter, empty front matter).
5. **Prompt-friendly.** The prompt body is Markdown with embedded Go template directives.
   The format must not require escaping Markdown syntax or template delimiters.

## Considered Options

- YAML front matter in Markdown
- TOML front matter in Markdown
- Pure YAML with a designated prompt key
- Separate config and prompt files

## Decision Outcome

Chosen option: **YAML front matter in Markdown**, because it provides the best balance of
single-file convenience, ecosystem familiarity, and parsing simplicity for this use case.

The workflow file uses the widely-adopted front matter convention: the file opens with `---`,
followed by YAML configuration, followed by a closing `---`, followed by the Markdown prompt
body. If the file does not start with `---`, the entire contents are treated as the prompt
body with an empty config map. The YAML front matter must decode to a map; non-map YAML
(e.g., a bare scalar or list) is a parse error.

**Parsing rules:**

1. Normalize line endings: replace all `\r\n` with `\n` before any delimiter scanning.
2. If the normalized file starts with `---\n`, scan for the next line that is exactly `---`.
3. Bytes between the delimiters are YAML front matter; decode to `map[string]any`.
4. Remaining bytes after the closing delimiter are the prompt template, trimmed of leading
   and trailing whitespace.
5. If no opening `---` is found, `config` is an empty map and the entire file is the prompt
   template.

This convention is established by Jekyll, Hugo, Astro, and most static site generators.
DevOps engineers encounter it in documentation repositories. The syntax requires no escaping
of Markdown headings, code fences, or Go template `{{ }}` directives in the prompt body.

### Considered Options in Detail

**TOML front matter in Markdown.** Uses `+++` delimiters instead of `---`, with TOML
between them. TOML is gaining traction in the Go and Rust ecosystems (Cargo, Hugo config)
and has stronger typing than YAML (native datetime, integer vs float distinction, no
"Norway problem" where `NO` becomes boolean `false`). However, TOML's table syntax
(`[tracker]`, `[agent]`) is less compact than YAML for nested config, and the `+++`
delimiter convention is less widely recognized outside Hugo. The target audience (DevOps
engineers managing Jira workflows) is more likely to encounter YAML daily than TOML. TOML
also lacks native support for multiline strings as clean as YAML's `|` literal blocks,
which matters for hook scripts.

**Pure YAML with a designated prompt key.** The entire file is YAML, with the prompt
template stored under a key like `prompt: |`. This eliminates the front matter parsing step
entirely — one `yaml.Unmarshal` call handles everything. However, it forces the prompt body
into a YAML literal block, which means the author must maintain correct YAML indentation
for the entire prompt. Long prompts with Markdown headings, code fences, and template
directives become difficult to read and edit. Any indentation error corrupts the entire
file. The prompt ceases to be "just Markdown" and becomes "Markdown embedded in YAML,"
which hurts the authoring experience.

**Separate config and prompt files.** Configuration lives in `sortie.yaml` (or
`sortie.toml`), and the prompt template lives in a separate Markdown file referenced by
path. This provides the cleanest separation and avoids any parsing ambiguity. However, it
doubles the number of files to manage, breaks the single-file mental model, and introduces
a file-reference indirection that complicates validation (missing prompt file, relative path
resolution, file watcher must track two files). For a tool designed to be dropped into a
repository with minimal ceremony, two files create unnecessary friction.

## Consequences

### Positive

- Single file to discover, review, and version.
- Prompt body is native Markdown — no YAML indentation, no escaping.
- YAML `|` literal blocks allow inline multi-line hook scripts.
- Familiar convention for the target audience (Jekyll, Hugo, Astro).

### Negative

- **No IDE schema validation for front matter.** A standalone `sortie.yaml` would allow JSON
  Schema binding in VS Code / GoLand for autocompletion and error highlighting. YAML embedded
  in Markdown front matter lacks standard IDE schema support. Mitigation: Dispatch Preflight
  Validation ([architecture Section 6.3](../architecture/06-configuration-specification.md#63-dispatch-preflight-validation)) catches configuration errors before the first poll
  cycle, but errors surface at runtime rather than at edit time. To close this gap, the CLI
  should expose a `sortie validate [path]` subcommand that parses and validates the workflow
  file without starting the orchestrator, enabling integration into pre-commit hooks and CI
  pipelines. This requires a corresponding addition to the architecture doc (Section 17.7).
- **Inline hook scripts are triple-nested (Bash in YAML in Markdown).** IDEs cannot provide
  syntax highlighting or linting for shell scripts inside YAML literal blocks inside `.md`
  files. Multi-line inline scripts are fragile: shell error line numbers cannot be correlated
  back to `WORKFLOW.md` line numbers, and YAML indentation errors silently truncate the
  script. The config layer should support file-path references (e.g.,
  `after_create: ./hooks/setup.sh`) as an alternative to inline scripts from the initial
  implementation, reserving inline values for one-liners like
  `before_run: git checkout -b {{.issue.identifier}}`. This requires amending the hook field
  definitions in the architecture doc (Section 5.3.4) from `shell script string` to
  `shell script string or file path`.
- **YAML type coercion risks.** YAML 1.1 treats bare `NO`, `ON`, `YES` as booleans. The
  implementation must use `gopkg.in/yaml.v3`, which follows YAML 1.2 semantics for
  `map[string]any` targets (these values decode as strings, not booleans). The parser test
  matrix must include cases with values like `NO`, `ON`, `YES`, and `null` as string
  literals to verify correct behavior.
