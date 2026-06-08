// Package jira implements [domain.TrackerAdapter] for Atlassian Jira.
// It targets the Cloud REST API v3 by default and the Server / Data
// Center REST API v2 when api_version is "2". Issues are fetched via
// JQL search and normalized to domain types: labels lowercased,
// integer-only priority (non-integers become nil), and blocker refs
// extracted from inward "Blocks" issuelinks. Descriptions and comment
// bodies are flattened from ADF on v3 and preserved as raw wiki markup
// on v2. Registered under kind "jira" via an init function.
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
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

// JiraAdapter implements [domain.TrackerAdapter] against Atlassian
// Jira. The selected API version and the base path derived from it are
// immutable after construction. Safe for concurrent use.
type JiraAdapter struct {
	client       *httpkit.Client
	project      string
	activeStates []string
	endpoint     string
	queryFilter  string
	apiVersion   string
	basePath     string
	metrics      domain.Metrics // nil-safe: check before calling
}

// NewJiraAdapter creates a [JiraAdapter] from adapter configuration.
// Required config keys: "endpoint", "api_key", "project". Optional:
// "active_states" (defaults to Backlog, Selected for Development, In
// Progress), "query_filter" (raw JQL fragment), "api_version" ("2" or
// "3", default "3").
//
// On version "3" the api_key must be email:token and authenticates
// with Basic. On version "2" a colon-free api_key is a Personal Access
// Token and authenticates with Bearer, while a user:password api_key
// authenticates with Basic. An api_version other than "2" or "3", a
// malformed api_key, or a Cloud endpoint combined with api_version "2"
// returns a [*domain.TrackerError] and blocks construction.
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

	apiVersion, err := normalizeAPIVersion(config["api_version"])
	if err != nil {
		return nil, err
	}

	if err := checkHostVersion(endpoint, apiVersion); err != nil {
		return nil, err
	}

	apiKey, _ := config["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "missing required config key: api_key",
		}
	}

	authHeader, err := resolveAuth(apiVersion, apiKey)
	if err != nil {
		return nil, err
	}

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
		client:       newJiraClient(endpoint, authHeader, userAgent),
		project:      project,
		activeStates: activeStates,
		endpoint:     endpoint,
		queryFilter:  queryFilter,
		apiVersion:   apiVersion,
		basePath:     basePath(apiVersion),
	}, nil
}

// normalizeAPIVersion coerces a raw config value to a validated Jira
// REST API version. It accepts a string, an int, or a whole-number
// float64, trims surrounding whitespace, and resolves an absent or
// empty value to "3". A value other than "2" or "3" returns a
// [*domain.TrackerError] of kind [domain.ErrTrackerPayload].
func normalizeAPIVersion(raw any) (string, error) {
	var v string
	switch n := raw.(type) {
	case nil:
		v = ""
	case string:
		v = n
	case int:
		v = strconv.Itoa(n)
	case float64:
		if n != 2 && n != 3 {
			return "", &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf(`tracker.api_version must be "2" or "3", got %v`, raw),
			}
		}
		v = strconv.Itoa(int(n))
	default:
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf(`tracker.api_version must be "2" or "3", got %v`, raw),
		}
	}

	v = strings.TrimSpace(v)
	switch v {
	case "":
		return "3", nil
	case "2", "3":
		return v, nil
	default:
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf(`tracker.api_version must be "2" or "3", got %q`, v),
		}
	}
}

// basePath returns the Jira REST API base path for the given version.
func basePath(apiVersion string) string {
	return "/rest/api/" + apiVersion
}

// resolveAuth selects the Authorization header value from the API
// version and api_key. A key containing a colon is parsed as
// user:secret and produces a Basic header on both versions; an empty
// user or empty secret returns [domain.ErrTrackerAuth]. A colon-free
// key produces a Bearer header on version "2" and returns
// [domain.ErrTrackerAuth] on version "3". The api_key is never empty:
// the caller rejects an empty value earlier.
func resolveAuth(apiVersion, apiKey string) (string, error) {
	idx := strings.Index(apiKey, ":")
	if idx >= 0 {
		if idx < 1 || idx == len(apiKey)-1 {
			return "", authFormatError(apiVersion)
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(apiKey)), nil
	}

	if apiVersion == "2" {
		return "Bearer " + apiKey, nil
	}

	return "", authFormatError(apiVersion)
}

// authFormatError reports an invalid api_key shape for the given API
// version. Version 2 accepts a personal access token or user:password
// Basic credentials; version 3 requires email:token Basic credentials.
func authFormatError(apiVersion string) error {
	if apiVersion == "2" {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: "api_key for api_version 2 must be a personal access token or in user:password format",
		}
	}
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerAuth,
		Message: "api_key for api_version 3 must be in email:token format",
	}
}

// checkHostVersion runs a static consistency check between the endpoint
// host and the API version. It performs no network I/O. A Cloud host
// (one ending in .atlassian.net) combined with version "2" returns a
// [*domain.TrackerError] of kind [domain.ErrTrackerPayload]. A
// non-Cloud host combined with version "3" logs a warning and returns
// nil so construction proceeds, except for a loopback or localhost host,
// which is a test or local-dev endpoint and never a real Server / Data
// Center instance, so it is not warned. All other combinations return
// nil.
func checkHostVersion(endpoint, apiVersion string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	isCloud := strings.HasSuffix(host, ".atlassian.net")

	if isCloud && apiVersion == "2" {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("tracker.api_version 2 targets Jira Server / Data Center; endpoint %s is Jira Cloud, which requires api_version 3", host),
		}
	}

	if !isCloud && apiVersion == "3" && !isLocalEndpoint(host) {
		slog.Warn("api_version 3 against non-cloud endpoint will return 404; set tracker.api_version 2 for jira server or data center",
			slog.String("host", host))
	}

	return nil
}

// isLocalEndpoint reports whether host is a test or local-dev endpoint:
// the literal "localhost" or a loopback IP address. Private IPs and
// internal FQDNs are not local, since real Server / Data Center
// instances commonly live on those.
func isLocalEndpoint(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FetchCandidateIssues returns issues in configured active states
// for the configured project. Comments are set to nil on all returned
// issues. Results are ordered by priority then creation time.
func (a *JiraAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		var fetchErr error
		jql := buildCandidateJQL(a.project, a.activeStates, a.queryFilter)
		issues, fetchErr = a.paginatedSearch(ctx, jql, searchFields)
		return fetchErr
	})
	return issues, err
}

// FetchIssueByID returns a fully populated issue including comments.
// The issueID is the Jira issue key (e.g. "PROJ-123").
func (a *JiraAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		params := url.Values{"fields": {searchFields}}
		body, _, err := a.client.Get(ctx, a.basePath+"/issue/"+url.PathEscape(issueID), params)
		if err != nil {
			if domain.IsNotFound(err) {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerNotFound,
					Message: fmt.Sprintf("issue not found: %s", issueID),
				}
			}
			return err
		}

		var ji jiraIssue
		if err := json.Unmarshal(body, &ji); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issue response",
				Err:     err,
			}
		}

		fetchedIssue := normalizeSearchIssue(a.apiVersion, a.endpoint, ji)

		comments, err := a.fetchComments(ctx, issueID)
		if err != nil {
			return err
		}
		fetchedIssue.Comments = comments
		issue = fetchedIssue
		return nil
	})
	return issue, err
}

// FetchIssuesByStates returns issues in the specified states. Used
// for startup terminal cleanup. Returns immediately when states is
// empty.
func (a *JiraAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		var fetchErr error
		jql := buildStatesFetchJQL(a.project, states, a.queryFilter)
		issues, fetchErr = a.paginatedSearch(ctx, jql, searchFields)
		return fetchErr
	})
	return issues, err
}

// FetchIssueStatesByIDs returns the current state for each requested
// issue ID (Jira internal numeric ID). Issues not found are omitted
// from the result map. Batches IDs into groups of 40 to stay within
// URI length limits. The queryFilter is intentionally not applied.
func (a *JiraAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}

	var statesByID map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		fetchedStates := make(map[string]string, len(issueIDs))
		for start := 0; start < len(issueIDs); start += batchSize {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			end := min(start+batchSize, len(issueIDs))
			batch := issueIDs[start:end]

			jql := buildIDINJQL(batch)
			if jql == "" {
				continue
			}

			issues, err := a.paginatedSearch(ctx, jql, "status")
			if err != nil {
				return err
			}
			for _, iss := range issues {
				fetchedStates[iss.ID] = iss.State
			}
		}
		statesByID = fetchedStates
		return nil
	})
	return statesByID, err
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

	var statesByKey map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		fetchedStates := make(map[string]string, len(identifiers))
		for start := 0; start < len(identifiers); start += batchSize {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			end := min(start+batchSize, len(identifiers))
			batch := identifiers[start:end]

			jql := buildKeyINJQL(batch)
			issues, err := a.paginatedSearch(ctx, jql, "status")
			if err != nil {
				return err
			}
			for _, iss := range issues {
				fetchedStates[iss.Identifier] = iss.State
			}
		}
		statesByKey = fetchedStates
		return nil
	})
	return statesByKey, err
}

// FetchIssueComments returns comments for the specified issue.
// Returns an empty non-nil slice when no comments exist.
func (a *JiraAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	comments := make([]domain.Comment, 0)
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		var fetchErr error
		comments, fetchErr = a.fetchComments(ctx, issueID)
		return fetchErr
	})
	return comments, err
}

// TransitionIssue moves an issue to the specified target state by
// finding and executing the matching Jira workflow transition.
// Available transitions are fetched via GET, matched by target status
// name (case-insensitive, first match), then executed via POST.
func (a *JiraAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	return trackermetrics.Track(a.metrics, "transition", func() error {
		path := a.basePath + "/issue/" + url.PathEscape(issueID) + "/transitions"

		body, _, err := a.client.Get(ctx, path, nil)
		if err != nil {
			return err
		}

		var resp transitionsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
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
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal transition request",
				Err:     err,
			}
		}

		_, err = a.client.Send(ctx, "POST", path, bytes.NewReader(postBody))
		return err
	})
}

// CommentIssue posts a comment on the specified Jira issue. On version
// "3" the text is split by newlines into ADF paragraph nodes. On
// version "2" the text is sent verbatim as a raw wiki-markup body.
func (a *JiraAdapter) CommentIssue(ctx context.Context, issueID string, text string) error {
	return trackermetrics.Track(a.metrics, "comment", func() error {
		path := a.basePath + "/issue/" + url.PathEscape(issueID) + "/comment"

		body := commentPayload(a.apiVersion, text)
		payload, err := json.Marshal(body)
		if err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal comment request",
				Err:     err,
			}
		}

		_, err = a.client.Send(ctx, "POST", path, bytes.NewReader(payload))
		return err
	})
}

// commentPayload builds the comment-create request body for the given
// API version. On version "2" it returns a raw wiki-markup string body;
// on any other version it returns an ADF document.
func commentPayload(apiVersion, text string) any {
	if apiVersion == "2" {
		return map[string]any{"body": text}
	}
	return buildADFComment(text)
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
	path := a.basePath + "/issue/" + url.PathEscape(issueID)

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

	_, err = a.client.Send(ctx, "PUT", path, bytes.NewReader(payload))
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

// paginatedSearch executes a paginated JQL search and returns all
// normalized issues. Comments are set to nil. Version "2" uses an
// offset loop; any other version uses cursor pagination.
func (a *JiraAdapter) paginatedSearch(ctx context.Context, jql, fields string) ([]domain.Issue, error) {
	if a.apiVersion == "2" {
		return a.paginatedSearchV2(ctx, jql, fields)
	}

	params := url.Values{
		"jql":        {jql},
		"fields":     {fields},
		"maxResults": {maxSearchResults},
	}

	paginator := httpkit.NewTokenPaginator(a.client, a.basePath+"/search/jql", params, "nextPageToken", func(body []byte) ([]domain.Issue, string, error) {
		var sr searchResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			return nil, "", &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse search response",
				Err:     err,
			}
		}

		issues := make([]domain.Issue, 0, len(sr.Issues))
		for _, ji := range sr.Issues {
			issue := normalizeSearchIssue(a.apiVersion, a.endpoint, ji)
			issue.Comments = nil
			issues = append(issues, issue)
		}
		return issues, sr.NextPageToken, nil
	}, httpkit.PaginatorOptions{})

	return paginator.All(ctx)
}

// paginatedSearchV2 executes an offset-paginated JQL search against the
// v2 search endpoint and returns all normalized issues. Comments are
// set to nil. The loop terminates on an empty page or when the cursor
// reaches the reported total, mirroring the comment-pagination loop.
func (a *JiraAdapter) paginatedSearchV2(ctx context.Context, jql, fields string) ([]domain.Issue, error) {
	issues := make([]domain.Issue, 0)
	startAt := 0

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		params := url.Values{
			"jql":        {jql},
			"fields":     {fields},
			"maxResults": {maxSearchResults},
			"startAt":    {strconv.Itoa(startAt)},
		}

		body, _, err := a.client.Get(ctx, a.basePath+"/search", params)
		if err != nil {
			return nil, err
		}

		var sr searchResponseV2
		if err := json.Unmarshal(body, &sr); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse search response",
				Err:     err,
			}
		}

		for _, ji := range sr.Issues {
			issue := normalizeSearchIssue(a.apiVersion, a.endpoint, ji)
			issue.Comments = nil
			issues = append(issues, issue)
		}

		if len(sr.Issues) == 0 || startAt+len(sr.Issues) >= sr.Total {
			break
		}
		startAt += len(sr.Issues)
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

		body, _, err := a.client.Get(ctx, a.basePath+"/issue/"+url.PathEscape(issueID)+"/comment", params)
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
	return normalizeComments(a.apiVersion, allComments), nil
}
