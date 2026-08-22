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
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("linear", NewLinearAdapter, registry.TrackerMeta{
		RequiresProject:       true,
		RequiresAPIKey:        true,
		ValidateTrackerConfig: validateConfig,
		DefaultActiveStates:   defaultActiveStates,
		DefaultTerminalStates: defaultTerminalStates,
		BlockerSource:         registry.BlockersFromCandidates,
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
	queryFilter    map[string]any
	casingCache    map[string]string
	log            *slog.Logger
	metrics        domain.Metrics
}

// NewLinearAdapter creates a [LinearAdapter] from adapter configuration.
//
// Required config keys: "api_key" and "project" (the Linear team key).
// Optional: "endpoint" (defaults to the Linear GraphQL URL; a present value
// that does not parse to an absolute http(s) URL with a host returns a
// [*domain.TrackerError] of kind [domain.ErrTrackerPayload]), "active_states",
// "terminal_states", and "handoff_state". The constructor runs a credential
// check and a team-states preflight; a missing key or project, an invalid key,
// an unknown team, or a configured state name absent from the team returns a
// [*domain.TrackerError] and blocks construction.
func NewLinearAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	endpointRaw, _ := config["endpoint"].(string)
	endpoint, redactedEndpoint, endpointOK := resolveEndpoint(endpointRaw)
	if !endpointOK {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("linear: endpoint %q is not a valid absolute http(s) url", redactedEndpoint),
		}
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

	rawQueryFilter, _ := config["query_filter"].(string)
	queryFilter, err := parseQueryFilter(rawQueryFilter)
	if err != nil {
		return nil, err
	}

	userAgent, _ := config["user_agent"].(string)
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	client := newLinearClient(endpoint, apiKey, userAgent)
	return newAdapter(client, project, activeStates, terminalStates, handoffState, queryFilter, slog.Default())
}

// newAdapter runs the preflight through client and assembles the adapter with
// canonical-cased state lists. It is the seam shared by the production
// constructor and offline tests.
func newAdapter(client graphQLClient, project string, active, terminal []string, handoff string, queryFilter map[string]any, log *slog.Logger) (domain.TrackerAdapter, error) {
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
		queryFilter:    queryFilter,
		casingCache:    casingCache,
		log:            log,
	}, nil
}

// parseQueryFilter decodes the operator-supplied query_filter into an
// IssueFilter fragment. It returns nil with a nil error when raw is empty.
//
// A value that does not parse as JSON, parses as a non-object, or carries a
// top-level reserved key returns a [*domain.TrackerError] of kind
// [domain.ErrTrackerPayload]. The reserved-key check inspects "team" before
// "state" so a fragment naming both always reports "team", keeping the error
// message stable. The returned map is stored and merged into the fetch filter,
// so the value travels in the GraphQL variables object, never the query text.
func parseQueryFilter(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}

	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("linear: tracker.query_filter is not valid json: %v", err),
		}
	}

	filter, ok := decoded.(map[string]any)
	if !ok {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "linear: tracker.query_filter must be a json object",
		}
	}

	for _, reserved := range []string{"team", "state"} {
		if _, present := filter[reserved]; present {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("linear: tracker.query_filter must not contain a reserved key %q", reserved),
			}
		}
	}

	return filter, nil
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

// buildFetchFilter constructs the IssueFilter object for a single fetch from the
// configured team, the supplied states, and the operator filter.
//
// It allocates a fresh map on every call and never writes into operatorFilter,
// so concurrent fetches do not share or rewrite the stored fragment. The team
// and state constraints are always set by the adapter; the reserved-key guard in
// [parseQueryFilter] guarantees operatorFilter cannot overwrite them, and the
// shallow copy leaves nested operator objects intact because Linear ANDs sibling
// IssueFilter fields.
func buildFetchFilter(teamKey string, states []string, operatorFilter map[string]any) map[string]any {
	filter := map[string]any{
		"team":  map[string]any{"key": map[string]any{"eq": teamKey}},
		"state": map[string]any{"name": map[string]any{"in": states}},
	}
	maps.Copy(filter, operatorFilter)
	return filter
}

// fetchIssues paginates the issues connection for the given query and states,
// normalizing each node with Comments left nil.
func (a *LinearAdapter) fetchIssues(ctx context.Context, query string, states []string) ([]domain.Issue, error) {
	variables := map[string]any{
		"filter": buildFetchFilter(a.project, states, a.queryFilter),
		"first":  topLevelPageSize,
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
		if inline.PageInfo.EndCursor == "" {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerMissingCursor,
				Message: "linear graphql: next comment page reported without an end cursor",
			}
		}
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

// mutationDecoder extracts the errors array and the payload success boolean
// from a mutation response body, returning a payload error only on JSON
// unmarshal failure. It leaves the result policy to [runMutation].
type mutationDecoder func(body []byte) (errs []graphQLError, success bool, err error)

// runMutation submits a mutation and applies the per-mutation result policy. A
// non-empty errors array is a failure regardless of the payload success value;
// an empty errors array with success false maps to [domain.ErrTrackerAPI]. It
// reuses the read-path transport, rate-limit recorder, and classifier, and does
// not record metrics, so a public method that wraps it in
// [trackermetrics.Track] emits one sample per logical write.
func runMutation(ctx context.Context, client graphQLClient, log *slog.Logger, query string, variables map[string]any, decode mutationDecoder) error {
	body, headers, err := client.Execute(ctx, query, variables)
	if err != nil {
		return err
	}
	recordRateLimit(headers, log)

	errs, success, err := decode(body)
	if err != nil {
		return err
	}
	if classified := classifyGraphQLErrors(errs); classified != nil {
		return classified
	}
	if !success {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: "linear graphql: mutation returned success: false",
		}
	}
	return nil
}

// decodeIssueUpdateSuccess decodes an issueUpdate mutation response. It is
// shared by the transition mutation and the label-attach mutation because both
// return IssuePayload.success.
func decodeIssueUpdateSuccess(body []byte) ([]graphQLError, bool, error) {
	var resp graphQLResponse[issueUpdateData]
	if err := unmarshal(body, &resp); err != nil {
		return nil, false, err
	}
	return resp.Errors, resp.Data.IssueUpdate.Success, nil
}

// TransitionIssue moves an issue to the workflow state named targetState. It
// resolves the state name to a team-scoped UUID with a case-insensitive filter,
// then applies it; issueID and targetState are passed verbatim.
//
// A missing issue returns [domain.ErrTrackerNotFound]; a name that matches no
// workflow state in the issue's team returns [domain.ErrTrackerPayload].
func (a *LinearAdapter) TransitionIssue(ctx context.Context, issueID, targetState string) error {
	return trackermetrics.Track(a.metrics, "transition", func() error {
		body, headers, execErr := a.client.Execute(ctx, queryResolveStateID, map[string]any{
			"issueId":   issueID,
			"stateName": targetState,
		})
		if execErr != nil {
			return execErr
		}
		recordRateLimit(headers, a.log)

		var resp graphQLResponse[stateResolveData]
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
		nodes := resp.Data.Issue.Team.States.Nodes
		if len(nodes) == 0 {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("no workflow state named %q in the team of issue %s", targetState, issueID),
			}
		}

		return runMutation(ctx, a.client, a.log, queryIssueUpdateState, map[string]any{
			"id":      issueID,
			"stateId": nodes[0].ID,
		}, decodeIssueUpdateSuccess)
	})
}

// CommentIssue posts text as a markdown comment on the issue. The body is sent
// verbatim with no wrapping or escaping; issueID is passed verbatim.
//
// The method performs no internal retry because commentCreate is not
// idempotent and the orchestrator owns retry policy.
func (a *LinearAdapter) CommentIssue(ctx context.Context, issueID, text string) error {
	return trackermetrics.Track(a.metrics, "comment", func() error {
		return runMutation(ctx, a.client, a.log, queryCommentCreate, map[string]any{
			"issueId": issueID,
			"body":    text,
		}, decodeCommentCreateSuccess)
	})
}

// AddLabel attaches the named label to the issue, creating the label
// team-scoped when it does not yet exist. The label is appended through
// addedLabelIds, so the issue's other labels are preserved; label and issueID
// are passed verbatim.
//
// A label-create forbidden for the key returns [domain.ErrTrackerAuth]. On any
// payload-class create error the method re-resolves once, using a label that a
// concurrent create produced; only a re-resolve that still finds nothing
// surfaces the original create error.
func (a *LinearAdapter) AddLabel(ctx context.Context, issueID, label string) error {
	return trackermetrics.Track(a.metrics, "add_label", func() error {
		labelID, found, err := a.resolveLabelID(ctx, label)
		if err != nil {
			return err
		}

		if !found {
			teamID, teamErr := a.resolveTeamID(ctx, issueID)
			if teamErr != nil {
				return teamErr
			}

			created, createErr := a.createLabel(ctx, teamID, label)
			switch {
			case createErr == nil:
				labelID = created
			case isPayloadError(createErr):
				labelID, found, err = a.resolveLabelID(ctx, label)
				if err != nil {
					return err
				}
				if !found {
					return createErr
				}
			default:
				return createErr
			}
		}

		return runMutation(ctx, a.client, a.log, queryIssueAddLabel, map[string]any{
			"id":       issueID,
			"labelIds": []string{labelID},
		}, decodeIssueUpdateSuccess)
	})
}

// resolveLabelID resolves a label name to its UUID, preferring a label scoped
// to the configured team over a workspace-scoped label. It reports found false
// with a nil error when no label matches. The name is matched case-insensitively
// and sent verbatim.
func (a *LinearAdapter) resolveLabelID(ctx context.Context, name string) (string, bool, error) {
	body, headers, err := a.client.Execute(ctx, queryResolveLabel, map[string]any{"name": name})
	if err != nil {
		return "", false, err
	}
	recordRateLimit(headers, a.log)

	var resp graphQLResponse[labelResolveData]
	if err := unmarshal(body, &resp); err != nil {
		return "", false, err
	}
	if classified := classifyGraphQLErrors(resp.Errors); classified != nil {
		return "", false, classified
	}

	var workspaceID string
	var hasWorkspace bool
	for _, node := range resp.Data.IssueLabels.Nodes {
		if node.Team != nil {
			if node.Team.Key == a.project {
				return node.ID, true, nil
			}
			continue
		}
		if !hasWorkspace {
			workspaceID = node.ID
			hasWorkspace = true
		}
	}
	if hasWorkspace {
		return workspaceID, true, nil
	}
	return "", false, nil
}

// resolveTeamID resolves the UUID of the team that owns the issue. A missing
// issue returns [domain.ErrTrackerNotFound]. issueID is passed verbatim.
func (a *LinearAdapter) resolveTeamID(ctx context.Context, issueID string) (string, error) {
	body, headers, err := a.client.Execute(ctx, queryResolveIssueTeam, map[string]any{"issueId": issueID})
	if err != nil {
		return "", err
	}
	recordRateLimit(headers, a.log)

	var resp graphQLResponse[teamResolveData]
	if err := unmarshal(body, &resp); err != nil {
		return "", err
	}
	if classified := classifyGraphQLErrors(resp.Errors); classified != nil {
		return "", classified
	}
	if resp.Data.Issue == nil {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("issue not found: %s", issueID),
		}
	}
	return resp.Data.Issue.Team.ID, nil
}

// createLabel creates a team-scoped label and returns its UUID. It always
// passes teamId because workspace-scoped label management is more likely to
// require elevated access. The name is sent verbatim. A forbidden create
// surfaces as [domain.ErrTrackerAuth] through the shared classifier.
func (a *LinearAdapter) createLabel(ctx context.Context, teamID, name string) (string, error) {
	var labelID string
	decode := func(body []byte) ([]graphQLError, bool, error) {
		var resp graphQLResponse[labelCreateData]
		if err := unmarshal(body, &resp); err != nil {
			return nil, false, err
		}
		if resp.Data.IssueLabelCreate.IssueLabel != nil {
			labelID = resp.Data.IssueLabelCreate.IssueLabel.ID
		}
		return resp.Errors, resp.Data.IssueLabelCreate.Success, nil
	}

	if err := runMutation(ctx, a.client, a.log, queryLabelCreate, map[string]any{
		"teamId": teamID,
		"name":   name,
	}, decode); err != nil {
		return "", err
	}
	if labelID == "" {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("linear graphql: issueLabelCreate reported success but returned no label id for %q", name),
		}
	}
	return labelID, nil
}

// decodeCommentCreateSuccess decodes a commentCreate mutation response.
func decodeCommentCreateSuccess(body []byte) ([]graphQLError, bool, error) {
	var resp graphQLResponse[commentCreateData]
	if err := unmarshal(body, &resp); err != nil {
		return nil, false, err
	}
	return resp.Errors, resp.Data.CommentCreate.Success, nil
}

// isPayloadError reports whether err is a [*domain.TrackerError] of kind
// [domain.ErrTrackerPayload].
func isPayloadError(err error) bool {
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		return false
	}
	return te.Kind == domain.ErrTrackerPayload
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
