package storage

import (
	"context"
	"time"
)

// HandoffAbsenceErrorPrefix marks a [RunHistory.Error] where a handoff was
// withheld because no work was observed. Keeping it stable lets the retry
// and polling paths reconstruct the consecutive-absence sequence without a
// verdict field.
const HandoffAbsenceErrorPrefix = "handoff withheld: "

// The verdicts that follow [HandoffAbsenceErrorPrefix] in a [RunHistory.Error].
// Whoever words the stored error for a person matches on them, so they are
// shared rather than repeated.
const (
	HandoffVerdictAbsence      = "absence of work observed"
	HandoffVerdictUndetermined = "evidence not determinable"
)

// ModelTokens is one model's token counts.
type ModelTokens struct {
	// InputTokens includes cache reads and writes, as [RunHistory.InputTokens] does.
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// ModelCost is the recorded price of one [RunModelPart]. A nil
// EstimatedCostUSD and an empty CostStatus store no value.
type ModelCost struct {
	EstimatedCostUSD  *float64
	CostStatus        string
	PriceSource       string
	PriceTableVersion string

	// UnratedClasses names the token classes that had no rate. An empty
	// CostStatus stores no value for it whatever it holds, so an unpriced part
	// reads as not recorded.
	UnratedClasses []string
}

// RunModelPart is one stored model part of a run, as read.
type RunModelPart struct {
	Model            string
	Tokens           ModelTokens
	EstimatedCostUSD *float64 // nil when absent
	CostStatus       string   // "" when absent

	// UnratedRecorded is false when no UnratedClasses value was stored;
	// UnratedClasses is nil when none was stored or it is empty.
	UnratedClasses  []string
	UnratedRecorded bool

	// PriceSource is the layer that priced the part; empty when none is
	// recorded.
	PriceSource string
}

// Values of [RunHistory.UsageCompleteness].
const (
	// UsageComplete means the run's token figure covers every turn.
	UsageComplete = "complete"

	// UsageLowerBound means a turn spent tokens no figure covers, so the
	// run's token total is a lower bound.
	UsageLowerBound = "lower_bound"

	// UsageUnmeasured means the agent reported no usage, so spend is unknown
	// rather than zero.
	UsageUnmeasured = "unmeasured"
)

// ReportedAmount is one amount the agent reported for a run, in the unit it
// reported it in.
type ReportedAmount struct {
	Unit       string
	Model      string // Empty when the source attributes the amount to no model.
	Amount     float64
	LowerBound bool // True when the run may have spent more in Unit than Amount.
}

// RunHistory is a single completed run attempt, as stored.
// ID is assigned by the database on insert; leave it zero when calling
// [RunHistoryStore.AppendRunHistory].
type RunHistory struct {
	ID             int64
	IssueID        string
	Identifier     string
	DisplayID      string // Qualified form (e.g. "owner/repo#9"); empty when Identifier suffices.
	Attempt        int
	AgentAdapter   string
	Workspace      string
	StartedAt      string
	CompletedAt    string
	Status         string // "succeeded", "failed", "cancelled", "ci_failed", "needs_person", "budget_stopped", or "usage_limited".
	Error          *string
	WorkflowFile   string // Base filename; empty for a run stored before the field existed.
	TurnsCompleted int
	ReviewMetadata *string // JSON ReviewMetadata; nil when self-review did not run.
	Skills         *string // JSON array of the run's skill calls, [] when it called none; nil only when the row records no skill calls.
	RuleName       string  // Empty for legacy rows and fallback dispatches.
	TemplateID     string  // Empty for legacy rows and the workflow body template.

	// ConfiguredModel and ConfiguredEffort are what the attempt was
	// configured with; ReportedModel is what the runtime reported running.
	// Each is empty when unset, and for a run stored before the fields existed.
	ConfiguredModel  string
	ConfiguredEffort string
	ReportedModel    string

	InputTokens      int64 // 0 for a run stored before the field existed.
	OutputTokens     int64 // 0 for a run stored before the field existed.
	TotalTokens      int64 // 0 for a run stored before the field existed.
	CacheReadTokens  int64 // 0 for a run stored before the field existed.
	CacheWriteTokens int64 // 0 for a run stored before the field existed.

	// TokensMeasured is true when the token fields carry a runtime-reported
	// figure and false when spend is unknown rather than zero. Every writer
	// must set it explicitly: a store records the value as passed, so a field
	// left at its zero value records an unmeasured run.
	TokensMeasured bool

	// UnaccountedTurns counts turns that spent tokens no figure was proven
	// to cover; that overspend reaches none of the token fields, so a
	// non-zero count makes this row's total a lower bound. Orthogonal to
	// TokensMeasured.
	UnaccountedTurns int

	// StagePrevious and StagePreviousOutcome are the frozen stage.previous
	// pair the attempt rendered with; both are empty when no hop led to the
	// dispatch and for a run stored before the fields existed.
	StagePrevious        string
	StagePreviousOutcome string

	// ChainID groups the runs of one pass through a chain of stages; it is
	// empty only for a run stored before the field existed.
	ChainID string

	// StageTarget and StageResult record the hop decision the attempt's
	// exit reached; both are empty when it reached none. StageResult is
	// "advanced", "partial", "failed", or "ceiling".
	StageTarget string
	StageResult string

	// IssueTitle and IssueURL are the tracker's title and web link as read
	// when the attempt was dispatched; both are empty for a run stored
	// before the fields existed.
	IssueTitle string
	IssueURL   string

	// DispatchedAt is the RFC 3339 start of the first run of the pass
	// ChainID names; empty when the pass could not be dated.
	DispatchedAt string

	// LastPromptAt is the RFC 3339 moment, with nanoseconds in UTC, the run
	// began its last turn; empty when the run began none, on a ci_failed
	// row, and for a run stored before the field existed. Only the appends
	// write it and only [RunHistoryStore.LatestAgentRuns] reads it.
	LastPromptAt string

	// Trigger is the sortie_retries_total label of the schedule that
	// started the retry whose fire dispatched the run; empty for a poll-tick
	// dispatch and for a retry a reaction other than the CI fix started. Nil
	// reads and writes as absent: the retry never stored what started it, or
	// the run was stored before the field existed.
	Trigger *string

	// ReactionKind is the dispatching retry's reaction kind; empty when none.
	ReactionKind string

	// SessionID is the agent session the run used; empty when the run
	// started none.
	SessionID string

	// SSHHost is the SSH worker the run executed on; empty for a local run.
	SSHHost string

	// OriginRecorded is false when SessionID has no stored value, which only a
	// run stored before the field existed holds. It is ignored on write.
	OriginRecorded bool

	// FirstAfterHop is true on the first run dispatched after the stage hop
	// StagePrevious names, and false on every other run.
	FirstAfterHop bool

	// DispatchID keys the run's retained events: the agent run's dispatch
	// ID, and "" on a row no agent run produced. Nil reads and writes as absent:
	// only a run stored before the field existed.
	DispatchID *string

	// AgentBlocked is true when the run's agent ended it with a blocked soft
	// stop. Only the appends write it; the run-history reads do not select it.
	AgentBlocked bool

	// DeclaredOutcome is the recognized status token the run's agent ended
	// it with, and AgentText the masked statement written after it. Both
	// appends write them and only [RunHistoryStore.QueryRunHistory] reads them; each
	// is empty when the run ended on no declaration or predates the fields.
	DeclaredOutcome string
	AgentText       string

	// ModelUsage is the run's per-model token counts. Only the
	// append that also records run events writes it; the run-history reads
	// do not select it.
	ModelUsage map[string]ModelTokens

	// EstimatedCostUSD and CostStatus are the run's recorded cost. Nil and ""
	// store no value. Only [RunHistoryStore.QueryRunHistory] reads them.
	EstimatedCostUSD *float64
	CostStatus       string

	// ModelCosts prices the parts of ModelUsage, keyed like it. Only the
	// append that also records run events writes it.
	ModelCosts map[string]ModelCost

	// UsageProvenance is the sorted, comma-joined source and strategy pairs
	// that settled the run's token figure; empty when none settled one. It
	// is written only when UsageCompleteness is set.
	UsageProvenance string

	// UsageCompleteness is [UsageComplete], [UsageLowerBound], or
	// [UsageUnmeasured]; empty stores no value here and in UsageProvenance, which
	// reads as not recorded.
	UsageCompleteness string

	// ReportedAmounts is every amount the agent reported for the run. Only the
	// append that also records run events writes it.
	ReportedAmounts []ReportedAmount

	// Turns is the run's turn records, ascending by Seq. Only the append that
	// also records run events writes it; the run-history reads do not select
	// it.
	Turns []RunTurn
}

// Completeness returns the run's stored completeness, or derives it from
// TokensMeasured and UnaccountedTurns for a row recorded before provenance
// was kept.
func (h RunHistory) Completeness() string {
	if h.UsageCompleteness != "" {
		return h.UsageCompleteness
	}
	return UsageCompletenessOf(h.TokensMeasured, h.UnaccountedTurns)
}

// RunHistoryFilter selects stored [RunHistory] records; a zero field matches
// every one.
type RunHistoryFilter struct {
	IssueID    string
	Identifier string

	// RuleName nil matches every row; a pointer to "" selects the rows whose
	// rule_name is empty.
	RuleName    *string
	Status      string
	StageTarget string

	// CompletedSince keeps rows completed at or after it and CompletedBefore
	// rows completed before it, both compared as instants.
	CompletedSince  *time.Time
	CompletedBefore *time.Time

	// EndedSince keeps rows whose issue's outcome was observed at or after it
	// and EndedBefore rows whose issue's outcome was observed before it, both
	// compared as instants the way CompletedSince compares completed_at. A
	// row whose issue has no outcome matches no ending bound.
	EndedSince  *time.Time
	EndedBefore *time.Time

	// HasDispatchID keeps rows whose dispatch_id is a non-empty string.
	HasDispatchID bool

	// BeforeID keeps rows with an id below it; 0 starts at the newest row.
	BeforeID int64

	// Limit is the most rows returned; below 1 reads as 1.
	Limit int
}

// IssueGroupFilter selects the issues [RunHistoryStore.QueryIssueGroups] groups; a zero
// field matches every run.
type IssueGroupFilter struct {
	// IssueKey keeps the issues with a run whose identifier or
	// display_identifier equals it.
	IssueKey string

	// Search keeps the issues with a run whose identifier,
	// display_identifier, or issue_title contains it, ignoring ASCII case.
	Search string

	// RuleName nil matches every run; a pointer to "" selects the runs whose
	// rule_name is empty.
	RuleName    *string
	Status      string
	StageTarget string

	// BeforeID continues after the group whose newest matching run has this
	// id; 0 starts at the newest group.
	BeforeID int64

	// Limit is the most groups returned; below 1 reads as 1.
	Limit int
}

// IssueGroup is one issue with a run the filter matched, and the id of its
// newest such run.
type IssueGroup struct {
	IssueID     string
	NewestRunID int64
}

// IssueTokenUsage is the token and cost spend of a set of [RunHistory] records:
// one issue's, or those completed in a window.
type IssueTokenUsage struct {
	// TotalTokens sums total_tokens, prompt-cache reads included. It is not
	// the figure the token ceiling compares; see UncachedTokens.
	TotalTokens int64

	// UncachedTokens sums, per row, total_tokens less cache_read_tokens
	// floored at zero. Flooring each row keeps one row's excess cache reads
	// from cancelling another row's spend. Cache writes stay counted.
	UncachedTokens int64

	Sessions           int
	UnmeasuredSessions int

	// StoppedInFlight counts rows recording a session a budget ceiling
	// stopped while running.
	StoppedInFlight int

	// UnaccountedTurns is the summed count of turns that spent tokens no
	// figure was proven to cover, so a non-zero value makes UncachedTokens a
	// lower bound even when every row is measured.
	UnaccountedTurns int

	// InputTokens, OutputTokens, CacheReadTokens, and CacheWriteTokens sum
	// each raw class over the measured rows. Only [RunHistoryStore.TokenUsageByIssue]
	// fills them.
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64

	// RecordedCostUSD and the session counts aggregate the recorded cost. A
	// row that spent nothing is in neither priced nor unpriced.
	RecordedCostUSD  float64
	PricedSessions   int
	PartialSessions  int
	UnpricedSessions int
}

// SpendIncomplete reports whether UncachedTokens is a lower bound rather than
// the whole of the issue's spend. Either count that leaves spend out of the
// sum answers true.
func (u IssueTokenUsage) SpendIncomplete() bool {
	return u.UnmeasuredSessions > 0 || u.UnaccountedTurns > 0
}

// ModelUse is the use of one model under one agent kind across recorded runs.
type ModelUse struct {
	AgentKind string
	// Model is possibly empty.
	Model string
	// LastUsedAt is the latest completed_at of the runs, zero when none parses.
	LastUsedAt time.Time
	// Runs counts the runs that have a [RunModelPart] for the pair.
	Runs int
}

// RunHistoryStore records completed runs and serves the reads over them:
// per-issue history, token and spend totals, handoff-absence sequences, model
// use, and the statistics scan.
type RunHistoryStore interface {
	AppendRunHistory(ctx context.Context, run RunHistory) (RunHistory, error)
	RecordRunStageResult(ctx context.Context, runID int64, targetRule, result string) error
	ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error

	// price must not perform I/O: an implementation may hold its only
	// connection for the whole fill.
	RecordUnrecordedRunCosts(ctx context.Context, price func(UnrecordedRun) RunCostRecord) (int, error)

	QueryRunHistory(ctx context.Context, f RunHistoryFilter) ([]RunHistory, error)
	QueryRunHistoryByIssue(ctx context.Context, issueID string) ([]RunHistory, error)
	QueryRunModelUsage(ctx context.Context, runIDs []int64) (map[int64][]RunModelPart, error)
	QueryRunReportedAmounts(ctx context.Context, runIDs []int64) (map[int64][]ReportedAmount, error)
	QueryIssueGroups(ctx context.Context, f IssueGroupFilter) ([]IssueGroup, error)
	LoadLatestSuccessfulRunsForReactionRecovery(ctx context.Context, completedAfter time.Time, limit int) ([]RunHistory, error)
	LoadLatestSuccessfulRunsForIssues(ctx context.Context, issueIDs []string) ([]RunHistory, error)
	LatestAgentRuns(ctx context.Context, issueIDs []string) (map[string]RunHistory, error)
	LatestRunCompletionByIdentifier(ctx context.Context, identifiers []string) (map[string]string, error)
	CountRunHistoryByIssue(ctx context.Context, issueID string) (int, error)
	CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error)
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
	QueryBudgetExhaustedIssues(ctx context.Context, candidateIDs []string, maxSessions int) (map[string]int, error)
	TokenUsageByIssue(ctx context.Context, issueID string) (IssueTokenUsage, error)
	QueryTokenBudgetUsage(ctx context.Context, candidateIDs []string) (map[string]IssueTokenUsage, error)
	UsageCompletedBetween(ctx context.Context, since, until time.Time) (IssueTokenUsage, error)
	IssueSpendProvenance(ctx context.Context, issueID string) (IssueSpendProvenance, error)
	ListModelUse(ctx context.Context) ([]ModelUse, error)
	ListReportedModels(ctx context.Context) ([]string, error)
	RunHistoryCapabilities(ctx context.Context) (RunHistoryCapabilities, error)

	// visit must not call the store: an implementation may hold its only
	// connection until the scan ends, so such a call can block until ctx is
	// cancelled.
	ScanRunHistoryRange(ctx context.Context, caps RunHistoryCapabilities, since, until *time.Time, visit func(RunStatsRow) error) error
}
