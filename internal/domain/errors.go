package domain

import "fmt"

// TrackerErrorKind enumerates the normalized error categories that
// tracker adapters map their native errors to. The orchestrator uses
// these categories to decide retry, skip, or fail behavior.
type TrackerErrorKind string

const (
	// ErrUnsupportedTrackerKind indicates the configured tracker kind
	// has no registered adapter.
	ErrUnsupportedTrackerKind TrackerErrorKind = "unsupported_tracker_kind"

	// ErrMissingTrackerAPIKey indicates the tracker API key is absent
	// after environment variable resolution.
	ErrMissingTrackerAPIKey TrackerErrorKind = "missing_tracker_api_key"

	// ErrMissingTrackerProject indicates the tracker project is absent
	// when required by the adapter.
	ErrMissingTrackerProject TrackerErrorKind = "missing_tracker_project"

	// ErrTrackerTransport indicates a network or transport failure.
	ErrTrackerTransport TrackerErrorKind = "tracker_transport_error"

	// ErrTrackerAuth indicates an authentication or authorization failure.
	ErrTrackerAuth TrackerErrorKind = "tracker_auth_error"

	// ErrTrackerAPI indicates a non-200 HTTP or API-level error that
	// may be transient (e.g. rate limiting, server errors).
	ErrTrackerAPI TrackerErrorKind = "tracker_api_error"

	// ErrTrackerNotFound indicates the requested resource does not exist
	// in the tracker (e.g. HTTP 404). Non-retryable.
	ErrTrackerNotFound TrackerErrorKind = "tracker_not_found"

	// ErrTrackerPayload indicates a malformed or unexpected response
	// structure from the tracker.
	ErrTrackerPayload TrackerErrorKind = "tracker_payload_error"

	// ErrTrackerMissingCursor indicates a pagination integrity error
	// where the expected end cursor is absent.
	ErrTrackerMissingCursor TrackerErrorKind = "tracker_missing_end_cursor"
)

// TrackerError is a structured error returned by [TrackerAdapter]
// implementations. The Kind field enables the orchestrator to make
// category-based decisions (retry on transport, skip on auth, etc.)
// without inspecting error messages.
type TrackerError struct {
	// Kind is the normalized error category.
	Kind TrackerErrorKind

	// Message is an operator-friendly description of the failure.
	Message string

	// Err is the underlying error, if any.
	Err error
}

// Error returns a human-readable diagnostic including the error
// category and message.
func (e *TrackerError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("tracker: %s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("tracker: %s: %s", e.Kind, e.Message)
}

// Unwrap returns the underlying error for use with [errors.Is] and
// [errors.As].
func (e *TrackerError) Unwrap() error {
	return e.Err
}

// AgentErrorKind enumerates the normalized error categories that
// agent adapters map their native errors to. The orchestrator uses
// these categories to decide retry behavior.
type AgentErrorKind string

const (
	// ErrAgentNotFound indicates the configured agent command or
	// executable could not be located.
	ErrAgentNotFound AgentErrorKind = "agent_not_found"

	// ErrInvalidWorkspaceCwd indicates the workspace path provided to
	// the adapter is invalid or inaccessible.
	ErrInvalidWorkspaceCwd AgentErrorKind = "invalid_workspace_cwd"

	// ErrResponseTimeout indicates a request/response timeout during
	// startup or synchronous communication.
	ErrResponseTimeout AgentErrorKind = "response_timeout"

	// ErrTurnTimeout indicates the total turn duration exceeded the
	// configured turn_timeout_ms.
	ErrTurnTimeout AgentErrorKind = "turn_timeout"

	// ErrPortExit indicates the agent subprocess exited unexpectedly.
	ErrPortExit AgentErrorKind = "port_exit"

	// ErrResponseError indicates the agent returned a protocol-level
	// error response.
	ErrResponseError AgentErrorKind = "response_error"

	// ErrTurnFailed indicates the agent turn completed with a failure
	// status.
	ErrTurnFailed AgentErrorKind = "turn_failed"

	// ErrTurnCancelled indicates the agent turn was cancelled.
	ErrTurnCancelled AgentErrorKind = "turn_cancelled"

	// ErrTurnInputRequired indicates the agent requested user input,
	// which is a hard failure per policy.
	ErrTurnInputRequired AgentErrorKind = "turn_input_required"
)

// AgentError is a structured error returned by [AgentAdapter]
// implementations. The Kind field enables the orchestrator to make
// category-based decisions (retry on timeout, fail on input required,
// etc.) without inspecting error messages.
type AgentError struct {
	// Kind is the normalized error category.
	Kind AgentErrorKind

	// Message is an operator-friendly description of the failure.
	Message string

	// Err is the underlying error, if any.
	Err error
}

// Error returns a human-readable diagnostic including the error
// category and message.
func (e *AgentError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("agent: %s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("agent: %s: %s", e.Kind, e.Message)
}

// Unwrap returns the underlying error for use with [errors.Is] and
// [errors.As].
func (e *AgentError) Unwrap() error {
	return e.Err
}

// BackoffStrategy classifies the recommended delay behavior when
// retrying after a specific error category.
type BackoffStrategy string

const (
	// BackoffNone indicates the error is not retryable. The orchestrator
	// should release the claim instead of scheduling a retry.
	BackoffNone BackoffStrategy = "none"

	// BackoffExponential indicates exponential-backoff retry using the
	// formula min(10000 * 2^(attempt-1), max_retry_backoff_ms).
	BackoffExponential BackoffStrategy = "exponential"
)

// RetryClassification describes whether an error category is retryable
// and, if so, the recommended backoff strategy.
type RetryClassification struct {
	// Retryable is true when the error category represents a transient
	// or recoverable condition that may succeed on a subsequent attempt.
	Retryable bool

	// Backoff is the recommended delay strategy. Meaningful only when
	// Retryable is true; BackoffNone when Retryable is false.
	Backoff BackoffStrategy
}

// RetryClassification returns the default retry semantics for this
// tracker error kind. The orchestrator uses this to decide between
// exponential-backoff retry and releasing the claim.
func (k TrackerErrorKind) RetryClassification() RetryClassification {
	switch k {
	case ErrUnsupportedTrackerKind, ErrMissingTrackerAPIKey, ErrMissingTrackerProject:
		return RetryClassification{Retryable: false, Backoff: BackoffNone}
	case ErrTrackerAuth:
		return RetryClassification{Retryable: false, Backoff: BackoffNone}
	case ErrTrackerNotFound:
		return RetryClassification{Retryable: false, Backoff: BackoffNone}
	case ErrTrackerPayload:
		return RetryClassification{Retryable: false, Backoff: BackoffNone}
	case ErrTrackerTransport, ErrTrackerAPI, ErrTrackerMissingCursor:
		return RetryClassification{Retryable: true, Backoff: BackoffExponential}
	default:
		return RetryClassification{Retryable: true, Backoff: BackoffExponential}
	}
}

// RetryClassification returns the default retry semantics for this
// agent error kind. The orchestrator uses this to decide between
// exponential-backoff retry and releasing the claim.
func (k AgentErrorKind) RetryClassification() RetryClassification {
	switch k {
	case ErrAgentNotFound, ErrInvalidWorkspaceCwd:
		return RetryClassification{Retryable: false, Backoff: BackoffNone}
	// Stall-induced cancellations are retried by the reconciliation path,
	// not by the generic retry classification. The exit handler for
	// cancelled workers should only perform bookkeeping.
	case ErrTurnCancelled:
		return RetryClassification{Retryable: false, Backoff: BackoffNone}
	case ErrTurnInputRequired:
		return RetryClassification{Retryable: false, Backoff: BackoffNone}
	case ErrResponseTimeout, ErrTurnTimeout, ErrPortExit, ErrResponseError, ErrTurnFailed:
		return RetryClassification{Retryable: true, Backoff: BackoffExponential}
	default:
		return RetryClassification{Retryable: true, Backoff: BackoffExponential}
	}
}
