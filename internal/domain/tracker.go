package domain

import "context"

// TrackerAdapter defines the contract that all issue tracker
// integrations must satisfy. Each adapter normalizes its native API
// responses into domain types. Implementations must be safe for
// concurrent use by the orchestrator's poll loop and reconciliation
// goroutine.
type TrackerAdapter interface {
	// FetchCandidateIssues returns issues in configured active states
	// for the configured project. Results are normalized to [Issue].
	// The Comments field may be nil (not fetched) to reduce API cost;
	// callers requiring comments must use [TrackerAdapter.FetchIssueByID]
	// or [TrackerAdapter.FetchIssueComments]. Pagination is handled
	// internally by the adapter.
	FetchCandidateIssues(ctx context.Context) ([]Issue, error)

	// FetchIssueByID returns a single fully-populated issue including
	// comments. Used for pre-dispatch revalidation and prompt
	// rendering. Returns a [*TrackerError] with Kind
	// [ErrTrackerPayload] if the issue cannot be found.
	FetchIssueByID(ctx context.Context, issueID string) (Issue, error)

	// FetchIssuesByStates returns issues in the specified states.
	// Used for startup terminal cleanup. State names are compared
	// case-insensitively by the adapter.
	FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error)

	// FetchIssueStatesByIDs returns the current state for each
	// requested issue ID. The returned map is keyed by issue ID with
	// the value being the current state name. Issues not found in the
	// tracker are omitted from the map (not an error). Used for
	// active-run reconciliation.
	FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error)

	// FetchIssueStatesByIdentifiers returns the current state for each
	// requested issue identifier (human-readable key, e.g. "PROJ-123").
	// The returned map is keyed by identifier with the value being the
	// current state name. Issues not found in the tracker are omitted
	// from the map (not an error). Used for startup terminal workspace
	// cleanup.
	FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error)

	// FetchIssueComments returns comments for the specified issue.
	// Used for continuation runs and the agent workpad pattern.
	// Returns an empty non-nil slice when no comments exist.
	FetchIssueComments(ctx context.Context, issueID string) ([]Comment, error)

	// TransitionIssue moves an issue to the specified target state in
	// the tracker. Used by the orchestrator to perform handoff
	// transitions after a successful worker run. The targetState is
	// matched against the tracker's native state model by the adapter.
	//
	// Returns nil on success. Returns a [*TrackerError] on failure:
	//   - [ErrTrackerTransport]: network or server failure (connection error, HTTP 5xx).
	//   - [ErrTrackerAuth]: insufficient permissions for write operations.
	//   - [ErrTrackerAPI]: non-success response from the tracker (rate limit, unexpected status).
	//   - [ErrTrackerNotFound]: the issue does not exist.
	//   - [ErrTrackerPayload]: no available transition leads to targetState
	//     from the issue's current state, or the response is malformed.
	//
	// The orchestrator treats all errors as non-fatal: log and degrade
	// to continuation retry.
	TransitionIssue(ctx context.Context, issueID string, targetState string) error
}
