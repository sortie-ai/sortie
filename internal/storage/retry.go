package storage

import (
	"context"
)

// RetryEntry represents a persisted pending retry for a single issue.
// The timer_handle field from the domain model is runtime-only and is not
// included in the persisted representation.
type RetryEntry struct {
	IssueID    string  // Tracker-internal issue ID (primary key).
	Identifier string  // Human-readable ticket key (best-effort, for logs).
	Attempt    int     // Retry attempt number, 1-based.
	DueAtMs    int64   // Unix epoch milliseconds when the retry timer should fire.
	Error      *string // Last error message; nil when no error.
	SessionID  *string // Adapter-assigned session identifier from the previous worker attempt. Nil when no session was established.
	RuleName   string  // Dispatch rule name; empty for legacy rows and fallback dispatches.
	TemplateID string  // Resolved template path; empty selects the WORKFLOW.md body template.
	AgentKind  string  // Agent adapter kind; empty for legacy rows.

	// StagePrevious and StagePreviousOutcome are the frozen stage.previous
	// pair of the dispatch this retry continues; both are empty when no hop
	// led to it and for legacy rows.
	StagePrevious        string
	StagePreviousOutcome string

	// ChainID is the chain frozen with the retry's selection; empty for
	// legacy rows.
	ChainID string

	// DisplayID is the qualified issue form (e.g. "owner/repo#9"); IssueTitle
	// and IssueURL are the tracker's title and web link as read when the
	// retry was scheduled. Each is empty for legacy rows and for a retry
	// that holds no read of its issue.
	DisplayID  string
	IssueTitle string
	IssueURL   string

	// Trigger is the sortie_retries_total label of the schedule that
	// created the retry; empty for legacy rows and for a schedule that
	// records none.
	Trigger string

	// OriginTrigger is what started the retry, kept through every
	// put-back; empty when the retry never stored it, and for legacy rows.
	OriginTrigger string

	// UsageLimitNotified reports that the retried run ended in a provider
	// usage-limit refusal; stored as 0 or 1, false for legacy rows.
	UsageLimitNotified bool

	// ConfiguredModel is the model the retried dispatch was configured
	// with; empty when unset and for legacy rows.
	ConfiguredModel string

	// IssueType, IssueLabels, and IssueState are the tracker's facts as
	// read when the retry was scheduled. IssueLabels is nil when the read
	// recorded none (stored as an empty string) and an empty non-nil slice
	// when the read saw an issue without labels.
	IssueType   string
	IssueLabels []string
	IssueState  string

	// DispatchedAt is the RFC 3339 start of the first run of the pass this
	// retry belongs to; empty when the pass could not be dated.
	DispatchedAt string
}

// PendingRetry pairs a persisted [RetryEntry] with the computed delay
// remaining until its timer should fire. RemainingMs is zero when the entry's
// due time has already passed, meaning the retry should fire immediately on
// startup.
type PendingRetry struct {
	Entry       RetryEntry
	RemainingMs int64 // max(entry.DueAtMs - nowMs, 0); always >= 0
}

// RetryEntryStore records the issues scheduled for a retry.
type RetryEntryStore interface {
	SaveRetryEntry(ctx context.Context, entry RetryEntry) error
	LoadRetryEntries(ctx context.Context) ([]RetryEntry, error)
	DeleteRetryEntry(ctx context.Context, issueID string) error
	LoadRetryEntriesForRecovery(ctx context.Context, nowMs int64) ([]PendingRetry, error)
}
