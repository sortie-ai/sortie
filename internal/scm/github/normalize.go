package github

import (
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
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

// normalizeIssue maps a GitHub API issue response to a [domain.Issue].
// The ID and Identifier are both set to the issue number since the
// GitHub REST API indexes issues by number, not by global integer ID.
// Parent and Comments remain at their zero values; BlockedBy is
// initialized to a non-nil empty slice. Callers requiring full
// population must use [GitHubAdapter.FetchIssueByID].
//
// DisplayID is left empty; callers that know the repository
// owner and name should set it to "owner/repo#N" after normalization.
func normalizeIssue(gi githubIssue, activeStates, terminalStates []string, handoffState string) domain.Issue {
	num := strconv.Itoa(gi.Number)

	desc := ""
	if gi.Body != nil {
		desc = *gi.Body
	}

	labels := make([]string, 0, len(gi.Labels))
	for _, l := range gi.Labels {
		labels = append(labels, strings.ToLower(l.Name))
	}

	assignee := ""
	if len(gi.Assignees) > 0 {
		assignee = gi.Assignees[0].Login
	}

	issueType := ""
	if gi.Type != nil {
		issueType = gi.Type.Name
	}

	return domain.Issue{
		ID:          num,
		Identifier:  num,
		Title:       gi.Title,
		Description: desc,
		State:       extractState(gi.Labels, gi.State, activeStates, terminalStates, handoffState),
		URL:         gi.HTMLURL,
		Labels:      labels,
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
func normalizeBlockers(blockers []githubIssue, activeStates, terminalStates []string, handoffState string) []domain.BlockerRef {
	refs := make([]domain.BlockerRef, 0, len(blockers))
	for _, b := range blockers {
		num := strconv.Itoa(b.Number)
		refs = append(refs, domain.BlockerRef{
			ID:         num,
			Identifier: num,
			State:      extractState(b.Labels, b.State, activeStates, terminalStates, handoffState),
		})
	}
	return refs
}

// normalizeComments converts GitHub comment responses to
// [domain.Comment] values. Returns a non-nil empty slice when input
// is empty.
func normalizeComments(comments []githubComment) []domain.Comment {
	normalized := make([]domain.Comment, 0, len(comments))
	for _, c := range comments {
		normalized = append(normalized, domain.Comment{
			ID:        strconv.FormatInt(c.ID, 10),
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return normalized
}

// isPullRequest returns true when the GitHub API entry represents a
// pull request rather than an issue. The issues list endpoint
// co-mingles both; this filter removes PRs.
func isPullRequest(gi githubIssue) bool {
	return gi.PullRequest != nil
}
