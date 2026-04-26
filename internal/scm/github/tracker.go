// Package github provides GitHub SCM adapter implementations for Sortie.
// The package includes [GitHubAdapter], which implements [domain.TrackerAdapter]
// against the GitHub Issues and Labels REST API, and [GitHubCIProvider], which
// implements [domain.CIStatusProvider] against the GitHub Checks API. Both
// adapters share the internal HTTP transport and ETag cache ([etagCache]).
// Start with [NewGitHubAdapter] for tracker integration or
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
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
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
	client         *httpkit.Client
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
	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		var fetchErr error
		if a.queryFilter != "" {
			issues, fetchErr = a.fetchCandidatesViaSearch(ctx)
			return fetchErr
		}
		issues, fetchErr = a.fetchCandidatesViaIssues(ctx)
		return fetchErr
	})
	return issues, err
}

func (a *GitHubAdapter) fetchCandidatesViaIssues(ctx context.Context) ([]domain.Issue, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues"
	params := url.Values{
		"state":     {"open"},
		"sort":      {"created"},
		"direction": {"asc"},
		"per_page":  {"50"},
	}

	activeSet := make(map[string]struct{}, len(a.activeStates))
	for _, s := range a.activeStates {
		activeSet[s] = struct{}{}
	}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]domain.Issue, error) {
		var raw []githubIssue
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issues response",
				Err:     err,
			}
		}

		issues := make([]domain.Issue, 0, len(raw))
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
		return issues, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", "/repos/{owner}/{repo}/issues"))
		},
	})

	return paginator.All(ctx)
}

func (a *GitHubAdapter) fetchCandidatesViaSearch(ctx context.Context) ([]domain.Issue, error) {
	q := fmt.Sprintf("repo:%s/%s type:issue state:open %s", a.owner, a.repo, a.queryFilter)
	params := url.Values{
		"q":        {q},
		"sort":     {"created"},
		"order":    {"asc"},
		"per_page": {"50"},
	}

	activeSet := make(map[string]struct{}, len(a.activeStates))
	for _, s := range a.activeStates {
		activeSet[s] = struct{}{}
	}

	paginator := httpkit.NewLinkPaginator(a.client, "/search/issues", params, func(body []byte) ([]domain.Issue, error) {
		var page searchResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse search response",
				Err:     err,
			}
		}
		if page.IncompleteResults {
			slog.Warn("github search returned incomplete results",
				slog.Int("total_count", page.TotalCount))
		}

		issues := make([]domain.Issue, 0, len(page.Items))
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
		return issues, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", "/search/issues"))
		},
	})

	return paginator.All(ctx)
}

// FetchIssueByID returns a fully populated issue including comments,
// blockers, and parent. The issueID is the issue number as a string.
func (a *GitHubAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		basePath := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID)

		body, _, err := a.client.Get(ctx, basePath, nil)
		if err != nil {
			if domain.IsNotFound(err) {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerNotFound,
					Message: fmt.Sprintf("issue not found: %s", issueID),
				}
			}
			return err
		}

		var gi githubIssue
		if err := json.Unmarshal(body, &gi); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issue response",
				Err:     err,
			}
		}

		if isPullRequest(gi) {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("resource is a pull request, not an issue: %s", issueID),
			}
		}

		fetchedIssue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState)
		a.qualifyDisplayID(&fetchedIssue)

		fetchedIssue.BlockedBy, err = a.fetchBlockers(ctx, issueID)
		if err != nil {
			return err
		}

		fetchedIssue.Parent, err = a.fetchParent(ctx, issueID)
		if err != nil {
			return err
		}

		fetchedIssue.Comments, err = a.fetchAllComments(ctx, issueID)
		if err != nil {
			return err
		}

		issue = fetchedIssue
		return nil
	})
	return issue, err
}

func (a *GitHubAdapter) fetchBlockers(ctx context.Context, issueID string) ([]domain.BlockerRef, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/dependencies/blocked_by"
	params := url.Values{"per_page": {"50"}}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]githubIssue, error) {
		var blockers []githubIssue
		if err := json.Unmarshal(body, &blockers); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse blockers response",
				Err:     err,
			}
		}
		return blockers, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", "/repos/{owner}/{repo}/issues/{issue_id}/dependencies/blocked_by"))
		},
	})

	blockers, err := paginator.All(ctx)
	if err != nil {
		if domain.IsNotFound(err) {
			return []domain.BlockerRef{}, nil
		}
		return nil, err
	}

	return normalizeBlockers(blockers, a.activeStates, a.terminalStates, a.handoffState), nil
}

func (a *GitHubAdapter) fetchParent(ctx context.Context, issueID string) (*domain.ParentRef, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/parent"

	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		if domain.IsNotFound(err) {
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

	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		matchedIssues := make([]domain.Issue, 0)
		seen := make(map[string]struct{})

		if needOpenFetch {
			issues, err := a.fetchOpenIssuesByStates(ctx, stateSet, seen)
			if err != nil {
				return err
			}
			matchedIssues = append(matchedIssues, issues...)
		}

		for _, label := range requestedTerminal {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			issues, err := a.fetchClosedIssuesByLabel(ctx, label, seen)
			if err != nil {
				return err
			}
			matchedIssues = append(matchedIssues, issues...)
		}

		matched = matchedIssues
		return nil
	})
	return matched, err
}

func (a *GitHubAdapter) fetchOpenIssuesByStates(ctx context.Context, stateSet map[string]struct{}, seen map[string]struct{}) ([]domain.Issue, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues"
	params := url.Values{
		"state":     {"open"},
		"sort":      {"created"},
		"direction": {"asc"},
		"per_page":  {"50"},
	}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]domain.Issue, error) {
		var raw []githubIssue
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issues response",
				Err:     err,
			}
		}

		issues := make([]domain.Issue, 0, len(raw))
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
		return issues, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", path),
				slog.String("state", "open"))
		},
	})

	return paginator.All(ctx)
}

func (a *GitHubAdapter) fetchClosedIssuesByLabel(ctx context.Context, label string, seen map[string]struct{}) ([]domain.Issue, error) {
	q := fmt.Sprintf(`repo:%s/%s type:issue state:closed label:"%s"`, a.owner, a.repo, label)
	params := url.Values{
		"q":        {q},
		"sort":     {"created"},
		"order":    {"asc"},
		"per_page": {"50"},
	}

	paginator := httpkit.NewLinkPaginator(a.client, "/search/issues", params, func(body []byte) ([]domain.Issue, error) {
		var page searchResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse search response",
				Err:     err,
			}
		}
		if page.IncompleteResults {
			slog.Warn("github search returned incomplete results",
				slog.String("label", label))
		}

		issues := make([]domain.Issue, 0, len(page.Items))
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
		return issues, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", "/search/issues"),
				slog.String("label", label))
		},
	})

	return paginator.All(ctx)
}

// FetchIssueStatesByIDs returns the current state for each requested
// issue ID. Since ID and Identifier are both the issue number, this
// delegates to [GitHubAdapter.fetchStatesByNumbers].
func (a *GitHubAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		fetchedStates, err := a.fetchStatesByNumbers(ctx, issueIDs)
		if err != nil {
			return err
		}
		states = fetchedStates
		return nil
	})
	return states, err
}

// FetchIssueStatesByIdentifiers returns the current state for each
// requested issue identifier. Since ID and Identifier are both the
// issue number, this delegates to [GitHubAdapter.fetchStatesByNumbers].
func (a *GitHubAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		fetchedStates, err := a.fetchStatesByNumbers(ctx, identifiers)
		if err != nil {
			return err
		}
		states = fetchedStates
		return nil
	})
	return states, err
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

		body, responseETag, notModified, err := a.client.GetConditional(ctx, path, etag, nil)
		if err != nil {
			if domain.IsNotFound(err) {
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
	comments := make([]domain.Comment, 0)
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		var fetchErr error
		comments, fetchErr = a.fetchAllComments(ctx, issueID)
		return fetchErr
	})
	return comments, err
}

func (a *GitHubAdapter) fetchAllComments(ctx context.Context, issueNumber string) ([]domain.Comment, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueNumber) + "/comments"
	params := url.Values{"per_page": {"50"}}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]githubComment, error) {
		var comments []githubComment
		if err := json.Unmarshal(body, &comments); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse comments response",
				Err:     err,
			}
		}
		return comments, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", "/repos/{owner}/{repo}/issues/{issue_id}/comments"))
		},
	})

	allComments, err := paginator.All(ctx)
	if err != nil {
		if domain.IsNotFound(err) {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueNumber),
			}
		}
		return nil, err
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

	return trackermetrics.Track(a.metrics, "transition", func() error {
		isHandoffTarget := a.handoffState != "" && targetLower == a.handoffState
		if !isActiveState(targetLower, a.activeStates) && !isTerminalState(targetLower, a.terminalStates) && !isHandoffTarget {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("invalid target state: %q is not a configured active, terminal, or handoff state", targetState),
			}
		}

		basePath := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID)

		body, _, err := a.client.Get(ctx, basePath, nil)
		if err != nil {
			return err
		}

		var gi githubIssue
		if err := json.Unmarshal(body, &gi); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issue response",
				Err:     err,
			}
		}

		currentLabel := findCurrentStateLabel(gi.Labels, a.activeStates, a.terminalStates, a.handoffState)
		currentNative := gi.State

		if currentLabel != "" && currentLabel != targetLower {
			labelPath := basePath + "/labels/" + url.PathEscape(currentLabel)
			err := a.client.SendNoBody(ctx, "DELETE", labelPath)
			if err != nil && !domain.IsNotFound(err) {
				return err
			}
		}

		if currentLabel != targetLower {
			payload, err := json.Marshal(map[string][]string{"labels": {targetLower}})
			if err != nil {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: "failed to marshal label payload",
					Err:     err,
				}
			}
			if _, err := a.client.Send(ctx, "POST", basePath+"/labels", bytes.NewReader(payload)); err != nil {
				return err
			}
		}

		if isTerminalState(targetLower, a.terminalStates) && currentNative == "open" {
			payload, err := json.Marshal(map[string]any{"state": "closed", "state_reason": "completed"})
			if err != nil {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: "failed to marshal state payload",
					Err:     err,
				}
			}
			if _, err := a.client.Send(ctx, "PATCH", basePath, bytes.NewReader(payload)); err != nil {
				return err
			}
		} else if isActiveState(targetLower, a.activeStates) && currentNative == "closed" {
			payload, err := json.Marshal(map[string]any{"state": "open"})
			if err != nil {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: "failed to marshal state payload",
					Err:     err,
				}
			}
			if _, err := a.client.Send(ctx, "PATCH", basePath, bytes.NewReader(payload)); err != nil {
				return err
			}
		}

		return nil
	})
}

// CommentIssue posts a Markdown comment on the specified issue.
// GitHub natively accepts Markdown, so no format conversion is needed.
func (a *GitHubAdapter) CommentIssue(ctx context.Context, issueID string, text string) error {
	return trackermetrics.Track(a.metrics, "comment", func() error {
		path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/comments"

		payload, err := json.Marshal(map[string]string{"body": text})
		if err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal comment payload",
				Err:     err,
			}
		}

		_, err = a.client.Send(ctx, "POST", path, bytes.NewReader(payload))
		return err
	})
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

	if _, err := a.client.Send(ctx, "POST", path, bytes.NewReader(payload)); err != nil {
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
