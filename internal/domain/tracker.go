package domain

import "context"

// TrackerAdapter defines the contract that all issue tracker
// integrations must satisfy.
//
// Each adapter normalizes its native API responses into domain types.
// Implementations must be safe for concurrent use by the orchestrator's
// poll loop and reconciliation goroutine.
type TrackerAdapter interface {
	// FetchCandidateIssues returns issues in configured active states
	// for the configured project.
	//
	// Results are normalized to [Issue]. The Comments field may be nil
	// (not fetched) to reduce API cost; callers requiring comments must
	// use [TrackerAdapter.FetchIssueByID] or
	// [TrackerAdapter.FetchIssueComments]. Pagination is handled
	// internally by the adapter.
	FetchCandidateIssues(ctx context.Context) ([]Issue, error)

	// FetchIssueByID returns a single fully-populated issue including
	// comments.
	//
	// Used for pre-dispatch revalidation and prompt rendering. Returns
	// a [*TrackerError] with Kind [ErrTrackerNotFound] if the issue
	// does not exist.
	FetchIssueByID(ctx context.Context, issueID string) (Issue, error)

	// FetchIssuesByStates returns issues in the specified states.
	//
	// Used for startup terminal cleanup. State names are compared
	// case-insensitively by the adapter.
	FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error)

	// FetchIssueStatesByIDs returns the current state for each
	// requested issue ID.
	//
	// The returned map is keyed by issue ID with the value being the
	// current state name. Issues not found in the tracker are omitted
	// from the map (not an error). Used for active-run reconciliation.
	FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error)

	// FetchIssueStatesByIdentifiers returns the current state for each
	// requested issue identifier (human-readable key, e.g. "PROJ-123").
	//
	// The returned map is keyed by identifier with the value being the
	// current state name. Issues not found in the tracker are omitted
	// from the map (not an error). Used for startup terminal workspace
	// cleanup.
	FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error)

	// FetchIssueComments returns comments for the specified issue.
	//
	// Used for continuation runs and the agent workpad pattern.
	// Returns an empty non-nil slice when no comments exist.
	// Returns a [*TrackerError] with Kind [ErrTrackerNotFound]
	// if the issue does not exist.
	FetchIssueComments(ctx context.Context, issueID string) ([]Comment, error)

	// TransitionIssue moves an issue to the specified target state in
	// the tracker.
	//
	// Used by the orchestrator to perform handoff transitions after a
	// successful worker run. The targetState is matched against the
	// tracker's native state model by the adapter.
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

	// CommentIssue posts a plain-text comment on the specified issue in
	// the tracker.
	//
	// The text parameter is plain text; adapters convert to their native
	// format internally (e.g. ADF wrapping for Jira).
	//
	// Returns nil on success. Returns a [*TrackerError] on tracker failure:
	//   - [ErrTrackerTransport]: network or server failure.
	//   - [ErrTrackerAuth]: insufficient permissions.
	//   - [ErrTrackerAPI]: non-success response (rate limit, unexpected status).
	//   - [ErrTrackerNotFound]: the issue does not exist.
	//   - [ErrTrackerPayload]: malformed request or response.
	//
	// When ctx is canceled or its deadline is exceeded, implementations
	// may return ctx.Err() directly (e.g. [context.Canceled] or
	// [context.DeadlineExceeded]) instead of a [*TrackerError].
	//
	// All errors are non-fatal — the orchestrator logs WARN and continues.
	CommentIssue(ctx context.Context, issueID string, text string) error

	// AddLabel adds a label to the specified issue. Used for CI failure
	// escalation. Returns nil on success.
	//
	// Adapters that do not support labels return nil (no-op) rather
	// than an error. The orchestrator treats all errors as non-fatal.
	AddLabel(ctx context.Context, issueID string, label string) error
}
