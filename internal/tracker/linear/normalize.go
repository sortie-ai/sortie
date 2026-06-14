package linear

import (
	"cmp"
	"log/slog"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// graphQLResponse is the Linear GraphQL envelope. Data is decoded into a
// per-operation typed payload; a non-empty Errors slice drives classification.
type graphQLResponse[T any] struct {
	Data   T              `json:"data"`
	Errors []graphQLError `json:"errors"`
}

// graphQLError is one entry of the GraphQL errors array.
type graphQLError struct {
	Message    string                 `json:"message"`
	Path       []any                  `json:"path"`
	Extensions graphQLErrorExtensions `json:"extensions"`
}

// graphQLErrorExtensions carries Linear's structured error metadata. Type is
// the stable classification key; Code is diagnostic only.
type graphQLErrorExtensions struct {
	Type                   string `json:"type"`
	Code                   string `json:"code"`
	UserPresentableMessage string `json:"userPresentableMessage"`
	UserError              bool   `json:"userError"`
}

// linearPageInfo is the Relay-style page envelope reused across connections.
type linearPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// linearStateName is the workflow-state selection on an issue.
type linearStateName struct {
	Name string `json:"name"`
}

// linearUser is the nullable assignee or comment author selection.
type linearUser struct {
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
	Email       string `json:"email"`
}

// linearBotActor is the comment actor selection for bot or integration authors.
type linearBotActor struct {
	Name string `json:"name"`
}

// linearParent is the nullable parent-issue selection.
type linearParent struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// linearLabel is one label node.
type linearLabel struct {
	Name string `json:"name"`
}

// linearLabelConn is the nested labels connection. PageInfo carries
// hasNextPage so the normalizer can emit the nested-overflow tripwire.
type linearLabelConn struct {
	Nodes    []linearLabel  `json:"nodes"`
	PageInfo linearPageInfo `json:"pageInfo"`
}

// linearRelatedIssue is the issue referenced by an inverse relation node.
type linearRelatedIssue struct {
	ID         string          `json:"id"`
	Identifier string          `json:"identifier"`
	State      linearStateName `json:"state"`
}

// linearRelation is one inverseRelations node.
type linearRelation struct {
	Type  string             `json:"type"`
	Issue linearRelatedIssue `json:"issue"`
}

// linearRelationConn is the nested inverseRelations connection. PageInfo
// carries hasNextPage for the nested-overflow tripwire.
type linearRelationConn struct {
	Nodes    []linearRelation `json:"nodes"`
	PageInfo linearPageInfo   `json:"pageInfo"`
}

// linearComment is one comment node.
type linearComment struct {
	ID        string          `json:"id"`
	Body      string          `json:"body"`
	CreatedAt string          `json:"createdAt"`
	User      *linearUser     `json:"user"`
	BotActor  *linearBotActor `json:"botActor"`
}

// linearCommentConn is the comments connection, present only on the by-id
// query. A pointer distinguishes "not selected" from "selected and empty".
type linearCommentConn struct {
	Nodes    []linearComment `json:"nodes"`
	PageInfo linearPageInfo  `json:"pageInfo"`
}

// linearIssue is the issue node selected by the candidate and by-id queries.
type linearIssue struct {
	ID               string             `json:"id"`
	Identifier       string             `json:"identifier"`
	Title            string             `json:"title"`
	Description      *string            `json:"description"`
	Priority         *float64           `json:"priority"`
	BranchName       string             `json:"branchName"`
	URL              string             `json:"url"`
	CreatedAt        string             `json:"createdAt"`
	UpdatedAt        string             `json:"updatedAt"`
	State            linearStateName    `json:"state"`
	Assignee         *linearUser        `json:"assignee"`
	Parent           *linearParent      `json:"parent"`
	Labels           linearLabelConn    `json:"labels"`
	InverseRelations linearRelationConn `json:"inverseRelations"`
	Comments         *linearCommentConn `json:"comments"`
}

// linearIssueConn is the top-level issues connection.
type linearIssueConn struct {
	Nodes    []linearIssue  `json:"nodes"`
	PageInfo linearPageInfo `json:"pageInfo"`
}

// issuesData is the data payload for the candidate and by-states queries.
type issuesData struct {
	Issues linearIssueConn `json:"issues"`
}

// issueByIDData is the data payload for the by-id query.
type issueByIDData struct {
	Issue *linearIssue `json:"issue"`
}

// commentData is the data payload for the comment continuation query.
type commentData struct {
	Issue *struct {
		Comments linearCommentConn `json:"comments"`
	} `json:"issue"`
}

// stateBatchIssue is one node of the state-batch query results.
type stateBatchIssue struct {
	ID         string          `json:"id"`
	Identifier string          `json:"identifier"`
	Number     float64         `json:"number"`
	State      linearStateName `json:"state"`
}

// stateBatchData is the data payload for both state-batch queries.
type stateBatchData struct {
	Issues struct {
		Nodes []stateBatchIssue `json:"nodes"`
	} `json:"issues"`
}

// viewerData is the data payload for the credential-check query.
type viewerData struct {
	Viewer struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"viewer"`
}

// teamState is one workflow state in the team-states preflight result.
type teamState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

// teamStatesData is the data payload for the team-states preflight query.
type teamStatesData struct {
	Teams struct {
		Nodes []struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			States struct {
				Nodes []teamState `json:"nodes"`
			} `json:"states"`
		} `json:"nodes"`
	} `json:"teams"`
}

// stateResolveData is the data payload for the transition state-resolution
// query. A nil Issue distinguishes a missing issue from a present issue with no
// matching state.
type stateResolveData struct {
	Issue *struct {
		ID   string `json:"id"`
		Team struct {
			States struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	} `json:"issue"`
}

// teamResolveData is the data payload for the team-resolve query. A nil Issue
// distinguishes a missing issue from a present issue.
type teamResolveData struct {
	Issue *struct {
		ID   string `json:"id"`
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
	} `json:"issue"`
}

// issueUpdateData is the data payload for both issueUpdate mutations, the
// transition and the label attach. Only the success boolean drives correctness.
type issueUpdateData struct {
	IssueUpdate struct {
		Success bool `json:"success"`
	} `json:"issueUpdate"`
}

// commentCreateData is the data payload for the commentCreate mutation.
type commentCreateData struct {
	CommentCreate struct {
		Success bool `json:"success"`
		Comment *struct {
			ID string `json:"id"`
		} `json:"comment"`
	} `json:"commentCreate"`
}

// labelResolveData is the data payload for the label-resolve query. A node's
// Team is a pointer so a workspace-scoped label (team null) is distinguishable
// from a team-scoped label.
type labelResolveData struct {
	IssueLabels struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Team *struct {
				ID string `json:"id"`
			} `json:"team"`
		} `json:"nodes"`
	} `json:"issueLabels"`
}

// labelCreateData is the data payload for the issueLabelCreate mutation.
type labelCreateData struct {
	IssueLabelCreate struct {
		Success    bool `json:"success"`
		IssueLabel *struct {
			ID string `json:"id"`
		} `json:"issueLabel"`
	} `json:"issueLabelCreate"`
}

// normalizeIssue maps a [linearIssue] to a [domain.Issue].
//
// Labels are lowercased, priority 0 maps to nil while 1..4 map to a non-nil
// pointer, a null description maps to the empty string, and blockers are
// derived from inverse relations of type "blocks". Comments are left nil; the
// by-id caller populates them. When log is non-nil, a nested labels or
// inverseRelations connection truncated at its first-page cap emits a WARN.
func normalizeIssue(li linearIssue, log *slog.Logger) domain.Issue {
	labelNames := make([]string, 0, len(li.Labels.Nodes))
	for _, label := range li.Labels.Nodes {
		labelNames = append(labelNames, label.Name)
	}

	warnNestedOverflow(log, li.Identifier, "labels", li.Labels.PageInfo.HasNextPage)
	warnNestedOverflow(log, li.Identifier, "inverseRelations", li.InverseRelations.PageInfo.HasNextPage)

	var description string
	if li.Description != nil {
		description = *li.Description
	}

	var parent *domain.ParentRef
	if li.Parent != nil {
		parent = &domain.ParentRef{
			ID:         li.Parent.ID,
			Identifier: li.Parent.Identifier,
		}
	}

	return domain.Issue{
		ID:          li.ID,
		Identifier:  li.Identifier,
		Title:       li.Title,
		Description: description,
		Priority:    normalizePriority(li.Priority),
		State:       li.State.Name,
		BranchName:  li.BranchName,
		URL:         li.URL,
		Labels:      issuekit.NormalizeLabels(labelNames),
		Assignee:    resolveAssignee(li.Assignee),
		Parent:      parent,
		BlockedBy:   extractBlockers(li.InverseRelations),
		CreatedAt:   li.CreatedAt,
		UpdatedAt:   li.UpdatedAt,
	}
}

// normalizePriority maps a Linear priority float to the domain priority.
// A nil pointer or a 0 value maps to nil; 1..4 map to a non-nil pointer.
func normalizePriority(p *float64) *int {
	if p == nil {
		return nil
	}
	value := int(*p)
	if value == 0 {
		return nil
	}
	return &value
}

// resolveAssignee resolves the assignee identity using the displayName, name,
// then email fallback chain. A nil user maps to the empty string.
func resolveAssignee(user *linearUser) string {
	if user == nil {
		return ""
	}
	switch {
	case user.DisplayName != "":
		return user.DisplayName
	case user.Name != "":
		return user.Name
	default:
		return user.Email
	}
}

// extractBlockers derives blocker references from inverse relations whose type
// equals "blocks", compared case-insensitively after trimming. It always
// returns a non-nil slice.
func extractBlockers(rel linearRelationConn) []domain.BlockerRef {
	blockers := make([]domain.BlockerRef, 0, len(rel.Nodes))
	for _, node := range rel.Nodes {
		if !strings.EqualFold(strings.TrimSpace(node.Type), "blocks") {
			continue
		}
		blockers = append(blockers, domain.BlockerRef{
			ID:         node.Issue.ID,
			Identifier: node.Issue.Identifier,
			State:      node.Issue.State.Name,
		})
	}
	return blockers
}

// warnNestedOverflow emits a single WARN when a nested connection reports more
// pages at its first-page cap. The truncation is logged rather than treated as
// an error because the read path does not paginate nested connections, and a
// dropped label or blocker must remain observable.
func warnNestedOverflow(log *slog.Logger, identifier, connection string, hasNextPage bool) {
	if log == nil || !hasNextPage {
		return
	}
	log.Warn("nested connection truncated",
		slog.String("issue_identifier", identifier),
		slog.String("connection", connection))
}

// normalizeComments maps comment nodes to [domain.Comment] values, resolving
// the author via the user.displayName, user.name, then botActor.name chain.
// It returns a non-nil empty slice when there are no comments.
func normalizeComments(nodes []linearComment) []domain.Comment {
	staged := make([]issuekit.SourceComment, 0, len(nodes))
	for _, node := range nodes {
		staged = append(staged, issuekit.SourceComment{
			ID:        node.ID,
			Author:    resolveCommentAuthor(node),
			Body:      node.Body,
			CreatedAt: node.CreatedAt,
		})
	}
	return issuekit.NormalizeComments(staged)
}

// resolveCommentAuthor resolves the comment author using the user.displayName,
// user.name, then botActor.name fallback chain, else the empty string.
func resolveCommentAuthor(comment linearComment) string {
	if comment.User != nil {
		if comment.User.DisplayName != "" {
			return comment.User.DisplayName
		}
		if comment.User.Name != "" {
			return comment.User.Name
		}
	}
	if comment.BotActor != nil {
		return comment.BotActor.Name
	}
	return ""
}

// sortCommentsByCreatedAt sorts comments in ascending createdAt order in place.
// Linear returns comments newest-first; this restores chronological order.
func sortCommentsByCreatedAt(comments []domain.Comment) {
	slices.SortFunc(comments, func(a, b domain.Comment) int {
		return strings.Compare(a.CreatedAt, b.CreatedAt)
	})
}

// sortByPriorityThenCreated sorts issues in place by normalized priority
// ascending, then by creation time ascending. Issues with no priority sort
// after issues with a priority. This is the dispatch-order source of truth; the
// server sort hint is not trusted.
func sortByPriorityThenCreated(issues []domain.Issue) {
	slices.SortFunc(issues, func(a, b domain.Issue) int {
		if c := comparePriority(a.Priority, b.Priority); c != 0 {
			return c
		}
		return cmp.Compare(a.CreatedAt, b.CreatedAt)
	})
}

// comparePriority orders two normalized priorities ascending, treating a nil
// priority as lower precedence (sorted last).
func comparePriority(a, b *int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return cmp.Compare(*a, *b)
	}
}
