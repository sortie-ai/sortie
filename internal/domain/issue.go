// Package domain defines the normalized types and adapter interfaces
// shared across all orchestration layers. Start with [Issue] for the
// core issue model and [TrackerAdapter] for the tracker integration
// contract.
package domain

import "time"

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

	// Labels contains the issue's labels, normalized to lowercase by
	// the adapter. Non-nil empty slice when no labels exist.
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

	// BlockersUnresolved reports that the producer of this Issue did
	// not resolve BlockedBy, so the list is not authoritative and the
	// dispatch gate holds the issue. False, the zero value, means
	// BlockedBy is authoritative.
	BlockersUnresolved bool

	// CreatedAt is an ISO-8601 timestamp. Empty string when absent.
	CreatedAt string

	// UpdatedAt is an ISO-8601 timestamp. Empty string when absent.
	UpdatedAt string
}

// BlockerRef identifies an issue that blocks the parent issue.
// Adapters populate values from inverse "blocks" relations in the tracker.
type BlockerRef struct {
	// ID is the tracker-internal ID. Empty string when unavailable.
	ID string

	// Identifier is the human-readable key. Empty string when
	// unavailable.
	Identifier string

	// State is the current state of the blocking issue. Empty string
	// when unknown, which is treated as non-terminal (conservative).
	State string

	// DisplayID is the qualified form of Identifier, set by the same
	// rule the adapter applies to an issue's own DisplayID. Empty
	// means Identifier is already display-ready.
	DisplayID string
}

// ParentRef identifies the parent issue for a sub-task.
type ParentRef struct {
	// ID is the tracker-internal ID of the parent.
	ID string

	// Identifier is the human-readable key of the parent.
	Identifier string
}

// CommentAuthorKind is the kind of account that wrote a comment.
type CommentAuthorKind string

const (
	// CommentAuthorUnknown means the read cannot tell the kind of account.
	CommentAuthorUnknown CommentAuthorKind = ""

	// CommentAuthorPerson is a person's account, and the tracker reports
	// no app acting for it.
	CommentAuthorPerson CommentAuthorKind = "person"

	// CommentAuthorBot is an automation account, which the tracker marks
	// as a bot or as an app's own account.
	CommentAuthorBot CommentAuthorKind = "bot"

	// CommentAuthorApp is a person's account, and the tracker reports that
	// an app created the comment for it.
	CommentAuthorApp CommentAuthorKind = "app"

	// CommentAuthorExternal is a person the tracker marks as outside its
	// own users, such as a service-desk customer or someone without an
	// account whose comment an integration relayed. The forges never
	// report it.
	CommentAuthorExternal CommentAuthorKind = "external"
)

// Comment represents a single comment on an issue. Adapters normalize
// native comment structures into this type.
type Comment struct {
	// ID is the tracker-internal comment identifier.
	ID string

	// Author is the display name or username of the comment author.
	Author string

	// AuthorID is the author's account identity as the tracker reports it,
	// a login or an account ID and never a display name, compared exactly.
	// A tracker with no account system reports its one author string
	// instead. It is empty when the tracker reports no author.
	AuthorID string

	// AuthorKind is the kind of the author's account;
	// [CommentAuthorUnknown] when the read cannot tell.
	AuthorKind CommentAuthorKind

	// Body is the comment text content.
	Body string

	// CreatedAt is the tracker's timestamp text, passed through unchanged.
	// It is RFC 3339 with fractional seconds optional and a "Z" or "±hh:mm"
	// offset, or the same with a "±hhmm" offset. [Comment.CreatedTime]
	// parses exactly these forms.
	CreatedAt string

	// UpdatedAt is the tracker's last-update timestamp text, passed through
	// unchanged, in the forms CreatedAt accepts; it is empty when the
	// tracker reports none. It narrows a read and never decides whether a
	// comment is new. [Comment.UpdatedTime] parses it.
	UpdatedAt string
}

// commentTimeLayouts are the forms [Comment.CreatedTime] and
// [Comment.UpdatedTime] accept. Parsing with a fractional-seconds-free
// layout also accepts fractional input.
var commentTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
}

// CreatedTime reports the instant CreatedAt names. It parses RFC 3339
// (fractional seconds optional) and the same form with a ±hhmm offset, and
// returns an error for anything else, the empty string included.
func (c Comment) CreatedTime() (time.Time, error) {
	return parseCommentTime(c.CreatedAt)
}

// UpdatedTime reports the instant UpdatedAt names, in the forms
// [Comment.CreatedTime] accepts, and returns an error for anything else,
// the empty string included.
func (c Comment) UpdatedTime() (time.Time, error) {
	return parseCommentTime(c.UpdatedAt)
}

func parseCommentTime(text string) (time.Time, error) {
	var firstErr error
	for _, layout := range commentTimeLayouts {
		t, err := time.Parse(layout, text)
		if err == nil {
			return t, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return time.Time{}, firstErr
}

// CommentTemplateMaps converts comments to template form, one map per
// comment with keys id, author, body, created_at. A nil input yields nil;
// any other input yields a non-nil slice of equal length.
func CommentTemplateMaps(comments []Comment) []map[string]any {
	if comments == nil {
		return nil
	}
	maps := make([]map[string]any, len(comments))
	for i, c := range comments {
		maps[i] = map[string]any{
			"id":         c.ID,
			"author":     c.Author,
			"body":       c.Body,
			"created_at": c.CreatedAt,
		}
	}
	return maps
}

// ToTemplateMap converts the Issue to a map[string]any with snake_case
// keys suitable for prompt template rendering as the "issue" variable.
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
		comments = CommentTemplateMaps(iss.Comments)
	}

	var blockedBy any
	if !iss.BlockersUnresolved {
		bb := make([]map[string]any, len(iss.BlockedBy))
		for i, b := range iss.BlockedBy {
			bb[i] = map[string]any{
				"id":         b.ID,
				"identifier": b.Identifier,
				"state":      b.State,
				"display_id": b.DisplayID,
			}
		}
		blockedBy = bb
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
