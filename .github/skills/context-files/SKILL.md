---
name: context-files
description: >
  Create or validate project context files (AGENTS.md, CLAUDE.md, GEMINI.md).
  Use when bootstrapping a new project, initializing agent configuration,
  writing a context file, or when asked to create, review, audit, or validate
  an existing context file. Handles codebase archaeology, user interviews,
  golden-rule validation, and platform-specific formatting.
  Do NOT use for creating Agent Skills (use creating-agent-skills instead)
  or .instructions.md files (use agent-customization instead).
metadata:
  version: 2026-04-12
---

# Context File Generator

Your job is to dig up the knowledge that is NOT in the code — the traps a new developer hits on day one. Agents waste tokens rediscovering what the team already knows. This skill prevents that.

## The golden rule

Before writing any line, ask:

> "Can an agent discover this on its own by reading the codebase?"

If yes, delete the line. Agents read `package.json`, `tsconfig.json`, directory trees, linter configs, and README files. They know every popular framework. Restating this wastes tokens and dilutes the instructions that matter.

## Determine mode

- **Create mode:** User wants to generate a new context file or has no context file yet.
- **Validate mode:** User wants to audit or review an existing context file.

If unclear, ask the user which mode they need.

### Determine target format

Ask the user which file they need, or infer from their project:

| File | Platform | Notes |
|------|----------|-------|
| `AGENTS.md` | Universal (Codex, Cursor, Copilot) | Preferred for multi-platform teams |
| `CLAUDE.md` | Claude Code | Supports `@import` syntax |
| `GEMINI.md` | Gemini CLI | Configurable name via `settings.json` |

If the project uses multiple platforms, recommend `AGENTS.md` as the canonical file with bridge files for others. See `references/platform-formats.md` for platform-specific details.

---

## Create mode

### Phase 1: Deep archaeological analysis

Scan the codebase for non-discoverable knowledge. Your value is finding what agents miss.

**Skip the surface layer** (agents already read these):

- `package.json`, `composer.json`, `go.mod`, `requirements.txt`, `pyproject.toml`
- `tsconfig.json`, `.eslintrc`, `.prettierrc`, linter configs
- `README.md`, directory structure

**Dig into the deep layer** using `references/archaeological-checklist.md`:

1. **Build and test traps.** Scan `Makefile`, `Justfile`, `Taskfile.yml`, `scripts/`, `package.json` scripts, CI configs. Find test commands that differ from the obvious one, build steps with side effects, CI-only steps that must be replicated locally.

2. **Environment traps.** Scan `.nvmrc`, `.node-version`, `.tool-versions`, `.env.example`, `docker-compose.yml`. Find required runtime versions, environment variables needed to start the app, Docker quirks.

3. **Git and workflow traps.** Scan `.husky/`, `.pre-commit-config.yaml`, `CONTRIBUTING.md`. Find pre-commit hooks, branch naming conventions, commit message formats enforced by hooks.

4. **Monorepo structure.** Scan `pnpm-workspace.yaml`, `lerna.json`, `nx.json`, `turbo.json`. Find build order constraints, shared configs that must not be overridden.

5. **Code archaeology.** Search for `HACK`, `FIXME`, `XXX`, `WORKAROUND`, `DO NOT`, `NEVER`, `WARNING`, `DEPRECATED` in comments. Find off-limits areas, intentional workarounds that look wrong.

6. **Architecture traps.** Read source code in key directories. Find custom middleware that resembles standard patterns, database wrappers, auth deviations, API response wrappers, shared cache prefixes or queue names that couple services.

7. **Configuration traps.** Scan `.npmrc`, `.yarnrc.yml`, `composer.json` config, `.editorconfig`. Find private registry URLs, resolution overrides, patched dependencies.

Collect findings silently. Do not output analysis results yet.

### Phase 2: The interview

Present findings from Phase 1 as context. Ask these questions and **stop to wait for the user's response**:

**Question 1: Landmines.** "What breaks if an agent does the obvious thing? What traps does a new developer hit on day one?" Provide 3-5 suspected traps from your analysis.

**Question 2: Sacred areas.** "What files, directories, or patterns must an agent NEVER touch without permission?" Provide candidates from your analysis (generated code dirs, migration files, shared configs).

**Question 3: Non-obvious commands.** "Which commands differ from what an agent would guess? (test, build, lint, deploy, migrate)" Provide candidates found in build scripts and CI configs.

**Question 4: Unwritten conventions.** "What team conventions exist that no linter enforces and no documentation captures?" Give 2-3 examples of what you mean: naming conventions, PR processes, module ownership, error handling philosophy.

**Question 5: Refactoring strategy.** "When an agent touches legacy code, what should it do?" Options: Strict (enforce current standards on new code, contain old code), Consistent (follow existing patterns), Rewrite (aggressively refactor everything touched).

### Phase 3: Generate the file

Synthesize analysis and user answers. Use the template from the **Output template** section below.

**Hard constraints:**

- Target 30-80 lines. Absolute maximum 100 lines for the root file.
- Every line must contain information an agent cannot discover by reading the codebase.
- No tech stack summaries. No directory trees. No framework documentation.
- No code snippets, function bodies, or import examples.
- No numbered section prefixes ("1. System Identity"). Use plain descriptive headings.
- No sections named "Appendix", "Quick Reference", "Overview", or "Cheat Sheet".
- Omit any section that has nothing non-discoverable to say. An empty section is worse than no section.

For CLAUDE.md or GEMINI.md, adapt the template following the platform-specific conventions in `references/platform-formats.md`.

### Phase 4: Self-review

Before presenting the file, review every line against the golden rule checklist:

1. Could an agent find this by reading `package.json`, config files, or the directory tree? → Delete it.
2. Could an agent find this by reading the README? → Delete it.
3. Does a linter or formatter enforce this? → Delete it.
4. Is this framework documentation the agent already knows? → Delete it.
5. Would this survive the "new hire test"? If a senior developer read the codebase for an hour, would they figure this out? If yes → Delete it.

Count the remaining lines. If the file exceeds 100 lines, split reference material into separate docs and link from the "Reference docs" section.

Run the validation script on the generated file:

```bash
python3 scripts/validate_context_file.py <path-to-generated-file>
```

If the script is unavailable, use the manual checklist below.

### Phase 5: Bridge files

If the user needs multi-platform support, generate bridge files:

- **CLAUDE.md** bridge: `@AGENTS.md`
- **Copilot** bridge: add `Refer to [AGENTS.MD](../AGENTS.md)` in `.github/copilot-instructions.md`
- **GEMINI.md** bridge: set `context.fileName` to `["AGENTS.md", "GEMINI.md"]` in `.gemini/settings.json`

Do not duplicate content across platform files. One source of truth, references everywhere else.

---

## Validate mode

When the user asks to review an existing context file:

### Step 1: Read the file

Read the context file and the project's codebase (package.json, config files, README, directory structure).

### Step 2: Run automated validation

```bash
python3 scripts/validate_context_file.py <path-to-context-file>
```

If the script is unavailable, proceed with manual review.

### Step 3: Golden rule audit

For each line in the file, check:

- **Discoverable?** Can the agent find this from config files, directory tree, or README?
- **Linter-enforceable?** Does a linter or formatter handle this?
- **Framework docs?** Is this documentation the agent already knows?
- **Stale?** Does this reference deprecated workflows or removed files?

### Step 4: Structure audit

Check against the quality checklist:

- [ ] Root file is under 100 lines (aim for 30-60)
- [ ] No project structure descriptions
- [ ] No tech stack summaries
- [ ] No framework documentation
- [ ] No style rules that a linter enforces
- [ ] No codebase overviews that duplicate the README
- [ ] Clear section headings (Commands, Gotchas, Boundaries)
- [ ] Boundaries split into Always / Ask first / Never
- [ ] Reference docs use progressive disclosure (pointers, not copies)
- [ ] Every command has been verified (wrong commands are worse than no commands)

### Step 5: Report

Present findings grouped by severity:

- **Delete:** Lines that fail the golden rule (provide the line and the reason)
- **Add:** Non-discoverable knowledge found in the codebase but missing from the file
- **Fix:** Stale, incorrect, or ambiguous lines
- **Structure:** Formatting or organizational improvements

---

## Output template

Use the template in `assets/context-file-template.md`. Omit any section that has nothing non-discoverable to say.

For CLAUDE.md or GEMINI.md, adapt the template following the conventions in `references/platform-formats.md`.

---

## Constraints

These constraints apply to you, the generator agent:

1. **No hallucinated dependencies.** Only state what you found in config files.
2. **Surface assumptions.** If you make a choice (e.g., assume a directory is the API layer), tell the user. Silent assumptions cascade into silent failures.
3. **Ask when unclear.** If Phase 1 raised questions you cannot resolve from code alone, ask the user before generating. Do not guess.
4. **Strategy, not tactics.** Context files define WHAT and WHY. Code snippets, boilerplate, and library syntax belong in `.github/instructions/` files or Agent Skills. If you find yourself writing code, stop.
5. **Legacy firewalls.** If the codebase has bad patterns, the context file must explicitly forbid replicating them. Document the target architecture, not the messy reality.
6. **Accuracy over comprehensiveness.** A short file with 10 genuine landmines beats a long file with 50 discoverable facts.
