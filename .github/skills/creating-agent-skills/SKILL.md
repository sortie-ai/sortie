---
name: creating-agent-skills
description: Use when creating, improving, comparing, evaluating or packaging Agent Skills following the agentskills.io specification. Also use when deciding whether a skill is the right solution vs MCP servers, custom instructions, AGENTS.md, or Cursor Rules. Handles SKILL.md authoring, frontmatter optimization, description writing, progressive disclosure, cross-platform compatibility, and distribution.
metadata:
  version: 2026-04-12
---

# Creating Agent Skills

Create first-class Agent Skills that work across every major agent platform. A skill is a self-contained package of instructions, scripts, and resources that transforms a general-purpose agent into a specialized one for a specific domain or task.

## Core Philosophy

**The context window is a shared resource.** Your skill competes for tokens with the system prompt, conversation history, other skills' metadata, and the user's actual request. Every line must earn its place.

**The agent is already smart.** Only add context it doesn't have. Before writing any instruction, ask: "Does the agent really need this?" If you're explaining what a PDF is, or how a for-loop works - cut it.

**Explain WHY, not just WHAT.** Agents respond better to reasoning than rigid commands. Instead of "ALWAYS use pdfplumber" write "Use pdfplumber for text extraction because it handles multi-column layouts and rotated text better than alternatives."

## Skill Anatomy

```plaintext
skill-name/
├── SKILL.md              # Required: YAML frontmatter + markdown body
├── scripts/              # Optional: executable code (runs via bash, output enters context)
├── references/           # Optional: docs loaded into context on demand
└── assets/               # Optional: templates, images, schemas for output generation
```

Only SKILL.md is required. Delete unused directories.

## Workflow

### Phase 1: Understand Intent

Before writing anything, clarify:

1. **Is a skill the right solution?** See [references/skills-ecosystem.md](references/skills-ecosystem.md) if the task might be better solved by custom instructions, AGENTS.md, MCP servers, or Cursor Rules
2. **What capability** does this skill provide? (one focused domain)
3. **When should it trigger?** (user phrases, file types, task contexts)
4. **What does the agent lack?** (only add context it doesn't already have)
5. **What does success look like?** (output format, quality bar)
6. **Does it need scripts, references, or just instructions?**

If the conversation already contains a workflow to capture ("turn this into a skill"), extract: steps taken, tools used, corrections made, I/O formats observed. Confirm with the user before proceeding.

### Phase 2: Initialize

Scaffold the directory:

```bash
python3 scripts/init_skill.py <skill-name> --path <output-directory>
```

This creates the directory structure with a SKILL.md template. If the script is unavailable or `python3` is not installed, create file manually: a directory matching the skill name, containing a SKILL.md file and all necessary subdirectories/files.

### Phase 3: Write the Frontmatter

The frontmatter is the most critical part. Agents scan 100+ skill descriptions at startup to decide which to load. A poor description means the skill never triggers.

```yaml
---
name: skill-name
description: "What this skill does AND when to use it. Include trigger phrases, file types, task contexts. Be slightly pushy - agents under-trigger. Max 1024 characters."
---
```

**Name rules** (must match directory name):

- Lowercase alphanumeric + hyphens only, 1-64 chars
- No leading/trailing/consecutive hyphens
- Cannot contain "anthropic" or "claude"
- Prefer gerund form: `processing-pdfs`, `analyzing-data`, `testing-code`

**Description strategy**: The description serves two purposes - skill discovery and triggering. Include both WHAT it does and WHEN to use it. Include edge cases that should trigger it:

```yaml
# ✅ Do - specific, pushy, includes edge cases
description: "Build fast dashboards to display data. Use whenever the user mentions dashboards, data visualization, metrics, charts, graphs, or wants to display any kind of data visually, even if they don't explicitly ask for a dashboard."

# ❌ Don't - vague, no triggers
description: "Helps with data display."
```

**Always write in third person.** Descriptions are injected into system prompts. "Processes PDFs" not "I can help you process PDFs."

**Use negative triggers to prevent over-activation.** When a skill triggers too broadly, add explicit exclusions:

```yaml
description: "Advanced statistical analysis for CSV files. Use for regression, clustering, hypothesis testing. Do NOT use for basic data exploration, formatting, or simple aggregation."
```

Negative triggers act as a filter. Without them, a broad description pulls the skill into conversations where it adds overhead without value.

**Debug descriptions by asking the agent.** Say: "When would you use the [skill-name] skill?" The agent will quote its understanding of the scope. Adjust based on what it reports vs. what you intended.

See [references/frontmatter-fields.md](references/frontmatter-fields.md) for all optional fields: `license`, `compatibility`, `metadata`, `allowed-tools`.

### Phase 4: Write the Instructions Body

The body loads when the skill triggers. Target under 500 lines - split longer content into reference files.

**Choose a structure** based on the skill's nature:

| Pattern     | Best For             | Key Feature                               |
| ----------- | -------------------- | ----------------------------------------- |
| Workflow    | Sequential processes | Step-by-step with checklists              |
| Task-based  | Tool collections     | Grouped by operation type                 |
| Reference   | Standards/specs      | Organized by domain                       |
| Conditional | Branching logic      | Decision tree with pointers to references |

See [references/writing-patterns.md](references/writing-patterns.md) for detailed examples of each pattern, degrees of freedom calibration, multishot prompting, checklists, script integration, and progressive disclosure strategies.


**Match freedom to fragility:**

- **High freedom** (code reviews, creative tasks): Give general direction, trust the agent
- **Medium freedom** (report generation, data analysis): Provide templates with customization guidance
- **Low freedom** (database migrations, file format operations): Exact scripts, specific sequences

**Writing principles:**

- Use imperative form: "Extract text with pdfplumber" not "You can extract text..."
- Provide one default approach per task, mention alternatives only when context requires them
- Include concrete input/output examples - they communicate style better than descriptions
- Use consistent terminology throughout (pick "field" or "control", not both)
- For workflows: provide a checklist the agent can copy and track progress

**Example pattern** (input/output pairs teach style effectively):

```markdown
## Commit message format

**Example 1:**
Input: Added user authentication with JWT tokens
Output:
feat(auth): implement JWT-based authentication
Add login endpoint and token validation middleware

**Example 2:**
Input: Fixed bug where dates displayed incorrectly
Output:
fix(reports): correct date formatting in timezone conversion
```

**Feedback loop pattern** (critical for quality-sensitive operations):

```markdown
1. Make edits to the document
2. Validate: python3 scripts/validate.py output/
3. If validation fails: review errors, fix, validate again
4. If python script is unavailable, use a checklist and verify each item manually:
   - [ ] Step 1
   - [ ] Step 2
   - [ ] Step 3
5. Only proceed when validation passes
```

### Phase 5: Bundle Resources

**scripts/** - Executable code for deterministic operations. Scripts run via bash; only their output enters the context window. This makes them ideal for validation, file processing, and data transformation.

- Handle errors explicitly with helpful messages
- Document inputs, outputs, and exit codes
- Test every script before including it

**references/** - Additional documentation loaded on demand. Keep one level deep from SKILL.md (no nested references - agents may only partially read deeply nested files).

- For files over 100 lines, include a table of contents at the top
- Reference clearly from SKILL.md with guidance on WHEN to read each file
- Split by domain, not by size: `references/aws.md`, `references/gcp.md`

**assets/** - Templates, images, schemas used in output generation. Not loaded for reasoning.

### Phase 6: Validate

Run validation against the agentskills.io specification:

```bash
python scripts/validate_skill.py
```

This checks frontmatter fields, name format, description constraints, body size, reference depth, and directory structure.

### Phase 7: Test and Iterate

A skill isn't done until tested with real prompts. Start by iterating on a single challenging task until the agent succeeds, then expand to broader coverage.

**Step 1: Single-task iteration.** Pick the hardest task the skill should handle. Run it repeatedly. Fix what breaks. This gives faster signal than testing broadly from the start.

**Step 2: Triggering tests.** Write 10-20 queries across three groups:

- _Should trigger (obvious):_ "Help me set up a new project"
- _Should trigger (paraphrased):_ "Initialize a workspace for Q4"
- _Should NOT trigger:_ "Help me write Python code"

Target: 80-90% correct activation. If under-triggering, add trigger phrases. If over-triggering, add negative triggers or narrow scope.

**Step 3: Functional tests.** Run the same request 3-5 times. Verify:

- Steps execute in correct order
- Tool calls succeed
- Output matches expected format
- Inconsistent results reveal ambiguous instructions

**Step 4: Performance comparison.** Compare the same task with and without the skill. Count messages exchanged, tool call failures, and total tokens. If the skill doesn't improve at least one metric, simplify it.

**Step 5: Iterate.** Read the agent's thinking process, not only the output. Is it wasting time? Missing steps? Confused by instructions? Generalize from feedback rather than overfitting to specific examples. Keep instructions lean. Remove anything that isn't pulling its weight.

If automated evaluation is desired, create `evals/evals.json`:

```json
{
  "skill_name": "my-skill",
  "evals": [
    {
      "id": 1,
      "prompt": "User's task prompt",
      "expected_output": "Description of expected result",
      "assertions": ["Output includes X", "Correctly handles Y"]
    }
  ]
}
```

## Progressive Disclosure

Skills use three-level loading to manage context efficiently:

1. **Metadata** (~100 tokens): `name` + `description` loaded at startup for ALL installed skills
2. **Instructions** (<5000 tokens recommended): Full SKILL.md body loaded when skill triggers
3. **Resources** (as needed): Referenced files loaded only when required

This means your description must work hard at ~100 tokens to win selection, while SKILL.md body should be comprehensive but efficient.

## Cross-Platform Compatibility

The SKILL.md format, frontmatter schema, and progressive disclosure pattern are universal. Write once, use everywhere. Platform-specific differences are mainly about storage locations:

| Platform | Project Scope | User Scope | Notes |
|----------|--------------|------------|-------|
| VS Code / GitHub Copilot | `.github/skills/` | User profile (VS Code settings) | |
| Claude Code | `.claude/skills/` | `~/.claude/skills/` | |
| Cursor | `.cursor/skills/` | `~/.cursor/skills/` | |
| Gemini CLI | `.gemini/skills/` | `~/.gemini/skills/` | Also reads `.agents/skills/` with priority |
| OpenAI Codex | `.agents/skills/` | `~/.agents/skills/` | |
| Generic (cross-platform) | `.agents/skills/` | N/A | |

Note: `.agents/` is emerging as a cross-platform location. OpenAI Codex uses `.agents/skills/` (not `.codex/`). Gemini CLI supports both `.gemini/skills/` and `.agents/skills/`, with `.agents/skills/` taking precedence when both exist. Google Antigravity and OpenCode also use `.agents/`.

**Precedence**: Project-level skills override personal skills, which override extension/plugin skills. Exception: Codex shows both skills in selectors when names collide (no merge/override).

**Codex-specific**: Scans `.agents/skills/` from CWD up to repo root, then `~/.agents/skills/`, then `/etc/codex/skills/`. Invoke with `$skill-name`. Built-in `$skill-creator` and `$skill-installer` help with authoring.

## Distribution

**Via skills.sh** (Vercel's package manager):

```bash
# Users install with:
npx skills add <owner>/<repo>
# Or specific skill from a multi-skill repo:
npx skills add <owner>/<repo> --skill "skill-name"
```

To make a skill distributable via https://skills.sh, publish it to a GitHub repository following the standard directory structure. For details see: https://skills.sh/docs/cli

**Via .skill package** (Claude-specific):
A `.skill` file is a zip archive with `.skill` extension containing the skill directory. Users upload it in Claude.ai Settings > Features.

**Via Claude Code Plugin marketplace**:
```bash
/plugin marketplace add <owner>/<repo>
```

## Platform Extensions

**OpenAI Codex** supports an optional `agents/openai.yaml` alongside SKILL.md for UI metadata and invocation control:

```yaml
interface:
  display_name: "My Skill"
  short_description: "User-facing description"
  icon_small: "./assets/icon.svg"
  brand_color: "#3B82F6"
policy:
  allow_implicit_invocation: false  # require explicit $skill invocation
dependencies:
  tools:
    - type: "mcp"
      value: "serverName"
      description: "Required MCP server"
```

This file is Codex-specific and safely ignored by other platforms.

## Anti-Patterns

**Critical (cause skill activation failures):**

- **Workflow summary in the description** - If the description contains the procedure, the agent may skip the skill body entirely. Description triggers; body teaches.

```yaml
# BAD - this IS the workflow, agent may skip the body
description: "Analyzes git diff, identifies the change type, generates a commit message"

# GOOD - this tells WHEN to activate and WHAT capabilities exist
description: "Use when generating commit messages. Handles conventional commits, scope detection, breaking changes."
```

- **Vague descriptions** - No trigger phrases means no activation. "Helps with documents" matches nothing.

- **Monolithic skills** - A single skill covering "all development workflows" loads slowly and activates imprecisely. Split broad capabilities into focused skills (`run-tests`, `create-migration`, `deploy-staging`).

**High impact (degrade agent performance):**

- **README-style documentation** - Skills explain WHAT things are. Agents need HOW to do things. Write procedures with steps, not explanations with context.

- **External dependencies** - Skills requiring `git clone` or network downloads before working are fragile. Bundle what you need in the skill directory.

- **Command lists without verification** - Flat command lists without conditional logic or error handling produce brittle workflows. Include verification steps and failure handling.

- **First/second person in descriptions** - Descriptions inject into system prompts (third person). "I can help you..." breaks voice. Use "Handles...", "Use when...".

**Medium impact (reduce quality):**

- **Verbose explanations** of concepts the agent already knows
- **Multiple equivalent options** without a clear default
- **Windows-style paths** (backslashes) - always use forward slashes
- **Deeply nested references** (SKILL.md → advanced.md → details.md) - keep one level deep
- **Time-sensitive information** ("After August 2025, use the new API")
- **Inconsistent terminology** (mixing "field"/"box"/"element"/"control")
- **Heavy-handed MUSTs without reasoning** - explain why, let the agent understand
