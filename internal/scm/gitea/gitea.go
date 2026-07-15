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
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("gitea", NewGiteaAdapter, registry.TrackerMeta{
		RequiresProject:       true,
		RequiresAPIKey:        true,
		ValidateTrackerConfig: validateConfig,
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
	queryFilter    url.Values // parsed tracker.query_filter; nil or empty when unset
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

	raw, _ := config["query_filter"].(string)
	filter, err := parseGiteaQueryFilter(raw)
	if err != nil {
		return nil, err
	}

	adapter := &GiteaAdapter{
		client:         client,
		owner:          owner,
		repo:           repo,
		activeStates:   activeStates,
		terminalStates: terminalStates,
		handoffState:   handoffState,
		queryFilter:    filter,
		log:            slog.Default(),
	}

	warnUnrecognizedFilterKeys(adapter.log, filter)
	if hasNonEmptyLabel(filter["labels"]) {
		known, fetchErr := adapter.fetchLabelNames(context.Background())
		if fetchErr != nil {
			adapter.log.Warn("failed to fetch labels for query_filter diagnostic",
				slog.Any("error", fetchErr))
		} else {
			reportUnresolvedLabels(adapter.log, filter["labels"], known)
		}
	}

	return adapter, nil
}

// parseGiteaQueryFilter parses the operator tracker.query_filter into url.Values.
// It returns (nil, nil) when raw is empty.
//
// A value that does not parse as a URL query, or that names a reserved
// adapter-owned key, returns a [*domain.TrackerError] of kind
// [domain.ErrTrackerPayload]. The reserved keys state, type, page, and limit are
// checked in that fixed order so a fragment naming several of them always
// reports the first, keeping the error message stable.
func parseGiteaQueryFilter(raw string) (url.Values, error) {
	if raw == "" {
		return nil, nil
	}

	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "gitea: tracker.query_filter is not a valid url query",
			Err:     err,
		}
	}

	for _, reserved := range []string{"state", "type", "page", "limit"} {
		if _, present := values[reserved]; present {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("gitea: tracker.query_filter must not contain a reserved key %q", reserved),
			}
		}
	}

	return values, nil
}

// fetchLabelNames pages the repository label catalog and returns the set of
// label names keyed by their exact, original casing.
//
// It is a separate read from [GiteaAdapter.resolveLabelIndex], whose
// lowercased-key map backs the write paths and cannot support the
// case-sensitive comparison the query_filter labels diagnostic requires. A page
// decode failure returns [domain.ErrTrackerPayload].
func (a *GiteaAdapter) fetchLabelNames(ctx context.Context) (map[string]struct{}, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/labels"
	params := url.Values{"limit": {"50"}}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]giteaLabel, error) {
		var raw []giteaLabel
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse labels response",
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

	labels, err := paginator.All(ctx)
	if err != nil {
		return nil, err
	}

	names := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		names[l.Name] = struct{}{}
	}
	return names, nil
}

// reportUnresolvedLabels warns once for each query_filter label absent from
// known by exact, case-sensitive comparison.
//
// Each value may hold several comma-separated names, matching Gitea's labels
// parameter. The comparison is case-sensitive because Gitea matches label names
// case-sensitively and silently drops the whole filter on a miss, so a
// case-insensitive check would hide the exact mismatch this diagnostic exists to
// surface.
func reportUnresolvedLabels(log *slog.Logger, values []string, known map[string]struct{}) {
	for _, value := range values {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, ok := known[name]; ok {
				continue
			}
			log.Warn("query_filter label does not resolve against repository labels",
				slog.String("label", name))
		}
	}
}

// hasNonEmptyLabel reports whether values holds at least one label name that is
// non-empty after splitting on commas and trimming surrounding whitespace.
//
// The query_filter labels diagnostic reads the repository label catalog, so an
// empty or whitespace-only labels= fragment carries no label constraint and must
// not trigger that construction-time fetch. A nil slice reports false.
func hasNonEmptyLabel(values []string) bool {
	for _, value := range values {
		for name := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(name) != "" {
				return true
			}
		}
	}
	return false
}

// knownGiteaFilterKeys is the set of repository issue-list filter parameters
// Gitea honors, excluding the four adapter-owned keys rejected at parse. A
// query_filter key outside this set is a likely operator typo.
var knownGiteaFilterKeys = map[string]struct{}{
	"labels":       {},
	"q":            {},
	"milestones":   {},
	"since":        {},
	"before":       {},
	"created_by":   {},
	"assigned_by":  {},
	"mentioned_by": {},
}

// warnUnrecognizedFilterKeys warns once for each query_filter key outside
// [knownGiteaFilterKeys].
//
// The key is not dropped: Gitea silently ignores an unrecognized parameter and
// returns every open issue, so an unrecognized key widens rather than narrows
// the candidate set. The warn surfaces the likely typo while the key still
// merges into the request.
func warnUnrecognizedFilterKeys(log *slog.Logger, filter url.Values) {
	for key := range filter {
		if _, ok := knownGiteaFilterKeys[key]; ok {
			continue
		}
		log.Warn("query_filter contains an unrecognized key",
			slog.String("key", key))
	}
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

		candidates, fetchErr := a.listAndFilter(ctx, "open", activeSet, true)
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
			open, err := a.listAndFilter(ctx, "open", requested, true)
			if err != nil {
				return err
			}
			result = append(result, open...)
		}

		if needClosed {
			closed, err := a.listAndFilter(ctx, "closed", requested, false)
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

// mergeQueryParams returns a fresh url.Values holding every entry of base
// followed by every entry of filter. It never writes into base or filter, so a
// concurrent fetch never shares or rewrites the stored operator filter.
//
// Because the reserved keys are rejected at parse, base (state, type, limit) and
// filter are disjoint and no adapter-owned parameter is overwritten.
func mergeQueryParams(base, filter url.Values) url.Values {
	merged := make(url.Values, len(base)+len(filter))
	for key, vals := range base {
		for _, v := range vals {
			merged.Add(key, v)
		}
	}
	for key, vals := range filter {
		for _, v := range vals {
			merged.Add(key, v)
		}
	}
	return merged
}

// listAndFilter paginates the issue-list route for the given native state and
// returns issues whose derived state is in keep. Pull requests are skipped,
// DisplayID is qualified to owner/repo#N, and Comments is nil on every issue.
//
// State filtering is client-side; the configured labels are never pushed into
// the Gitea labels query parameter, whose AND semantics and silent drop on an
// unresolvable name make it unusable for correctness-critical filtering.
//
// When applyFilter is true and an operator query_filter is configured, its
// params are merged into the request; when false, the request carries only the
// adapter-owned params.
func (a *GiteaAdapter) listAndFilter(ctx context.Context, nativeState string, keep map[string]struct{}, applyFilter bool) ([]domain.Issue, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues"
	params := url.Values{
		"state": {nativeState},
		"type":  {"issues"},
		"limit": {"50"},
	}
	if applyFilter && len(a.queryFilter) > 0 {
		params = mergeQueryParams(params, a.queryFilter)
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

// resolveLabelIndex pages the repository label catalog to exhaustion and returns
// a case-insensitive name-to-id map keyed by the lowercased label name.
//
// Paging to exhaustion is required: a first-page-only resolver would miss a
// label defined on a later page and then either fail to resolve a real label or
// spuriously create a duplicate. A page decode failure returns
// [domain.ErrTrackerPayload].
func (a *GiteaAdapter) resolveLabelIndex(ctx context.Context) (map[string]int64, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/labels"
	params := url.Values{"limit": {"50"}}

	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]giteaLabel, error) {
		var raw []giteaLabel
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse labels response",
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

	labels, err := paginator.All(ctx)
	if err != nil {
		return nil, err
	}

	index := make(map[string]int64, len(labels))
	for _, l := range labels {
		index[strings.ToLower(l.Name)] = l.ID
	}
	return index, nil
}

// ensureLabelID resolves lowered against index and returns its label id. When
// the label is absent it creates the label with the default color and returns
// the new id.
//
// Gitea rejects a create without a color, so a fixed neutral color accompanies
// every create. A create failure returns the classifier-mapped error directly.
// index is updated with the created id on success.
func (a *GiteaAdapter) ensureLabelID(ctx context.Context, index map[string]int64, lowered string) (int64, error) {
	if id, ok := index[lowered]; ok {
		return id, nil
	}

	payload, err := json.Marshal(map[string]string{"name": lowered, "color": "cccccc"})
	if err != nil {
		return 0, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal create-label payload",
			Err:     err,
		}
	}

	path := "/repos/" + a.owner + "/" + a.repo + "/labels"
	body, err := a.client.Send(ctx, "POST", path, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}

	var created giteaLabel
	if err := json.Unmarshal(body, &created); err != nil {
		return 0, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse create-label response",
			Err:     err,
		}
	}

	index[lowered] = created.ID
	return created.ID, nil
}

// TransitionIssue moves an issue to targetState by swapping its state label and
// reconciling the native open/closed status. A target that is not a configured
// active, terminal, or handoff state returns [domain.ErrTrackerPayload] before
// any request; an index resolving to a pull request returns
// [domain.ErrTrackerNotFound].
//
// Gitea has no transition API, so the move is composed from label and state
// edits: the current state label is removed by id, the target label is resolved
// or created and attached by id, and a terminal or active target patches the
// native state. Every step is idempotent, so a partial failure converges on
// retry, and a no-op transition performs no label work.
func (a *GiteaAdapter) TransitionIssue(ctx context.Context, issueID, targetState string) error {
	targetLower := strings.ToLower(targetState)

	return trackermetrics.Track(a.metrics, "transition", func() error {
		isHandoffTarget := a.handoffState != "" && targetLower == a.handoffState
		if !slices.Contains(a.activeStates, targetLower) &&
			!slices.Contains(a.terminalStates, targetLower) &&
			!isHandoffTarget {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("invalid target state: %q is not a configured active, terminal, or handoff state", targetState),
			}
		}

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

		currentLabel := findCurrentStateLabel(gi.Labels, a.activeStates, a.terminalStates, a.handoffState)

		if currentLabel != targetLower {
			index, err := a.resolveLabelIndex(ctx)
			if err != nil {
				return err
			}

			if currentLabel != "" {
				if currentID, ok := index[currentLabel]; ok {
					labelPath := basePath + "/labels/" + strconv.FormatInt(currentID, 10)
					if err := a.client.SendNoBody(ctx, "DELETE", labelPath); err != nil && !domain.IsNotFound(err) {
						return err
					}
				}
			}

			targetID, err := a.ensureLabelID(ctx, index, targetLower)
			if err != nil {
				return err
			}

			payload, err := json.Marshal(map[string][]int64{"labels": {targetID}})
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

		if slices.Contains(a.terminalStates, targetLower) && gi.State == "open" {
			payload, err := json.Marshal(map[string]string{"state": "closed"})
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
		} else if slices.Contains(a.activeStates, targetLower) && gi.State == "closed" {
			payload, err := json.Marshal(map[string]string{"state": "open"})
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

// CommentIssue posts text as a Markdown comment on the issue. The text is sent
// verbatim with no conversion, and the created-comment response is ignored.
func (a *GiteaAdapter) CommentIssue(ctx context.Context, issueID, text string) error {
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

// AddLabel attaches label to the issue, resolving or creating the label id
// first. The label name is lowercased to match the read path, and the attach is
// additive, so existing labels are preserved and no read-modify-write occurs.
//
// The label is attached by id, never by name: Gitea silently ignores an unknown
// name on the attach route, so an unresolved label would otherwise be a silent
// no-op instead of a created-then-attached label.
func (a *GiteaAdapter) AddLabel(ctx context.Context, issueID, label string) error {
	return trackermetrics.Track(a.metrics, "add_label", func() error {
		lowered := strings.ToLower(label)

		index, err := a.resolveLabelIndex(ctx)
		if err != nil {
			return err
		}

		labelID, err := a.ensureLabelID(ctx, index, lowered)
		if err != nil {
			return err
		}

		path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/labels"
		payload, err := json.Marshal(map[string][]int64{"labels": {labelID}})
		if err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal label payload",
				Err:     err,
			}
		}

		if _, err := a.client.Send(ctx, "POST", path, bytes.NewReader(payload)); err != nil {
			return err
		}
		return nil
	})
}
