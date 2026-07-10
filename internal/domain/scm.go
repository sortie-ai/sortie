package domain

import (
	"context"
	"fmt"
	"time"
)

// MergeStrategy enumerates the supported pull-request merge strategies.
// Implementations map these to the provider's merge-method values.
type MergeStrategy string

const (
	// StrategyMerge produces a merge commit on the base branch.
	StrategyMerge MergeStrategy = "merge"

	// StrategySquash squashes the PR commits into a single commit on
	// the base branch.
	StrategySquash MergeStrategy = "squash"

	// StrategyRebase rebases the PR commits onto the base branch.
	StrategyRebase MergeStrategy = "rebase"
)

// ReviewDecision summarizes the aggregated review state for a pull request.
type ReviewDecision string

const (
	// ReviewDecisionApproved indicates the PR has been approved and
	// no later CHANGES_REQUESTED supersedes the approval.
	ReviewDecisionApproved ReviewDecision = "APPROVED"

	// ReviewDecisionChangesRequested indicates at least one reviewer
	// has requested changes and the request is the most recent review.
	ReviewDecisionChangesRequested ReviewDecision = "CHANGES_REQUESTED"

	// ReviewDecisionReviewRequired indicates the PR requires review
	// but no qualifying review has been submitted yet.
	ReviewDecisionReviewRequired ReviewDecision = "REVIEW_REQUIRED"

	// ReviewDecisionNotRequired indicates the PR does not require
	// review under the active branch protection policy.
	ReviewDecisionNotRequired ReviewDecision = "NOT_REQUIRED"
)

// MergeabilityState summarizes whether a pull request is ready to merge.
// Implementations map provider-specific status strings to this enum.
type MergeabilityState string

const (
	// MergeabilityClean indicates all required checks pass and the PR
	// is ready to merge.
	MergeabilityClean MergeabilityState = "clean"

	// MergeabilityUnstable indicates the PR is mergeable but some
	// non-required checks are failing.
	MergeabilityUnstable MergeabilityState = "unstable"

	// MergeabilityBlocked indicates branch protection prevents the
	// merge (missing review, behind base, draft state).
	MergeabilityBlocked MergeabilityState = "blocked"

	// MergeabilityDirty indicates the PR has merge conflicts.
	MergeabilityDirty MergeabilityState = "dirty"

	// MergeabilityUnknown indicates the provider is still computing
	// mergeability.
	MergeabilityUnknown MergeabilityState = "unknown"
)

// PRMergeStatus carries the merge precondition state for a pull request.
// ReviewDecision and CIConclusion are populated by their dedicated reads
// so a single status read can answer one question at a time.
type PRMergeStatus struct {
	// ReviewDecision is the aggregated review decision. Unset when the
	// caller obtains the value from a separate read.
	ReviewDecision ReviewDecision

	// CIConclusion is the aggregated CI conclusion string. Unset when
	// the caller obtains the value from a separate read.
	CIConclusion string

	// Draft reports whether the PR is in draft state.
	Draft bool

	// Mergeability is the normalized mergeability classification.
	Mergeability MergeabilityState

	// HeadSHA is the latest commit SHA on the PR head branch.
	HeadSHA string

	// BranchName is the source branch name (PR head).
	BranchName string

	// BaseBranch is the PR target (base) branch name, e.g. "main" or
	// "develop". Populated by GetMergeability from the same PR object it
	// already fetches; empty only when the provider omits the base ref.
	BaseBranch string
}

// MergeResult carries the outcome of a successful merge call.
type MergeResult struct {
	// SHA is the merge commit SHA returned by the provider.
	SHA string

	// Merged reports whether the merge completed.
	Merged bool

	// Message is the provider-supplied status text, if any.
	Message string
}

// LabelEvent is one normalized entry from a pull request's label-event
// journal. Adapters map provider-specific event shapes to this type.
type LabelEvent struct {
	// ID is the provider journal entry id: opaque to the orchestrator and
	// unique per PR. Adapters normalize it to a lexically-sortable form so
	// that ordering entries by (At, ID) as strings matches their journal
	// (chronological) order.
	ID string

	// Label is the normalized (lowercased) label name.
	Label string

	// Actor is the login of the acting user.
	Actor string

	// Added is true for a labeled event and false for an unlabeled event.
	Added bool

	// At is the journal timestamp in UTC.
	At time.Time
}

// SCMAdapter exposes read and write operations against the SCM platform.
// Implementations must be safe for concurrent use.
type SCMAdapter interface {
	// FetchPendingReviews returns review comments from non-bot
	// CHANGES_REQUESTED reviews on the given PR. Comments marked as
	// outdated by the platform are included with Outdated=true; the
	// caller is responsible for filtering.
	//
	// Returns an empty non-nil slice when no pending review comments
	// exist. Returns a [*SCMError] on failure.
	FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]ReviewComment, error)

	// FetchBotReviewComments returns review comments authored by
	// automated review bots on the given PR. A comment is bot-authored
	// when the platform reports its author as an automated bot account
	// OR when the author login matches an entry in botUsernames under a
	// case-insensitive comparison. The two conditions form a union:
	// either one qualifies. botUsernames may be nil or empty, in which
	// case only the platform's bot-account signal selects comments.
	//
	// Unlike [SCMAdapter.FetchPendingReviews], selection does not require
	// any particular review state. Comments marked as outdated by the
	// platform are included with Outdated=true; the caller is responsible
	// for filtering.
	//
	// Returns an empty non-nil slice when no bot review comments exist.
	// Returns a [*SCMError] on failure.
	FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]ReviewComment, error)

	// GetReviewDecision returns the aggregated review decision for the
	// given PR. Returns a [*SCMError] on failure.
	GetReviewDecision(ctx context.Context, prNumber int, owner, repo string) (ReviewDecision, error)

	// GetCIStatus returns the aggregated CI conclusion string for the
	// PR head ref. Empty string means no required checks exist on the
	// PR. Returns a [*SCMError] on failure.
	GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error)

	// GetMergeability returns the merge precondition state for the
	// given PR. The ReviewDecision and CIConclusion fields are left
	// unset; callers obtain those from the dedicated reads.
	//
	// The platform may need additional time to compute mergeability
	// after a recent push; callers must treat [MergeabilityUnknown]
	// as a deferral condition. Returns a [*SCMError] on failure.
	GetMergeability(ctx context.Context, prNumber int, owner, repo string) (PRMergeStatus, error)

	// MergePR merges the given PR with the requested strategy. Empty
	// commitTitle and commitMessage defer to the provider's
	// strategy-specific defaults. expectedHeadSHA, when non-empty, is
	// sent as the precondition SHA so the provider rejects the call
	// when the head has moved between the precondition check and the
	// merge request.
	//
	// Every merge-endpoint rejection that the provider signals with
	// HTTP 405 (method not allowed) or HTTP 409 (conflict) is returned
	// as a [*SCMError] with Kind [ErrSCMConflict]. This single kind
	// collapses three distinct semantic conditions that the caller
	// must disambiguate before dispatching the outcome:
	//
	//   - the PR was already merged between the precondition check
	//     and this call, by a prior Sortie attempt or any external
	//     actor;
	//   - the head SHA has moved since the precondition check, so the
	//     provider refused the precondition;
	//   - branch protection rejected the merge for a reason other
	//     than the precondition.
	//
	// Callers disambiguate the already-merged subcase by inspecting
	// the [SCMError] Message for the substring "already merged"
	// (case-insensitive); implementations must surface that phrase
	// in Message when, and only when, the provider's response body
	// indicates the PR was already merged. Without that marker the
	// caller treats the conflict as a transient precondition failure
	// and reattempts after re-reading the merge state.
	//
	// Returns a [*SCMError] on any other failure.
	MergePR(ctx context.Context, prNumber int, owner, repo string, strategy MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (MergeResult, error)

	// DeleteBranch deletes the branch reference. Returns nil on
	// success. Returns a [*SCMError] with Kind [ErrSCMNotFound] when
	// the branch is already gone; callers treat this as a successful
	// no-op. Returns a [*SCMError] on any other failure.
	DeleteBranch(ctx context.Context, owner, repo, branch string) error

	// ListLabelEvents returns the label-event journal for the given PR,
	// oldest first. Returns an empty non-nil slice when the PR has no
	// label events. Returns a [*SCMError] on failure.
	ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]LabelEvent, error)

	// RemoveLabel removes the named label from the given PR. Returns nil
	// on success and treats an already-absent label as a successful
	// no-op: a [*SCMError] with Kind [ErrSCMNotFound] is mapped to nil,
	// matching the [SCMAdapter.DeleteBranch] precedent. Returns a
	// [*SCMError] on any other failure.
	RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error
}

// SCMErrorKind enumerates the normalized error categories that SCM
// adapter implementations map their native errors to. The orchestrator
// uses these categories for logging and observability; all SCM errors
// are non-fatal.
type SCMErrorKind string

const (
	// ErrSCMTransport indicates a network or transport failure
	// (connection error, DNS failure, TLS handshake failure).
	ErrSCMTransport SCMErrorKind = "scm_transport_error"

	// ErrSCMAuth indicates an authentication or authorization failure
	// (expired token, insufficient scopes).
	ErrSCMAuth SCMErrorKind = "scm_auth_error"

	// ErrSCMAPI indicates a non-success HTTP status or API-level error
	// (rate limiting, server error, unexpected status code).
	ErrSCMAPI SCMErrorKind = "scm_api_error"

	// ErrSCMNotFound indicates the requested resource does not exist
	// on the SCM platform (HTTP 404).
	ErrSCMNotFound SCMErrorKind = "scm_not_found"

	// ErrSCMPayload indicates a malformed or unexpected response
	// structure from the SCM platform.
	ErrSCMPayload SCMErrorKind = "scm_payload_error"

	// ErrSCMConflict indicates the merge endpoint rejected the call
	// with HTTP 405 (method not allowed) or HTTP 409 (conflict). The
	// kind collapses every such rejection into one category; callers
	// disambiguate the underlying subcase by inspecting the [SCMError]
	// Message rather than the Kind alone.
	//
	// The already-merged subcase is signaled by the substring
	// "already merged" (case-insensitive) in Message and is dispatched
	// as success. Every other ErrSCMConflict is treated as a transient
	// precondition failure (head SHA drift, branch protection refusal)
	// and reattempted after re-reading the merge state. See [MergePR]
	// for the full disambiguation contract that implementations honor.
	ErrSCMConflict SCMErrorKind = "scm_conflict_error"
)

// SCMError is a structured error returned by [SCMAdapter]
// implementations. The Kind field enables structured logging and
// observability without inspecting error messages.
type SCMError struct {
	// Kind is the normalized error category.
	Kind SCMErrorKind

	// Message is an operator-friendly description of the failure.
	Message string

	// Err is the underlying error, if any.
	Err error
}

// Error returns a human-readable diagnostic including the error
// category and message.
func (e *SCMError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("scm: %s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("scm: %s: %s", e.Kind, e.Message)
}

// Unwrap returns the underlying error for use with [errors.Is] and
// [errors.As].
func (e *SCMError) Unwrap() error {
	return e.Err
}
