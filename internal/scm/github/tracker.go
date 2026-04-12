// Package github provides GitHub SCM adapter implementations for Sortie.
// The package includes [GitHubAdapter], which implements [domain.TrackerAdapter]
// against the GitHub Issues and Labels REST API, and [GitHubCIProvider], which
// implements [domain.CIStatusProvider] against the GitHub Checks API. Both
// adapters share the internal HTTP client ([githubClient]) and ETag cache
// ([etagCache]). Start with [NewGitHubAdapter] for tracker integration or
// [NewGitHubCIProvider] for CI status queries.
//
// Unlike the Jira adapter, this adapter stores both activeStates and
// terminalStates. Jira has native workflow states that the adapter
// reads directly, so it needs only activeStates for candidate
// filtering. GitHub has no native workflow states — only open and
// closed — so the adapter must derive Sortie states entirely from
// labels. This requires knowledge of both active and terminal state
// sets for extractState fallback logic, FetchIssuesByStates
// open/closed routing, and TransitionIssue close/reopen decisions.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("github", NewGitHubAdapter, registry.TrackerMeta{
		RequiresProject:       true,
		RequiresAPIKey:        true,
		ValidateTrackerConfig: validateConfig,
	})
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*GitHubAdapter)(nil)

// maxPages is the upper bound on paginated fetches. At 50 items per
// page this allows up to 10,000 results before the adapter stops and
// returns what it has.
const maxPages = 200

// defaultActiveStates is applied when the config omits active_states.
var defaultActiveStates = []string{"backlog", "in-progress", "review"}

// defaultTerminalStates is applied when the config omits terminal_states.
var defaultTerminalStates = []string{"done", "wontfix"}

// GitHubAdapter implements [domain.TrackerAdapter] against the GitHub
// REST API. Safe for concurrent use.
type GitHubAdapter struct {
	client         *githubClient
	owner          string
	repo           string
	activeStates   []string
	terminalStates []string
	handoffState   string // normalized to lowercase; empty when not configured
	queryFilter    string
	metrics        domain.Metrics
	etagCache      *etagCache
}

// NewGitHubAdapter creates a [GitHubAdapter] from adapter configuration.
// Required config keys: "api_key" (personal access token or fine-grained
// token), "project" (owner/repo format). Optional: "endpoint" (defaults
// to https://api.github.com), "active_states", "terminal_states",
// "handoff_state" (single state name recognized as a valid transition
// target and state label; normalized to lowercase),
// "query_filter", "user_agent", "etag_cache_size" (int, default 1000;
// set to 0 to disable ETag caching).
func NewGitHubAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	apiKey, _ := config["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "missing required config key: api_key",
		}
	}

	project, _ := config["project"].(string)
	if project == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerProject,
			Message: "missing required config key: project",
		}
	}

	owner, repo, ok := strings.Cut(project, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "project must be in owner/repo format",
		}
	}

	endpoint, _ := config["endpoint"].(string)
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	endpoint = strings.TrimRight(endpoint, "/")

	activeRaw := typeutil.ExtractStringSlice(config["active_states"])
	if len(activeRaw) == 0 {
		activeRaw = defaultActiveStates
	}
	activeStates := make([]string, len(activeRaw))
	for i, s := range activeRaw {
		activeStates[i] = strings.ToLower(s)
	}

	terminalRaw := typeutil.ExtractStringSlice(config["terminal_states"])
	if len(terminalRaw) == 0 {
		terminalRaw = defaultTerminalStates
	}
	terminalStates := make([]string, len(terminalRaw))
	for i, s := range terminalRaw {
		terminalStates[i] = strings.ToLower(s)
	}

	handoffRaw, _ := config["handoff_state"].(string)
	handoffState := strings.ToLower(strings.TrimSpace(handoffRaw))

	queryFilter, _ := config["query_filter"].(string)

	userAgent, _ := config["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	etagCacheSize := 1000
	if v, ok := config["etag_cache_size"]; ok {
		switch n := v.(type) {
		case int:
			if n >= 0 {
				etagCacheSize = n
			}
		case float64:
			if n >= 0 && n == float64(int(n)) {
				etagCacheSize = int(n)
			}
		}
	}

	return &GitHubAdapter{
		client:         newGitHubClient(endpoint, apiKey, userAgent),
		owner:          owner,
		repo:           repo,
		activeStates:   activeStates,
		terminalStates: terminalStates,
		handoffState:   handoffState,
		queryFilter:    queryFilter,
		etagCache:      newETagCache(etagCacheSize),
	}, nil
}

// FetchCandidateIssues returns issues in configured active states for
// the configured repository. When queryFilter is empty, uses the
// issues endpoint with client-side state filtering. When queryFilter
// is set, routes through the search endpoint for server-side
// filtering. Comments are set to nil on all returned issues.
func (a *GitHubAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	if a.queryFilter != "" {
		return a.fetchCandidatesViaSearch(ctx)
	}
	return a.fetchCandidatesViaIssues(ctx)
}

func (a *GitHubAdapter) fetchCandidatesViaIssues(ctx context.Context) ([]domain.Issue, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues"
	params := url.Values{
		"state":     {"open"},
		"sort":      {"created"},
		"direction": {"asc"},
		"per_page":  {"50"},
	}

	body, nextURL, err := a.client.do(ctx, "GET", path, params)
	if err != nil {
		a.incTrackerRequest("fetch_candidates", "error")
		return nil, err
	}

	var raw []githubIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		a.incTrackerRequest("fetch_candidates", "error")
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse issues response",
			Err:     err,
		}
	}

	activeSet := make(map[string]struct{}, len(a.activeStates))
	for _, s := range a.activeStates {
		activeSet[s] = struct{}{}
	}

	var issues []domain.Issue
	for _, gi := range raw {
		if isPullRequest(gi) {
			continue
		}
		issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
		a.qualifyDisplayID(&issue)
		if _, ok := activeSet[issue.State]; !ok {
			continue
		}
		issue.Comments = nil
		issues = append(issues, issue)
	}

	pageCount := 1
	for nextURL != "" && pageCount < maxPages {
		pageCount++
		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			a.incTrackerRequest("fetch_candidates", "error")
			return nil, err
		}

		raw = raw[:0]
		if err := json.Unmarshal(body, &raw); err != nil {
			a.incTrackerRequest("fetch_candidates", "error")
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issues response",
				Err:     err,
			}
		}
		for _, gi := range raw {
			if isPullRequest(gi) {
				continue
			}
			issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
			a.qualifyDisplayID(&issue)
			if _, ok := activeSet[issue.State]; !ok {
				continue
			}
			issue.Comments = nil
			issues = append(issues, issue)
		}
	}

	if pageCount >= maxPages {
		slog.Warn("pagination limit reached", //nolint:gosec // endpoint is an internal API path constant, not user input
			slog.Int("max_pages", maxPages),
			slog.String("endpoint", path))
	}

	if issues == nil {
		issues = []domain.Issue{}
	}
	a.incTrackerRequest("fetch_candidates", "success")
	return issues, nil
}

func (a *GitHubAdapter) fetchCandidatesViaSearch(ctx context.Context) ([]domain.Issue, error) {
	q := fmt.Sprintf("repo:%s/%s type:issue state:open %s", a.owner, a.repo, a.queryFilter)
	params := url.Values{
		"q":        {q},
		"sort":     {"created"},
		"order":    {"asc"},
		"per_page": {"50"},
	}

	body, nextURL, err := a.client.do(ctx, "GET", "/search/issues", params)
	if err != nil {
		a.incTrackerRequest("fetch_candidates", "error")
		return nil, err
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		a.incTrackerRequest("fetch_candidates", "error")
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse search response",
			Err:     err,
		}
	}
	if sr.IncompleteResults {
		slog.Warn("github search returned incomplete results",
			slog.Int("total_count", sr.TotalCount))
	}

	activeSet := make(map[string]struct{}, len(a.activeStates))
	for _, s := range a.activeStates {
		activeSet[s] = struct{}{}
	}

	var issues []domain.Issue
	for _, gi := range sr.Items {
		if isPullRequest(gi) {
			continue
		}
		issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
		a.qualifyDisplayID(&issue)
		if _, ok := activeSet[issue.State]; !ok {
			continue
		}
		issue.Comments = nil
		issues = append(issues, issue)
	}

	pageCount := 1
	for nextURL != "" && pageCount < maxPages {
		pageCount++
		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			a.incTrackerRequest("fetch_candidates", "error")
			return nil, err
		}

		var page searchResponse
		if err := json.Unmarshal(body, &page); err != nil {
			a.incTrackerRequest("fetch_candidates", "error")
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse search response",
				Err:     err,
			}
		}
		for _, gi := range page.Items {
			if isPullRequest(gi) {
				continue
			}
			issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
			a.qualifyDisplayID(&issue)
			if _, ok := activeSet[issue.State]; !ok {
				continue
			}
			issue.Comments = nil
			issues = append(issues, issue)
		}
	}

	if pageCount >= maxPages {
		slog.Warn("pagination limit reached", //nolint:gosec // endpoint is an internal API path constant, not user input
			slog.Int("max_pages", maxPages),
			slog.String("endpoint", "/search/issues"))
	}

	if issues == nil {
		issues = []domain.Issue{}
	}
	a.incTrackerRequest("fetch_candidates", "success")
	return issues, nil
}

// FetchIssueByID returns a fully populated issue including comments,
// blockers, and parent. The issueID is the issue number as a string.
func (a *GitHubAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	basePath := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID)

	// Fetch the issue itself.
	body, _, err := a.client.do(ctx, "GET", basePath, nil)
	if err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		if isNotFound(err) {
			return domain.Issue{}, &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueID),
			}
		}
		return domain.Issue{}, err
	}

	var gi githubIssue
	if err := json.Unmarshal(body, &gi); err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		return domain.Issue{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse issue response",
			Err:     err,
		}
	}

	if isPullRequest(gi) {
		a.incTrackerRequest("fetch_issue", "error")
		return domain.Issue{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("resource is a pull request, not an issue: %s", issueID),
		}
	}

	issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
	a.qualifyDisplayID(&issue)

	// Fetch blockers (dependencies/blocked_by).
	issue.BlockedBy, err = a.fetchBlockers(ctx, issueID)
	if err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		return domain.Issue{}, err
	}

	// Fetch parent.
	issue.Parent, err = a.fetchParent(ctx, issueID)
	if err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		return domain.Issue{}, err
	}

	// Fetch comments.
	comments, err := a.fetchAllComments(ctx, issueID)
	if err != nil {
		a.incTrackerRequest("fetch_issue", "error")
		return domain.Issue{}, err
	}
	issue.Comments = comments

	a.incTrackerRequest("fetch_issue", "success")
	return issue, nil
}

func (a *GitHubAdapter) fetchBlockers(ctx context.Context, issueID string) ([]domain.BlockerRef, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/dependencies/blocked_by"
	params := url.Values{"per_page": {"50"}}

	body, nextURL, err := a.client.do(ctx, "GET", path, params)
	if err != nil {
		if isNotFound(err) {
			return []domain.BlockerRef{}, nil
		}
		return nil, err
	}

	var blockers []githubIssue
	if err := json.Unmarshal(body, &blockers); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse blockers response",
			Err:     err,
		}
	}

	pageCount := 1
	for nextURL != "" && pageCount < maxPages {
		pageCount++
		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			return nil, err
		}

		var page []githubIssue
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse blockers response",
				Err:     err,
			}
		}
		blockers = append(blockers, page...)
	}

	if pageCount >= maxPages {
		slog.Warn("pagination limit reached", //nolint:gosec // endpoint is an internal API path constant, not user input
			slog.Int("max_pages", maxPages),
			slog.String("endpoint", path))
	}

	return normalizeBlockers(blockers, a.activeStates, a.terminalStates, a.handoffState), nil
}

func (a *GitHubAdapter) fetchParent(ctx context.Context, issueID string) (*domain.ParentRef, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/parent"

	body, _, err := a.client.do(ctx, "GET", path, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	var parent githubIssue
	if err := json.Unmarshal(body, &parent); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse parent response",
			Err:     err,
		}
	}

	num := strconv.Itoa(parent.Number)
	return &domain.ParentRef{
		ID:         num,
		Identifier: num,
	}, nil
}

// FetchIssuesByStates returns issues in the specified states using a
// dual strategy: the issues endpoint for active states (open issues,
// client-side filtering) and the search endpoint for terminal states
// (closed issues, server-side label filtering).
func (a *GitHubAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	stateSet := make(map[string]struct{}, len(states))
	for _, s := range states {
		stateSet[strings.ToLower(s)] = struct{}{}
	}

	// Partition requested states into open-fetch vs closed-search.
	// Non-terminal states (active and unknown) route through the issues
	// endpoint (state=open). An unknown state label on a closed issue
	// will not be found — this is intentional: only configured terminal
	// states warrant the closed-issue search path.
	var requestedTerminal []string
	needOpenFetch := false
	for s := range stateSet {
		if isTerminalState(s, a.terminalStates) {
			requestedTerminal = append(requestedTerminal, s)
		} else {
			needOpenFetch = true
		}
	}

	var matched []domain.Issue
	seen := make(map[string]struct{})

	// Active/unknown states: issues endpoint with client-side filtering.
	if needOpenFetch {
		issues, err := a.fetchOpenIssuesByStates(ctx, stateSet, seen)
		if err != nil {
			a.incTrackerRequest("fetch_by_states", "error")
			return nil, err
		}
		matched = append(matched, issues...)
	}

	// Terminal states: search endpoint with server-side label filtering.
	for _, label := range requestedTerminal {
		if ctx.Err() != nil {
			a.incTrackerRequest("fetch_by_states", "error")
			return nil, ctx.Err()
		}

		issues, err := a.fetchClosedIssuesByLabel(ctx, label, seen)
		if err != nil {
			a.incTrackerRequest("fetch_by_states", "error")
			return nil, err
		}
		matched = append(matched, issues...)
	}

	if matched == nil {
		matched = []domain.Issue{}
	}
	a.incTrackerRequest("fetch_by_states", "success")
	return matched, nil
}

func (a *GitHubAdapter) fetchOpenIssuesByStates(ctx context.Context, stateSet map[string]struct{}, seen map[string]struct{}) ([]domain.Issue, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues"
	params := url.Values{
		"state":     {"open"},
		"sort":      {"created"},
		"direction": {"asc"},
		"per_page":  {"50"},
	}

	body, nextURL, err := a.client.do(ctx, "GET", path, params)
	if err != nil {
		return nil, err
	}

	var issues []domain.Issue
	var raw []githubIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse issues response",
			Err:     err,
		}
	}
	for _, gi := range raw {
		if isPullRequest(gi) {
			continue
		}
		issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
		a.qualifyDisplayID(&issue)
		if _, ok := stateSet[issue.State]; !ok {
			continue
		}
		if _, dup := seen[issue.Identifier]; dup {
			continue
		}
		issue.Comments = nil
		issues = append(issues, issue)
		seen[issue.Identifier] = struct{}{}
	}

	pageCount := 1
	for nextURL != "" && pageCount < maxPages {
		pageCount++
		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			return nil, err
		}

		raw = raw[:0]
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issues response",
				Err:     err,
			}
		}
		for _, gi := range raw {
			if isPullRequest(gi) {
				continue
			}
			issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
			a.qualifyDisplayID(&issue)
			if _, ok := stateSet[issue.State]; !ok {
				continue
			}
			if _, dup := seen[issue.Identifier]; dup {
				continue
			}
			issue.Comments = nil
			issues = append(issues, issue)
			seen[issue.Identifier] = struct{}{}
		}
	}

	if pageCount >= maxPages {
		slog.Warn("pagination limit reached", //nolint:gosec // endpoint is an internal API path constant, not user input
			slog.Int("max_pages", maxPages),
			slog.String("endpoint", path),
			slog.String("state", "open"))
	}

	return issues, nil
}

func (a *GitHubAdapter) fetchClosedIssuesByLabel(ctx context.Context, label string, seen map[string]struct{}) ([]domain.Issue, error) {
	q := fmt.Sprintf(`repo:%s/%s type:issue state:closed label:"%s"`, a.owner, a.repo, label)
	params := url.Values{
		"q":        {q},
		"sort":     {"created"},
		"order":    {"asc"},
		"per_page": {"50"},
	}

	body, nextURL, err := a.client.do(ctx, "GET", "/search/issues", params)
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
	if sr.IncompleteResults {
		slog.Warn("github search returned incomplete results",
			slog.String("label", label))
	}

	var issues []domain.Issue
	for _, gi := range sr.Items {
		if isPullRequest(gi) {
			continue
		}
		issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
		a.qualifyDisplayID(&issue)
		if _, dup := seen[issue.Identifier]; dup {
			continue
		}
		issue.Comments = nil
		issues = append(issues, issue)
		seen[issue.Identifier] = struct{}{}
	}

	pageCount := 1
	for nextURL != "" && pageCount < maxPages {
		pageCount++
		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			return nil, err
		}

		var page searchResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse search response",
				Err:     err,
			}
		}
		for _, gi := range page.Items {
			if isPullRequest(gi) {
				continue
			}
			issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
			a.qualifyDisplayID(&issue)
			if _, dup := seen[issue.Identifier]; dup {
				continue
			}
			issue.Comments = nil
			issues = append(issues, issue)
			seen[issue.Identifier] = struct{}{}
		}
	}

	if pageCount >= maxPages {
		slog.Warn("pagination limit reached", //nolint:gosec // endpoint is an internal API path constant, not user input
			slog.Int("max_pages", maxPages),
			slog.String("endpoint", "/search/issues"),
			slog.String("label", label))
	}

	return issues, nil
}

// FetchIssueStatesByIDs returns the current state for each requested
// issue ID. Since ID and Identifier are both the issue number, this
// delegates to [GitHubAdapter.fetchStatesByNumbers].
func (a *GitHubAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	states, err := a.fetchStatesByNumbers(ctx, issueIDs)
	if err != nil {
		a.incTrackerRequest("fetch_states_by_ids", "error")
		return nil, err
	}
	a.incTrackerRequest("fetch_states_by_ids", "success")
	return states, nil
}

// FetchIssueStatesByIdentifiers returns the current state for each
// requested issue identifier. Since ID and Identifier are both the
// issue number, this delegates to [GitHubAdapter.fetchStatesByNumbers].
func (a *GitHubAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	states, err := a.fetchStatesByNumbers(ctx, identifiers)
	if err != nil {
		a.incTrackerRequest("fetch_states_by_identifiers", "error")
		return nil, err
	}
	a.incTrackerRequest("fetch_states_by_identifiers", "success")
	return states, nil
}

func (a *GitHubAdapter) fetchStatesByNumbers(ctx context.Context, numbers []string) (map[string]string, error) {
	if len(numbers) == 0 {
		return map[string]string{}, nil
	}

	states := make(map[string]string, len(numbers))
	for _, num := range numbers {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(num)
		etag, cachedState, cacheHit := a.etagCache.lookup(path)

		body, responseETag, notModified, err := a.client.doConditional(ctx, "GET", path, nil, etag)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}

		if notModified && cacheHit {
			a.etagCache.touch(path)
			states[num] = cachedState
			continue
		}

		var gi githubIssue
		if err := json.Unmarshal(body, &gi); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issue response",
				Err:     err,
			}
		}
		if isPullRequest(gi) {
			continue
		}
		state := extractState(gi.Labels, gi.State, a.activeStates, a.terminalStates, a.handoffState)

		if responseETag != "" {
			a.etagCache.put(path, responseETag, state)
		}

		states[num] = state
	}

	return states, nil
}

// FetchIssueComments returns comments for the specified issue.
// Returns an empty non-nil slice when no comments exist.
func (a *GitHubAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	comments, err := a.fetchAllComments(ctx, issueID)
	if err != nil {
		a.incTrackerRequest("fetch_comments", "error")
		return nil, err
	}
	a.incTrackerRequest("fetch_comments", "success")
	return comments, nil
}

func (a *GitHubAdapter) fetchAllComments(ctx context.Context, issueNumber string) ([]domain.Comment, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueNumber) + "/comments"
	params := url.Values{"per_page": {"50"}}

	body, nextURL, err := a.client.do(ctx, "GET", path, params)
	if err != nil {
		if isNotFound(err) {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueNumber),
			}
		}
		return nil, err
	}

	var allComments []githubComment
	var page []githubComment
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse comments response",
			Err:     err,
		}
	}
	allComments = append(allComments, page...)

	pageCount := 1
	for nextURL != "" && pageCount < maxPages {
		pageCount++
		body, nextURL, err = a.client.doURL(ctx, nextURL)
		if err != nil {
			return nil, err
		}

		page = page[:0]
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse comments response",
				Err:     err,
			}
		}
		allComments = append(allComments, page...)
	}

	if pageCount >= maxPages {
		slog.Warn("pagination limit reached", //nolint:gosec // endpoint is an internal API path constant, not user input
			slog.Int("max_pages", maxPages),
			slog.String("endpoint", path))
	}

	return normalizeComments(allComments), nil
}

// TransitionIssue moves an issue to the specified target state by
// swapping the state label and updating the native open/closed status
// as needed. Up to three sequential API calls: remove old label, add
// new label, patch native state. Partial failures converge on retry
// because individual label operations are idempotent.
func (a *GitHubAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	targetLower := strings.ToLower(targetState)

	isHandoffTarget := a.handoffState != "" && targetLower == a.handoffState
	if !isActiveState(targetLower, a.activeStates) && !isTerminalState(targetLower, a.terminalStates) && !isHandoffTarget {
		a.incTrackerRequest("transition", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("invalid target state: %q is not a configured active, terminal, or handoff state", targetState),
		}
	}

	basePath := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID)

	// Fetch current issue to read labels and native state.
	body, _, err := a.client.do(ctx, "GET", basePath, nil)
	if err != nil {
		a.incTrackerRequest("transition", "error")
		return err
	}

	var gi githubIssue
	if err := json.Unmarshal(body, &gi); err != nil {
		a.incTrackerRequest("transition", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse issue response",
			Err:     err,
		}
	}

	currentLabel := findCurrentStateLabel(gi.Labels, a.activeStates, a.terminalStates, a.handoffState)
	currentNative := gi.State

	// Remove old state label if present and different.
	if currentLabel != "" && currentLabel != targetLower {
		labelPath := basePath + "/labels/" + url.PathEscape(currentLabel)
		err := a.client.doNoBody(ctx, "DELETE", labelPath)
		if err != nil && !isNotFound(err) {
			a.incTrackerRequest("transition", "error")
			return err
		}
	}

	// Add target state label if different from current.
	if currentLabel != targetLower {
		payload, err := json.Marshal(map[string][]string{"labels": {targetLower}})
		if err != nil {
			a.incTrackerRequest("transition", "error")
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal label payload",
				Err:     err,
			}
		}
		if _, err := a.client.doJSON(ctx, "POST", basePath+"/labels", bytes.NewReader(payload)); err != nil {
			a.incTrackerRequest("transition", "error")
			return err
		}
	}

	// Open/close the issue if the native state needs to change.
	if isTerminalState(targetLower, a.terminalStates) && currentNative == "open" {
		payload, err := json.Marshal(map[string]any{"state": "closed", "state_reason": "completed"})
		if err != nil {
			a.incTrackerRequest("transition", "error")
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal state payload",
				Err:     err,
			}
		}
		if _, err := a.client.doJSON(ctx, "PATCH", basePath, bytes.NewReader(payload)); err != nil {
			a.incTrackerRequest("transition", "error")
			return err
		}
	} else if isActiveState(targetLower, a.activeStates) && currentNative == "closed" {
		payload, err := json.Marshal(map[string]any{"state": "open"})
		if err != nil {
			a.incTrackerRequest("transition", "error")
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal state payload",
				Err:     err,
			}
		}
		if _, err := a.client.doJSON(ctx, "PATCH", basePath, bytes.NewReader(payload)); err != nil {
			a.incTrackerRequest("transition", "error")
			return err
		}
	}

	a.incTrackerRequest("transition", "success")
	return nil
}

// CommentIssue posts a Markdown comment on the specified issue.
// GitHub natively accepts Markdown, so no format conversion is needed.
func (a *GitHubAdapter) CommentIssue(ctx context.Context, issueID string, text string) error {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/comments"

	payload, err := json.Marshal(map[string]string{"body": text})
	if err != nil {
		a.incTrackerRequest("comment", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal comment payload",
			Err:     err,
		}
	}

	if _, err := a.client.doJSON(ctx, "POST", path, bytes.NewReader(payload)); err != nil {
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
func (a *GitHubAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// AddLabel adds a label to the specified issue via the GitHub Labels API.
func (a *GitHubAdapter) AddLabel(ctx context.Context, issueID string, label string) error {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/labels"

	payload, err := json.Marshal(map[string][]string{"labels": {label}})
	if err != nil {
		a.incTrackerRequest("add_label", "error")
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal label payload",
			Err:     err,
		}
	}

	if _, err := a.client.doJSON(ctx, "POST", path, bytes.NewReader(payload)); err != nil {
		a.incTrackerRequest("add_label", "error")
		return err
	}
	a.incTrackerRequest("add_label", "success")
	return nil
}

func (a *GitHubAdapter) incTrackerRequest(operation, outcome string) {
	if a.metrics != nil {
		a.metrics.IncTrackerRequests(operation, outcome)
	}
}

// isNotFound checks whether an error is a TrackerError with kind
// ErrTrackerNotFound (HTTP 404).
func isNotFound(err error) bool {
	te, ok := err.(*domain.TrackerError)
	return ok && te.Kind == domain.ErrTrackerNotFound
}
