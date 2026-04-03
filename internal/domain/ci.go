package domain

import (
	"context"
	"fmt"
)

// CIStatus represents the aggregate CI pipeline status for a git ref.
type CIStatus string

const (
	// CIStatusPending indicates CI checks are still running or no
	// checks have been reported yet.
	CIStatusPending CIStatus = "pending"

	// CIStatusPassing indicates all checks have completed
	// successfully.
	CIStatusPassing CIStatus = "passing"

	// CIStatusFailing indicates at least one check has completed
	// with a failure conclusion.
	CIStatusFailing CIStatus = "failing"
)

// CheckConclusion represents the normalized conclusion of a single CI
// check run. Adapters map their platform-native conclusions to these
// values. Unknown or unmappable conclusions map to
// [CheckConclusionPending].
type CheckConclusion string

const (
	// CheckConclusionSuccess indicates the check run succeeded.
	CheckConclusionSuccess CheckConclusion = "success"

	// CheckConclusionFailure indicates the check run failed.
	CheckConclusionFailure CheckConclusion = "failure"

	// CheckConclusionCancelled indicates the check run was cancelled.
	CheckConclusionCancelled CheckConclusion = "cancelled"

	// CheckConclusionTimedOut indicates the check run timed out.
	CheckConclusionTimedOut CheckConclusion = "timed_out"

	// CheckConclusionNeutral indicates the check run completed with a
	// neutral outcome.
	CheckConclusionNeutral CheckConclusion = "neutral"

	// CheckConclusionSkipped indicates the check run was skipped.
	CheckConclusionSkipped CheckConclusion = "skipped"

	// CheckConclusionPending indicates the check run has not yet
	// concluded. Also used as the default for unknown platform
	// conclusions.
	CheckConclusionPending CheckConclusion = "pending"
)

// CheckRunStatus represents the execution status of a single CI check
// run.
type CheckRunStatus string

const (
	// CheckRunStatusQueued indicates the check run is waiting to
	// execute.
	CheckRunStatusQueued CheckRunStatus = "queued"

	// CheckRunStatusInProgress indicates the check run is currently
	// executing.
	CheckRunStatusInProgress CheckRunStatus = "in_progress"

	// CheckRunStatusCompleted indicates the check run has finished
	// executing.
	CheckRunStatusCompleted CheckRunStatus = "completed"
)

// CheckRun represents a single CI check run within a pipeline.
type CheckRun struct {
	// Name is the check run name as defined by the CI platform
	// (e.g. "test", "lint", "build").
	Name string

	// Status is the execution status of the check run. Adapters
	// normalize to one of the three [CheckRunStatus] values.
	Status CheckRunStatus

	// Conclusion is the normalized outcome. Meaningful only when
	// Status is [CheckRunStatusCompleted]. Zero value when the check
	// has not completed.
	Conclusion CheckConclusion

	// DetailsURL is the web URL to the check run's detail page on
	// the CI platform. Empty string when unavailable.
	DetailsURL string
}

// CIResult is the normalized CI pipeline status for a git ref,
// returned by [CIStatusProvider.FetchCIStatus].
type CIResult struct {
	// Status is the aggregate pipeline status computed from individual
	// check runs.
	Status CIStatus

	// CheckRuns is the list of individual check runs. Non-nil empty
	// slice when the ref has no check runs (distinct from nil, which
	// means check runs were not fetched).
	CheckRuns []CheckRun

	// LogExcerpt is a truncated log from the first failing check run.
	// Empty string when all checks pass, the provider does not support
	// log fetching, MaxLogLines is zero in config, or the log could
	// not be retrieved. The orchestrator omits the log section from
	// the continuation prompt when this field is empty.
	//
	// CI logs may contain secrets accidentally printed by build
	// scripts. Adapters must truncate to a configurable line count
	// and should strip ANSI escape sequences. Consumers must not
	// persist log excerpts to the database or expose them via
	// unauthenticated API endpoints.
	LogExcerpt string

	// FailingCount is the number of check runs with a failure
	// conclusion. Precomputed by the adapter for template convenience.
	FailingCount int

	// Ref is the git ref (branch name or SHA) that was queried.
	// Echoed back for observability and template rendering.
	Ref string
}

// ToTemplateMap converts the CIResult to a map[string]any with
// snake_case keys suitable for prompt template rendering as the
// "ci_failure" variable.
func (r *CIResult) ToTemplateMap() map[string]any {
	runs := r.CheckRuns
	if runs == nil {
		runs = []CheckRun{}
	}
	checkRunMaps := make([]map[string]any, len(runs))
	for i, cr := range runs {
		checkRunMaps[i] = map[string]any{
			"name":        cr.Name,
			"status":      string(cr.Status),
			"conclusion":  string(cr.Conclusion),
			"details_url": cr.DetailsURL,
		}
	}

	return map[string]any{
		"status":        string(r.Status),
		"check_runs":    checkRunMaps,
		"log_excerpt":   r.LogExcerpt,
		"failing_count": r.FailingCount,
		"ref":           r.Ref,
	}
}

// CIStatusProvider defines the contract that all CI/CD platform
// integrations must satisfy. Each adapter normalizes its native API
// responses into domain [CIResult] values.
//
// Implementations must be safe for concurrent use by the
// orchestrator's reconcile loop. The orchestrator may call
// FetchCIStatus for multiple workspaces concurrently.
type CIStatusProvider interface {
	// FetchCIStatus returns the aggregate CI pipeline status for the
	// given git ref (branch name or commit SHA).
	//
	// The ref parameter is a git ref string. Adapters that require a
	// full commit SHA must resolve branch names to SHAs internally.
	//
	// Returns a zero-value [CIResult] and a non-nil [*CIError] on
	// failure. All error categories are non-fatal from the
	// orchestrator's perspective.
	FetchCIStatus(ctx context.Context, ref string) (CIResult, error)
}

// CIErrorKind enumerates the normalized error categories that CI
// status provider adapters map their native errors to. The
// orchestrator uses these categories for logging and observability;
// all CI errors are non-fatal.
type CIErrorKind string

const (
	// ErrCITransport indicates a network or transport failure
	// (connection error, DNS failure, TLS handshake failure).
	ErrCITransport CIErrorKind = "ci_transport_error"

	// ErrCIAuth indicates an authentication or authorization failure
	// (expired token, insufficient scopes).
	ErrCIAuth CIErrorKind = "ci_auth_error"

	// ErrCIAPI indicates a non-success HTTP status or API-level error
	// (rate limiting, server error, unexpected status code).
	ErrCIAPI CIErrorKind = "ci_api_error"

	// ErrCINotFound indicates the requested ref or repository does
	// not exist on the CI platform (HTTP 404).
	ErrCINotFound CIErrorKind = "ci_not_found"

	// ErrCIPayload indicates a malformed or unexpected response
	// structure from the CI platform.
	ErrCIPayload CIErrorKind = "ci_payload_error"
)

// CIError is a structured error returned by [CIStatusProvider]
// implementations. The Kind field enables structured logging and
// observability without inspecting error messages.
type CIError struct {
	// Kind is the normalized error category.
	Kind CIErrorKind

	// Message is an operator-friendly description of the failure.
	Message string

	// Err is the underlying error, if any.
	Err error
}

// Error returns a human-readable diagnostic including the error
// category and message.
func (e *CIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("ci: %s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("ci: %s: %s", e.Kind, e.Message)
}

// Unwrap returns the underlying error for use with [errors.Is] and
// [errors.As].
func (e *CIError) Unwrap() error {
	return e.Err
}
