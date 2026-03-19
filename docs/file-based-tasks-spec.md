# Sortie File-Based Tasks Specification

- **Status:** Informational RFC
- **Version:** 1.0.0
- **Date:** 2026-03-19

---

## 1. Overview

This document specifies the JSON file format consumed by the Sortie file-based tracker adapter (`kind: file`). The adapter implements the `TrackerAdapter` interface using a local JSON file as the issue store, enabling development, testing, and offline workflows without a live tracker API.

The file is the single source of truth. The adapter performs read-only access — it never writes to the file. External tooling or manual edits are responsible for mutations.

## 2. Terminology

| Term               | Definition                                                                            |
| ------------------ | ------------------------------------------------------------------------------------- |
| **Issue**          | A single work item (bug, story, task) tracked by Sortie.                              |
| **Adapter**        | The component that reads the JSON file and produces normalized `domain.Issue` values. |
| **Normalization**  | Deterministic transformations the adapter applies at read time (Section 7).           |
| **Active state**   | A state name configured as eligible for candidate selection.                          |
| **Terminal state** | A state name indicating the issue is complete (e.g. "Done", "Cancelled").             |

## 3. File Structure

The file MUST be a valid JSON document containing a top-level array. Each element of the array is an Issue Object (Section 4). An empty array (`[]`) is valid and represents a project with no issues.

```
File = JSON-Array<IssueObject>
```

The file MUST use UTF-8 encoding. A BOM is not required and SHOULD be omitted. Trailing commas and comments are not permitted (strict JSON).

### 3.1 File Path Resolution

The file path is provided via the `path` configuration key and is resolved relative to the process working directory at the time the adapter is constructed. Absolute paths are accepted.

## 4. Issue Object

An Issue Object is a JSON object with the fields described below. Fields are partitioned into **required** and **optional**.

### 4.1 Required Fields

Every Issue Object MUST contain all four required fields. Each MUST be a non-empty JSON string.

| Field        | JSON Type | Constraints                                      | Description                                                                                                                                    |
| ------------ | --------- | ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `id`         | string    | Non-empty. Unique across all issues in the file. | Stable tracker-internal identifier used for lookups and map keys.                                                                              |
| `identifier` | string    | Non-empty. Unique across all issues in the file. | Human-readable key (e.g. `"PROJ-1"`). Convention is `PREFIX-N` but this is not enforced by the adapter.                                        |
| `title`      | string    | Non-empty.                                       | Issue summary.                                                                                                                                 |
| `state`      | string    | Non-empty.                                       | Current workflow state. Stored with original casing. Compared case-insensitively by the adapter against `active_states` and `terminal_states`. |

### 4.2 Optional Fields

Optional fields MAY be omitted entirely from the JSON object. When omitted, the adapter applies the default value specified below.

| Field         | JSON Type                | Default When Absent  | Constraints                                                                                                                |
| ------------- | ------------------------ | -------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `description` | string                   | `""` (empty string)  | Issue body text.                                                                                                           |
| `priority`    | integer or null          | `nil` (null)         | Lower values = higher priority. MUST be a JSON integer or `null`. See Section 7.2 for normalization of non-integer values. |
| `branch_name` | string                   | `""` (empty string)  | Git branch metadata.                                                                                                       |
| `url`         | string                   | `""` (empty string)  | Web link to the issue.                                                                                                     |
| `labels`      | array of strings         | `[]` (empty array)   | Tag/label values. MUST NOT be `null`. See Section 7.1 for case normalization.                                              |
| `assignee`    | string                   | `""` (empty string)  | Identity string of the assignee.                                                                                           |
| `issue_type`  | string                   | `""` (empty string)  | Tracker-defined type (e.g. `"Bug"`, `"Story"`, `"Task"`).                                                                  |
| `parent`      | object or null           | `null`               | Parent issue reference. See Section 5.1.                                                                                   |
| `comments`    | array of objects or null | `null` (not fetched) | Issue comments. See Section 5.2 and Section 7.3 for null semantics. MUST NOT be a non-null non-array value.                |
| `blocked_by`  | array of objects         | `[]` (empty array)   | Blocking issue references. MUST NOT be `null`. See Section 5.3.                                                            |
| `created_at`  | string                   | `""` (empty string)  | ISO-8601 timestamp (e.g. `"2026-03-01T10:00:00Z"`).                                                                        |
| `updated_at`  | string                   | `""` (empty string)  | ISO-8601 timestamp.                                                                                                        |

## 5. Nested Types

### 5.1 Parent Reference

A Parent Reference is a JSON object identifying the parent issue for a sub-task.

| Field        | JSON Type | Required | Description                        |
| ------------ | --------- | -------- | ---------------------------------- |
| `id`         | string    | Yes      | Tracker-internal ID of the parent. |
| `identifier` | string    | Yes      | Human-readable key of the parent.  |

Example:

```json
{ "id": "10000", "identifier": "PROJ-0" }
```

When the issue has no parent, the `parent` field MUST be either `null` or absent. It MUST NOT be an empty object `{}`.

### 5.2 Comment

A Comment is a JSON object representing a single comment on an issue.

| Field        | JSON Type | Required | Description                             |
| ------------ | --------- | -------- | --------------------------------------- |
| `id`         | string    | Yes      | Tracker-internal comment identifier.    |
| `author`     | string    | Yes      | Display name or username of the author. |
| `body`       | string    | Yes      | Comment text content.                   |
| `created_at` | string    | Yes      | ISO-8601 timestamp.                     |

Example:

```json
{
  "id": "c1",
  "author": "bob",
  "body": "Needs SSO support.",
  "created_at": "2026-03-01T10:00:00Z"
}
```

### 5.3 Blocker Reference

A Blocker Reference is a JSON object identifying an issue that blocks the parent issue.

| Field        | JSON Type | Required | Description                                                                                                                   |
| ------------ | --------- | -------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `id`         | string    | Yes      | Tracker-internal ID of the blocking issue.                                                                                    |
| `identifier` | string    | Yes      | Human-readable key of the blocking issue.                                                                                     |
| `state`      | string    | No       | Current state of the blocking issue. Empty string when unknown, which the orchestrator treats as non-terminal (conservative). |

Example:

```json
{ "id": "10002", "identifier": "PROJ-2", "state": "Done" }
```

When `state` is absent or empty, the orchestrator assumes the blocker is NOT resolved. This is a safety invariant — unknown blocker state blocks dispatch.

## 6. Uniqueness Constraints

Within a single file:

1. **`id` values MUST be unique.** No two Issue Objects may share the same `id`. The adapter uses `id` for exact-match lookups (`FetchIssueByID`, `FetchIssueStatesByIDs`). Duplicate `id` values result in undefined behavior — the adapter may return either issue.
2. **`identifier` values MUST be unique.** `identifier` is used for human-facing operations, workspace naming, and log output. Duplicates cause ambiguity.
3. **Comment `id` values SHOULD be unique** within the scope of a single issue's `comments` array. Uniqueness across issues is not required.

## 7. Normalization Rules

The adapter applies the following deterministic transformations at read time. The JSON file stores raw values; the adapter produces normalized `domain.Issue` values. Implementors MUST reproduce these rules exactly.

### 7.1 Labels

All label strings are lowercased by the adapter using Unicode-aware case folding (`strings.ToLower`).

- JSON: `["BUG", "High-Priority"]` → Domain: `["bug", "high-priority"]`
- JSON: `[]` → Domain: `[]` (non-nil empty slice)
- JSON: absent → Domain: `[]` (non-nil empty slice)
- JSON: `null` → **Invalid.** The `labels` field MUST NOT be `null`.

### 7.2 Priority

The adapter accepts ONLY JSON integer values. All other JSON types are normalized to `nil` (null).

| JSON Value | Domain Value | Explanation                        |
| ---------- | ------------ | ---------------------------------- |
| `2`        | `*int(2)`    | Valid integer.                     |
| `1`        | `*int(1)`    | Valid integer.                     |
| `0`        | `*int(0)`    | Valid integer (zero is permitted). |
| `null`     | `nil`        | Explicit null.                     |
| absent     | `nil`        | Missing field.                     |
| `"high"`   | `nil`        | String — rejected.                 |
| `2.5`      | `nil`        | Float — rejected.                  |
| `true`     | `nil`        | Boolean — rejected.                |

**Implementation detail:** The adapter unmarshals the raw JSON bytes into a Go `int`. If successful, it re-marshals the integer and compares the canonical bytes to the original raw bytes. If they differ (as happens with `2.5` which Go silently truncates to `2`), the value is rejected. This ensures only true JSON integers survive normalization.

### 7.3 Comments Null Semantics

The `comments` field carries tri-state semantics:

| JSON Value     | Domain Value                        | Meaning                                                                                    |
| -------------- | ----------------------------------- | ------------------------------------------------------------------------------------------ |
| `null`         | `nil` (nil slice)                   | Comments were **not fetched**. The caller cannot distinguish "no comments" from "unknown". |
| `[]`           | `[]Comment{}` (non-nil empty slice) | Comments were fetched; **none exist**.                                                     |
| `[{...}, ...]` | `[]Comment{...}` (populated slice)  | Comments were fetched; N comments exist.                                                   |
| absent         | `nil` (nil slice)                   | Same as `null` — not fetched.                                                              |

This distinction is critical for prompt rendering. When comments are `nil`, the orchestrator knows it must call `FetchIssueComments` separately if comments are needed.

**Context-dependent override in `FetchIssueByID`:** When an issue has `null` comments in the file and is retrieved via `FetchIssueByID`, the adapter coerces `nil` to an empty non-nil slice `[]Comment{}`. This is because `FetchIssueByID` is defined as returning a "fully-populated" issue. Listing operations (`FetchCandidateIssues`, `FetchIssuesByStates`) always set comments to `nil` regardless of the file value, to match the adapter contract.

### 7.4 State Comparison

States are stored with their original casing in the JSON file and in the normalized `domain.Issue`. Comparison against `active_states` and `terminal_states` is case-insensitive — both sides are lowercased before comparison.

- File: `"To Do"`, Config: `active_states: ["to do"]` → **Match.**
- File: `"IN PROGRESS"`, Config: `active_states: ["in progress"]` → **Match.**

### 7.5 BlockedBy

- JSON: `[{...}]` → Domain: `[]BlockerRef{...}` (populated slice)
- JSON: `[]` → Domain: `[]BlockerRef{}` (non-nil empty slice)
- JSON: absent → Domain: `[]BlockerRef{}` (non-nil empty slice)
- JSON: `null` → **Invalid.** The `blocked_by` field MUST NOT be `null`.

### 7.6 Parent

- JSON: `{"id": "...", "identifier": "..."}` → Domain: `*ParentRef{...}`
- JSON: `null` → Domain: `nil`
- JSON: absent → Domain: `nil`

## 8. Adapter Configuration

The file adapter is registered under the kind `"file"` in the Sortie tracker registry. Configuration is provided via the WORKFLOW.md front matter or equivalent configuration map.

### 8.1 Configuration Keys

| Key             | Type             | Required | Description                                                                                                                        |
| --------------- | ---------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `path`          | string           | Yes      | Filesystem path to the JSON file. Resolved relative to the process working directory. Empty string is an error.                    |
| `active_states` | array of strings | No       | States considered active for `FetchCandidateIssues`. Compared case-insensitively. When absent or empty, all issues are candidates. |

### 8.2 WORKFLOW.md Integration

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

`terminal_states` is consumed by the orchestrator, not the file adapter directly. The adapter uses only `path` and `active_states`.

### 8.3 Configuration Errors

If `path` is missing or empty, the adapter constructor returns a `TrackerError` with kind `tracker_payload_error` and the message `"missing required config key: path"`.

## 9. Adapter Operations

The file adapter implements all five `TrackerAdapter` operations. The file is re-read from disk on each operation call to support test scenarios that modify the fixture between operations.

### 9.1 FetchCandidateIssues

Returns issues whose state matches the configured `active_states` (case-insensitive). If no active states are configured, returns all issues. Comments are set to `nil` on all returned issues regardless of the file contents.

**Returns:** `[]Issue` (non-nil, possibly empty).

### 9.2 FetchIssueByID

Returns a single fully-populated issue including comments, located by exact `id` match. If the issue's comments are `null` in the file, they are coerced to a non-nil empty slice.

**Returns:** `(Issue, nil)` on success; `(zero Issue, *TrackerError{Kind: tracker_payload_error})` if not found.

### 9.3 FetchIssuesByStates

Returns issues whose state matches any value in the provided states slice (case-insensitive). An empty states slice returns immediately with a non-nil empty slice and does not read the file. Comments are set to `nil` on returned issues.

**Returns:** `[]Issue` (non-nil, possibly empty).

### 9.4 FetchIssueStatesByIDs

Returns a map of issue ID to current state for each requested ID. Issues not found in the file are silently omitted from the result (not an error). An empty IDs slice returns immediately with a non-nil empty map and does not read the file.

**Returns:** `map[string]string` (non-nil, possibly empty).

### 9.5 FetchIssueComments

Returns comments for the specified issue. If the issue exists but has `null` comments in the file, returns a non-nil empty slice. If the issue exists with an empty array `[]`, returns a non-nil empty slice. If the issue is not found, returns a `TrackerError` with kind `tracker_payload_error`.

**Returns:** `([]Comment, nil)` on success; `(nil, *TrackerError)` if not found.

## 10. Error Handling

All adapter errors are returned as `*TrackerError` values with the following structure:

```
TrackerError {
    Kind:    TrackerErrorKind  // always "tracker_payload_error" for the file adapter
    Message: string            // operator-friendly description
    Err:     error             // underlying OS or JSON error, may be nil
}
```

The file adapter only produces errors of kind `tracker_payload_error`. It does not produce `tracker_transport_error`, `tracker_auth_error`, or `tracker_api_error` because there is no network or authentication involved.

Error conditions:

| Condition                                        | Message Pattern                       |
| ------------------------------------------------ | ------------------------------------- |
| File cannot be read (missing, permission denied) | `"failed to read file: <path>"`       |
| File contains invalid JSON                       | `"failed to parse file: <path>"`      |
| Issue not found by ID                            | `"issue not found: <id>"`             |
| Missing `path` config key                        | `"missing required config key: path"` |

## 11. Validation Checklist

Producers of issue files MUST verify the following invariants after every create or edit:

1. The file is syntactically valid JSON.
2. The top-level value is a JSON array.
3. Every Issue Object contains `id`, `identifier`, `title`, `state` — all non-empty strings.
4. No duplicate `id` values across issues.
5. No duplicate `identifier` values across issues.
6. `priority` is a JSON integer, `null`, or absent. Never a string, float, or boolean.
7. `labels` is a JSON array of strings or absent. Never `null`.
8. `comments` is a JSON array of Comment objects, `null`, or absent. Never a non-null non-array value.
9. `blocked_by` is a JSON array of Blocker Reference objects or absent. Never `null`.
10. `parent` is a Parent Reference object, `null`, or absent. Never an empty object.
11. Timestamps use ISO-8601 format (e.g. `"2026-03-01T10:00:00Z"`).
12. Blocker References contain at minimum `id` and `identifier`; `state` is optional.
13. Parent References contain both `id` and `identifier`.
14. Comment objects contain `id`, `author`, `body`, and `created_at`.

## 12. Examples

### 12.1 Minimal Valid File

```json
[]
```

### 12.2 Minimal Issue (Required Fields Only)

```json
[
  {
    "id": "1",
    "identifier": "TASK-1",
    "title": "Do the thing",
    "state": "To Do"
  }
]
```

### 12.3 Fully Populated Issue

```json
[
  {
    "id": "10001",
    "identifier": "PROJ-1",
    "title": "Implement OAuth2 login flow",
    "description": "Add OAuth2 login with SSO support for enterprise accounts.",
    "priority": 2,
    "state": "To Do",
    "branch_name": "feature/PROJ-1",
    "url": "https://tracker.example.com/PROJ-1",
    "labels": ["feature", "Auth"],
    "assignee": "alice",
    "issue_type": "Story",
    "parent": {
      "id": "10000",
      "identifier": "PROJ-0"
    },
    "comments": [
      {
        "id": "c1",
        "author": "bob",
        "body": "Needs SSO support for SAML providers.",
        "created_at": "2026-03-01T10:00:00Z"
      }
    ],
    "blocked_by": [
      {
        "id": "10002",
        "identifier": "PROJ-2",
        "state": "In Progress"
      }
    ],
    "created_at": "2026-02-28T09:00:00Z",
    "updated_at": "2026-03-02T14:30:00Z"
  }
]
```

### 12.4 Comments Tri-State

```json
[
  {
    "id": "1",
    "identifier": "A-1",
    "title": "Comments not fetched",
    "state": "Open",
    "comments": null
  },
  {
    "id": "2",
    "identifier": "A-2",
    "title": "Comments fetched, none exist",
    "state": "Open",
    "comments": []
  },
  {
    "id": "3",
    "identifier": "A-3",
    "title": "Comments fetched, two exist",
    "state": "Open",
    "comments": [
      {
        "id": "c1",
        "author": "alice",
        "body": "First.",
        "created_at": "2026-03-01T10:00:00Z"
      },
      {
        "id": "c2",
        "author": "bob",
        "body": "Second.",
        "created_at": "2026-03-01T11:00:00Z"
      }
    ]
  }
]
```

### 12.5 Priority Edge Cases

```json
[
  {
    "id": "1",
    "identifier": "P-1",
    "title": "Integer priority",
    "state": "Open",
    "priority": 2
  },
  {
    "id": "2",
    "identifier": "P-2",
    "title": "Null priority",
    "state": "Open",
    "priority": null
  },
  {
    "id": "3",
    "identifier": "P-3",
    "title": "Absent priority",
    "state": "Open"
  },
  {
    "id": "4",
    "identifier": "P-4",
    "title": "String priority (becomes nil)",
    "state": "Open",
    "priority": "high"
  },
  {
    "id": "5",
    "identifier": "P-5",
    "title": "Float priority (becomes nil)",
    "state": "Open",
    "priority": 2.5
  },
  {
    "id": "6",
    "identifier": "P-6",
    "title": "Boolean priority (becomes nil)",
    "state": "Open",
    "priority": true
  }
]
```

Issues P-4, P-5, and P-6 are valid JSON but their priority values are normalized to `nil` at read time.

## 13. JSON Schema (Informative)

The following JSON Schema is provided for reference. It describes the file format but does not encode all normalization rules (those are specified in Section 7).

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Sortie File-Based Issues",
  "description": "JSON file consumed by the Sortie file-based tracker adapter.",
  "type": "array",
  "items": {
    "type": "object",
    "required": ["id", "identifier", "title", "state"],
    "properties": {
      "id": {
        "type": "string",
        "minLength": 1,
        "description": "Stable tracker-internal ID. Must be unique across all issues."
      },
      "identifier": {
        "type": "string",
        "minLength": 1,
        "description": "Human-readable key (e.g. PROJ-1). Must be unique across all issues."
      },
      "title": {
        "type": "string",
        "minLength": 1,
        "description": "Issue summary."
      },
      "state": {
        "type": "string",
        "minLength": 1,
        "description": "Current workflow state. Stored with original casing."
      },
      "description": {
        "type": "string",
        "description": "Issue body text."
      },
      "priority": {
        "oneOf": [{ "type": "integer" }, { "type": "null" }],
        "description": "Numeric priority. Lower = higher. Non-integer values are normalized to nil."
      },
      "branch_name": {
        "type": "string",
        "description": "Git branch metadata."
      },
      "url": {
        "type": "string",
        "description": "Web link to the issue."
      },
      "labels": {
        "type": "array",
        "items": { "type": "string" },
        "description": "Tag/label values. Lowercased by the adapter at read time."
      },
      "assignee": {
        "type": "string",
        "description": "Identity string of the assignee."
      },
      "issue_type": {
        "type": "string",
        "description": "Tracker-defined type (Bug, Story, Task, etc.)."
      },
      "parent": {
        "oneOf": [
          {
            "type": "object",
            "required": ["id", "identifier"],
            "properties": {
              "id": { "type": "string" },
              "identifier": { "type": "string" }
            },
            "additionalProperties": false
          },
          { "type": "null" }
        ],
        "description": "Parent issue reference for sub-tasks."
      },
      "comments": {
        "oneOf": [
          {
            "type": "array",
            "items": {
              "type": "object",
              "required": ["id", "author", "body", "created_at"],
              "properties": {
                "id": { "type": "string" },
                "author": { "type": "string" },
                "body": { "type": "string" },
                "created_at": { "type": "string", "format": "date-time" }
              },
              "additionalProperties": false
            }
          },
          { "type": "null" }
        ],
        "description": "Issue comments. null = not fetched, [] = fetched but none exist."
      },
      "blocked_by": {
        "type": "array",
        "items": {
          "type": "object",
          "required": ["id", "identifier"],
          "properties": {
            "id": { "type": "string" },
            "identifier": { "type": "string" },
            "state": { "type": "string" }
          },
          "additionalProperties": false
        },
        "description": "Blocking issue references."
      },
      "created_at": {
        "type": "string",
        "format": "date-time",
        "description": "ISO-8601 creation timestamp."
      },
      "updated_at": {
        "type": "string",
        "format": "date-time",
        "description": "ISO-8601 last-updated timestamp."
      }
    },
    "additionalProperties": false
  }
}
```

## 14. Conformance

An implementation conforms to this specification if it:

1. Accepts any file that passes the validation checklist (Section 11).
2. Rejects files that are not valid JSON or are not top-level arrays.
3. Applies all normalization rules from Section 7 identically.
4. Implements all five operations from Section 9 with the specified return-value contracts.
5. Produces `TrackerError` values as described in Section 10.
6. Treats `comments: null` and absent `comments` as "not fetched" (`nil`), distinct from `comments: []` ("fetched, none exist").
7. Never writes to or modifies the issue file.

## 15. Revision History

| Version | Date       | Changes                                              |
| ------- | ---------- | ---------------------------------------------------- |
| 1.0.0   | 2026-03-19 | Initial specification extracted from implementation. |
