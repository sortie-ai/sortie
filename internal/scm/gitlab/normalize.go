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

// gitlabUser carries the username used for an issue assignee and a note
// author.
type gitlabUser struct {
	Username string `json:"username"`
}

// gitlabNote is one note from the issue notes route. System is the
// authoritative marker distinguishing a journal entry from a human
// comment.
type gitlabNote struct {
	ID        int64      `json:"id"`
	Author    gitlabUser `json:"author"`
	Body      string     `json:"body"`
	CreatedAt string     `json:"created_at"`
	System    bool       `json:"system"`
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
		State:       deriveState(gi.Labels, gi.State, activeStates, terminalStates, handoffState, iid, log),
		URL:         gi.WebURL,
		Labels:      issuekit.NormalizeLabels(gi.Labels),
		Assignee:    assignee,
		IssueType:   gi.IssueType,
		BlockedBy:   []domain.BlockerRef{},
		CreatedAt:   gi.CreatedAt,
		UpdatedAt:   gi.UpdatedAt,
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
