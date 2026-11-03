// Package gitea implements [domain.TrackerAdapter] for the Gitea REST API v1.
// It talks to a self-hosted Gitea instance over the shared HTTP transport with
// no third-party Gitea client library, normalizing issue responses to domain
// types: labels lowercased, priority always nil (Gitea exposes no priority
// field), parent always nil (Gitea has no sub-issue concept), state derived
// from labels against the configured active, terminal, and handoff lists (an
// open issue carrying none reads no state and is never a candidate), and
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
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/issuekit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("gitea", NewGiteaAdapter, registry.TrackerMeta{
		RequiresProject:        true,
		RequiresAPIKey:         true,
		ValidateTrackerConfig:  validateConfig,
		RequiresTerminalStates: true,
		BlockerSource:          registry.BlockersPerIssue,
	})
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*GiteaAdapter)(nil)
var _ domain.CommentChangeReader = (*GiteaAdapter)(nil)
var _ domain.CommentAuthorAccessReader = (*GiteaAdapter)(nil)
var _ domain.ProjectLinker = (*GiteaAdapter)(nil)

// maxPages is the upper bound on paginated fetches. At 50 items per page this
// allows up to 10,000 results before the adapter stops and returns what it has.
const maxPages = 200

// GiteaAdapter implements [domain.TrackerAdapter] against the Gitea REST API v1.
// All fields except metrics are set once at construction and never mutated, so
// the adapter is safe for concurrent use. The embedded state source supplies the
// workflow's state lists, which an operation reads once at its start.
type GiteaAdapter struct {
	*issuekit.StateSource
	client      *httpkit.Client
	owner       string
	repo        string
	projectURL  string
	model       *issuekit.LabelStateModel
	queryFilter url.Values // parsed tracker.query_filter; nil or empty when unset
	log         *slog.Logger
	metrics     domain.Metrics
	access      *scmcore.WriteAccessCache
}

// NewGiteaAdapter creates a [GiteaAdapter] from adapter configuration and runs a
// construction-time preflight against the instance.
//
// Required config keys: "api_key" (access token), "project" (owner/repo), and
// "endpoint" (instance base URL, which must parse as an absolute http(s) URL
// with a host; there is no default host). Optional: "active_states",
// "terminal_states", "handoff_state", "in_progress_state" (label names, with no
// default list), and "user_agent". A missing or malformed key, or a preflight failure (invalid
// token or unknown repository), returns a [*domain.TrackerError] and blocks
// construction.
func NewGiteaAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	apiKey, fault := typeutil.StringField(config, "api_key")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	if apiKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "missing required config key: api_key",
		}
	}

	project, fault := typeutil.StringField(config, "project")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
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

	endpoint, fault := typeutil.StringField(config, "endpoint")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	if endpoint == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "tracker.endpoint is required for gitea",
		}
	}
	parsedEndpoint, ok := httpkit.ParseEndpoint(endpoint)
	if !ok {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("gitea: endpoint %q is not a valid absolute http(s) url", parsedEndpoint.Redacted),
		}
	}
	baseURL := parsedEndpoint.Base
	if !strings.HasSuffix(baseURL, "/api/v1") {
		baseURL += "/api/v1"
	}

	trackerStates, err := issuekit.TrackerStatesFromConfig(config)
	if err != nil {
		return nil, err
	}

	userAgent, fault := typeutil.StringField(config, "user_agent")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	// query_filter is read here, ahead of the preflight below, so a
	// mistyped value is reported without depending on network reachability.
	raw, fault := typeutil.StringField(config, "query_filter")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}

	client := newGiteaClient(baseURL, apiKey, userAgent)

	if err := runPreflight(context.Background(), client, owner, repo); err != nil {
		return nil, err
	}

	filter, err := parseGiteaQueryFilter(raw)
	if err != nil {
		return nil, err
	}

	source := issuekit.NewStateSource(trackerStates)
	adapter := &GiteaAdapter{
		StateSource: source,
		client:      client,
		owner:       owner,
		repo:        repo,
		projectURL:  strings.TrimSuffix(parsedEndpoint.Base, "/api/v1") + "/" + owner + "/" + repo,
		model:       issuekit.NewLabelStateModel(source, slog.Default()),
		queryFilter: filter,
		log:         slog.Default(),
	}
	adapter.access = scmcore.NewWriteAccessCache(adapter.lookupWriteAccess)

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

// ProjectURL returns the web address of the configured repository.
func (a *GiteaAdapter) ProjectURL() string {
	return a.projectURL
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
	labels, err := a.paginateLabels(ctx, "/repos/"+a.owner+"/"+a.repo+"/labels")
	if err != nil {
		return nil, err
	}

	names := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		names[l.Name] = struct{}{}
	}
	return names, nil
}

// readCatalogLabelNames returns the names of the labels an issue of this
// repository can carry: the repository's own and its owner's organization
// labels. The organization read is a 404 when the owner is a user, which reads
// as no organization labels.
func (a *GiteaAdapter) readCatalogLabelNames(ctx context.Context) ([]string, error) {
	repoLabels, err := a.paginateLabels(ctx, "/repos/"+a.owner+"/"+a.repo+"/labels")
	if err != nil {
		return nil, err
	}

	orgLabels, err := a.paginateLabels(ctx, "/orgs/"+url.PathEscape(a.owner)+"/labels")
	if err != nil && !domain.IsNotFound(err) {
		return nil, err
	}

	return giteaLabelNames(append(repoLabels, orgLabels...)), nil
}

// paginateLabels pages a label-array route to exhaustion through the Link
// paginator. A page decode failure returns [domain.ErrTrackerPayload].
func (a *GiteaAdapter) paginateLabels(ctx context.Context, path string) ([]giteaLabel, error) {
	paginator := httpkit.NewLinkPaginator(a.client, path, url.Values{"limit": {"50"}}, func(body []byte) ([]giteaLabel, error) {
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

	return paginator.All(ctx)
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
// are skipped and Comments is nil on every returned issue. An empty active list
// returns no issue and sends no request.
//
// Gitea lists newest-first and offers no sort parameter, so the adapter
// re-sorts client-side to hand the orchestrator the same ascending order the
// other trackers produce.
func (a *GiteaAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	states := a.model.Snapshot()
	if len(states.Active) == 0 {
		return []domain.Issue{}, nil
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		a.model.CheckActiveLabels(ctx, states, a.readCatalogLabelNames)

		open, fetchErr := a.listIssues(ctx, "open", states, true)
		if fetchErr != nil {
			return fetchErr
		}

		candidates := a.model.Candidates(states, open)
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

	snapshot := a.model.Snapshot()

	requested := make(map[string]struct{}, len(states))
	for _, s := range states {
		requested[strings.ToLower(s)] = struct{}{}
	}

	needOpen := false
	needClosed := false
	for s := range requested {
		if slices.Contains(snapshot.Terminal, s) {
			needClosed = true
		} else {
			needOpen = true
		}
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		result := make([]domain.Issue, 0)

		if needOpen {
			open, err := a.listIssues(ctx, "open", snapshot, true)
			if err != nil {
				return err
			}
			result = append(result, inStates(open, requested)...)
		}

		if needClosed {
			closed, err := a.listIssues(ctx, "closed", snapshot, false)
			if err != nil {
				return err
			}
			result = append(result, inStates(closed, requested)...)
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

// listIssues paginates the issue-list route for the given native state and
// returns every issue, each derived under states. Pull requests are skipped,
// DisplayID is qualified to owner/repo#N, and Comments is nil on every issue.
//
// State filtering is the caller's, client-side; the configured labels are never
// pushed into the Gitea labels query parameter, whose AND semantics and silent
// drop on an unresolvable name make it unusable for correctness-critical
// filtering.
//
// When applyFilter is true and an operator query_filter is configured, its
// params are merged into the request; when false, the request carries only the
// adapter-owned params.
func (a *GiteaAdapter) listIssues(ctx context.Context, nativeState string, states issuekit.LabelStates, applyFilter bool) ([]domain.Issue, error) {
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

	issues := make([]domain.Issue, 0, len(raw))
	for _, gi := range raw {
		if isPullRequest(gi) {
			continue
		}
		issue := normalizeIssue(gi, a.model, states)
		setDisplayID(&issue, a.owner, a.repo)
		issue.Comments = nil
		issues = append(issues, issue)
	}
	return issues, nil
}

// inStates returns the issues whose state is a key of keep, in order. An issue
// that reads no state is never kept.
func inStates(issues []domain.Issue, keep map[string]struct{}) []domain.Issue {
	kept := make([]domain.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := keep[issue.State]; ok && issue.State != "" {
			kept = append(kept, issue)
		}
	}
	return kept
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
		states := a.model.Snapshot()
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

		fetched := normalizeIssue(gi, a.model, states)
		setDisplayID(&fetched, a.owner, a.repo)

		blockers, err := a.fetchBlockers(ctx, issueID, states)
		if err != nil {
			return err
		}
		fetched.BlockedBy = blockers
		fetched.BlockersUnresolved = false

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

// FetchIssueBlockers returns the blockers of one issue, read from the
// forge's dependencies route.
func (a *GiteaAdapter) FetchIssueBlockers(ctx context.Context, index string) ([]domain.BlockerRef, error) {
	blockers := make([]domain.BlockerRef, 0)
	err := trackermetrics.Track(a.metrics, "fetch_blockers", func() error {
		var fetchErr error
		blockers, fetchErr = a.fetchBlockers(ctx, index, a.model.Snapshot())
		return fetchErr
	})
	return blockers, err
}

// fetchBlockers returns the issues blocking the given issue, read from the
// dependencies route through the Link paginator, each derived under states.
func (a *GiteaAdapter) fetchBlockers(ctx context.Context, index string, states issuekit.LabelStates) ([]domain.BlockerRef, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(index) + "/dependencies"
	params := url.Values{"limit": {"50"}}

	raw, err := a.paginateIssues(ctx, path, params)
	if err != nil {
		return nil, err
	}

	return normalizeBlockers(raw, a.model, states, a.owner, a.repo), nil
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
	raw, err := a.getComments(ctx, index, nil)
	if err != nil {
		return nil, err
	}
	return normalizeComments(raw), nil
}

// getComments issues the one comments request with params and decodes the
// response.
func (a *GiteaAdapter) getComments(ctx context.Context, index string, params url.Values) ([]giteaComment, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(index) + "/comments"

	body, _, err := a.client.Get(ctx, path, params)
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
	return raw, nil
}

// FetchIssueCommentChanges reads the comments in one request carrying the
// route's since filter. The point is the newest update time seen, so the
// filter's whole-second resolution re-reads the comments updated in that
// second.
func (a *GiteaAdapter) FetchIssueCommentChanges(ctx context.Context, issueID string, since domain.CommentReadPoint) (domain.CommentChanges, error) {
	var changes domain.CommentChanges
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		point := decodeCommentPoint(since)
		var params url.Values
		if !point.IsZero() {
			params = url.Values{"since": {point.Truncate(time.Second).Format(time.RFC3339)}}
		}

		raw, err := a.getComments(ctx, issueID, params)
		if err != nil {
			return err
		}

		comments := normalizeComments(raw)
		newest := point
		for _, c := range comments {
			updated, err := c.UpdatedTime()
			if err != nil {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: fmt.Sprintf("comment %s has an unparseable updated_at", c.ID),
					Err:     err,
				}
			}
			if updated.After(newest) {
				newest = updated
			}
		}

		changes = domain.CommentChanges{Comments: comments, Next: encodeCommentPoint(newest)}
		return nil
	})
	if err != nil {
		return domain.CommentChanges{}, err
	}
	return changes, nil
}

// decodeCommentPoint returns the instant a point names, or the zero time for
// an empty or foreign point.
func decodeCommentPoint(p domain.CommentReadPoint) time.Time {
	t, err := time.Parse(time.RFC3339Nano, string(p))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// encodeCommentPoint is the inverse of [decodeCommentPoint]; the zero time
// encodes as the zero point.
func encodeCommentPoint(t time.Time) domain.CommentReadPoint {
	if t.IsZero() {
		return ""
	}
	return domain.CommentReadPoint(t.UTC().Format(time.RFC3339Nano))
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
		states = issuekit.StatesOf(fetched)
		return nil
	})
	return states, err
}

// FetchIssueStatesAndLabelsByIDs returns the derived state and labels for
// each requested issue index, keyed by the requested value. The query filter
// is not applied.
func (a *GiteaAdapter) FetchIssueStatesAndLabelsByIDs(ctx context.Context, ids []string) (map[string]domain.IssueStateAndLabels, error) {
	var reads map[string]domain.IssueStateAndLabels
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		fetched, fetchErr := a.fetchStatesByIndexes(ctx, ids)
		if fetchErr != nil {
			return fetchErr
		}
		reads = fetched
		return nil
	})
	return reads, err
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
		states = issuekit.StatesOf(fetched)
		return nil
	})
	return states, err
}

// fetchStatesByIndexes fetches each issue by index and derives its state and
// labels, keying the result by the requested value. It returns an empty map with no
// request when indexes is empty.
//
// A 404 or a pull-request entry omits that index from the result rather than
// failing. Cancellation is checked before each request. No conditional-request
// cache is used because Gitea sends no ETag.
func (a *GiteaAdapter) fetchStatesByIndexes(ctx context.Context, indexes []string) (map[string]domain.IssueStateAndLabels, error) {
	if len(indexes) == 0 {
		return map[string]domain.IssueStateAndLabels{}, nil
	}

	snapshot := a.model.Snapshot()
	return issuekit.ReadStates(ctx, indexes, func(ctx context.Context, index string) (domain.IssueStateAndLabels, bool, error) {
		return a.readState(ctx, snapshot, index)
	})
}

// readState reads one issue's state and labels, reporting false for an
// issue that does not exist or is a pull request.
func (a *GiteaAdapter) readState(ctx context.Context, snapshot issuekit.LabelStates, index string) (domain.IssueStateAndLabels, bool, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(index)
	body, _, err := a.client.Get(ctx, path, nil)
	if err != nil {
		if domain.IsNotFound(err) {
			return domain.IssueStateAndLabels{}, false, nil
		}
		return domain.IssueStateAndLabels{}, false, err
	}

	var gi giteaIssue
	if err := json.Unmarshal(body, &gi); err != nil {
		return domain.IssueStateAndLabels{}, false, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse issue response",
			Err:     err,
		}
	}
	if isPullRequest(gi) {
		return domain.IssueStateAndLabels{}, false, nil
	}

	labels := giteaLabelNames(gi.Labels)
	return domain.IssueStateAndLabels{
		State:  a.model.Derive(snapshot, index, labels, gi.State == "closed"),
		Labels: issuekit.NormalizeLabels(labels),
	}, true, nil
}

// resolveLabelIndex pages the repository label catalog to exhaustion and returns
// a case-insensitive name-to-id map keyed by the lowercased label name.
//
// Paging to exhaustion is required: a first-page-only resolver would miss a
// label defined on a later page and then either fail to resolve a real label or
// spuriously create a duplicate. A page decode failure returns
// [domain.ErrTrackerPayload].
func (a *GiteaAdapter) resolveLabelIndex(ctx context.Context) (map[string]int64, error) {
	labels, err := a.paginateLabels(ctx, "/repos/"+a.owner+"/"+a.repo+"/labels")
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
// active, terminal, or handoff state, and a handoff target on a closed issue,
// return [domain.ErrTrackerPayload] before any write; an index resolving to a
// pull request returns [domain.ErrTrackerNotFound].
//
// Gitea has no transition API, so the move is composed from label and state
// edits. The target label is resolved or created and attached by id before any
// other state label is removed by the id the issue reports, and a close follows
// the label writes, so the issue always reads the state it started in or the
// target. Every step is idempotent, so a partial failure converges on retry, and
// a no-op transition performs no label work.
func (a *GiteaAdapter) TransitionIssue(ctx context.Context, issueID, targetState string) error {
	return trackermetrics.Track(a.metrics, "transition", func() error {
		states := a.model.Snapshot()
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

		plan, err := states.PlanTransition(targetState, giteaLabelNames(gi.Labels), gi.State == "closed")
		if err != nil {
			return err
		}
		return issuekit.ApplyTransition(ctx, plan, issuekit.TransitionOps{
			Labels: a.labelOps(issueID),
			SetClosed: func(ctx context.Context, closed bool) error {
				return a.setIssueClosed(ctx, basePath, closed)
			},
		})
	})
}

// setIssueClosed patches the native open/closed status of the issue at
// issuePath.
func (a *GiteaAdapter) setIssueClosed(ctx context.Context, issuePath string, closed bool) error {
	state := "open"
	if closed {
		state = "closed"
	}
	payload, err := json.Marshal(map[string]string{"state": state})
	if err != nil {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal state payload",
			Err:     err,
		}
	}
	_, err = a.client.Send(ctx, "PATCH", issuePath, bytes.NewReader(payload))
	return err
}

// CommentIssue posts text as a Markdown comment on the issue and returns the
// created comment's identity, the decimal form of its numeric id. The text is
// sent verbatim with no conversion.
func (a *GiteaAdapter) CommentIssue(ctx context.Context, issueID, text string) (string, error) {
	var commentID string
	err := trackermetrics.Track(a.metrics, "comment", func() error {
		var postErr error
		commentID, postErr = a.postComment(ctx, issueID, text)
		return postErr
	})
	if err != nil {
		return "", err
	}
	return commentID, nil
}

func (a *GiteaAdapter) postComment(ctx context.Context, issueID, body string) (string, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueID) + "/comments"

	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to marshal comment payload",
			Err:     err,
		}
	}

	respBody, err := a.client.Send(ctx, "POST", path, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}

	var created giteaComment
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse comment creation response",
			Err:     err,
		}
	}
	if created.ID == 0 {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "comment creation response carries no id",
		}
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// AddLabel attaches label to the issue, resolving or creating the label id
// first, and confirms it against the issue's labels. The label name is
// lowercased to match the read path, and the attach is additive, so existing
// labels are preserved and no read-modify-write occurs. Returns
// [domain.ErrTrackerPayload] when the label is blank or the issue does not
// carry it afterwards.
//
// The label is attached by id, never by name: Gitea silently ignores an unknown
// name on the attach route, so an unresolved label would otherwise be a silent
// no-op instead of a created-then-attached label.
func (a *GiteaAdapter) AddLabel(ctx context.Context, issueID, label string) error {
	return trackermetrics.Track(a.metrics, "add_label", func() error {
		return issuekit.AddIssueLabel(ctx, label, a.labelOps(issueID))
	})
}

// RemoveLabel removes every label of the issue that names label, ignoring
// letter case, by the ids the issue reports, so an organization label that the
// repository catalog lacks is removed too. The removal is confirmed against the
// issue's labels, and an issue that carries no such label receives no write.
// Returns [domain.ErrTrackerPayload] when the label is blank or the issue still
// carries it afterwards.
func (a *GiteaAdapter) RemoveLabel(ctx context.Context, issueID, label string) error {
	return trackermetrics.Track(a.metrics, "remove_label", func() error {
		return issuekit.RemoveIssueLabel(ctx, label, a.labelOps(issueID))
	})
}

func (a *GiteaAdapter) labelOps(issueID string) issuekit.IssueLabelOps {
	return giteaIssueLabelOps(a.client, a.owner, a.repo, issueID, func(ctx context.Context, lowered string) (int64, error) {
		index, err := a.resolveLabelIndex(ctx)
		if err != nil {
			return 0, err
		}
		return a.ensureLabelID(ctx, index, lowered)
	})
}
