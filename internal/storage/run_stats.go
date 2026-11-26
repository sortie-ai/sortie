package storage

// RunHistoryCapabilities reports which optional [RunHistory] fields a
// database carries, so a read-only caller can pick a projection the schema
// supports. A field the underlying database does not carry holds its zero
// value in every [RunStatsRow] the read returns.
type RunHistoryCapabilities struct {
	HasTurnsCompleted   bool // [RunStatsRow.TurnsCompleted] is stored
	HasReviewMetadata   bool // [RunStatsRow.ReviewMetadata] is stored
	HasRuleRouting      bool // [RunStatsRow.RuleName] and [RunStatsRow.TemplateID] are stored
	HasTokens           bool // [RunStatsRow.InputTokens], [RunStatsRow.OutputTokens], [RunStatsRow.TotalTokens], and [RunStatsRow.CacheReadTokens] are stored
	HasTokenMeasurement bool // [RunStatsRow.TokensMeasured] is stored
	HasCacheWriteTokens bool // [RunStatsRow.CacheWriteTokens] is stored

	// HasConfiguredSettings is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.ModelBreakdown] depends on it.
	HasConfiguredSettings bool

	// HasStageChains is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.ChainBreakdown] depends on it.
	HasStageChains bool

	// HasTrigger is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.TriggerBreakdown] depends on it, and
	// [RunHistoryCapabilities.OriginBreakdown] through it.
	HasTrigger bool // [RunStatsRow.Trigger] and [RunStatsRow.ReactionKind] are stored

	// HasFirstAfterHop is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.OriginBreakdown] depends on it.
	HasFirstAfterHop bool // [RunStatsRow.FirstAfterHop] is stored

	// HasAgentBlocked is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.FunnelBreakdown] depends on it.
	HasAgentBlocked bool // [RunStatsRow.AgentBlocked] is stored

	// HasRecordedCost is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.RecordedCost] depends on it.
	HasRecordedCost bool

	// HasSkills is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.SkillBreakdown] depends on it.
	HasSkills bool

	// HasUsageProvenance is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.UsageProvenance] depends on it. It is true when
	// the database stores [RunStatsRow.UsageCompleteness] and
	// [RunStatsRow.UsageProvenance] and the amounts a run reported, which
	// [RunStatsRow.ReportedUnits] reads.
	HasUsageProvenance bool // [RunStatsRow.UsageCompleteness] and [RunStatsRow.UsageProvenance] are stored

	// HasRunTurns is outside [RunHistoryCapabilities.Full]. It is true when
	// the database has the turn-record tables that [RunTurnStore] reads.
	HasRunTurns bool
}

// Full reports whether the database carries every optional [RunHistory]
// field group. It is the single owner of the capability tier decision:
// [RunHistoryStore.ScanRunHistoryRange] selects its projection from this predicate and
// no caller may re-derive the conjunction or branch on an individual flag,
// because a database migrated only partway is the common case rather than
// an exceptional one.
func (c RunHistoryCapabilities) Full() bool {
	return c.HasTurnsCompleted && c.HasReviewMetadata && c.HasRuleRouting && c.HasTokens && c.HasTokenMeasurement
}

// ModelBreakdown reports whether a read can group runs by model: Full plus
// the configured-settings fields.
func (c RunHistoryCapabilities) ModelBreakdown() bool {
	return c.Full() && c.HasConfiguredSettings
}

// ChainBreakdown reports whether a read can group runs by stage chain:
// ModelBreakdown plus the stage-chain fields.
func (c RunHistoryCapabilities) ChainBreakdown() bool {
	return c.ModelBreakdown() && c.HasStageChains
}

// TriggerBreakdown reports whether a read can group runs by retry trigger:
// ChainBreakdown plus the trigger field.
func (c RunHistoryCapabilities) TriggerBreakdown() bool {
	return c.ChainBreakdown() && c.HasTrigger
}

// OriginBreakdown reports whether a read can group runs by run origin:
// TriggerBreakdown plus the first-run-after-hop field.
func (c RunHistoryCapabilities) OriginBreakdown() bool {
	return c.TriggerBreakdown() && c.HasFirstAfterHop
}

// FunnelBreakdown reports whether a read can fold runs into stage funnels:
// OriginBreakdown plus the blocked-run field.
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
// RecordedCost plus the skills field.
func (c RunHistoryCapabilities) SkillBreakdown() bool {
	return c.RecordedCost() && c.HasSkills
}

// UsageProvenance reports whether the stats report can read each run's usage
// completeness, provenance, and reported units: RecordedCost plus the
// provenance fields.
func (c RunHistoryCapabilities) UsageProvenance() bool {
	return c.RecordedCost() && c.HasUsageProvenance
}

// RunStatsRow is the narrow [RunHistory] projection the aggregate read
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
	ReviewMetadata   *string // nil when no value is stored or the store lacks the field
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64 // 0 when the database lacks the field
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

	// Trigger is nil when no value is stored or
	// [RunHistoryCapabilities.TriggerBreakdown] reports false.
	Trigger *string

	// ReactionKind is empty when no value is stored or it is empty, or
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

	// EstimatedCostUSD is nil when no value is stored or
	// [RunHistoryCapabilities.RecordedCost] reports false. CostStatus is empty
	// for a status with no stored value and under the same condition.
	EstimatedCostUSD *float64
	CostStatus       string

	// Parts are the run's [RunModelPart] records ordered by model, carrying
	// Model, Tokens, EstimatedCostUSD, CostStatus, and PriceSource;
	// UnratedClasses and UnratedRecorded stay zero. Nil when the run has none
	// or [RunHistoryCapabilities.RecordedCost] reports false.
	Parts []RunModelPart

	// UnaccountedTurns is [RunHistory.UnaccountedTurns]; zero when
	// [RunHistoryCapabilities.RecordedCost] reports false.
	UnaccountedTurns int

	// Skills is [RunHistory.Skills] as stored; nil when no value is stored or
	// [RunHistoryCapabilities.SkillBreakdown] reports false.
	Skills *string

	// UsageCompleteness and UsageProvenance are the stored values; each is
	// empty when no value is stored or [RunHistoryCapabilities.UsageProvenance]
	// reports false.
	UsageCompleteness string
	UsageProvenance   string

	// ReportedUnits holds the distinct units of the run's reported amounts,
	// ascending, US dollars included. It is nil when the run reported none or
	// [RunHistoryCapabilities.UsageProvenance] reports false.
	ReportedUnits []string
}
