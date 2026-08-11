package gitlab

import (
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// gitlabIssue is one issue from the GitLab REST v4 issue routes. The read
// path decodes only the fields it consumes; unmodeled wire fields are
// ignored.
type gitlabIssue struct {
	IID         int              `json:"iid"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	State       string           `json:"state"`
	IssueType   string           `json:"issue_type"`
	WebURL      string           `json:"web_url"`
	Labels      []string         `json:"labels"`
	Assignees   []gitlabUser     `json:"assignees"`
	References  gitlabReferences `json:"references"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
}

// gitlabReferences carries the qualified display forms GitLab computes for
// an issue.
type gitlabReferences struct {
	Full string `json:"full"`
}

// gitlabUser carries the username and instance-global id used for an
// issue assignee, a note author, and a merge-request reviewer. The
// tracker read path uses only Username; the source-control bot lookup
// addresses users by ID.
type gitlabUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// gitlabNote is one note from the issue notes and merge-request notes
// routes. System is the authoritative marker distinguishing a journal
// entry from a human comment. Position is nil for a top-level note and
// non-nil for a diff note attached to a specific line.
type gitlabNote struct {
	ID        int64               `json:"id"`
	Author    gitlabUser          `json:"author"`
	Body      string              `json:"body"`
	CreatedAt string              `json:"created_at"`
	System    bool                `json:"system"`
	Position  *gitlabNotePosition `json:"position"`
}

// gitlabNotePosition is the diff anchor of a merge-request diff note. A
// nil OldLine or NewLine mirrors GitLab's own null rendering for a line
// added or removed relative to the other side of the diff.
type gitlabNotePosition struct {
	HeadSHA string `json:"head_sha"`
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
	OldLine *int   `json:"old_line"`
	NewLine *int   `json:"new_line"`
}

// gitlabNoteCreated is the note-creation response. ID is a pointer so
// that an absent id, which GitLab returns when the body was consumed
// entirely as quick actions, is distinguishable from id zero.
// CommandsChanges is non-empty when GitLab executed one or more quick
// actions found in the body.
type gitlabNoteCreated struct {
	ID              *int64         `json:"id"`
	CommandsChanges map[string]any `json:"commands_changes"`
}

// gitlabTokenInfo is the token-introspection record the construction
// preflight decodes. It never carries the token value itself.
type gitlabTokenInfo struct {
	Scopes    []string `json:"scopes"`
	Active    bool     `json:"active"`
	Revoked   bool     `json:"revoked"`
	ExpiresAt string   `json:"expires_at"`
}

// gitlabErrorBody covers the four error envelopes GitLab returns on a
// non-2xx response. Message is raw because GitLab may send a non-string
// value there.
type gitlabErrorBody struct {
	Message          json.RawMessage `json:"message"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

// normalizeIssue maps a gitlabIssue to a [domain.Issue]. ID and Identifier
// are both the decimal iid; the instance-global id is never read.
// DisplayID falls back to "<project>#<iid>" when the server-provided
// reference is empty. Priority, Parent, and BranchName are always nil,
// nil, and empty; BlockedBy is a non-nil empty slice.
//
// Comments is left nil; callers that fetch comments assign them
// afterwards.
func normalizeIssue(gi gitlabIssue, project string, activeStates, terminalStates []string, handoffState string, log *slog.Logger) domain.Issue {
	iid := strconv.Itoa(gi.IID)

	displayID := gi.References.Full
	if displayID == "" {
		displayID = project + "#" + iid
	}

	assignee := ""
	if len(gi.Assignees) > 0 {
		assignee = gi.Assignees[0].Username
	}

	return domain.Issue{
		ID:          iid,
		Identifier:  iid,
		DisplayID:   displayID,
		Title:       gi.Title,
		Description: gi.Description,
		State: issuekit.DeriveLabelState(gi.Labels, gi.State, "opened", "closed",
			issuekit.LabelStates{Active: activeStates, Terminal: terminalStates, Handoff: handoffState}, iid, log),
		URL:       gi.WebURL,
		Labels:    issuekit.NormalizeLabels(gi.Labels),
		Assignee:  assignee,
		IssueType: gi.IssueType,
		BlockedBy: []domain.BlockerRef{},
		CreatedAt: gi.CreatedAt,
		UpdatedAt: gi.UpdatedAt,
	}
}

// normalizeComments maps GitLab notes to [domain.Comment] values, dropping
// any note whose System flag is true. A note carrying internal: true
// passes through, because it is a genuine human comment rather than a
// journal entry. Returns a non-nil empty slice when raw is empty.
func normalizeComments(raw []gitlabNote) []domain.Comment {
	staged := make([]issuekit.SourceComment, 0, len(raw))
	for _, n := range raw {
		if n.System {
			continue
		}
		staged = append(staged, issuekit.SourceComment{
			ID:        strconv.FormatInt(n.ID, 10),
			Author:    n.Author.Username,
			Body:      n.Body,
			CreatedAt: n.CreatedAt,
		})
	}
	return issuekit.NormalizeComments(staged)
}
