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
}
