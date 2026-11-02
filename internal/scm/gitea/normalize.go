package gitea

import (
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// giteaIssue is one issue from the Gitea REST API issue routes. The read path
// decodes only the fields it consumes; unmodeled wire fields are ignored.
type giteaIssue struct {
	Number      int          `json:"number"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	State       string       `json:"state"`
	Ref         string       `json:"ref"`
	HTMLURL     string       `json:"html_url"`
	Labels      []giteaLabel `json:"labels"`
	Assignees   []giteaUser  `json:"assignees"`
	PullRequest *giteaPR     `json:"pull_request"`
	CreatedAt   string       `json:"created_at"`
	UpdatedAt   string       `json:"updated_at"`
}

// giteaLabel carries the label id and name. The read path consumes Name; the
// write path resolves Name to ID for id-based attach and remove.
type giteaLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// giteaUser carries the login used for assignee, comment author, and the
// credential-check identity. Type is empty on Forgejo and older Gitea.
type giteaUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// giteaComment is one comment from the issue comments route.
type giteaComment struct {
	ID        int64     `json:"id"`
	User      giteaUser `json:"user"`
	Body      string    `json:"body"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}

// giteaPR is a marker: a non-nil pointer identifies a pull request rather than
// an issue.
type giteaPR struct{}

// giteaErrorBody is the uniform Gitea API error envelope.
type giteaErrorBody struct {
	Message string `json:"message"`
}

// giteaLabelNames extracts the label names from a Gitea label list, in the
// order the API returned them.
func giteaLabelNames(labels []giteaLabel) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

// normalizeIssue maps a Gitea API issue response to a [domain.Issue]. ID and
// Identifier are both set to the repo-scoped index; the global id is never used.
// Parent and Comments remain nil; BlockedBy is a non-nil empty slice.
//
// BlockersUnresolved is always set true: the candidate route carries no
// dependency field, so every candidate this function produces needs a
// blocker read through the shared resolver or [GiteaAdapter.FetchIssueByID].
//
// DisplayID is left empty; callers apply [setDisplayID] after normalization.
// State is derived by model under states, with a closed native status read as
// closed.
func normalizeIssue(gi giteaIssue, model *issuekit.LabelStateModel, states issuekit.LabelStates) domain.Issue {
	num := strconv.Itoa(gi.Number)
	labelNames := giteaLabelNames(gi.Labels)

	assignee := ""
	if len(gi.Assignees) > 0 {
		assignee = gi.Assignees[0].Login
	}

	return domain.Issue{
		ID:                 num,
		Identifier:         num,
		Title:              gi.Title,
		Description:        gi.Body,
		State:              model.Derive(states, num, labelNames, gi.State == "closed"),
		BranchName:         gi.Ref,
		URL:                gi.HTMLURL,
		Labels:             issuekit.NormalizeLabels(labelNames),
		Assignee:           assignee,
		BlockedBy:          []domain.BlockerRef{},
		BlockersUnresolved: true,
		CreatedAt:          gi.CreatedAt,
		UpdatedAt:          gi.UpdatedAt,
	}
}

// setDisplayID sets issue.DisplayID to "owner/repo#N" so dashboard and API
// consumers see a fully qualified reference. It is idempotent: a DisplayID
// that is already set is not overwritten.
func setDisplayID(issue *domain.Issue, owner, repo string) {
	if issue.DisplayID != "" {
		return
	}
	issue.DisplayID = owner + "/" + repo + "#" + issue.Identifier
}

// normalizeComments converts Gitea comment responses to [domain.Comment]
// values. Returns a non-nil empty slice when input is empty.
func normalizeComments(raw []giteaComment) []domain.Comment {
	source := make([]issuekit.SourceComment, len(raw))
	for i, c := range raw {
		source[i] = issuekit.SourceComment{
			ID:         strconv.FormatInt(c.ID, 10),
			Author:     c.User.Login,
			AuthorID:   c.User.Login,
			AuthorKind: commentAuthorKind(c.User),
			Body:       c.Body,
			CreatedAt:  c.CreatedAt,
			UpdatedAt:  c.UpdatedAt,
		}
	}
	return issuekit.NormalizeComments(source)
}

// commentAuthorKind maps the user type to a kind. An absent or unrecognized
// type is unknown rather than person, because Forgejo and older Gitea send no
// type and would otherwise report their bots as people.
func commentAuthorKind(u giteaUser) domain.CommentAuthorKind {
	switch {
	case strings.EqualFold(u.Type, "Bot"):
		return domain.CommentAuthorBot
	case strings.EqualFold(u.Type, "User"):
		return domain.CommentAuthorPerson
	default:
		return domain.CommentAuthorUnknown
	}
}

// normalizeBlockers converts blocker issue responses to [domain.BlockerRef]
// values. Returns a non-nil empty slice when input is empty. Each blocker's
// state is derived by model under states from its labels and native state, and
// its DisplayID is qualified the same way [setDisplayID] qualifies an issue's
// own DisplayID.
func normalizeBlockers(blockers []giteaIssue, model *issuekit.LabelStateModel, states issuekit.LabelStates, owner, repo string) []domain.BlockerRef {
	refs := make([]domain.BlockerRef, 0, len(blockers))
	for _, b := range blockers {
		num := strconv.Itoa(b.Number)
		ref := domain.BlockerRef{
			ID:         num,
			Identifier: num,
			State:      model.Derive(states, num, giteaLabelNames(b.Labels), b.State == "closed"),
		}
		if ref.DisplayID == "" {
			ref.DisplayID = owner + "/" + repo + "#" + num
		}
		refs = append(refs, ref)
	}
	return refs
}

// isPullRequest reports whether the Gitea API entry represents a pull request
// rather than an issue.
func isPullRequest(gi giteaIssue) bool {
	return gi.PullRequest != nil
}
