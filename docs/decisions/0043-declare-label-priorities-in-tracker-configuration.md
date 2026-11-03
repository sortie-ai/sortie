---
status: proposed
date: 2026-11-03
---

# Declare Label Priorities in Tracker Configuration

## Context and Problem Statement

Gitea, GitHub, and GitLab issues have no native priority field. Operators already express priority with labels such as `priority/high` and `P1`, but Sortie leaves their normalized priority unset. The existing priority sort therefore falls through to creation time, and dispatch rules using `match.priority` cannot match these issues.

## Decision Drivers

1. Use the priority conventions operators already maintain, without guessing their meaning.
2. Preserve workflows that declare no priority mapping.
3. Keep matching bounded and normalization at the adapter boundary, consistent with [the dispatch-rule decision](0011-dispatch-rule-configuration.md) and its [successor](0031-let-a-dispatch-rule-carry-its-agent-kinds-settings.md).

## Considered Options

- Declare an exact label-to-integer map in tracker configuration
- Infer priorities from conventional label names or the forge's label ordering
- Leave priority unset and express every priority choice through dispatch label rules

## Decision Outcome

Chosen option: **declare `tracker.priority_labels`**, an optional map from label names to integers for `gitea`, `github`, and `gitlab` only. An explicit operator-declared map is configuration, not a synthetic mapping: the operator supplies the meaning and nothing is guessed.

The map matches the adapters' normalized lowercase label set exactly, ignoring letter case, without globs. An issue with several mapped labels takes the smallest integer, meaning the highest urgency; an issue with no mapped label keeps null priority. The shared implementation lives in `internal/issuekit` and runs in each adapter's issue normalizer. It populates the existing domain priority, making the existing dispatch sort and `match.priority` work on label trackers with no new tracker field or scheduler policy.

Configuration load rejects unsupported kinds, non-maps, blank keys, keys repeated after lowercasing, non-integer or out-of-range values, and names also used by workflow states. The integer range is the domain's Go `int` range. Gitea and GitLab reuse their advisory catalog diagnostics to warn about unresolved priority labels, comparing names case-insensitively. Changes require a restart, like `tracker.query_filter`.

## Consequences

- Existing workflows keep unset priority until the operator declares a map. Labels remain available for label matching and prompt rendering.
- Priority meaning is reviewable in `WORKFLOW.md` and independent of repository naming conventions or forge-specific label ordering.
- Operators maintain the map and restart Sortie after editing it. Missing catalog labels warn where catalog diagnostics already exist; no priority labels are created automatically.
- Workflow state labels cannot also encode priority, preventing state transitions from silently changing urgency.

## Confirmation

Table tests cover shared matching and strict configuration validation. Each adapter's normalizer and construction wiring are tested. An orchestrator test fetches label-tracker candidates and proves a newer urgent issue wins a single dispatch slot and matches a priority rule; without a map the older issue wins. The tracker schema, normalization contract, workflow reference, adapter notes, and changelog describe the same behavior.
