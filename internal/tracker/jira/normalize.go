package jira

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// blockerLinkTypeName is the Jira link type name used to identify
// blocking relationships. Links with this type and a non-nil
// inwardIssue produce a BlockerRef.
const blockerLinkTypeName = "Blocks"

// searchResponse represents GET /rest/api/3/search/jql response.
type searchResponse struct {
	Issues        []jiraIssue `json:"issues"`
	NextPageToken string      `json:"nextPageToken,omitempty"`
}

// jiraIssue represents a single issue in a search or issue-detail response.
type jiraIssue struct {
	ID     string     `json:"id"`
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
}

type jiraFields struct {
	Summary     string          `json:"summary"`
	Status      *jiraStatus     `json:"status"`
	Priority    *jiraPriority   `json:"priority"`
	Labels      []string        `json:"labels"`
	Assignee    *jiraUser       `json:"assignee"`
	IssueType   *jiraIssueType  `json:"issuetype"`
	Parent      *jiraParent     `json:"parent"`
	IssueLinks  []jiraIssueLink `json:"issuelinks"`
	Description json.RawMessage `json:"description"`
	Created     string          `json:"created"`
	Updated     string          `json:"updated"`
}

type jiraStatus struct {
	Name string `json:"name"`
}

type jiraPriority struct {
	ID string `json:"id"`
}

type jiraUser struct {
	DisplayName string `json:"displayName"`
}

type jiraIssueType struct {
	Name string `json:"name"`
}

type jiraParent struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type jiraIssueLink struct {
	Type         jiraLinkType     `json:"type"`
	InwardIssue  *jiraLinkedIssue `json:"inwardIssue,omitempty"`
	OutwardIssue *jiraLinkedIssue `json:"outwardIssue,omitempty"`
}

type jiraLinkType struct {
	Name   string `json:"name"`
	Inward string `json:"inward"`
}

type jiraLinkedIssue struct {
	ID     string                 `json:"id"`
	Key    string                 `json:"key"`
	Fields *jiraLinkedIssueFields `json:"fields,omitempty"`
}

type jiraLinkedIssueFields struct {
	Status *jiraStatus `json:"status,omitempty"`
}

// commentResponse represents GET /rest/api/3/issue/{id}/comment response.
type commentResponse struct {
	StartAt    int           `json:"startAt"`
	MaxResults int           `json:"maxResults"`
	Total      int           `json:"total"`
	Comments   []jiraComment `json:"comments"`
}

type jiraComment struct {
	ID      string          `json:"id"`
	Author  *jiraUser       `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
}

// normalizeSearchIssue maps a Jira API issue to a domain.Issue. The
// endpoint is used to construct the browse URL. Labels are lowercased,
// priority parsed as integer (nil on failure), and blocker refs
// extracted from issuelinks.
func normalizeSearchIssue(endpoint string, ji jiraIssue) domain.Issue {
	issue := domain.Issue{
		ID:          ji.ID,
		Identifier:  ji.Key,
		Title:       ji.Fields.Summary,
		Description: flattenADF(unmarshalADF(ji.Fields.Description)),
		URL:         endpoint + "/browse/" + ji.Key,
		CreatedAt:   ji.Fields.Created,
		UpdatedAt:   ji.Fields.Updated,
	}

	if ji.Fields.Status != nil {
		issue.State = ji.Fields.Status.Name
	}

	if ji.Fields.Priority != nil {
		if v, err := strconv.Atoi(ji.Fields.Priority.ID); err == nil {
			issue.Priority = &v
		}
	}

	if ji.Fields.Labels != nil {
		labels := make([]string, len(ji.Fields.Labels))
		for i, l := range ji.Fields.Labels {
			labels[i] = strings.ToLower(l)
		}
		issue.Labels = labels
	} else {
		issue.Labels = []string{}
	}

	if ji.Fields.Assignee != nil {
		issue.Assignee = ji.Fields.Assignee.DisplayName
	}

	if ji.Fields.IssueType != nil {
		issue.IssueType = ji.Fields.IssueType.Name
	}

	if ji.Fields.Parent != nil {
		issue.Parent = &domain.ParentRef{
			ID:         ji.Fields.Parent.ID,
			Identifier: ji.Fields.Parent.Key,
		}
	}

	issue.BlockedBy = extractBlockers(ji.Fields.IssueLinks)

	return issue
}

// extractBlockers filters issuelinks for inward "Blocks" relationships
// and returns the corresponding BlockerRef values.
func extractBlockers(links []jiraIssueLink) []domain.BlockerRef {
	blockers := []domain.BlockerRef{}
	for _, link := range links {
		if link.Type.Name != blockerLinkTypeName || link.InwardIssue == nil {
			continue
		}
		ref := domain.BlockerRef{
			ID:         link.InwardIssue.ID,
			Identifier: link.InwardIssue.Key,
		}
		if link.InwardIssue.Fields != nil && link.InwardIssue.Fields.Status != nil {
			ref.State = link.InwardIssue.Fields.Status.Name
		}
		blockers = append(blockers, ref)
	}
	return blockers
}

// normalizeComments maps Jira comment objects to domain.Comment
// values. ADF bodies are flattened to plain text. Nil author fields
// produce an empty author string.
func normalizeComments(comments []jiraComment) []domain.Comment {
	normalized := make([]domain.Comment, len(comments))
	for i, c := range comments {
		normalized[i] = domain.Comment{
			ID:        c.ID,
			Body:      flattenADF(unmarshalADF(c.Body)),
			CreatedAt: c.Created,
		}
		if c.Author != nil {
			normalized[i].Author = c.Author.DisplayName
		}
	}
	return normalized
}

// unmarshalADF decodes a json.RawMessage into an any value suitable
// for flattenADF. Returns nil on empty or invalid input.
func unmarshalADF(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// transitionsResponse represents GET /rest/api/3/issue/{id}/transitions.
type transitionsResponse struct {
	Transitions []jiraTransition `json:"transitions"`
}

// jiraTransition represents a single available workflow transition.
type jiraTransition struct {
	ID   string               `json:"id"`
	Name string               `json:"name"`
	To   jiraTransitionTarget `json:"to"`
}

// jiraTransitionTarget represents the destination status of a transition.
type jiraTransitionTarget struct {
	Name string `json:"name"`
}
