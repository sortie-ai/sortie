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

// giteaLabel carries the label id and name. The read path consumes Name; the
// write path resolves Name to ID for id-based attach and remove.
type giteaLabel struct {
	ID   int64  `json:"id"`
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
// The state lists and log are threaded to [issuekit.DeriveLabelState].
func normalizeIssue(gi giteaIssue, activeStates, terminalStates []string, handoffState string, log *slog.Logger) domain.Issue {
	num := strconv.Itoa(gi.Number)
	labelNames := giteaLabelNames(gi.Labels)

	assignee := ""
	if len(gi.Assignees) > 0 {
		assignee = gi.Assignees[0].Login
	}

	states := issuekit.LabelStates{Active: activeStates, Terminal: terminalStates, Handoff: handoffState}

	return domain.Issue{
		ID:                 num,
		Identifier:         num,
		Title:              gi.Title,
		Description:        gi.Body,
		State:              issuekit.DeriveLabelState(labelNames, gi.State, "open", "closed", states, num, log),
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
// state is derived from its labels and native state, and its DisplayID is
// qualified the same way [setDisplayID] qualifies an issue's own DisplayID.
func normalizeBlockers(blockers []giteaIssue, activeStates, terminalStates []string, handoffState, owner, repo string, log *slog.Logger) []domain.BlockerRef {
	states := issuekit.LabelStates{Active: activeStates, Terminal: terminalStates, Handoff: handoffState}

	refs := make([]domain.BlockerRef, 0, len(blockers))
	for _, b := range blockers {
		num := strconv.Itoa(b.Number)
		ref := domain.BlockerRef{
			ID:         num,
			Identifier: num,
			State:      issuekit.DeriveLabelState(giteaLabelNames(b.Labels), b.State, "open", "closed", states, num, log),
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
