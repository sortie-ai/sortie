// Package linear implements [domain.TrackerAdapter] for Linear over its single
// GraphQL endpoint. Issues are fetched with cursor pagination and normalized to
// domain types: labels lowercased, priority 0 mapped to nil and 1..4 to a
// pointer, state names preserved with original casing, branch names treated as
// opaque, and blocker refs extracted from inverse relations of type "blocks".
//
// Application errors arrive inside HTTP 200 bodies, so the adapter classifies a
// response by its errors array before the HTTP status. The constructor runs a
// credential check and a team-states preflight that validates configured state
// names and caches their canonical casing for the case-sensitive state filter.
// Registered under kind "linear" via an init function.
package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("linear", NewLinearAdapter, registry.TrackerMeta{
		RequiresProject: true,
		RequiresAPIKey:  true,
	})
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*LinearAdapter)(nil)

// defaultEndpoint is the Linear GraphQL endpoint used when the config omits it.
const defaultEndpoint = "https://api.linear.app/graphql"

// topLevelPageSize is the page size for top-level issue connections.
const topLevelPageSize = 50

// stateBatchChunkSize is the maximum number of ids or numbers per state-batch
// request.
const stateBatchChunkSize = 50

// defaultActiveStates is applied when the config omits active_states.
var defaultActiveStates = []string{"Backlog", "Todo", "In Progress"}

// defaultTerminalStates is applied when the config omits terminal_states.
var defaultTerminalStates = []string{"Done", "Canceled", "Duplicate"}

// LinearAdapter implements [domain.TrackerAdapter] against Linear. All fields
// except metrics are set once at construction and never mutated, so the adapter
// is safe for concurrent use.
type LinearAdapter struct {
	client         graphQLClient
	project        string
	activeStates   []string
	terminalStates []string
	handoffState   string
	casingCache    map[string]string
	log            *slog.Logger
	metrics        domain.Metrics
}

// NewLinearAdapter creates a [LinearAdapter] from adapter configuration.
//
// Required config keys: "api_key" and "project" (the Linear team key).
// Optional: "endpoint" (defaults to the Linear GraphQL URL), "active_states",
// "terminal_states", and "handoff_state". The constructor runs a credential
// check and a team-states preflight; a missing key or project, an invalid key,
// an unknown team, or a configured state name absent from the team returns a
// [*domain.TrackerError] and blocks construction.
func NewLinearAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	endpoint, _ := config["endpoint"].(string)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

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

	activeStates := typeutil.ExtractStringSlice(config["active_states"])
	if len(activeStates) == 0 {
		activeStates = defaultActiveStates
	}
	terminalStates := typeutil.ExtractStringSlice(config["terminal_states"])
	if len(terminalStates) == 0 {
		terminalStates = defaultTerminalStates
	}
	handoffState, _ := config["handoff_state"].(string)

	userAgent, _ := config["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	client := newLinearClient(endpoint, apiKey, userAgent)
	return newAdapter(client, project, activeStates, terminalStates, handoffState, slog.Default())
}

// newAdapter runs the preflight through client and assembles the adapter with
// canonical-cased state lists. It is the seam shared by the production
// constructor and offline tests.
func newAdapter(client graphQLClient, project string, active, terminal []string, handoff string, log *slog.Logger) (domain.TrackerAdapter, error) {
	casingCache, err := runPreflight(context.Background(), client, project, active, terminal, handoff, log)
	if err != nil {
		return nil, err
	}

	return &LinearAdapter{
		client:         client,
		project:        project,
		activeStates:   canonicalize(active, casingCache),
		terminalStates: canonicalize(terminal, casingCache),
		handoffState:   canonicalOne(handoff, casingCache),
		casingCache:    casingCache,
		log:            log,
	}, nil
}

// canonicalize maps each configured name to its canonical casing from the
// preflight cache. Names absent from the cache pass through unchanged; the
// preflight has already rejected such configurations.
func canonicalize(names []string, casingCache map[string]string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = canonicalOne(name, casingCache)
	}
	return out
}

// canonicalOne returns the canonical casing of name, or name unchanged when it
// is empty or absent from the cache.
func canonicalOne(name string, casingCache map[string]string) string {
	if name == "" {
		return ""
	}
	if canonical, ok := casingCache[strings.ToLower(name)]; ok {
		return canonical
	}
	return name
}

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter operates
// without recording metrics. Not safe to call concurrently with adapter
// operations.
func (a *LinearAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// FetchCandidateIssues returns issues in the configured active states for the
// configured team, paginated to exhaustion and sorted client-side by normalized
// priority then creation time. Comments are nil on every returned issue.
func (a *LinearAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		fetched, fetchErr := a.fetchIssues(ctx, queryCandidateIssues, a.activeStates)
		if fetchErr != nil {
			return fetchErr
		}
		sortByPriorityThenCreated(fetched)
		issues = fetched
		return nil
	})
	return issues, err
}

// FetchIssuesByStates returns issues in the specified states for the configured
// team. It returns immediately with an empty slice when states is empty. State
// names are canonical-cased through the preflight cache.
func (a *LinearAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		fetched, fetchErr := a.fetchIssues(ctx, queryIssuesByStates, canonicalize(states, a.casingCache))
		if fetchErr != nil {
			return fetchErr
		}
		issues = fetched
		return nil
	})
	return issues, err
}

// fetchIssues paginates the issues connection for the given query and states,
// normalizing each node with Comments left nil.
func (a *LinearAdapter) fetchIssues(ctx context.Context, query string, states []string) ([]domain.Issue, error) {
	variables := map[string]any{
		"teamKey": a.project,
		"states":  states,
		"first":   topLevelPageSize,
	}

	nodes, err := paginate(ctx, a.client, query, variables, decodeIssuesPage, a.log)
	if err != nil {
		return nil, err
	}

	issues := make([]domain.Issue, 0, len(nodes))
	for _, node := range nodes {
		issue := normalizeIssue(node, a.log)
		issue.Comments = nil
		issues = append(issues, issue)
	}
	return issues, nil
}

// FetchIssueByID returns a fully populated issue including all comments. The
// issueID is a UUID or human identifier, passed verbatim. A nonexistent issue
// returns a [*domain.TrackerError] of kind [domain.ErrTrackerNotFound].
func (a *LinearAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		body, headers, execErr := a.client.Execute(ctx, queryIssueByID, map[string]any{"id": issueID})
		if execErr != nil {
			return execErr
		}
		recordRateLimit(headers, a.log)

		var resp graphQLResponse[issueByIDData]
		if err := unmarshal(body, &resp); err != nil {
			return err
		}
		if classified := classifyGraphQLErrors(resp.Errors); classified != nil {
			return classified
		}
		if resp.Data.Issue == nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueID),
			}
		}

		fetched := normalizeIssue(*resp.Data.Issue, a.log)
		comments, commentErr := a.collectComments(ctx, issueID, resp.Data.Issue.Comments)
		if commentErr != nil {
			return commentErr
		}
		fetched.Comments = comments
		issue = fetched
		return nil
	})
	return issue, err
}

// collectComments merges the inline first comment page with all continuation
// pages and returns the merged set sorted ascending by creation time. The
// inline connection is nil when the by-id query did not select comments.
func (a *LinearAdapter) collectComments(ctx context.Context, issueID string, inline *linearCommentConn) ([]domain.Comment, error) {
	nodes := make([]linearComment, 0)
	if inline != nil {
		nodes = append(nodes, inline.Nodes...)
	}

	if inline != nil && inline.PageInfo.HasNextPage {
		variables := map[string]any{
			"id":    issueID,
			"first": topLevelPageSize,
			"after": inline.PageInfo.EndCursor,
		}
		rest, err := paginate(ctx, a.client, queryIssueComments, variables, decodeCommentsPage, a.log)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, rest...)
	}

	comments := normalizeComments(nodes)
	sortCommentsByCreatedAt(comments)
	return comments, nil
}

// FetchIssueComments returns all comments for an issue sorted ascending by
// creation time. It returns an empty non-nil slice when no comments exist and a
// [*domain.TrackerError] of kind [domain.ErrTrackerNotFound] when the issue does
// not exist.
func (a *LinearAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	comments := make([]domain.Comment, 0)
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		variables := map[string]any{
			"id":    issueID,
			"first": topLevelPageSize,
		}
		nodes, fetchErr := paginate(ctx, a.client, queryIssueComments, variables, decodeCommentsPage, a.log)
		if fetchErr != nil {
			return fetchErr
		}
		normalized := normalizeComments(nodes)
		sortCommentsByCreatedAt(normalized)
		comments = normalized
		return nil
	})
	return comments, err
}

// FetchIssueStatesByIDs returns the current state for each requested UUID,
// keyed by id. It returns an empty map with no API call when issueIDs is empty.
// Issues absent from the response are omitted from the result.
func (a *LinearAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}

	states := map[string]string{}
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		for start := 0; start < len(issueIDs); start += stateBatchChunkSize {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			end := min(start+stateBatchChunkSize, len(issueIDs))
			chunk := issueIDs[start:end]

			nodes, fetchErr := a.fetchStateBatch(ctx, queryIssueStatesByIDs, map[string]any{
				"ids":   chunk,
				"first": len(chunk),
			})
			if fetchErr != nil {
				return fetchErr
			}
			for _, node := range nodes {
				states[node.ID] = node.State.Name
			}
		}
		return nil
	})
	return states, err
}

// FetchIssueStatesByIdentifiers returns the current state for each requested
// human identifier, keyed by identifier. It returns an empty map with no API
// call when identifiers is empty. Each identifier is split into its team key
// and number on the last hyphen; an identifier whose trailing part is not an
// integer is skipped. Issues absent from the response are omitted.
func (a *LinearAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	if len(identifiers) == 0 {
		return map[string]string{}, nil
	}

	numbers := extractNumbers(identifiers)
	if len(numbers) == 0 {
		return map[string]string{}, nil
	}

	states := map[string]string{}
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		for start := 0; start < len(numbers); start += stateBatchChunkSize {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			end := min(start+stateBatchChunkSize, len(numbers))
			chunk := numbers[start:end]

			nodes, fetchErr := a.fetchStateBatch(ctx, queryIssueStatesByNumbers, map[string]any{
				"teamKey": a.project,
				"numbers": chunk,
				"first":   len(chunk),
			})
			if fetchErr != nil {
				return fetchErr
			}
			for _, node := range nodes {
				states[node.Identifier] = node.State.Name
			}
		}
		return nil
	})
	return states, err
}

// fetchStateBatch runs a single state-batch query and returns its nodes.
func (a *LinearAdapter) fetchStateBatch(ctx context.Context, query string, variables map[string]any) ([]stateBatchIssue, error) {
	body, headers, err := a.client.Execute(ctx, query, variables)
	if err != nil {
		return nil, err
	}
	recordRateLimit(headers, a.log)

	var resp graphQLResponse[stateBatchData]
	if err := unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if classified := classifyGraphQLErrors(resp.Errors); classified != nil {
		return nil, classified
	}
	return resp.Data.Issues.Nodes, nil
}

// TransitionIssue is a compile-satisfying stub for the read-only adapter. The
// write path is implemented separately; this returns a typed error rather than
// silently succeeding so a handoff transition is never falsely reported.
func (a *LinearAdapter) TransitionIssue(_ context.Context, _, _ string) error {
	return writePathNotImplemented("TransitionIssue")
}

// CommentIssue is a compile-satisfying stub for the read-only adapter. See
// [LinearAdapter.TransitionIssue] for the stub rationale.
func (a *LinearAdapter) CommentIssue(_ context.Context, _, _ string) error {
	return writePathNotImplemented("CommentIssue")
}

// AddLabel is a compile-satisfying stub for the read-only adapter. See
// [LinearAdapter.TransitionIssue] for the stub rationale.
func (a *LinearAdapter) AddLabel(_ context.Context, _, _ string) error {
	return writePathNotImplemented("AddLabel")
}

// writePathNotImplemented builds the typed error returned by the write stubs.
func writePathNotImplemented(operation string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf("linear write path not implemented: %s", operation),
	}
}

// extractNumbers splits each identifier on the last hyphen and collects the
// trailing integer parts. Identifiers whose trailing part is not an integer are
// skipped because they cannot match any issue number.
func extractNumbers(identifiers []string) []float64 {
	numbers := make([]float64, 0, len(identifiers))
	for _, identifier := range identifiers {
		idx := strings.LastIndex(identifier, "-")
		if idx < 0 || idx == len(identifier)-1 {
			continue
		}
		n, err := strconv.Atoi(identifier[idx+1:])
		if err != nil {
			continue
		}
		numbers = append(numbers, float64(n))
	}
	return numbers
}

// unmarshal decodes a GraphQL response body, returning a payload error on
// failure.
func unmarshal(body []byte, dest any) error {
	if err := json.Unmarshal(body, dest); err != nil {
		return payloadError(err)
	}
	return nil
}
