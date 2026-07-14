// Package gitea implements [domain.TrackerAdapter] for the Gitea REST API v1.
// It talks to a self-hosted Gitea instance over the shared HTTP transport with
// no third-party Gitea client library, normalizing issue responses to domain
// types: labels lowercased, priority always nil (Gitea exposes no priority
// field), parent always nil (Gitea has no sub-issue concept), state derived
// from labels against the configured active, terminal, and handoff lists, and
// blocker refs read from the issue dependencies route.
//
// The constructor runs a two-call preflight (GET /user and GET
// /repos/{owner}/{repo}) that fails construction on an invalid token or a
// mistyped project, so a misconfiguration surfaces at startup rather than on
// the first poll. Registered under kind "gitea" via an init function.
package gitea

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("gitea", NewGiteaAdapter, registry.TrackerMeta{
		RequiresProject: true,
		RequiresAPIKey:  true,
	})
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*GiteaAdapter)(nil)

// maxPages is the upper bound on paginated fetches. At 50 items per page this
// allows up to 10,000 results before the adapter stops and returns what it has.
const maxPages = 200

// defaultActiveStates is applied when the config omits active_states.
var defaultActiveStates = []string{"backlog", "in-progress", "review"}

// defaultTerminalStates is applied when the config omits terminal_states.
var defaultTerminalStates = []string{"done", "wontfix"}

// GiteaAdapter implements [domain.TrackerAdapter] against the Gitea REST API v1.
// All fields except metrics are set once at construction and never mutated, so
// the adapter is safe for concurrent use.
type GiteaAdapter struct {
	client         *httpkit.Client
	owner          string
	repo           string
	activeStates   []string
	terminalStates []string
	handoffState   string
	log            *slog.Logger
	metrics        domain.Metrics
}

// NewGiteaAdapter creates a [GiteaAdapter] from adapter configuration and runs a
// construction-time preflight against the instance.
//
// Required config keys: "api_key" (access token), "project" (owner/repo), and
// "endpoint" (instance base URL; there is no default host). Optional:
// "active_states", "terminal_states", "handoff_state" (label names, lowercased),
// and "user_agent". A missing or malformed key, or a preflight failure (invalid
// token or unknown repository), returns a [*domain.TrackerError] and blocks
// construction.
func NewGiteaAdapter(config map[string]any) (domain.TrackerAdapter, error) {
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
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "tracker.endpoint is required for gitea",
		}
	}
	baseURL := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(baseURL, "/api/v1") {
		baseURL += "/api/v1"
	}

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

	userAgent, _ := config["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	client := newGiteaClient(baseURL, apiKey, userAgent)

	if err := runPreflight(context.Background(), client, owner, repo); err != nil {
		return nil, err
	}

	return &GiteaAdapter{
		client:         client,
		owner:          owner,
		repo:           repo,
		activeStates:   activeStates,
		terminalStates: terminalStates,
		handoffState:   handoffState,
		log:            slog.Default(),
	}, nil
}

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter operates
// without recording metrics. Safe to call before any adapter operations. Not
// safe to call concurrently with adapter operations.
func (a *GiteaAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// FetchCandidateIssues returns open issues whose derived state is in the
// configured active states, sorted ascending by creation time. Pull requests
// are skipped and Comments is nil on every returned issue.
//
// Gitea lists newest-first and offers no sort parameter, so the adapter
// re-sorts client-side to hand the orchestrator the same ascending order the
// other trackers produce.
func (a *GiteaAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		activeSet := make(map[string]struct{}, len(a.activeStates))
		for _, s := range a.activeStates {
			activeSet[s] = struct{}{}
		}

		candidates, fetchErr := a.listAndFilter(ctx, "open", activeSet)
		if fetchErr != nil {
			return fetchErr
		}

		slices.SortFunc(candidates, func(x, y domain.Issue) int {
			return cmp.Compare(x.CreatedAt, y.CreatedAt)
		})

		issues = candidates
		return nil
	})
	return issues, err
}

// FetchIssuesByStates returns issues whose derived state is in the requested
// set. It returns an empty slice with no API call when states is empty.
//
// The requested states are partitioned by whether they are terminal: the open
// listing runs when any requested state is non-terminal, and the closed listing
// runs when any requested state is terminal. Open and closed sets are disjoint,
// so no cross-fetch deduplication is needed. Comments is nil on every issue.
func (a *GiteaAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	requested := make(map[string]struct{}, len(states))
	for _, s := range states {
		requested[strings.ToLower(s)] = struct{}{}
	}

	terminalSet := make(map[string]struct{}, len(a.terminalStates))
	for _, s := range a.terminalStates {
		terminalSet[s] = struct{}{}
	}

	needOpen := false
	needClosed := false
	for s := range requested {
		if _, ok := terminalSet[s]; ok {
			needClosed = true
		} else {
			needOpen = true
		}
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		result := make([]domain.Issue, 0)

		if needOpen {
			open, err := a.listAndFilter(ctx, "open", requested)
			if err != nil {
				return err
			}
			result = append(result, open...)
		}

		if needClosed {
			closed, err := a.listAndFilter(ctx, "closed", requested)
			if err != nil {
				return err
			}
			result = append(result, closed...)
		}

		issues = result
		return nil
	})
	return issues, err
}

// listAndFilter paginates the issue-list route for the given native state and
// returns issues whose derived state is in keep. Pull requests are skipped,
// DisplayID is qualified to owner/repo#N, and Comments is nil on every issue.
//
// State filtering is client-side; the configured labels are never pushed into
// the Gitea labels query parameter, whose AND semantics and silent drop on an
// unresolvable name make it unusable for correctness-critical filtering.
func (a *GiteaAdapter) listAndFilter(ctx context.Context, nativeState string, keep map[string]struct{}) ([]domain.Issue, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues"
	params := url.Values{
		"state": {nativeState},
		"type":  {"issues"},
		"limit": {"50"},
	}

	raw, err := a.paginateIssues(ctx, path, params)
	if err != nil {
		return nil, err
	}

	filtered := make([]domain.Issue, 0, len(raw))
	for _, gi := range raw {
		if isPullRequest(gi) {
			continue
		}
		issue := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState, a.log)
		setDisplayID(&issue, a.owner, a.repo)
		if _, ok := keep[issue.State]; !ok {
			continue
		}
		issue.Comments = nil
		filtered = append(filtered, issue)
	}
	return filtered, nil
}

// paginateIssues fetches every page of an issue-array route through the
// Link-header paginator and returns the accumulated raw issues. It serves both
// the issue-list routes and the dependencies route, which return JSON arrays of
// full issue objects.
//
// A decode failure returns [domain.ErrTrackerPayload]. An absent Link header is
// the normal end of results, never a missing-cursor error, because Gitea uses
// no cursors.
func (a *GiteaAdapter) paginateIssues(ctx context.Context, path string, params url.Values) ([]giteaIssue, error) {
	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]giteaIssue, error) {
		var raw []giteaIssue
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issues response",
				Err:     err,
			}
		}
		return raw, nil
	}, httpkit.PaginatorOptions{
		MaxPages: maxPages,
		OnLimitReached: func(limit int) {
			a.log.Warn("pagination limit reached",
				slog.Int("max_pages", limit),
				slog.String("endpoint", path))
		},
	})

	return paginator.All(ctx)
}

// FetchIssueByID returns a fully populated issue including comments and
// blockers. issueID is the repo-scoped index. A 404, or an index that resolves
// to a pull request, returns [domain.ErrTrackerNotFound].
//
// Parent is always nil and no parent request is issued: Gitea has no parent or
// sub-issue route.
func (a *GiteaAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID)

		body, _, err := a.client.Get(ctx, path, nil)
		if err != nil {
			if domain.IsNotFound(err) {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerNotFound,
					Message: fmt.Sprintf("issue not found: %s", issueID),
				}
			}
			return err
		}

		var gi giteaIssue
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

		fetched := normalizeIssue(gi, a.activeStates, a.terminalStates, a.handoffState, a.log)
		setDisplayID(&fetched, a.owner, a.repo)

		blockers, err := a.fetchBlockers(ctx, issueID)
		if err != nil {
			return err
		}
		fetched.BlockedBy = blockers

		comments, err := a.fetchComments(ctx, issueID)
		if err != nil {
			return err
		}
		fetched.Comments = comments

		issue = fetched
		return nil
	})
	return issue, err
}

// fetchBlockers returns the issues blocking the given issue, read from the
// dependencies route through the Link paginator. A 404 is tolerated and yields
// a non-nil empty slice.
func (a *GiteaAdapter) fetchBlockers(ctx context.Context, index string) ([]domain.BlockerRef, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(index) + "/dependencies"
	params := url.Values{"limit": {"50"}}

	raw, err := a.paginateIssues(ctx, path, params)
	if err != nil {
		if domain.IsNotFound(err) {
			return []domain.BlockerRef{}, nil
		}
		return nil, err
	}

	return normalizeBlockers(raw, a.activeStates, a.terminalStates, a.handoffState, a.log), nil
}

// FetchIssueComments returns all comments for the issue. It returns a non-nil
// empty slice when the issue has no comments and [domain.ErrTrackerNotFound]
// when the index does not exist.
func (a *GiteaAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	comments := make([]domain.Comment, 0)
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		fetched, fetchErr := a.fetchComments(ctx, issueID)
		if fetchErr != nil {
			return fetchErr
		}
		comments = fetched
		return nil
	})
	return comments, err
}

// fetchComments fetches all comments for an issue in a single request. The
// Gitea comments route is unpaginated and returns the complete list in one
// response, so it is never routed through the Link paginator. A 404 maps to
// [domain.ErrTrackerNotFound]; an issue with no comments yields a non-nil empty
// slice.
func (a *GiteaAdapter) fetchComments(ctx context.Context, index string) ([]domain.Comment, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(index) + "/comments"

	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		if domain.IsNotFound(err) {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", index),
			}
		}
		return nil, err
	}

	var raw []giteaComment
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse comments response",
			Err:     err,
		}
	}

	return normalizeComments(raw), nil
}

// FetchIssueStatesByIDs returns the derived state for each requested issue
// index, keyed by the requested value. ID and Identifier are both the index, so
// this delegates to [GiteaAdapter.fetchStatesByIndexes].
func (a *GiteaAdapter) FetchIssueStatesByIDs(ctx context.Context, ids []string) (map[string]string, error) {
	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		fetched, fetchErr := a.fetchStatesByIndexes(ctx, ids)
		if fetchErr != nil {
			return fetchErr
		}
		states = fetched
		return nil
	})
	return states, err
}

// FetchIssueStatesByIdentifiers returns the derived state for each requested
// issue index, keyed by the requested value. ID and Identifier are both the
// index, so this delegates to [GiteaAdapter.fetchStatesByIndexes].
func (a *GiteaAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, ids []string) (map[string]string, error) {
	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		fetched, fetchErr := a.fetchStatesByIndexes(ctx, ids)
		if fetchErr != nil {
			return fetchErr
		}
		states = fetched
		return nil
	})
	return states, err
}

// fetchStatesByIndexes fetches each issue by index and derives its state,
// keying the result by the requested value. It returns an empty map with no
// request when indexes is empty.
//
// A 404 or a pull-request entry omits that index from the result rather than
// failing. Cancellation is checked before each request. No conditional-request
// cache is used because Gitea sends no ETag.
func (a *GiteaAdapter) fetchStatesByIndexes(ctx context.Context, indexes []string) (map[string]string, error) {
	if len(indexes) == 0 {
		return map[string]string{}, nil
	}

	states := make(map[string]string, len(indexes))
	for _, index := range indexes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(index)
		body, _, err := a.client.Get(ctx, path, nil)
		if err != nil {
			if domain.IsNotFound(err) {
				continue
			}
			return nil, err
		}

		var gi giteaIssue
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

		states[index] = deriveState(gi.Labels, gi.State, a.activeStates, a.terminalStates, a.handoffState, index, a.log)
	}

	return states, nil
}

// TransitionIssue is not yet implemented; the Gitea write path is a separate
// task. It returns [domain.ErrTrackerPayload] and issues no request, so the
// orchestrator never mistakes a missing transition for a completed one.
func (a *GiteaAdapter) TransitionIssue(ctx context.Context, issueID, targetState string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "gitea: TransitionIssue is not yet implemented",
	}
}

// CommentIssue is not yet implemented; the Gitea write path is a separate task.
// It returns [domain.ErrTrackerPayload] and issues no request.
func (a *GiteaAdapter) CommentIssue(ctx context.Context, issueID, text string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "gitea: CommentIssue is not yet implemented",
	}
}

// AddLabel is not yet implemented; the Gitea write path is a separate task. It
// returns [domain.ErrTrackerPayload] and issues no request, so a silently
// ignored label attach cannot masquerade as success.
func (a *GiteaAdapter) AddLabel(ctx context.Context, issueID, label string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "gitea: AddLabel is not yet implemented",
	}
}
