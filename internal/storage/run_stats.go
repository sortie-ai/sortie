package storage

// RunHistoryCapabilities reports which optional run_history columns a
// database carries, so a read-only caller can pick a projection the schema
// supports. A field the underlying table does not carry holds its zero
// value in every [RunStatsRow] the read returns.
type RunHistoryCapabilities struct {
	HasTurnsCompleted   bool // migration 005
	HasReviewMetadata   bool // migration 007
	HasRuleRouting      bool // migration 010: rule_name, template_id
	HasTokens           bool // migration 011: the four token columns
	HasTokenMeasurement bool // migration 012: tokens_measured
	HasCacheWriteTokens bool // migration 020

	// HasConfiguredSettings is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.ModelBreakdown] depends on it.
	HasConfiguredSettings bool

	// HasStageChains is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.ChainBreakdown] depends on it.
	HasStageChains bool

	// HasTrigger is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.TriggerBreakdown] depends on it, and
	// [RunHistoryCapabilities.OriginBreakdown] through it.
	HasTrigger bool // migration 028: trigger, reaction_kind

	// HasFirstAfterHop is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.OriginBreakdown] depends on it.
	HasFirstAfterHop bool // migration 032: first_after_hop

	// HasAgentBlocked is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.FunnelBreakdown] depends on it.
	HasAgentBlocked bool // migration 037: agent_blocked

	// HasRecordedCost is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.RecordedCost] depends on it.
	HasRecordedCost bool

	// HasSkills is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.SkillBreakdown] depends on it.
	HasSkills bool

	// HasUsageProvenance is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.UsageProvenance] depends on it. It is true when
	// run_history carries usage_completeness and usage_provenance and the
	// run_reported_amounts table exists.
	HasUsageProvenance bool // migration 050
}

// Full reports whether the database carries every optional run_history
// column group. It is the single owner of the capability tier decision:
// [RunHistoryStore.ScanRunHistoryRange] selects its projection from this predicate and
// no caller may re-derive the conjunction or branch on an individual flag,
// because a database migrated only partway is the common case rather than
// an exceptional one.
func (c RunHistoryCapabilities) Full() bool {
	return c.HasTurnsCompleted && c.HasReviewMetadata && c.HasRuleRouting && c.HasTokens && c.HasTokenMeasurement
}

// ModelBreakdown reports whether a read can group runs by model: Full plus
// the configured-settings columns.
func (c RunHistoryCapabilities) ModelBreakdown() bool {
	return c.Full() && c.HasConfiguredSettings
}

// ChainBreakdown reports whether a read can group runs by stage chain:
// ModelBreakdown plus the stage-chain columns.
func (c RunHistoryCapabilities) ChainBreakdown() bool {
	return c.ModelBreakdown() && c.HasStageChains
}

// TriggerBreakdown reports whether a read can group runs by retry trigger:
// ChainBreakdown plus the trigger column.
func (c RunHistoryCapabilities) TriggerBreakdown() bool {
	return c.ChainBreakdown() && c.HasTrigger
}

// OriginBreakdown reports whether a read can group runs by run origin:
// TriggerBreakdown plus the first-run-after-hop column.
func (c RunHistoryCapabilities) OriginBreakdown() bool {
	return c.TriggerBreakdown() && c.HasFirstAfterHop
}

// FunnelBreakdown reports whether a read can fold runs into stage funnels:
// OriginBreakdown plus the blocked-run column.
func (c RunHistoryCapabilities) FunnelBreakdown() bool {
	return c.OriginBreakdown() && c.HasAgentBlocked
}

// RecordedCost reports whether the stats report can read the recorded cost.
// It requires FunnelBreakdown too, so a partly migrated database reports no
// cost rather than a partial one.
func (c RunHistoryCapabilities) RecordedCost() bool {
	return c.FunnelBreakdown() && c.HasRecordedCost
}

// SkillBreakdown reports whether a read can fold each run's skill calls:
// RecordedCost plus the skills column.
func (c RunHistoryCapabilities) SkillBreakdown() bool {
	return c.RecordedCost() && c.HasSkills
}

// UsageProvenance reports whether the stats report can read each run's usage
// completeness, provenance, and reported units: RecordedCost plus the
// provenance columns.
func (c RunHistoryCapabilities) UsageProvenance() bool {
	return c.RecordedCost() && c.HasUsageProvenance
}

// RunStatsRow is the narrow run_history projection the aggregate read
// returns. A field the schema does not carry holds its zero value.
type RunStatsRow struct {
	// IssueID is filled on every capability tier.
	IssueID string

	Status           string
	AgentAdapter     string
	RuleName         string
	TemplateID       string
	StartedAt        string // ISO-8601 as stored
	CompletedAt      string // ISO-8601 as stored
	TurnsCompleted   int
	ReviewMetadata   *string // nil when the column is NULL or absent
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64 // 0 when the database lacks the column
	TokensMeasured   bool

	// ConfiguredModel and ReportedModel are empty when the run named or
	// reported none, or [RunHistoryCapabilities.ModelBreakdown] reports false.
	ConfiguredModel string
	ReportedModel   string

	// Each is empty unless [RunHistoryCapabilities.ChainBreakdown] reports
	// true.
	Identifier    string
	DisplayID     string
	ChainID       string
	StagePrevious string
	StageResult   string

	// Trigger is nil when the column is NULL or
	// [RunHistoryCapabilities.TriggerBreakdown] reports false.
	Trigger *string

	// ReactionKind is empty when the column is NULL or empty, or
	// [RunHistoryCapabilities.TriggerBreakdown] reports false.
	ReactionKind string

	// FirstAfterHop is false unless [RunHistoryCapabilities.OriginBreakdown]
	// reports true.
	FirstAfterHop bool

	// Each is false unless [RunHistoryCapabilities.FunnelBreakdown] reports
	// true. HandoffWithheld marks a failed run whose error begins with
	// [HandoffAbsenceErrorPrefix].
	AgentBlocked    bool
	HandoffWithheld bool

	// EstimatedCostUSD is nil when the column is NULL or
	// [RunHistoryCapabilities.RecordedCost] reports false. CostStatus is empty
	// for a NULL status and under the same condition.
	EstimatedCostUSD *float64
	CostStatus       string

	// Parts are the run's run_model_usage rows ordered by model, carrying
	// Model, Tokens, EstimatedCostUSD, CostStatus, and PriceSource;
	// UnratedClasses and UnratedRecorded stay zero. Nil when the run has none
	// or [RunHistoryCapabilities.RecordedCost] reports false.
	Parts []RunModelPart

	// UnaccountedTurns is run_history.unaccounted_turns; zero when
	// [RunHistoryCapabilities.RecordedCost] reports false.
	UnaccountedTurns int

	// Skills is run_history.skills as stored; nil when the column is NULL or
	// [RunHistoryCapabilities.SkillBreakdown] reports false.
	Skills *string

	// UsageCompleteness and UsageProvenance are the stored values; each is
	// empty when the column is NULL or [RunHistoryCapabilities.UsageProvenance]
	// reports false.
	UsageCompleteness string
	UsageProvenance   string

	// ReportedUnits holds the distinct units of the run's reported amounts,
	// ascending, US dollars included. It is nil when the run reported none or
	// [RunHistoryCapabilities.UsageProvenance] reports false.
	ReportedUnits []string
}
