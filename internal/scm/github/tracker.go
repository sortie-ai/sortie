// Package github provides GitHub SCM adapter implementations for Sortie.
// The package includes [GitHubAdapter], which implements [domain.TrackerAdapter]
// against the GitHub Issues and Labels REST API, and [GitHubCIProvider], which
// implements [domain.CIStatusProvider] against the GitHub Checks API. Both
// adapters share the internal HTTP transport and ETag cache ([etagCache]).
// Start with [NewGitHubAdapter] for tracker integration or
// [NewGitHubCIProvider] for CI status queries.
//
// GitHub has no native workflow states, only open and closed, so the
// adapter derives Sortie states from labels through
// [issuekit.LabelStateModel]. An open issue that carries no configured
// state label reads no state and is never a candidate; a closed issue
// reads a terminal state.
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
	"github.com/sortie-ai/sortie/internal/issuekit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("github", NewGitHubAdapter, registry.TrackerMeta{
		RequiresProject:        true,
		RequiresAPIKey:         true,
		RequiresTerminalStates: true,
		ValidateTrackerConfig:  validateConfig,
		BlockerSource:          registry.BlockersPerIssue,
	})
}

// Compile-time interface satisfaction checks.
var (
	_ domain.TrackerAdapter      = (*GitHubAdapter)(nil)
	_ domain.CommentChangeReader = (*GitHubAdapter)(nil)

	_ domain.CommentAuthorAccessReader = (*GitHubAdapter)(nil)
	_ domain.ProjectLinker             = (*GitHubAdapter)(nil)
)

// maxPages is the upper bound on paginated fetches. At 50 items per
// page this allows up to 10,000 results before the adapter stops and
// returns what it has.
const maxPages = 200

// GitHubAdapter implements [domain.TrackerAdapter] against the GitHub
// REST API. Safe for concurrent use.
type GitHubAdapter struct {
	*issuekit.StateSource
	client      *httpkit.Client
	owner       string
	repo        string
	queryFilter string
	projectURL  string
	metrics     domain.Metrics
	etagCache   *etagCache
	model       *issuekit.LabelStateModel
	access      *scmcore.WriteAccessCache
}

// NewGitHubAdapter creates a [GitHubAdapter] from adapter configuration.
// Required config keys: "api_key" (personal access token or fine-grained
// token), "project" (owner/repo format). Optional: "endpoint" (defaults
// to https://api.github.com; a value that does not parse as an absolute
// http or https URL with a host returns a [*domain.TrackerError] of kind
// [domain.ErrTrackerPayload]), "active_states", "terminal_states",
// "handoff_state" (single state name recognized as a valid transition
// target and state label), "in_progress_state"; these set the initial
// state vocabulary, with no default, until SetTrackerStates installs the
// workflow's live one. Also optional: "query_filter", "user_agent",
// "etag_cache_size" (int, default 1000; set to 0 to disable conditional
// requests, state reads and the comment change read alike).
func NewGitHubAdapter(config map[string]any) (domain.TrackerAdapter, error) {
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

	endpointRaw, fault := typeutil.StringField(config, "endpoint")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	endpoint, redactedEndpoint, endpointOK := resolveEndpoint(endpointRaw)
	if !endpointOK {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("github: endpoint %q is not a valid absolute http(s) url", redactedEndpoint),
		}
	}

	states, err := issuekit.TrackerStatesFromConfig(config)
	if err != nil {
		return nil, err
	}
	source := issuekit.NewStateSource(states)

	queryFilter, fault := typeutil.StringField(config, "query_filter")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}

	userAgent, fault := typeutil.StringField(config, "user_agent")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
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

	adapter := &GitHubAdapter{
		StateSource: source,
		client:      newGitHubClient(endpoint, apiKey, userAgent),
		owner:       owner,
		repo:        repo,
		queryFilter: queryFilter,
		projectURL:  webBase(endpoint) + "/" + owner + "/" + repo,
		etagCache:   newETagCache(etagCacheSize),
		model:       issuekit.NewLabelStateModel(source, slog.Default()),
	}
	adapter.access = scmcore.NewWriteAccessCache(adapter.lookupWriteAccess)
	return adapter, nil
}

// ProjectURL returns the web address of the configured repository.
func (a *GitHubAdapter) ProjectURL() string {
	return a.projectURL
}

// FetchCandidateIssues returns the open issues that carry a label of a
// configured active state for the configured repository, and none, with no
// request, when no active state is configured. When queryFilter is empty,
// uses the issues endpoint with client-side state filtering. When queryFilter
// is set, routes through the search endpoint for server-side filtering.
// Comments are set to nil on all returned issues.
func (a *GitHubAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	states := a.model.Snapshot()
	issues := make([]domain.Issue, 0)
	if len(states.Active) == 0 {
		return issues, nil
	}

	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		a.model.CheckActiveLabels(ctx, states, func(ctx context.Context) ([]string, error) {
			return readRepositoryLabelNames(ctx, a.client, a.owner, a.repo)
		})

		var fetchErr error
		if a.queryFilter != "" {
			issues, fetchErr = a.fetchCandidatesViaSearch(ctx, states)
			return fetchErr
		}
		issues, fetchErr = a.fetchCandidatesViaIssues(ctx, states)
		return fetchErr
	})
	return issues, err
}

func (a *GitHubAdapter) fetchCandidatesViaIssues(ctx context.Context, states issuekit.LabelStates) ([]domain.Issue, error) {
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
			issue := normalizeIssue(gi, a.model, states)
			a.qualifyDisplayID(&issue)
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

	open, err := paginator.All(ctx)
	if err != nil {
		return nil, err
	}
	return a.model.Candidates(states, open), nil
}

func (a *GitHubAdapter) fetchCandidatesViaSearch(ctx context.Context, states issuekit.LabelStates) ([]domain.Issue, error) {
	q := fmt.Sprintf("repo:%s/%s type:issue state:open %s", a.owner, a.repo, a.queryFilter)
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
				slog.Int("total_count", page.TotalCount))
		}

		issues := make([]domain.Issue, 0, len(page.Items))
		for _, gi := range page.Items {
			if isPullRequest(gi) {
				continue
			}
			issue := normalizeIssue(gi, a.model, states)
			a.qualifyDisplayID(&issue)
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

	open, err := paginator.All(ctx)
	if err != nil {
		return nil, err
	}
	return a.model.Candidates(states, open), nil
}

// FetchIssueByID returns a fully populated issue including comments,
// blockers, and parent. The issueID is the issue number as a string.
func (a *GitHubAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	states := a.model.Snapshot()
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

		fetchedIssue := normalizeIssue(gi, a.model, states)
		a.qualifyDisplayID(&fetchedIssue)

		fetchedIssue.BlockedBy, err = a.fetchBlockers(ctx, issueID, states)
		if err != nil {
			return err
		}
		fetchedIssue.BlockersUnresolved = false

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

// FetchIssueBlockers returns the blockers of one issue, read from the
// forge's dependencies route.
func (a *GitHubAdapter) FetchIssueBlockers(ctx context.Context, issueID string) ([]domain.BlockerRef, error) {
	blockers := make([]domain.BlockerRef, 0)
	states := a.model.Snapshot()
	err := trackermetrics.Track(a.metrics, "fetch_blockers", func() error {
		var fetchErr error
		blockers, fetchErr = a.fetchBlockers(ctx, issueID, states)
		return fetchErr
	})
	return blockers, err
}

func (a *GitHubAdapter) fetchBlockers(ctx context.Context, issueID string, states issuekit.LabelStates) ([]domain.BlockerRef, error) {
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
		return nil, err
	}

	return normalizeBlockers(blockers, a.model, states, a.owner, a.repo), nil
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

	snapshot := a.model.Snapshot()
	stateSet := make(map[string]struct{}, len(states))
	for _, s := range states {
		stateSet[strings.ToLower(s)] = struct{}{}
	}

	// Partition requested states into open-fetch vs closed-search.
	// Non-terminal states (active and unknown) route through the issues
	// endpoint (state=open). An unknown state label on a closed issue
	// will not be found; this is intentional: only configured terminal
	// states warrant the closed-issue search path.
	var requestedTerminal []string
	needOpenFetch := false
	for s := range stateSet {
		if isTerminalState(s, snapshot.Terminal) {
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
			issues, err := a.fetchOpenIssuesByStates(ctx, stateSet, seen, snapshot)
			if err != nil {
				return err
			}
			matchedIssues = append(matchedIssues, issues...)
		}

		for _, label := range requestedTerminal {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			issues, err := a.fetchClosedIssuesByLabel(ctx, label, seen, snapshot)
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

func (a *GitHubAdapter) fetchOpenIssuesByStates(ctx context.Context, stateSet map[string]struct{}, seen map[string]struct{}, states issuekit.LabelStates) ([]domain.Issue, error) {
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
			issue := normalizeIssue(gi, a.model, states)
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

func (a *GitHubAdapter) fetchClosedIssuesByLabel(ctx context.Context, label string, seen map[string]struct{}, states issuekit.LabelStates) ([]domain.Issue, error) {
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
			issue := normalizeIssue(gi, a.model, states)
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
		reads, err := a.fetchStatesByNumbers(ctx, issueIDs)
		if err != nil {
			return err
		}
		states = issuekit.StatesOf(reads)
		return nil
	})
	return states, err
}

// FetchIssueStatesAndLabelsByIDs returns the current state and labels for
// each requested issue ID. A conditional-request hit reports the labels the
// cache holds for the issue. The query filter is not applied.
func (a *GitHubAdapter) FetchIssueStatesAndLabelsByIDs(ctx context.Context, issueIDs []string) (map[string]domain.IssueStateAndLabels, error) {
	var reads map[string]domain.IssueStateAndLabels
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		fetched, err := a.fetchStatesByNumbers(ctx, issueIDs)
		if err != nil {
			return err
		}
		reads = fetched
		return nil
	})
	return reads, err
}

// FetchIssueStatesByIdentifiers returns the current state for each
// requested issue identifier. Since ID and Identifier are both the
// issue number, this delegates to [GitHubAdapter.fetchStatesByNumbers].
func (a *GitHubAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		reads, err := a.fetchStatesByNumbers(ctx, identifiers)
		if err != nil {
			return err
		}
		states = issuekit.StatesOf(reads)
		return nil
	})
	return states, err
}

func (a *GitHubAdapter) fetchStatesByNumbers(ctx context.Context, numbers []string) (map[string]domain.IssueStateAndLabels, error) {
	if len(numbers) == 0 {
		return map[string]domain.IssueStateAndLabels{}, nil
	}

	snapshot := a.model.Snapshot()
	return issuekit.ReadStates(ctx, numbers, func(ctx context.Context, num string) (domain.IssueStateAndLabels, bool, error) {
		return a.readState(ctx, snapshot, num)
	})
}

// readState reads one issue's state and labels, reporting false for an
// issue that does not exist or is a pull request.
func (a *GitHubAdapter) readState(ctx context.Context, snapshot issuekit.LabelStates, num string) (domain.IssueStateAndLabels, bool, error) {
	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(num)
	etag, cachedLabels, cachedClosed, cacheHit := a.etagCache.lookup(path)

	body, responseETag, notModified, err := a.client.GetConditional(ctx, path, etag, nil)
	if err != nil {
		if domain.IsNotFound(err) {
			return domain.IssueStateAndLabels{}, false, nil
		}
		return domain.IssueStateAndLabels{}, false, err
	}

	if notModified && cacheHit {
		a.etagCache.touch(path)
		return domain.IssueStateAndLabels{
			State:  a.model.Derive(snapshot, num, cachedLabels, cachedClosed),
			Labels: issuekit.NormalizeLabels(cachedLabels),
		}, true, nil
	}

	var gi githubIssue
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
	labels := githubLabelNames(gi.Labels)
	closed := isClosed(gi)
	if responseETag != "" {
		a.etagCache.put(path, responseETag, labels, closed)
	}
	return domain.IssueStateAndLabels{
		State:  a.model.Derive(snapshot, num, labels, closed),
		Labels: issuekit.NormalizeLabels(labels),
	}, true, nil
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

// commentPageSize is GitHub's maximum page size for the comments route.
const commentPageSize = 100

// commentPage is what a comment read remembers about one page: the ETag
// GitHub answered with and how many comments the page held.
type commentPage struct {
	ETag  string `json:"etag"`
	Count int    `json:"count"`
}

// FetchIssueCommentChanges reads the issue's comments page by page, each
// request conditional on the ETag the point carries, so a page that did not
// change answers 304 and contributes no comments. Comments holds every
// comment on a page that answered 200. With conditional requests disabled
// every read is full and Next is zero.
func (a *GitHubAdapter) FetchIssueCommentChanges(ctx context.Context, issueID string, since domain.CommentReadPoint) (domain.CommentChanges, error) {
	var changes domain.CommentChanges
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		var readErr error
		changes, readErr = a.readCommentChanges(ctx, issueID, since)
		return readErr
	})
	if err != nil {
		return domain.CommentChanges{}, err
	}
	return changes, nil
}

func (a *GitHubAdapter) readCommentChanges(ctx context.Context, issueNumber string, since domain.CommentReadPoint) (domain.CommentChanges, error) {
	conditional := a.etagCache.maxSize > 0
	var stored []commentPage
	if conditional {
		stored = decodeCommentPoint(since)
	}

	path := "/repos/" + a.owner + "/" + a.repo + "/issues/" + url.PathEscape(issueNumber) + "/comments"
	var changed []githubComment
	var pages []commentPage
	for number := 1; number <= maxPages; number++ {
		var validator string
		if number <= len(stored) {
			validator = stored[number-1].ETag
		}
		params := url.Values{
			"per_page": {strconv.Itoa(commentPageSize)},
			"page":     {strconv.Itoa(number)},
		}

		body, etag, notModified, err := a.client.GetConditional(ctx, path, validator, params)
		if err != nil {
			if domain.IsNotFound(err) {
				return domain.CommentChanges{}, &domain.TrackerError{
					Kind:    domain.ErrTrackerNotFound,
					Message: fmt.Sprintf("issue not found: %s", issueNumber),
				}
			}
			return domain.CommentChanges{}, err
		}

		var page commentPage
		if notModified {
			if validator == "" {
				return domain.CommentChanges{}, &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: "comments page answered not modified to an unconditional request",
				}
			}
			page = stored[number-1]
		} else {
			var items []githubComment
			if err := json.Unmarshal(body, &items); err != nil {
				return domain.CommentChanges{}, &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: "failed to parse comments response",
					Err:     err,
				}
			}
			changed = append(changed, items...)
			page = commentPage{ETag: etag, Count: len(items)}
		}
		pages = append(pages, page)

		// The page after a full page is always requested: the ETag follows
		// the response body alone, so a full last page keeps its ETag when a
		// new comment opens the next page.
		if page.Count < commentPageSize {
			break
		}
		if number == maxPages {
			slog.Warn("pagination limit reached",
				slog.Int("max_pages", maxPages),
				slog.String("endpoint", "/repos/{owner}/{repo}/issues/{issue_id}/comments"))
		}
	}

	changes := domain.CommentChanges{Comments: normalizeComments(changed)}
	if conditional {
		changes.Next = encodeCommentPoint(pages)
	}
	return changes, nil
}

// decodeCommentPoint returns the pages a point lists, and none for a point
// that is empty or that another adapter returned.
func decodeCommentPoint(point domain.CommentReadPoint) []commentPage {
	var pages []commentPage
	if err := json.Unmarshal([]byte(point), &pages); err != nil {
		return nil
	}
	return pages
}

func encodeCommentPoint(pages []commentPage) domain.CommentReadPoint {
	// Marshaling strings and integers cannot fail.
	encoded, _ := json.Marshal(pages)
	return domain.CommentReadPoint(encoded)
}

// TransitionIssue moves an issue to the specified target state. The target
// label goes on first, the labels of other configured states come off, and
// a close follows the label writes, so a failing write leaves the issue in
// its starting state or the target and never in no state. Returns
// [domain.ErrTrackerPayload] for a target outside the configured states and
// for a handoff target on a closed issue.
func (a *GitHubAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	return trackermetrics.Track(a.metrics, "transition", func() error {
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

		plan, err := a.model.Snapshot().PlanTransition(targetState, githubLabelNames(gi.Labels), isClosed(gi))
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

// setIssueClosed closes the issue as completed, or reopens it.
func (a *GitHubAdapter) setIssueClosed(ctx context.Context, issuePath string, closed bool) error {
	payload := `{"state":"open"}`
	if closed {
		payload = `{"state":"closed","state_reason":"completed"}`
	}
	_, err := a.client.Send(ctx, "PATCH", issuePath, strings.NewReader(payload))
	return err
}

// CommentIssue posts a Markdown comment on the specified issue and returns
// the created comment's identity, the decimal form of its numeric id.
// GitHub natively accepts Markdown, so no format conversion is needed.
func (a *GitHubAdapter) CommentIssue(ctx context.Context, issueID string, text string) (string, error) {
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

func (a *GitHubAdapter) postComment(ctx context.Context, issueID, body string) (string, error) {
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

	var created githubComment
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

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter
// operates without recording metrics. Safe to call before any
// adapter operations. Not safe to call concurrently with adapter
// operations.
func (a *GitHubAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// AddLabel adds a label to the specified issue via the GitHub Labels API and
// confirms it against the issue's labels. Returns [domain.ErrTrackerPayload]
// when the label is blank or the issue does not carry it afterwards.
func (a *GitHubAdapter) AddLabel(ctx context.Context, issueID string, label string) error {
	return trackermetrics.Track(a.metrics, "add_label", func() error {
		return issuekit.AddIssueLabel(ctx, label, a.labelOps(issueID))
	})
}

// RemoveLabel removes every label of the issue that names label, ignoring
// letter case, and confirms the removal against the issue's labels. An issue
// that carries none receives no write. Returns [domain.ErrTrackerPayload]
// when the label is blank or the issue still carries it afterwards.
func (a *GitHubAdapter) RemoveLabel(ctx context.Context, issueID string, label string) error {
	return trackermetrics.Track(a.metrics, "remove_label", func() error {
		return issuekit.RemoveIssueLabel(ctx, label, a.labelOps(issueID))
	})
}

func (a *GitHubAdapter) labelOps(issueID string) issuekit.IssueLabelOps {
	return githubIssueLabelOps(a.client, a.owner, a.repo, issueID)
}
