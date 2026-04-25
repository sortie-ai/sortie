// Package jira implements [domain.TrackerAdapter] for Atlassian Jira
// Cloud REST API v3. Issues are fetched via JQL search, normalized to
// domain types with ADF descriptions flattened to plain text, labels
// lowercased, integer-only priority (non-integers become nil), and
// blocker refs extracted from inward "Blocks" issuelinks. Registered
// under kind "jira" via an init function.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("jira", NewJiraAdapter, registry.TrackerMeta{
		RequiresProject: true,
		RequiresAPIKey:  true,
	})
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*JiraAdapter)(nil)

// searchFields is the comma-separated list of Jira fields requested
// in search and issue-detail operations.
const searchFields = "summary,status,priority,labels,assignee,issuetype,parent,issuelinks,created,updated,description"

// defaultActiveStates is applied when the config omits active_states.
var defaultActiveStates = []string{"Backlog", "Selected for Development", "In Progress"}

// maxSearchResults is the page size for cursor-based search pagination.
const maxSearchResults = "50"

// maxCommentResults is the page size for offset-based comment pagination.
const maxCommentResults = 50

// batchSize is the maximum number of issue keys per JQL IN clause
// to keep GET URLs within safe URI length limits.
const batchSize = 40

// JiraAdapter implements [domain.TrackerAdapter] against Jira Cloud
// REST API v3. Safe for concurrent use.
type JiraAdapter struct {
	client       *jiraClient
	project      string
	activeStates []string
	endpoint     string
	queryFilter  string
	metrics      domain.Metrics // nil-safe: check before calling
}

// NewJiraAdapter creates a [JiraAdapter] from adapter configuration.
// Required config keys: "endpoint", "api_key" (email:token format),
// "project". Optional: "active_states" (defaults to Backlog, Selected
// for Development, In Progress), "query_filter" (raw JQL fragment).
func NewJiraAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	endpoint, _ := config["endpoint"].(string)
	if endpoint == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "missing required config key: endpoint",
		}
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.Contains(endpoint, "/rest/api/") {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: `endpoint must be the Jira base URL without "/rest/api/..." path`,
		}
	}

	apiKey, _ := config["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "missing required config key: api_key",
		}
	}

	idx := strings.Index(apiKey, ":")
	if idx < 1 || idx == len(apiKey)-1 {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: "api_key must be in email:token format",
		}
	}
	email := apiKey[:idx]
	token := apiKey[idx+1:]

	project, _ := config["project"].(string)
	if project == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerProject,
			Message: "missing required config key: project",
		}
	}

	activeStates := typeutil.ExtractStringSlice(config["active_states"])
	if len(activeStates) == 0 {
		activeStates = defaultActiveStates
	}

	queryFilter, _ := config["query_filter"].(string)

	userAgent, _ := config["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	return &JiraAdapter{
		client:       newJiraClient(endpoint, email, token, userAgent),
		project:      project,
		activeStates: activeStates,
		endpoint:     endpoint,
		queryFilter:  queryFilter,
	}, nil
}

// FetchCandidateIssues returns issues in configured active states
// for the configured project. Comments are set to nil on all returned
// issues. Results are ordered by priority then creation time.
func (a *JiraAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	jql := buildCandidateJQL(a.project, a.activeStates, a.queryFilter)
	issues, err := a.paginatedSearch(ctx, jql, searchFields)
	if err != nil {
		a.incTrackerRequest("fetch_candidates", "error")
		return nil, err
	}
	a.incTrackerRequest("fetch_candidates", "success")
	return issues, nil
}

// FetchIssueByID returns a fully populated issue including comments.
// The issueID is the Jira issue key (e.g. "PROJ-123").
func (a *JiraAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	params := url.Values{"fields": {searchFields}}
	body, err := a.client.do(ctx, "GET", "/rest/api/3/issue/"+url.PathEscape(issueID), params)
	if err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		if domain.IsNotFound(err) {
			return domain.Issue{}, &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueID),
			}
		}
		return domain.Issue{}, err
	}

	var ji jiraIssue
	if err := json.Unmarshal(body, &ji); err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		return domain.Issue{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse issue response",
			Err:     err,
		}
	}

	issue := normalizeSearchIssue(a.endpoint, ji)

	comments, err := a.fetchComments(ctx, issueID)
	if err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		return domain.Issue{}, err
	}
	issue.Comments = comments

	a.incTrackerRequest("fetch_issue", "success")
	return issue, nil
}

// FetchIssuesByStates returns issues in the specified states. Used
// for startup terminal cleanup. Returns immediately when states is
// empty.
func (a *JiraAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}
	jql := buildStatesFetchJQL(a.project, states, a.queryFilter)
	issues, err := a.paginatedSearch(ctx, jql, searchFields)
	if err != nil {
		a.incTrackerRequest("fetch_by_states", "error")
		return nil, err
	}
	a.incTrackerRequest("fetch_by_states", "success")
	return issues, nil
}

// FetchIssueStatesByIDs returns the current state for each requested
// issue ID (Jira internal numeric ID). Issues not found are omitted
// from the result map. Batches IDs into groups of 40 to stay within
// URI length limits. The queryFilter is intentionally not applied.
func (a *JiraAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}

	statesByID := make(map[string]string, len(issueIDs))

	var requested bool

	for start := 0; start < len(issueIDs); start += batchSize {
		if ctx.Err() != nil {
			a.incTrackerRequest("fetch_states_by_ids", "error")
			return nil, ctx.Err()
		}

		end := min(start+batchSize, len(issueIDs))
		batch := issueIDs[start:end]

		jql := buildIDINJQL(batch)
		if jql == "" {
			continue
		}
		requested = true
		issues, err := a.paginatedSearch(ctx, jql, "status")
		if err != nil {
			a.incTrackerRequest("fetch_states_by_ids", "error")
			return nil, err
		}
		for _, iss := range issues {
			statesByID[iss.ID] = iss.State
		}
	}

	if requested {
		a.incTrackerRequest("fetch_states_by_ids", "success")
	}
	return statesByID, nil
}

// FetchIssueStatesByIdentifiers returns the current state for each
// requested issue identifier (key). Batches identifiers into groups
// of 40 to stay within URI length limits. Issues not found are
// omitted from the result map. The queryFilter is intentionally not
// applied.
func (a *JiraAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	if len(identifiers) == 0 {
		return map[string]string{}, nil
	}

	statesByKey := make(map[string]string, len(identifiers))

	for start := 0; start < len(identifiers); start += batchSize {
		if ctx.Err() != nil {
			a.incTrackerRequest("fetch_states_by_identifiers", "error")
			return nil, ctx.Err()
		}

		end := min(start+batchSize, len(identifiers))
		batch := identifiers[start:end]

		jql := buildKeyINJQL(batch)
		issues, err := a.paginatedSearch(ctx, jql, "status")
		if err != nil {
			a.incTrackerRequest("fetch_states_by_identifiers", "error")
			return nil, err
		}
		for _, iss := range issues {
			statesByKey[iss.Identifier] = iss.State
		}
	}

	a.incTrackerRequest("fetch_states_by_identifiers", "success")
	return statesByKey, nil
}

// FetchIssueComments returns comments for the specified issue.
// Returns an empty non-nil slice when no comments exist.
func (a *JiraAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	comments, err := a.fetchComments(ctx, issueID)
	if err != nil {
		a.incTrackerRequest("fetch_comments", "error")
		return nil, err
	}
	a.incTrackerRequest("fetch_comments", "success")
	return comments, nil
}

// TransitionIssue moves an issue to the specified target state by
// finding and executing the matching Jira workflow transition.
// Available transitions are fetched via GET, matched by target status
// name (case-insensitive, first match), then executed via POST.
func (a *JiraAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	path := "/rest/api/3/issue/" + url.PathEscape(issueID) + "/transitions"

	body, err := a.client.do(ctx, "GET", path, nil)
	if err != nil {
		a.incTrackerRequest("transition", "error")
		return err
	}

	var resp transitionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		a.incTrackerRequest("transition", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("failed to parse transitions response for issue %s", issueID),
			Err:     err,
		}
	}

	var matchID string
	for _, t := range resp.Transitions {
		if strings.EqualFold(t.To.Name, targetState) {
			matchID = t.ID
			break
		}
	}

	if matchID == "" {
		a.incTrackerRequest("transition", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("no transition to state %q available for issue %s", targetState, issueID),
		}
	}

	postBody, err := json.Marshal(struct {
		Transition struct {
			ID string `json:"id"`
		} `json:"transition"`
	}{
		Transition: struct {
			ID string `json:"id"`
		}{ID: matchID},
	})
	if err != nil {
		a.incTrackerRequest("transition", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal transition request",
			Err:     err,
		}
	}

	_, err = a.client.doJSON(ctx, "POST", path, bytes.NewReader(postBody))
	if err != nil {
		a.incTrackerRequest("transition", "error")
		return err
	}
	a.incTrackerRequest("transition", "success")
	return nil
}

// CommentIssue posts a plain-text comment on the specified Jira issue.
// The text is split by newlines into ADF paragraph nodes before
// submission to the Jira v3 REST API.
func (a *JiraAdapter) CommentIssue(ctx context.Context, issueID string, text string) error {
	path := "/rest/api/3/issue/" + url.PathEscape(issueID) + "/comment"

	body := buildADFComment(text)
	payload, err := json.Marshal(body)
	if err != nil {
		a.incTrackerRequest("comment", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal comment request",
			Err:     err,
		}
	}

	_, err = a.client.doJSON(ctx, "POST", path, bytes.NewReader(payload))
	if err != nil {
		a.incTrackerRequest("comment", "error")
		return err
	}
	a.incTrackerRequest("comment", "success")
	return nil
}

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter
// operates without recording metrics. Safe to call before any
// adapter operations. Not safe to call concurrently with adapter
// operations.
func (a *JiraAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// AddLabel adds a label to the specified issue via the Jira REST API.
// Returns an error if the request fails; the orchestrator treats AddLabel
// errors as non-fatal.
func (a *JiraAdapter) AddLabel(ctx context.Context, issueID string, label string) error {
	path := "/rest/api/3/issue/" + url.PathEscape(issueID)

	payload, err := json.Marshal(map[string]any{
		"update": map[string]any{
			"labels": []map[string]any{
				{"add": label},
			},
		},
	})
	if err != nil {
		a.incTrackerRequest("add_label", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal label payload",
			Err:     err,
		}
	}

	_, err = a.client.doJSON(ctx, "PUT", path, bytes.NewReader(payload))
	if err != nil {
		a.incTrackerRequest("add_label", "error")
		return err
	}
	a.incTrackerRequest("add_label", "success")
	return nil
}

func (a *JiraAdapter) incTrackerRequest(operation, result string) {
	if a.metrics != nil {
		a.metrics.IncTrackerRequests(operation, result)
	}
}

// paginatedSearch executes a cursor-based paginated JQL search and
// returns all normalized issues. Comments are set to nil.
func (a *JiraAdapter) paginatedSearch(ctx context.Context, jql, fields string) ([]domain.Issue, error) {
	var issues []domain.Issue
	var nextPageToken string

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		params := url.Values{
			"jql":        {jql},
			"fields":     {fields},
			"maxResults": {maxSearchResults},
		}
		if nextPageToken != "" {
			params.Set("nextPageToken", nextPageToken)
		}

		body, err := a.client.do(ctx, "GET", "/rest/api/3/search/jql", params)
		if err != nil {
			return nil, err
		}

		var sr searchResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse search response",
				Err:     err,
			}
		}

		for _, ji := range sr.Issues {
			issue := normalizeSearchIssue(a.endpoint, ji)
			issue.Comments = nil
			issues = append(issues, issue)
		}

		if sr.NextPageToken == "" {
			break
		}
		nextPageToken = sr.NextPageToken
	}

	if issues == nil {
		issues = []domain.Issue{}
	}
	return issues, nil
}

// fetchComments retrieves all comments for an issue using offset-based
// pagination. Returns an empty non-nil slice when no comments exist.
func (a *JiraAdapter) fetchComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	var allComments []jiraComment
	startAt := 0

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		params := url.Values{
			"orderBy":    {"created"},
			"maxResults": {fmt.Sprintf("%d", maxCommentResults)},
			"startAt":    {fmt.Sprintf("%d", startAt)},
		}

		body, err := a.client.do(ctx, "GET", "/rest/api/3/issue/"+url.PathEscape(issueID)+"/comment", params)
		if err != nil {
			if domain.IsNotFound(err) {
				return nil, &domain.TrackerError{
					Kind:    domain.ErrTrackerNotFound,
					Message: fmt.Sprintf("issue not found: %s", issueID),
				}
			}
			return nil, err
		}

		var cr commentResponse
		if err := json.Unmarshal(body, &cr); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse comment response",
				Err:     err,
			}
		}

		allComments = append(allComments, cr.Comments...)

		if len(cr.Comments) == 0 || startAt+len(cr.Comments) >= cr.Total {
			break
		}
		startAt += len(cr.Comments)
	}

	if len(allComments) == 0 {
		return []domain.Comment{}, nil
	}
	return normalizeComments(allComments), nil
}
