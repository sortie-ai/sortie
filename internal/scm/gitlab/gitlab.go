// Package gitlab implements [domain.TrackerAdapter] for the GitLab REST API
// v4. It talks to GitLab.com or a self-managed instance over the shared
// HTTP transport with no third-party GitLab client library, normalizing
// issue responses to domain types: IDs derived from the project-scoped iid
// rather than the instance-global id, labels lowercased, priority always
// nil (GitLab issues carry no priority field), parent always nil (GitLab
// exposes no sub-issue relationship on this route), state derived from
// labels against the configured active, terminal, and handoff lists, and
// blocked_by always a non-nil empty slice (GitLab Community Edition has no
// blocks relation type).
//
// The constructor runs a two-call preflight, an advisory token
// introspection followed by an authoritative project check, that fails
// construction on an unreachable project, so a misconfiguration surfaces
// at startup rather than on the first poll. Registered under kind "gitlab"
// via an init function.
//
// This package sits under the source-control adapter family, but this
// stage implements only the tracker contract: the source-control and CI
// roles the family's path implies are not implemented here.
package gitlab

import (
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
	registry.Trackers.RegisterWithMeta("gitlab", NewGitLabAdapter, registry.TrackerMeta{
		RequiresProject:       true,
		RequiresAPIKey:        true,
		DefaultActiveStates:   defaultActiveStates,
		DefaultTerminalStates: defaultTerminalStates,
	})
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*GitLabAdapter)(nil)

// maxPages is the upper bound on paginated fetches. At 100 items per page
// this allows up to 20,000 results before the adapter stops and returns
// what it has.
const maxPages = 200

// stateBatchSize is the number of distinct iids requested per iids[] batch
// in the state-reconciliation routes. It is chosen to sit far below any
// plausible front-end request-line limit rather than close to a measured
// one.
const stateBatchSize = 50

// defaultActiveStates is applied when the config omits active_states.
var defaultActiveStates = []string{"backlog", "in-progress", "review"}

// defaultTerminalStates is applied when the config omits terminal_states.
var defaultTerminalStates = []string{"done", "wontfix"}

// GitLabAdapter implements [domain.TrackerAdapter] against the GitLab REST
// API v4. All fields except metrics are set once at construction and never
// mutated, so the adapter is safe for concurrent use.
type GitLabAdapter struct {
	client         *httpkit.Client
	project        string
	projectPath    string
	activeStates   []string
	terminalStates []string
	handoffState   string
	log            *slog.Logger
	metrics        domain.Metrics
}

// NewGitLabAdapter creates a [GitLabAdapter] from adapter configuration and
// runs a construction-time preflight against the instance.
//
// Required config keys: "api_key" (a GitLab access token) and "project" (a
// numeric project ID or a namespace path such as "group/project").
// Optional: "endpoint" (instance base URL, defaulting to
// "https://gitlab.com"), "active_states", "terminal_states",
// "handoff_state" (label names, lowercased), and "user_agent". A
// non-empty "query_filter" is rejected, because this adapter does not
// support it yet. A missing or malformed key, or a preflight failure
// (invalid token or an unreachable project), returns a
// [*domain.TrackerError] and blocks construction.
func NewGitLabAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	apiKey, _ := config["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "missing required config key: api_key",
		}
	}

	projectRaw, _ := config["project"].(string)
	project := strings.TrimSpace(projectRaw)
	if project == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerProject,
			Message: "missing required config key: project",
		}
	}

	queryFilterRaw, _ := config["query_filter"].(string)
	if strings.TrimSpace(queryFilterRaw) != "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "gitlab: tracker.query_filter is not supported yet; remove the key or leave it empty",
		}
	}

	endpointRaw, _ := config["endpoint"].(string)
	endpoint := strings.TrimSpace(endpointRaw)
	if endpoint == "" {
		endpoint = "https://gitlab.com"
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") || parsedEndpoint.Host == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("gitlab: tracker.endpoint %q is not a valid absolute http(s) url", endpoint),
		}
	}
	baseURL := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(baseURL, "/api/v4") {
		baseURL += "/api/v4"
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

	projectPath := url.PathEscape(project)
	client := newGitLabClient(baseURL, apiKey, userAgent)
	log := slog.Default()

	if err := runPreflight(context.Background(), client, projectPath, log); err != nil {
		return nil, err
	}

	return &GitLabAdapter{
		client:         client,
		project:        project,
		projectPath:    projectPath,
		activeStates:   activeStates,
		terminalStates: terminalStates,
		handoffState:   handoffState,
		log:            log,
	}, nil
}

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter
// operates without recording metrics. Safe to call before any adapter
// operations. Not safe to call concurrently with adapter operations.
func (a *GitLabAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// paginateIssues fetches every page of an issue-array route through the
// Link-header paginator and returns the accumulated raw issues. An absent
// Link header, or a final page carrying no rel="next", is the normal end
// of results, never a missing-cursor error, because GitLab exposes no
// cursor the adapter carries itself.
//
// A decode failure returns [domain.ErrTrackerPayload].
func (a *GitLabAdapter) paginateIssues(ctx context.Context, path string, params url.Values) ([]gitlabIssue, error) {
	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]gitlabIssue, error) {
		var raw []gitlabIssue
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

// paginateNotes fetches every page of a notes-array route through the
// Link-header paginator and returns the accumulated raw notes. Unlike the
// Gitea comments route, the GitLab notes route paginates and must be
// routed through the paginator.
//
// A decode failure returns [domain.ErrTrackerPayload].
func (a *GitLabAdapter) paginateNotes(ctx context.Context, path string, params url.Values) ([]gitlabNote, error) {
	paginator := httpkit.NewLinkPaginator(a.client, path, params, func(body []byte) ([]gitlabNote, error) {
		var raw []gitlabNote
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse notes response",
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

// parseIID parses raw as the shared identifier guard used identically by
// the single-fetch, comment-fetch, and batch paths. It trims surrounding
// whitespace, then accepts only a base-10, unsigned, non-fractional,
// non-zero-padded positive integer; a bare "0" is rejected as well,
// because no issue has iid 0. It issues no request; callers must check ok
// before building a request path.
func parseIID(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	if trimmed[0] == '0' && len(trimmed) > 1 {
		return 0, false
	}
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return 0, false
		}
	}

	n, err := strconv.Atoi(trimmed)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// listAndFilter paginates the issue-list route for the given native state
// and returns issues whose derived state is in keep. Comments is nil on
// every returned issue.
//
// State filtering stays client-side: the configured state labels are
// never pushed into the labels query parameter, whose AND semantics and
// lack of an OR filter make it unusable for correctness-critical
// filtering.
func (a *GitLabAdapter) listAndFilter(ctx context.Context, nativeState string, keep map[string]bool) ([]domain.Issue, error) {
	path := "/projects/" + a.projectPath + "/issues"
	params := url.Values{
		"state":      {nativeState},
		"issue_type": {"issue"},
		"scope":      {"all"},
		"per_page":   {"100"},
		"order_by":   {"created_at"},
		"sort":       {"asc"},
	}

	raw, err := a.paginateIssues(ctx, path, params)
	if err != nil {
		return nil, err
	}

	out := make([]domain.Issue, 0, len(raw))
	for _, gi := range raw {
		if gi.IssueType != "" && gi.IssueType != "issue" {
			continue
		}
		issue := normalizeIssue(gi, a.project, a.activeStates, a.terminalStates, a.handoffState, a.log)
		if !keep[issue.State] {
			continue
		}
		issue.Comments = nil
		out = append(out, issue)
	}
	return out, nil
}

// FetchCandidateIssues returns opened issues whose derived state is in the
// configured active states, oldest-first as ordered by the server, with
// Comments nil on every returned issue.
func (a *GitLabAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		keep := make(map[string]bool, len(a.activeStates))
		for _, s := range a.activeStates {
			keep[s] = true
		}

		fetched, fetchErr := a.listAndFilter(ctx, "opened", keep)
		if fetchErr != nil {
			return fetchErr
		}
		issues = fetched
		return nil
	})
	return issues, err
}

// FetchIssuesByStates returns issues whose derived state is in the
// requested set. It returns an empty slice with no request when states is
// empty.
//
// The requested states are partitioned by whether they are terminal: the
// opened listing runs when any requested state is non-terminal, and the
// closed listing runs when any requested state is terminal. Every opened
// result precedes every closed result, and no merge sort is applied. This
// method has no production caller; it exists to satisfy
// [domain.TrackerAdapter].
func (a *GitLabAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	requested := make(map[string]bool, len(states))
	for _, s := range states {
		requested[strings.ToLower(s)] = true
	}

	terminalSet := make(map[string]bool, len(a.terminalStates))
	for _, s := range a.terminalStates {
		terminalSet[s] = true
	}

	needOpened := false
	needClosed := false
	for s := range requested {
		if terminalSet[s] {
			needClosed = true
		} else {
			needOpened = true
		}
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		result := make([]domain.Issue, 0)

		if needOpened {
			opened, fetchErr := a.listAndFilter(ctx, "opened", requested)
			if fetchErr != nil {
				return fetchErr
			}
			result = append(result, opened...)
		}

		if needClosed {
			closed, fetchErr := a.listAndFilter(ctx, "closed", requested)
			if fetchErr != nil {
				return fetchErr
			}
			result = append(result, closed...)
		}

		issues = result
		return nil
	})
	return issues, err
}

// fetchComments returns comments for the issue identified by issueID,
// which is guarded through [parseIID] before any request is issued. A
// rejected issueID, or a 404 from the notes route, returns
// [domain.ErrTrackerNotFound].
func (a *GitLabAdapter) fetchComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	n, ok := parseIID(issueID)
	if !ok {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("issue not found: %s", issueID),
		}
	}

	path := "/projects/" + a.projectPath + "/issues/" + strconv.Itoa(n) + "/notes"
	params := url.Values{
		"activity_filter": {"only_comments"},
		"sort":            {"asc"},
		"per_page":        {"100"},
	}

	raw, err := a.paginateNotes(ctx, path, params)
	if err != nil {
		return nil, err
	}
	return normalizeComments(raw), nil
}

// FetchIssueByID returns a fully populated issue including comments.
// issueID is the issue iid as a decimal string, guarded through
// [parseIID] before any request is issued. A rejected issueID, a missing
// iid, or an entity whose issue_type is set and is not "issue" returns
// [domain.ErrTrackerNotFound].
//
// Parent is always nil and BlockedBy a non-nil empty slice; no parent or
// links request is issued.
func (a *GitLabAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		n, ok := parseIID(issueID)
		if !ok {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueID),
			}
		}
		decimal := strconv.Itoa(n)

		path := "/projects/" + a.projectPath + "/issues/" + decimal
		body, _, getErr := a.client.Get(ctx, path, nil)
		if getErr != nil {
			if domain.IsNotFound(getErr) {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerNotFound,
					Message: fmt.Sprintf("issue not found: %s", issueID),
				}
			}
			return getErr
		}

		var gi gitlabIssue
		if err := json.Unmarshal(body, &gi); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse issue response",
				Err:     err,
			}
		}
		if gi.IssueType != "" && gi.IssueType != "issue" {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("not an issue: %s", issueID),
			}
		}

		fetched := normalizeIssue(gi, a.project, a.activeStates, a.terminalStates, a.handoffState, a.log)

		comments, commentsErr := a.fetchComments(ctx, decimal)
		if commentsErr != nil {
			return commentsErr
		}
		fetched.Comments = comments

		issue = fetched
		return nil
	})
	return issue, err
}

// FetchIssueComments returns comments for the issue identified by
// issueID, oldest-first with system notes excluded. It returns a non-nil
// empty slice when the issue has none, and [domain.ErrTrackerNotFound]
// when issueID fails the identifier guard or the issue does not exist.
func (a *GitLabAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
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

// fetchStatesByIIDs returns the derived state for each requested iid
// string, keyed by the requested value. It returns an empty non-nil map
// with no request when requested is empty or when every value is empty or
// unparseable.
//
// Distinct positive integers are batched into chunks of [stateBatchSize]
// through the iids[] filter. A 404 on this route signals a lost project
// and is returned rather than swallowed; an absent iid simply omits its
// key from the result.
func (a *GitLabAdapter) fetchStatesByIIDs(ctx context.Context, requested []string) (map[string]string, error) {
	if len(requested) == 0 {
		return map[string]string{}, nil
	}

	order := make([]int, 0, len(requested))
	wanted := make(map[int][]string, len(requested))
	for _, value := range requested {
		n, ok := parseIID(value)
		if !ok {
			continue
		}
		if _, seen := wanted[n]; !seen {
			order = append(order, n)
		}
		wanted[n] = append(wanted[n], value)
	}
	if len(order) == 0 {
		return map[string]string{}, nil
	}

	states := make(map[string]string, len(requested))
	for chunkStart := 0; chunkStart < len(order); chunkStart += stateBatchSize {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		chunk := order[chunkStart:min(chunkStart+stateBatchSize, len(order))]

		params := url.Values{
			"state":    {"all"},
			"scope":    {"all"},
			"per_page": {"100"},
		}
		for _, n := range chunk {
			params.Add("iids[]", strconv.Itoa(n))
		}

		raw, err := a.paginateIssues(ctx, "/projects/"+a.projectPath+"/issues", params)
		if err != nil {
			return nil, err
		}

		for _, gi := range raw {
			if gi.IssueType != "" && gi.IssueType != "issue" {
				continue
			}
			keys, ok := wanted[gi.IID]
			if !ok {
				continue
			}
			derived := deriveState(gi.Labels, gi.State, a.activeStates, a.terminalStates, a.handoffState, strconv.Itoa(gi.IID), a.log)
			for _, key := range keys {
				states[key] = derived
			}
		}
	}

	return states, nil
}

// FetchIssueStatesByIDs returns the derived state for each requested iid,
// keyed by the requested string. ID and Identifier are both the iid, so
// this delegates to [GitLabAdapter.fetchStatesByIIDs].
func (a *GitLabAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		fetched, fetchErr := a.fetchStatesByIIDs(ctx, issueIDs)
		if fetchErr != nil {
			return fetchErr
		}
		states = fetched
		return nil
	})
	return states, err
}

// FetchIssueStatesByIdentifiers returns the derived state for each
// requested iid, keyed by the requested string. ID and Identifier are
// both the iid, so this delegates to [GitLabAdapter.fetchStatesByIIDs].
func (a *GitLabAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		fetched, fetchErr := a.fetchStatesByIIDs(ctx, identifiers)
		if fetchErr != nil {
			return fetchErr
		}
		states = fetched
		return nil
	})
	return states, err
}

// TransitionIssue returns [domain.ErrTrackerPayload] and issues no
// request. The GitLab write path is not implemented yet.
func (a *GitLabAdapter) TransitionIssue(_ context.Context, _, _ string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "gitlab: write path is not implemented yet",
	}
}

// CommentIssue returns [domain.ErrTrackerPayload] and issues no request.
// The GitLab write path is not implemented yet.
func (a *GitLabAdapter) CommentIssue(_ context.Context, _, _ string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "gitlab: write path is not implemented yet",
	}
}

// AddLabel returns [domain.ErrTrackerPayload] and issues no request. The
// GitLab write path is not implemented yet.
func (a *GitLabAdapter) AddLabel(_ context.Context, _, _ string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "gitlab: write path is not implemented yet",
	}
}
