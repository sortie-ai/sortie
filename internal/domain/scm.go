package domain

import (
	"context"
	"fmt"
)

// SCMAdapter provides read-only access to SCM platform features
// beyond CI status. Implementations must be safe for concurrent use.
type SCMAdapter interface {
	// FetchPendingReviews returns review comments from non-bot
	// CHANGES_REQUESTED reviews on the given PR. Comments marked as
	// outdated by the platform are included with Outdated=true; the
	// caller is responsible for filtering.
	//
	// Returns an empty non-nil slice when no pending review comments
	// exist. Returns a [*SCMError] on failure.
	FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]ReviewComment, error)
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
