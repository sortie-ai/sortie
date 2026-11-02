package domain

import (
	"context"
	"errors"
)

// TrackerStates is the workflow's state vocabulary as written in front
// matter, with no default applied and no entry normalized.
type TrackerStates struct {
	// Active is tracker.active_states.
	Active []string

	// Terminal is tracker.terminal_states.
	Terminal []string

	// Handoff is tracker.handoff_state.
	Handoff string

	// InProgress is tracker.in_progress_state.
	InProgress string
}

// IssueStateAndLabels is an issue's tracker state and labels as one by-ID
// read reports them.
type IssueStateAndLabels struct {
	State  string   // as FetchIssueStatesByIDs reports it; empty: no configured state
	Labels []string // every label, normalized as Issue.Labels; empty: none
}

// TrackerAdapter defines the contract that all issue tracker
// integrations must satisfy.
//
// Each adapter normalizes its native API responses into domain types.
// Implementations must be safe for concurrent use by the orchestrator's
// poll loop and reconciliation goroutine.
type TrackerAdapter interface {
	// FetchCandidateIssues returns issues in configured active states
	// for the configured project. An empty active list returns no issue
	// and sends no request.
	//
	// Results are normalized to [Issue]. The Comments field may be nil
	// (not fetched) to reduce API cost; callers requiring comments must
	// use [TrackerAdapter.FetchIssueByID] or
	// [TrackerAdapter.FetchIssueComments]. Pagination is handled
	// internally by the adapter.
	//
	// A returned issue either carries an authoritative BlockedBy or
	// has BlockersUnresolved set, according to the adapter's declared
	// blocker source.
	FetchCandidateIssues(ctx context.Context) ([]Issue, error)

	// FetchIssueByID returns a single fully-populated issue including
	// comments, oldest first.
	//
	// Used for pre-dispatch revalidation and prompt rendering. Returns
	// a [*TrackerError] with Kind [ErrTrackerNotFound] if the issue
	// does not exist.
	FetchIssueByID(ctx context.Context, issueID string) (Issue, error)

	// FetchIssuesByStates returns issues in the specified states.
	//
	// No orchestrator caller and no agent tool invokes this operation.
	// Adapters implement it to satisfy this interface; removing it would
	// be an interface change, not an adapter change. State names are
	// compared case-insensitively by the adapter.
	FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error)

	// FetchIssueStatesByIDs returns the current state for each
	// requested issue ID.
	//
	// The returned map is keyed by issue ID with the value being the
	// current state name. Issues not found in the tracker are omitted
	// from the map (not an error). An empty state means the issue reads no
	// configured state.
	FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error)

	// FetchIssueStatesAndLabelsByIDs returns the current state and labels of
	// each requested issue ID, keyed by issue ID.
	//
	// An issue is omitted exactly when FetchIssueStatesByIDs omits it. A
	// returned entry carries the issue's complete label set; an adapter that
	// cannot read every label of an issue it would return fails the call. The
	// query filter is not applied. It sends no request FetchIssueStatesByIDs
	// would not send for the same IDs, except for further pages of an issue's
	// labels. An empty issueIDs returns an empty map and sends no request; on
	// error the map is nil.
	FetchIssueStatesAndLabelsByIDs(ctx context.Context, issueIDs []string) (map[string]IssueStateAndLabels, error)

	// FetchIssueStatesByIdentifiers returns the current state for each
	// requested issue identifier (human-readable key, e.g. "PROJ-123").
	//
	// The returned map is keyed by identifier with the value being the
	// current state name. Issues not found in the tracker are omitted
	// from the map (not an error). Used for startup terminal workspace
	// cleanup.
	FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error)

	// FetchIssueComments returns comments for the specified issue, oldest
	// first.
	//
	// Used for continuation runs, the first run after a person releases a
	// park, and the agent workpad pattern. Returns an empty non-nil slice
	// when no comments exist.
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
	// On success it returns the created comment's identity and a nil
	// error. The identity is non-empty and equals the [Comment.ID] that
	// [TrackerAdapter.FetchIssueComments] and [TrackerAdapter.FetchIssueByID]
	// report for that comment. It comes from the response of the one create
	// request the call sends; the call sends no other request (no read-back,
	// no search).
	//
	// Returns an empty commentID and a [*TrackerError] on tracker failure:
	//   - [ErrTrackerTransport]: network or server failure.
	//   - [ErrTrackerAuth]: insufficient permissions.
	//   - [ErrTrackerAPI]: non-success response (rate limit, unexpected status).
	//   - [ErrTrackerNotFound]: the issue does not exist.
	//   - [ErrTrackerPayload]: malformed request or response, including a
	//     successful create response that does not carry the identity.
	//
	// When ctx is canceled or its deadline is exceeded, implementations
	// may return ctx.Err() directly (e.g. [context.Canceled] or
	// [context.DeadlineExceeded]) instead of a [*TrackerError].
	//
	// All errors are non-fatal; the orchestrator logs WARN and continues.
	CommentIssue(ctx context.Context, issueID string, text string) (commentID string, err error)

	// AddLabel adds a label to the specified issue and removes no other
	// label. A tracker that models a label group as single-select may
	// replace the group's other label on the issue.
	//
	// Stored labels are compared with the label ignoring letter case, and
	// the label is used as given, without trimming. Returns nil only when
	// the issue's label set after the write, as the write's response or a
	// read issued after it reports it, carries the label. A [TrackerAdapter.FetchIssueByID]
	// that starts after the nil return reflects the write unless another
	// party changed the issue's labels in between; the candidate listing
	// carries every label the tracker reports for each issue and shows the
	// write once the tracker's own listing does.
	//
	// Returns a [*TrackerError] on failure:
	//   - [ErrTrackerTransport]: network or server failure.
	//   - [ErrTrackerAuth]: insufficient permissions.
	//   - [ErrTrackerAPI]: non-success response from the tracker.
	//   - [ErrTrackerNotFound]: the issue does not exist or is not visible.
	//   - [ErrTrackerPayload]: the label is blank (only white space), the
	//     tracker rejected the write as invalid, or it acknowledged the
	//     write and the issue does not carry the label.
	//
	// When ctx is canceled or its deadline is exceeded, implementations
	// may return ctx.Err() directly. The orchestrator treats all errors as
	// non-fatal.
	AddLabel(ctx context.Context, issueID string, label string) error

	// RemoveLabel removes from the specified issue every label that names
	// label, and no other label. Labels name one another when they compare
	// equal ignoring letter case, so an issue carrying both "Stage-Plan" and
	// "stage-plan" carries neither after RemoveLabel("stage-plan"). The
	// label is matched as given, without trimming.
	//
	// An issue that carries no such label returns nil and receives no write
	// request. Otherwise a nil return means the issue's label set after
	// the write, as the write's response or a read issued after it reports
	// it, carries no label naming the argument. A
	// [TrackerAdapter.FetchIssueByID] that starts after the nil return
	// reflects the removal unless another party changed the issue's labels
	// in between; the candidate listing shows it once the tracker's own
	// listing does.
	//
	// Returns a [*TrackerError] on failure:
	//   - [ErrTrackerTransport]: network or server failure.
	//   - [ErrTrackerAuth]: insufficient permissions.
	//   - [ErrTrackerAPI]: non-success response from the tracker.
	//   - [ErrTrackerNotFound]: the issue does not exist or is not visible.
	//   - [ErrTrackerPayload]: the label is blank (only white space), the
	//     tracker rejected the write as invalid, or it acknowledged the
	//     removal and the issue still carries the label.
	//
	// When ctx is canceled or its deadline is exceeded, implementations
	// may return ctx.Err() directly.
	RemoveLabel(ctx context.Context, issueID string, label string) error

	// SetTrackerStates installs the function the adapter calls at the start
	// of each operation that reads the workflow's state lists; until then
	// the adapter uses the lists it was constructed with. Safe for
	// concurrent use.
	SetTrackerStates(current func() TrackerStates)
}

// ProjectLinker is the optional capability a tracker adapter implements
// when the project it reads has a web page a person can open.
type ProjectLinker interface {
	// ProjectURL returns the web address of the configured project.
	ProjectURL() string
}

// BlockerReader is the optional capability a tracker adapter
// implements when its candidate fetch cannot carry blockers.
type BlockerReader interface {
	// FetchIssueBlockers returns the blockers of one issue. It returns
	// a non-nil empty slice when the issue has none, and a
	// [*TrackerError] on failure.
	FetchIssueBlockers(ctx context.Context, issueID string) ([]BlockerRef, error)
}

// ErrNoBlockerReader reports that an adapter declared the per-issue
// blocker source and does not implement [BlockerReader]. Callers
// treat it as a condition the next attempt will not fix.
var ErrNoBlockerReader = errors.New("tracker adapter declares per-issue blockers and implements no reader")

// CommentReadPoint is the opaque position a [CommentChangeReader] returns
// so the next read of the same issue covers only what changed since. Its
// text means nothing to a caller, and it is meaningful only for the issue
// whose read returned it. The zero value reads the full list.
type CommentReadPoint string

// CommentChanges is the result of one [CommentChangeReader] read.
type CommentChanges struct {
	// Comments are the comments the read returns, oldest first by creation
	// time with ties by ascending ID, each ID at most once. It is never
	// nil on success.
	Comments []Comment

	// Next is the point the next read of the same issue passes.
	Next CommentReadPoint
}

// CommentChangeReader is the optional capability a tracker adapter
// implements to read an issue's comments created or edited since an
// earlier read.
type CommentChangeReader interface {
	// FetchIssueCommentChanges returns the comments of one issue created
	// or edited since the read that returned since; a zero or undecodable
	// since reads the full list. Comments may also hold unchanged
	// comments, so the caller decides what changed by identity and body.
	// Next can move without a changed comment. Errors are those of
	// [TrackerAdapter.FetchIssueComments].
	//
	// An adapter whose tracker cannot filter comments by update time may
	// leave out an edit to a comment older than every comment its read
	// returns, and that adapter's method godoc names the limit.
	//
	// A read from Next can omit every unchanged comment this read
	// returned, so the caller passes Next only once each of them is
	// recorded as given or left out by selection, and passes since again
	// when it discards the result before then.
	FetchIssueCommentChanges(ctx context.Context, issueID string, since CommentReadPoint) (CommentChanges, error)
}

// CommentAuthorAccessReader is the optional capability a tracker adapter
// implements when anyone who can read a project may comment on it.
type CommentAuthorAccessReader interface {
	// CommentAuthorCanWrite reports whether the author of c has write
	// access to the configured project: true is granted, false with a nil
	// error is refused, and false with an error is a failed lookup. It
	// reads c.AuthorID and c.ID only.
	//
	// An adapter remembers a grant for its lifetime and a refusal for
	// that comment only; an edit keeps the comment's ID, so an edited
	// comment is not asked about again. An empty AuthorID is refused
	// without a request. The error is [ErrTrackerAuth] when the token
	// cannot ask, and nothing is remembered on error.
	CommentAuthorCanWrite(ctx context.Context, c Comment) (bool, error)
}

// CommentAuthorKindReader is the optional capability a tracker adapter
// implements when its full comment reads report no author kind.
type CommentAuthorKindReader interface {
	// LookupCommentAuthorKind reports the kind that
	// [CommentChangeReader.FetchIssueCommentChanges] reports for the author
	// of c. It reads c.AuthorID only; an empty AuthorID is
	// [CommentAuthorUnknown] without a request.
	//
	// An adapter remembers an answer for its lifetime and nothing on
	// error. The errors are those of [TrackerAdapter.FetchIssueComments].
	LookupCommentAuthorKind(ctx context.Context, c Comment) (CommentAuthorKind, error)
}
