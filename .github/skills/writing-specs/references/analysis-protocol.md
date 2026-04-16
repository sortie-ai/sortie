# Analysis Protocol

## Contents

1. [Spec Compliance](#check-1-spec-compliance)
2. [Architecture Layer](#check-2-architecture-layer)
3. [Adapter Boundary](#check-3-adapter-boundary)
4. [Concurrency & State Safety](#check-4-concurrency--state-safety)
5. [Workspace Safety](#check-5-workspace-safety)
6. [Milestone Dependency](#check-6-milestone-dependency)
7. [Requirements Source](#check-7-requirements-source)
8. [Recording Findings](#recording-findings)

---

Seven checks, executed in order before designing anything. Each check gates the design — a failure must be resolved or explicitly flagged before the spec proceeds to drafting.

Record a one-line finding for each check. These findings populate the Spec Compliance Check table in the output.

---

## Check 1: Spec Compliance

Determine whether the architecture doc already defines this behavior.

- Does this feature have a defined behavior in `docs/architecture.md`? **If yes:** the spec is the design — do not redesign it. Cite the section and conform to it.
- Does the design introduce behavior not covered by the architecture doc? **If yes:** flag it explicitly in the spec as "extension beyond current spec" and note that the architecture doc may need updating.
- Does the design contradict any accepted ADR in `docs/decisions/`? **If yes:** stop. Surface the conflict to the user. Do not proceed until the contradiction is resolved.

**Failure action:** Stop and ask for clarification.

---

## Check 2: Architecture Layer

Identify which layer the feature belongs to and verify it stays there.

Layers (from bottom to top):
1. **Policy** — business rules, invariants
2. **Configuration** — typed config, YAML parsing, environment resolution
3. **Coordination** — orchestrator, poll loop, dispatch, state machine
4. **Execution** — workspace lifecycle, agent subprocess management
5. **Integration** — tracker and agent adapters (Jira, GitHub, Claude, Copilot)
6. **Observability** — logging, metrics, HTTP endpoints, dashboards

Questions:
- Which layer does this feature belong to?
- Does the design cross layer boundaries? If yes, justify or restructure.
- Does the design place integration-specific logic (Jira field names, Claude
  Code CLI flags) outside an adapter package? If yes, fix it.

**Failure action:** Restructure the design to respect layer boundaries.

---

## Check 3: Adapter Boundary

Verify that adapter interfaces remain stable and additive.

- Does this touch the `TrackerAdapter` or `AgentAdapter` interface?
- If adding a new adapter: is it a new package implementing the existing interface with zero changes to core?
- If modifying the interface: does every existing adapter still compile?
  Is there a migration path?

**Failure action:** If the interface must change, document the migration
path for all existing adapters in the spec.

---

## Check 4: Concurrency & State Safety

Verify the design respects single-writer semantics and context propagation.

- Does this touch the orchestrator's runtime state (`running`, `claimed`, `retry_attempts`)?
- The orchestrator serializes all state mutations through one authority — does the design respect single-writer semantics?
- Are goroutine lifecycles tied to `context.Context` for cancellation propagation?
- Is SQLite access single-writer compatible (WAL mode, no concurrentwrites)?

**Failure action:** Redesign to eliminate concurrent state mutation.

---

## Check 5: Workspace Safety

Verify security boundaries are maintained. These are non-negotiable.

- Does this feature touch workspace paths or directory creation?
- **Path containment:** workspace path must be under workspace root (absolute path prefix check). Reject symlink escapes.
- **Workspace key sanitization:** only `[A-Za-z0-9._-]` in directory names. No exceptions.
- **Agent cwd validation:** coding agent must launch with `cwd == workspace_path`. Verify before launch, not after.

**Failure action:** If any workspace safety invariant is weakened, reject the design. These are security boundaries, not suggestions.

---

## Check 6: Milestone Dependency

Verify the feature respects the project's build order.

- Which GitHub milestone does this belong to?
- Are all prerequisite milestones complete? If not, flag what must be built first.
- Is the task sized for a single agent session? If not, decompose it.

**Failure action:** Flag missing prerequisites. Do not design features that depend on unbuilt foundations.

---

## Check 7: Requirements Source

When Atlassian MCP tools are available, fetch external context.

- If a Jira issue ID or URL is provided, fetch the issue details, acceptance criteria, and linked issues using MCP tools.
- Check linked Confluence pages for architectural context or domain documentation.
- Extract constraints and requirements from Jira issue fields (priority, labels, components, fix version).

**Failure action:** If the Jira issue contradicts the feature request, surface the discrepancy before designing.

---

## Recording Findings

After completing all seven checks, summarize findings in this format before proceeding to spec drafting:

```
Analysis findings:
1. Spec compliance: [conforms / extends / conflicts] — Section X.Y
2. Architecture layer: [layer name] — [crosses boundaries: yes/no]
3. Adapter boundary: [no change / additive / breaking change]
4. Concurrency safety: [no state touch / single-writer preserved]
5. Workspace safety: [not applicable / invariants preserved]
6. Milestone dependency: [milestone name] — [prerequisites met: yes/no]
7. Requirements source: [Jira fetched / no external source]
```

These findings feed directly into the Spec Compliance Check table in Section 1 of the output template.
