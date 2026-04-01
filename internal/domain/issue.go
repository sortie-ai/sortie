// Package domain defines the normalized types and adapter interfaces
// shared across all orchestration layers. Start with [Issue] for the
// core issue model and [TrackerAdapter] for the tracker integration
// contract.
package domain

// Issue is the normalized issue record used by orchestration, prompt
// rendering, and observability output. Tracker adapters produce Issue
// values by normalizing their native API responses.
type Issue struct {
	// ID is the stable tracker-internal identifier used for map keys
	// and tracker lookups.
	ID string

	// Identifier is the human-readable ticket key (e.g. "ABC-123")
	// used for logs and workspace naming.
	Identifier string

	// DisplayID is the qualified form of Identifier for
	// dashboard and API display. For trackers where Identifier alone
	// is ambiguous (e.g. GitHub issue numbers), the adapter sets this
	// to a qualified form such as "owner/repo#9". Empty means
	// Identifier is already display-ready.
	DisplayID string

	// Title is the issue summary/title.
	Title string

	// Description is the issue body text. Empty string when absent.
	Description string

	// Priority is the numeric priority where lower numbers are higher
	// priority in dispatch sorting. Nil when unavailable or
	// non-numeric.
	Priority *int

	// State is the current tracker state name (e.g. "To Do",
	// "In Progress"). Stored with original casing; the orchestrator
	// normalizes both sides to lowercase when comparing.
	State string

	// BranchName is tracker-provided branch metadata. Empty string
	// when absent.
	BranchName string

	// URL is the web link to the issue in the tracker. Empty string
	// when absent.
	URL string

	// Labels are normalized to lowercase by the adapter. Non-nil
	// empty slice when no labels exist.
	Labels []string

	// Assignee is the identity string as provided by the tracker.
	// Empty string when absent.
	Assignee string

	// IssueType is the tracker-defined type (e.g. "Bug", "Story",
	// "Task"). Empty string when absent.
	IssueType string

	// Parent is the parent issue reference for sub-tasks. Nil when
	// the issue has no parent.
	Parent *ParentRef

	// Comments contains human feedback, review notes, and prior
	// agent workpad entries. Nil means comments were not fetched;
	// an empty non-nil slice means no comments exist.
	Comments []Comment

	// BlockedBy lists issues that block this one. Each ref's State
	// may be empty, which is treated as non-terminal (conservative).
	// Non-nil empty slice when no blockers exist.
	BlockedBy []BlockerRef

	// CreatedAt is an ISO-8601 timestamp. Empty string when absent.
	CreatedAt string

	// UpdatedAt is an ISO-8601 timestamp. Empty string when absent.
	UpdatedAt string
}

// BlockerRef identifies an issue that blocks the parent issue.
// Populated from inverse "blocks" relations in the tracker.
type BlockerRef struct {
	// ID is the tracker-internal ID. Empty string when unavailable.
	ID string

	// Identifier is the human-readable key. Empty string when
	// unavailable.
	Identifier string

	// State is the current state of the blocking issue. Empty string
	// when unknown, which is treated as non-terminal (conservative).
	State string
}

// ParentRef identifies the parent issue for a sub-task.
type ParentRef struct {
	// ID is the tracker-internal ID of the parent.
	ID string

	// Identifier is the human-readable key of the parent.
	Identifier string
}

// Comment represents a single comment on an issue. Adapters normalize
// native comment structures into this type.
type Comment struct {
	// ID is the tracker-internal comment identifier.
	ID string

	// Author is the display name or username of the comment author.
	Author string

	// Body is the comment text content.
	Body string

	// CreatedAt is an ISO-8601 timestamp.
	CreatedAt string
}

// ToTemplateMap converts the Issue to a map[string]any with snake_case
// keys matching the architecture specification field names. The result
// is suitable for passing to prompt template rendering as the "issue"
// variable.
func (iss *Issue) ToTemplateMap() map[string]any {
	var priority any
	if iss.Priority != nil {
		priority = *iss.Priority
	}

	var parent any
	if iss.Parent != nil {
		parent = map[string]any{
			"id":         iss.Parent.ID,
			"identifier": iss.Parent.Identifier,
		}
	}

	var comments any
	if iss.Comments != nil {
		cm := make([]map[string]any, len(iss.Comments))
		for i, c := range iss.Comments {
			cm[i] = map[string]any{
				"id":         c.ID,
				"author":     c.Author,
				"body":       c.Body,
				"created_at": c.CreatedAt,
			}
		}
		comments = cm
	}

	blockedBy := make([]map[string]any, len(iss.BlockedBy))
	for i, b := range iss.BlockedBy {
		blockedBy[i] = map[string]any{
			"id":         b.ID,
			"identifier": b.Identifier,
			"state":      b.State,
		}
	}

	labels := iss.Labels
	if labels == nil {
		labels = []string{}
	}

	return map[string]any{
		"id":          iss.ID,
		"identifier":  iss.Identifier,
		"title":       iss.Title,
		"description": iss.Description,
		"priority":    priority,
		"state":       iss.State,
		"branch_name": iss.BranchName,
		"url":         iss.URL,
		"labels":      labels,
		"assignee":    iss.Assignee,
		"issue_type":  iss.IssueType,
		"parent":      parent,
		"comments":    comments,
		"blocked_by":  blockedBy,
		"created_at":  iss.CreatedAt,
		"updated_at":  iss.UpdatedAt,
	}
}
