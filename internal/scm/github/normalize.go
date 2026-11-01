package github

import (
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// githubIssue represents a single issue from the GitHub REST API.
type githubIssue struct {
	ID                  int64                      `json:"id"`
	Number              int                        `json:"number"`
	Title               string                     `json:"title"`
	Body                *string                    `json:"body"`
	State               string                     `json:"state"`
	StateReason         *string                    `json:"state_reason"`
	HTMLURL             string                     `json:"html_url"`
	Labels              []githubLabel              `json:"labels"`
	Assignees           []githubUser               `json:"assignees"`
	Type                *githubIssueType           `json:"type"`
	PullRequest         *githubPR                  `json:"pull_request"`
	DependenciesSummary *githubDependenciesSummary `json:"issue_dependencies_summary"`
	CreatedAt           string                     `json:"created_at"`
	UpdatedAt           string                     `json:"updated_at"`
}

// githubDependenciesSummary is GitHub's per-issue dependency count.
// TotalBlockedBy counts every dependency; BlockedBy counts only those
// GitHub considers open, which is not the question the dispatch gate
// asks. A nil pointer means the field is absent or null, both of
// which mean "unknown" (a pull request co-mingled into the issues
// route, or a payload that omits the field).
type githubDependenciesSummary struct {
	BlockedBy      int `json:"blocked_by"`
	TotalBlockedBy int `json:"total_blocked_by"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubIssueType struct {
	Name string `json:"name"`
}

// githubPR is a marker struct: its mere presence (non-nil pointer)
// indicates the list entry is a pull request, not an issue.
type githubPR struct{}

// githubApp is a marker struct: a non-nil pointer means the comment was
// created through a GitHub App.
type githubApp struct{}

type githubComment struct {
	ID                    int64      `json:"id"`
	User                  githubUser `json:"user"`
	Body                  string     `json:"body"`
	CreatedAt             string     `json:"created_at"`
	UpdatedAt             string     `json:"updated_at"`
	PerformedViaGitHubApp *githubApp `json:"performed_via_github_app"`
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
// initialized to a non-nil empty slice.
//
// BlockersUnresolved is set true by default, because the candidate
// routes do not carry blockers; it is cleared, leaving BlockedBy
// empty, only when DependenciesSummary proves the issue has no
// dependencies at all. Otherwise a caller resolves blockers through
// the shared resolver or [GitHubAdapter.FetchIssueByID].
//
// DisplayID is left empty; callers that know the repository
// owner and name should set it to "owner/repo#N" after normalization.
func normalizeIssue(gi githubIssue, model *issuekit.LabelStateModel, states issuekit.LabelStates) domain.Issue {
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

	// Only a summary proving the issue has no dependencies at all lets a
	// candidate claim an authoritative blocker list. TotalBlockedBy counts
	// every dependency; BlockedBy counts only the ones the forge considers
	// open, which is a different question from whether a blocker sits
	// outside the configured terminal states, so it cannot stand in here.
	blockersUnresolved := gi.DependenciesSummary == nil || gi.DependenciesSummary.TotalBlockedBy != 0

	return domain.Issue{
		ID:                 num,
		Identifier:         num,
		Title:              gi.Title,
		Description:        desc,
		State:              model.Derive(states, num, labelNames, isClosed(gi)),
		URL:                gi.HTMLURL,
		Labels:             issuekit.NormalizeLabels(labelNames),
		Assignee:           assignee,
		IssueType:          issueType,
		BlockedBy:          []domain.BlockerRef{},
		BlockersUnresolved: blockersUnresolved,
		CreatedAt:          gi.CreatedAt,
		UpdatedAt:          gi.UpdatedAt,
	}
}

// qualifyIdentifier renders owner/repo#identifier, the qualified form
// GitHub issue and blocker references share.
func qualifyIdentifier(owner, repo, identifier string) string {
	return owner + "/" + repo + "#" + identifier
}

// qualifyDisplayID sets DisplayID to "owner/repo#N" so
// dashboard and API consumers see a fully qualified reference.
// It is idempotent: an already-qualified DisplayID is not overwritten.
func (a *GitHubAdapter) qualifyDisplayID(issue *domain.Issue) {
	if issue.DisplayID != "" {
		return
	}
	issue.DisplayID = qualifyIdentifier(a.owner, a.repo, issue.Identifier)
}

// normalizeBlockers converts blocker issue responses to
// [domain.BlockerRef] values. Returns a non-nil empty slice when
// input is empty. owner and repo qualify each ref's DisplayID the
// same way an issue's own DisplayID is qualified.
func normalizeBlockers(blockers []githubIssue, model *issuekit.LabelStateModel, states issuekit.LabelStates, owner, repo string) []domain.BlockerRef {
	refs := make([]domain.BlockerRef, 0, len(blockers))
	for _, b := range blockers {
		num := strconv.Itoa(b.Number)
		ref := domain.BlockerRef{
			ID:         num,
			Identifier: num,
			State:      model.Derive(states, num, githubLabelNames(b.Labels), isClosed(b)),
		}
		if ref.DisplayID == "" {
			ref.DisplayID = qualifyIdentifier(owner, repo, num)
		}
		refs = append(refs, ref)
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
			ID:         strconv.FormatInt(c.ID, 10),
			Author:     c.User.Login,
			AuthorID:   c.User.Login,
			AuthorKind: commentAuthorKind(c),
			Body:       c.Body,
			CreatedAt:  c.CreatedAt,
			UpdatedAt:  c.UpdatedAt,
		}
	}
	return issuekit.NormalizeComments(source)
}

// commentAuthorKind maps the comment's user type and creating app to a
// [domain.CommentAuthorKind]. An app's own [bot] account is a bot, so the
// user type decides before the app does.
func commentAuthorKind(c githubComment) domain.CommentAuthorKind {
	switch {
	case strings.EqualFold(c.User.Type, "Bot"):
		return domain.CommentAuthorBot
	case !strings.EqualFold(c.User.Type, "User"):
		return domain.CommentAuthorUnknown
	case c.PerformedViaGitHubApp != nil:
		return domain.CommentAuthorApp
	default:
		return domain.CommentAuthorPerson
	}
}

// isClosed reports whether the issue's native status is closed; every other
// native value reads as open.
func isClosed(gi githubIssue) bool {
	return gi.State == "closed"
}

// isPullRequest returns true when the GitHub API entry represents a
// pull request rather than an issue. The issues list endpoint
// co-mingles both; this filter removes PRs.
func isPullRequest(gi githubIssue) bool {
	return gi.PullRequest != nil
}
