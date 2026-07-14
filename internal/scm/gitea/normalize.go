package gitea

import (
	"log/slog"
	"strconv"

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

// giteaLabel carries the label name the read path consumes.
type giteaLabel struct {
	Name string `json:"name"`
}

// giteaUser carries the login used for assignee, comment author, and the
// credential-check identity.
type giteaUser struct {
	Login string `json:"login"`
}

// giteaComment is one comment from the issue comments route.
type giteaComment struct {
	ID        int64     `json:"id"`
	User      giteaUser `json:"user"`
	Body      string    `json:"body"`
	CreatedAt string    `json:"created_at"`
}

// giteaPR is a marker: a non-nil pointer identifies a pull request rather than
// an issue.
type giteaPR struct{}

// giteaErrorBody is the uniform Gitea API error envelope.
type giteaErrorBody struct {
	Message string `json:"message"`
}

// normalizeIssue maps a Gitea API issue response to a [domain.Issue]. ID and
// Identifier are both set to the repo-scoped index; the global id is never used.
// Parent and Comments remain nil; BlockedBy is a non-nil empty slice.
//
// DisplayID is left empty; callers apply [setDisplayID] after normalization.
// The state lists and log are threaded to [deriveState].
func normalizeIssue(gi giteaIssue, activeStates, terminalStates []string, handoffState string, log *slog.Logger) domain.Issue {
	num := strconv.Itoa(gi.Number)

	labelNames := make([]string, 0, len(gi.Labels))
	for _, l := range gi.Labels {
		labelNames = append(labelNames, l.Name)
	}

	assignee := ""
	if len(gi.Assignees) > 0 {
		assignee = gi.Assignees[0].Login
	}

	return domain.Issue{
		ID:          num,
		Identifier:  num,
		Title:       gi.Title,
		Description: gi.Body,
		State:       deriveState(gi.Labels, gi.State, activeStates, terminalStates, handoffState, num, log),
		BranchName:  gi.Ref,
		URL:         gi.HTMLURL,
		Labels:      issuekit.NormalizeLabels(labelNames),
		Assignee:    assignee,
		BlockedBy:   []domain.BlockerRef{},
		CreatedAt:   gi.CreatedAt,
		UpdatedAt:   gi.UpdatedAt,
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
			ID:        strconv.FormatInt(c.ID, 10),
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		}
	}
	return issuekit.NormalizeComments(source)
}

// normalizeBlockers converts blocker issue responses to [domain.BlockerRef]
// values. Returns a non-nil empty slice when input is empty. Each blocker's
// state is derived from its labels and native state.
func normalizeBlockers(blockers []giteaIssue, activeStates, terminalStates []string, handoffState string, log *slog.Logger) []domain.BlockerRef {
	refs := make([]domain.BlockerRef, 0, len(blockers))
	for _, b := range blockers {
		num := strconv.Itoa(b.Number)
		refs = append(refs, domain.BlockerRef{
			ID:         num,
			Identifier: num,
			State:      deriveState(b.Labels, b.State, activeStates, terminalStates, handoffState, num, log),
		})
	}
	return refs
}

// isPullRequest reports whether the Gitea API entry represents a pull request
// rather than an issue.
func isPullRequest(gi giteaIssue) bool {
	return gi.PullRequest != nil
}
