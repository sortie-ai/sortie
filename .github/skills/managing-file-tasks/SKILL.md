---
name: managing-file-tasks
description: >
  Create, validate, and update JSON issue files for the Sortie file-based tracker adapter.
  Use when asked to create tracker tasks, issue fixtures, test data for the file adapter,
  or when editing any JSON file that contains tracker issues (arrays of objects with "id",
  "identifier", "state" fields). Also use when asked to add, remove, or modify issues in
  an existing file-tracker JSON file. Do NOT use for Jira API interactions or non-JSON
  tracker formats.
---

# Managing File Tasks

Create and maintain JSON issue files consumed by the Sortie file-based tracker adapter (`internal/tracker/file`). The adapter reads a JSON array of issue objects from disk and normalizes them into `domain.Issue` values.

> **Authoritative specification:** `docs/file-based-tasks-spec.md` at the project root — the formal RFC defining the file format, all normalization rules, adapter operation contracts, and a JSON Schema. This skill is an operational guide; the spec is the source of truth for edge cases and conformance.

## Schema

The file is a JSON array. Each element is an issue object. See `examples/issues.json` for a canonical reference with all features demonstrated.

### Required fields

| Field        | Type   | Description                              |
| ------------ | ------ | ---------------------------------------- |
| `id`         | string | Stable tracker-internal ID for lookups   |
| `identifier` | string | Human-readable key (e.g. `"PROJ-1"`)     |
| `title`      | string | Issue summary                            |
| `state`      | string | Current state (e.g. `"To Do"`, `"Done"`) |

### Optional fields

| Field         | Type              | Default when absent | Notes                                             |
| ------------- | ----------------- | ------------------- | ------------------------------------------------- |
| `description` | string            | `""`                | Issue body text                                   |
| `priority`    | integer or null   | `nil`               | Lower = higher priority. Non-integers become nil  |
| `branch_name` | string            | `""`                | Branch metadata                                   |
| `url`         | string            | `""`                | Web link to the issue                             |
| `labels`      | string[]          | `[]`                | Adapter lowercases all values                     |
| `assignee`    | string            | `""`                | Identity string                                   |
| `issue_type`  | string            | `""`                | e.g. `"Bug"`, `"Story"`, `"Task"`                 |
| `parent`      | object or null    | `null`              | `{"id": "...", "identifier": "..."}`              |
| `comments`    | object[] or null  | `null` (not fetched)| `null` = not fetched; `[]` = fetched, none exist  |
| `blocked_by`  | object[]          | `[]`                | Each: `{"id", "identifier", "state"}`             |
| `created_at`  | string            | `""`                | ISO-8601 timestamp                                |
| `updated_at`  | string            | `""`                | ISO-8601 timestamp                                |

### Nested types

**Parent ref:** `{"id": "10000", "identifier": "PROJ-0"}`

**Comment:** `{"id": "c1", "author": "bob", "body": "Comment text.", "created_at": "2026-03-01T10:00:00Z"}`

**Blocker ref:** `{"id": "10002", "identifier": "PROJ-2", "state": "Done"}`

## Operations

### Create a new issue file

1. Start with a JSON array `[]`.
2. Add issue objects with at minimum `id`, `identifier`, `title`, `state`.
3. Assign unique `id` values (string, not integer). Use numeric strings like `"10001"`.
4. Assign unique `identifier` values following `PREFIX-N` convention.
5. Validate the result (see Validation section).

### Add an issue to an existing file

1. Read the existing file.
2. Determine the next available `id` by scanning existing IDs.
3. Append the new issue object to the array.
4. Validate the result.

### Update an issue

1. Read the file, locate the issue by `id` or `identifier`.
2. Modify only the target fields - preserve all other fields.
3. Validate the result.

### Remove an issue

1. Read the file, remove the issue object from the array.
2. Check if any remaining issues reference the removed issue in `blocked_by`. Update those references.
3. Validate the result.

## Validation

After every create or edit, verify:

```bash
python3 -m json.tool <path-to-file> > /dev/null && echo "Valid JSON"
```

Then check these rules manually:

- [ ] File is a JSON array (not an object)
- [ ] Every issue has `id`, `identifier`, `title`, `state` (all strings, non-empty)
- [ ] No duplicate `id` values across issues
- [ ] No duplicate `identifier` values across issues
- [ ] `priority` is an integer, `null`, or absent (never a string, float, or boolean)
- [ ] `labels` is a string array or absent (never `null`)
- [ ] `comments` is an object array, `null`, or absent (never an empty string)
- [ ] `blocked_by` is an object array or absent (never `null`)
- [ ] `parent` is an object with `id` + `identifier`, `null`, or absent
- [ ] Timestamps use ISO-8601 format (`2026-03-01T10:00:00Z`)
- [ ] Blocker refs in `blocked_by` have `id`, `identifier`, and optionally `state`

## Normalization awareness

The file adapter applies these normalizations at read time. The JSON file stores raw values:

- **Labels** are lowercased by the adapter. Store in any case in JSON; the adapter normalizes.
- **Priority** must be a JSON integer to survive normalization. Strings (`"high"`), floats (`2.5`), booleans (`true`) all become `nil` after normalization.
- **Comments `null`** means "not fetched" (adapter returns `nil`). **Comments `[]`** means "fetched, none exist" (adapter returns empty slice). This distinction matters for prompt rendering.
- **State** is stored with original casing. The adapter compares case-insensitively against `active_states` and `terminal_states` from the WORKFLOW.md config.

## WORKFLOW.md integration

To use a file-tracker JSON file with Sortie, configure the tracker in WORKFLOW.md front matter:

```yaml
tracker:
  kind: file
  path: path/to/issues.json
  active_states:
    - to do
    - in progress
  terminal_states:
    - done
    - cancelled
```

`path` is resolved relative to the process working directory.
