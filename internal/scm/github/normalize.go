package github

import (
	"log/slog"
	"strconv"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// githubIssue represents a single issue from the GitHub REST API.
type githubIssue struct {
	ID          int64            `json:"id"`
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        *string          `json:"body"`
	State       string           `json:"state"`
	StateReason *string          `json:"state_reason"`
	HTMLURL     string           `json:"html_url"`
	Labels      []githubLabel    `json:"labels"`
	Assignees   []githubUser     `json:"assignees"`
	Type        *githubIssueType `json:"type"`
	PullRequest *githubPR        `json:"pull_request"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubUser struct {
	Login string `json:"login"`
}

type githubIssueType struct {
	Name string `json:"name"`
}

// githubPR is a marker struct — its mere presence (non-nil pointer)
// indicates the list entry is a pull request, not an issue.
type githubPR struct{}

type githubComment struct {
	ID        int64      `json:"id"`
	User      githubUser `json:"user"`
	Body      string     `json:"body"`
	CreatedAt string     `json:"created_at"`
}

// searchResponse represents GET /search/issues response.
type searchResponse struct {
	TotalCount        int           `json:"total_count"`
	IncompleteResults bool          `json:"incomplete_results"`
	Items             []githubIssue `json:"items"`
}

// githubLabelNames extracts the label names from a GitHub label list, in
// the order the API returned them.
func githubLabelNames(labels []githubLabel) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

// normalizeIssue maps a GitHub API issue response to a [domain.Issue].
// The ID and Identifier are both set to the issue number since the
// GitHub REST API indexes issues by number, not by global integer ID.
// Parent and Comments remain at their zero values; BlockedBy is
// initialized to a non-nil empty slice. Callers requiring full
// population must use [GitHubAdapter.FetchIssueByID].
//
// DisplayID is left empty; callers that know the repository
// owner and name should set it to "owner/repo#N" after normalization.
func normalizeIssue(gi githubIssue, activeStates, terminalStates []string, handoffState string, log *slog.Logger) domain.Issue {
	num := strconv.Itoa(gi.Number)
	labelNames := githubLabelNames(gi.Labels)

	desc := ""
	if gi.Body != nil {
		desc = *gi.Body
	}

	assignee := ""
	if len(gi.Assignees) > 0 {
		assignee = gi.Assignees[0].Login
	}

	issueType := ""
	if gi.Type != nil {
		issueType = gi.Type.Name
	}

	states := issuekit.LabelStates{Active: activeStates, Terminal: terminalStates, Handoff: handoffState}

	return domain.Issue{
		ID:          num,
		Identifier:  num,
		Title:       gi.Title,
		Description: desc,
		State:       issuekit.DeriveLabelState(labelNames, gi.State, "open", "closed", states, num, log),
		URL:         gi.HTMLURL,
		Labels:      issuekit.NormalizeLabels(labelNames),
		Assignee:    assignee,
		IssueType:   issueType,
		BlockedBy:   []domain.BlockerRef{},
		CreatedAt:   gi.CreatedAt,
		UpdatedAt:   gi.UpdatedAt,
	}
}

// qualifyDisplayID sets DisplayID to "owner/repo#N" so
// dashboard and API consumers see a fully qualified reference.
// It is idempotent: an already-qualified DisplayID is not overwritten.
func (a *GitHubAdapter) qualifyDisplayID(issue *domain.Issue) {
	if issue.DisplayID != "" {
		return
	}
	issue.DisplayID = a.owner + "/" + a.repo + "#" + issue.Identifier
}

// normalizeBlockers converts blocker issue responses to
// [domain.BlockerRef] values. Returns a non-nil empty slice when
// input is empty.
func normalizeBlockers(blockers []githubIssue, activeStates, terminalStates []string, handoffState string, log *slog.Logger) []domain.BlockerRef {
	states := issuekit.LabelStates{Active: activeStates, Terminal: terminalStates, Handoff: handoffState}

	refs := make([]domain.BlockerRef, 0, len(blockers))
	for _, b := range blockers {
		num := strconv.Itoa(b.Number)
		refs = append(refs, domain.BlockerRef{
			ID:         num,
			Identifier: num,
			State:      issuekit.DeriveLabelState(githubLabelNames(b.Labels), b.State, "open", "closed", states, num, log),
		})
	}
	return refs
}

// normalizeComments converts GitHub comment responses to
// [domain.Comment] values. Returns a non-nil empty slice when input
// is empty.
func normalizeComments(comments []githubComment) []domain.Comment {
	source := make([]issuekit.SourceComment, len(comments))
	for i, c := range comments {
		source[i] = issuekit.SourceComment{
			ID:        strconv.FormatInt(c.ID, 10),
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		}
	}
	return issuekit.NormalizeComments(source)
}

// isPullRequest returns true when the GitHub API entry represents a
// pull request rather than an issue. The issues list endpoint
// co-mingles both; this filter removes PRs.
func isPullRequest(gi githubIssue) bool {
	return gi.PullRequest != nil
}
