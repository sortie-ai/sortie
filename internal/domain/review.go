package domain

// MaxVerificationOutputBytes is the per-stream cap on stdout and stderr
// captured from each verification command during the self-review loop.
// This prevents a runaway test suite from consuming unbounded memory.
const MaxVerificationOutputBytes = 65536

// VerificationResult holds the outcome of a single verification command
// executed during the self-review loop.
type VerificationResult struct {
	// Command is the shell command that was executed.
	Command string `json:"command"`

	// ExitCode is the process exit code. 0 indicates success.
	// -1 indicates the command could not be started or timed out.
	ExitCode int `json:"exit_code"`

	// Stdout is the captured standard output, truncated to
	// MaxVerificationOutputBytes.
	Stdout string `json:"stdout"`

	// Stderr is the captured standard error, truncated to
	// MaxVerificationOutputBytes.
	Stderr string `json:"stderr"`

	// DurationMS is the wall-clock execution time in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// TimedOut is true when the command exceeded VerificationTimeoutMS.
	TimedOut bool `json:"timed_out"`

	// ExecutionError is non-empty when the command could not be
	// started (binary not found, permission denied). Empty when the
	// command ran (regardless of exit code).
	ExecutionError string `json:"execution_error,omitempty"`
}

// ReviewIssue describes a single finding in the agent's review.
type ReviewIssue struct {
	// File is the relative path within the workspace.
	File string `json:"file"`

	// Line is the starting line number. 0 when not applicable.
	Line int `json:"line,omitempty"`

	// EndLine is the ending line number. 0 when not applicable.
	EndLine int `json:"end_line,omitempty"`

	// Severity is the finding severity: "error", "warning", "info".
	Severity string `json:"severity"`

	// Message describes the issue and suggested fix direction.
	Message string `json:"message"`
}

// ReviewVerdict is the structured response from the agent's review turn,
// read from .sortie/review_verdict.json in the workspace directory.
type ReviewVerdict struct {
	// Verdict is the agent's assessment: "pass" or "iterate".
	Verdict string `json:"verdict"`

	// Summary is a one-line summary of the review finding.
	Summary string `json:"summary"`

	// Issues is a list of specific findings. Present only when
	// Verdict is "iterate".
	Issues []ReviewIssue `json:"issues,omitempty"`
}

// ReviewIterationRecord captures one iteration of the self-review loop
// for persistence and observability.
type ReviewIterationRecord struct {
	// Iteration is the 1-based iteration number.
	Iteration int `json:"iteration"`

	// DiffSizeBytes is the size of the diff in bytes before truncation.
	DiffSizeBytes int `json:"diff_size_bytes"`

	// DiffTruncated is true when the diff was truncated to max_diff_bytes.
	DiffTruncated bool `json:"diff_truncated"`

	// VerificationResults holds the outcome of each verification command.
	VerificationResults []VerificationResult `json:"verification_results"`

	// Verdict is the parsed verdict from the agent. Empty when the
	// verdict file was missing or unparseable.
	Verdict string `json:"verdict"`

	// VerdictRaw is the raw JSON content of .sortie/review_verdict.json.
	// Empty when the file was absent.
	VerdictRaw string `json:"verdict_raw,omitempty"`

	// VerdictParseError is non-empty when the verdict file existed but
	// could not be parsed or was absent.
	VerdictParseError string `json:"verdict_parse_error,omitempty"`
}

// ReviewMetadata captures the self-review loop outcome for persistence,
// observability, and PR annotation.
type ReviewMetadata struct {
	// Enabled is true when self-review was configured and ran.
	Enabled bool `json:"enabled"`

	// Iterations holds the per-iteration records.
	Iterations []ReviewIterationRecord `json:"iterations"`

	// TotalIterations is the number of review iterations completed.
	TotalIterations int `json:"total_iterations"`

	// FinalVerdict is the last verdict: "pass", "iterate", or "none".
	FinalVerdict string `json:"final_verdict"`

	// CapReached is true when the iteration cap was reached without
	// a "pass" verdict.
	CapReached bool `json:"cap_reached"`
}
