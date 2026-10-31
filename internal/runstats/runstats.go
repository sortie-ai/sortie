// Package runstats provides the run statistics envelope and the aggregation
// that computes it from persisted run history.
//
// [New] constructs an [Aggregator] for the capabilities of the database being
// read. [Aggregator.Add] folds one [persistence.RunStatsRow] at a time, and
// [Aggregator.Report] returns the finished [Report]. The package imports only
// the domain and persistence layers, so any caller above them can serve the
// same figures.
package runstats

// Report is the "sortie stats --format json" envelope. Every slice
// field, by_trigger, by_origin, by_provenance, and chain_funnels included, is
// never nil; every pointer field is null when the figure it carries is
// unavailable. Spend is nil unless
// [persistence.RunHistoryCapabilities.RecordedCost] reports true.
type Report struct {
	GeneratedAt  string           `json:"generated_at"`
	WorkflowPath string           `json:"workflow_path"`
	DBPath       string           `json:"db_path"`
	Since        *string          `json:"since"`
	Until        *string          `json:"until"`
	SchemaTier   string           `json:"schema_tier"`
	Warnings     []string         `json:"warnings"`
	Summary      Summary          `json:"summary"`
	Spend        *Spend           `json:"spend"`
	ByStatus     []Group          `json:"by_status"`
	ByAdapter    []Group          `json:"by_adapter"`
	ByRule       []Group          `json:"by_rule"`
	ByTemplate   []Group          `json:"by_template"`
	ByModel      []Group          `json:"by_model"`
	ByChain      []ChainGroup     `json:"by_chain"`
	ChainFunnels []Funnel         `json:"chain_funnels"`
	ByTrigger    []Group          `json:"by_trigger"`
	ByOrigin     []Group          `json:"by_origin"`
	ByProvenance []Group          `json:"by_provenance"`
	SkillCalls   *SkillCallReport `json:"skill_calls"`
	SelfReview   *SelfReview      `json:"self_review"`
}

// Summary reports the report-wide figures. MeanTurnsSucceeded,
// Tokens, CostUSD, and CostPerSucceededRunUSD are null on the base
// schema tier; CostUSD and CostPerSucceededRunUSD are also null when
// no run that spent tokens is priced in range. Succeeded, Failed, Stopped,
// and Cancelled add up to Runs. CostUSD is the whole spend only while
// CostUnpricedRuns, CostPartialRuns, and TokensUnmeasuredRuns are all 0.
//
// TokensLowerBound and CostLowerBound mark a figure that counts only part of
// its runs. TokensLowerBoundRuns counts the measured runs with unaccounted
// turns. AgentPricedUSD is the share of CostUSD priced from amounts the agent
// reported and never exceeds CostUSD. UnmeasuredCause names why the unmeasured
// runs have no token figure and is null when none is unmeasured; UnmeasuredUnits
// lists the units those runs reported only when the cause is "amounts_only".
type Summary struct {
	Runs                   int      `json:"runs"`
	Issues                 int      `json:"issues"`
	Succeeded              int      `json:"succeeded"`
	Failed                 int      `json:"failed"`
	Stopped                int      `json:"stopped"`
	Cancelled              int      `json:"cancelled"`
	SuccessRate            float64  `json:"success_rate"`
	Duration               Duration `json:"duration_seconds"`
	MeanTurnsSucceeded     *float64 `json:"mean_turns_succeeded"`
	Tokens                 *Tokens  `json:"tokens"`
	CostUSD                *float64 `json:"cost_usd"`
	CostPerSucceededRunUSD *float64 `json:"cost_per_succeeded_run_usd"`
	ZeroDurationRuns       int      `json:"zero_duration_runs"`
	DurationExcludedRuns   int      `json:"duration_excluded_runs"`
	CostUnpricedRuns       int      `json:"cost_unpriced_runs"`
	CostPartialRuns        int      `json:"cost_partial_runs"`
	TokensUnmeasuredRuns   int      `json:"tokens_unmeasured_runs"`
	TokensLowerBoundRuns   int      `json:"tokens_lower_bound_runs"`
	TokensLowerBound       bool     `json:"tokens_lower_bound"`
	CostLowerBound         bool     `json:"cost_lower_bound"`
	AgentPricedUSD         *float64 `json:"agent_priced_usd"`
	UnmeasuredCause        *string  `json:"unmeasured_cause"`
	UnmeasuredUnits        []string `json:"unmeasured_units"`
}

// Spend is the recorded cost of the report's runs per UTC calendar period.
// Report.Spend is nil unless [persistence.RunHistoryCapabilities.RecordedCost]
// reports true.
type Spend struct {
	Period  string        `json:"period"`  // "day" or "month"
	Buckets []SpendBucket `json:"buckets"` // never nil; ascending by Start
}

// SpendBucket covers the report's runs whose completed_at falls in one UTC
// period. CostUSD is null when no run of the bucket is priced, and is the
// bucket's whole spend only while CostUnpricedRuns, CostPartialRuns, and
// TokensUnmeasuredRuns are all 0. CostLowerBound marks a bucket whose cost
// counts only part of its runs, and AgentPricedUSD is the share of CostUSD
// priced from amounts the agent reported.
type SpendBucket struct {
	Start                string   `json:"start"` // first instant of the period, RFC 3339 in UTC
	Runs                 int      `json:"runs"`
	CostUSD              *float64 `json:"cost_usd"`
	TokensUnmeasuredRuns int      `json:"tokens_unmeasured_runs"`
	CostUnpricedRuns     int      `json:"cost_unpriced_runs"`
	CostPartialRuns      int      `json:"cost_partial_runs"`
	CostLowerBound       bool     `json:"cost_lower_bound"`
	AgentPricedUSD       *float64 `json:"agent_priced_usd"`
}

// Duration reports p50, p95, and the arithmetic mean over a sample
// population, in seconds. All three are null when Samples is 0.
type Duration struct {
	P50     *float64 `json:"p50"`
	P95     *float64 `json:"p95"`
	Mean    *float64 `json:"mean"`
	Samples int      `json:"samples"`
}

// Tokens reports the stored token sums, never recomputed, and Uncached,
// which the report derives from Total and CacheRead so that Uncached plus
// CacheRead equals Total.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Total      int64 `json:"total"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Uncached   int64 `json:"uncached"`
}

// Group is one row of a by_status, by_adapter, by_rule, by_template,
// by_model, by_trigger, by_origin, or by_provenance breakdown. MeanTurns, Tokens, CostUSD, and
// CostPerSucceededRunUSD are null on the base schema tier; CostUSD and
// CostPerSucceededRunUSD are also null when the group holds no priced
// run. In by_status, Succeeded and SuccessRate are structural rather
// than informative: the "succeeded" row necessarily reports Succeeded
// equal to Runs. CostUnpricedRuns counts the group's measured runs, other
// than ci_failed, that spent tokens and recorded no cost; it is 0 on the
// base tier and on a database that records no cost, and CostUSD leaves
// those runs out. CostPartialRuns counts the group's runs, other than
// ci_failed, whose recorded cost is a lower bound; CostUSD includes them at
// that bound. A run counts in at most one of the two. CostPerSucceededRunUSD
// is not rounded to cents. A by_model row counts a run once for every model
// the run spent tokens on and carries that model's tokens and recorded cost,
// so its Runs can add up to more than Summary.Runs; a run with unaccounted
// turns counts as partial in every row whose cost is recorded. The lower-bound
// members read as on [Summary] and are zero values, with UnmeasuredUnits empty,
// on the base tier.
type Group struct {
	Name                   string   `json:"name"`
	Runs                   int      `json:"runs"`
	Succeeded              int      `json:"succeeded"`
	SuccessRate            float64  `json:"success_rate"`
	Share                  float64  `json:"share"`
	Duration               Duration `json:"duration_seconds"`
	MeanTurns              *float64 `json:"mean_turns"`
	Tokens                 *Tokens  `json:"tokens"`
	CostUSD                *float64 `json:"cost_usd"`
	CostPerSucceededRunUSD *float64 `json:"cost_per_succeeded_run_usd"`
	TokensUnmeasuredRuns   int      `json:"tokens_unmeasured_runs"`
	CostUnpricedRuns       int      `json:"cost_unpriced_runs"`
	CostPartialRuns        int      `json:"cost_partial_runs"`
	TokensLowerBoundRuns   int      `json:"tokens_lower_bound_runs"`
	TokensLowerBound       bool     `json:"tokens_lower_bound"`
	CostLowerBound         bool     `json:"cost_lower_bound"`
	AgentPricedUSD         *float64 `json:"agent_priced_usd"`
	UnmeasuredCause        *string  `json:"unmeasured_cause"`
	UnmeasuredUnits        []string `json:"unmeasured_units"`
}

// ChainGroup is one by_chain entry; Name is the chain identifier.
// by_chain lists only chains where a hop was due or made, so it is a
// listing rather than a partition of the report's runs.
type ChainGroup struct {
	Group
	Issue   string   `json:"issue"`
	Stages  []string `json:"stages"`
	LastHop string   `json:"last_hop"`
}

// Funnel is one chain definition: the configured chains that share a stage,
// merged. Stages follow chain order.
type Funnel struct {
	Stages []FunnelStage `json:"stages"`
}

// FunnelStage reports the passes that reached one stage of a [Funnel].
// Entered equals the sum of Arrivals and the sum of Outcomes, and Runs equals
// the by_rule group's runs for Rule. MedianSeconds is null when no pass of the
// stage has a usable duration.
type FunnelStage struct {
	Rule          string         `json:"rule"`
	Entry         bool           `json:"entry"`
	Last          bool           `json:"last"`
	Runs          int            `json:"runs"`
	Entered       int            `json:"entered"`
	Arrivals      FunnelArrivals `json:"arrivals"`
	Outcomes      FunnelOutcomes `json:"outcomes"`
	NoHopReasons  []NoHopReason  `json:"no_hop_reasons"`
	MedianSeconds *float64       `json:"median_seconds"`
}

// FunnelArrivals splits a stage's passes by how they reached it.
type FunnelArrivals struct {
	Hop   int `json:"hop"`
	Match int `json:"match"`
	Label int `json:"label"`
}

// FunnelOutcomes splits a stage's passes by how each one left it.
type FunnelOutcomes struct {
	Advanced  int `json:"advanced"`
	HandedOff int `json:"handed_off"`
	Ceiling   int `json:"ceiling"`
	HopFailed int `json:"hop_failed"`
	NoHop     int `json:"no_hop"`
	InStage   int `json:"in_stage"`
}

// NoHopReason counts the passes of one stage that stopped without a hop
// decision for Reason: run_failed, needs_person, budget_held,
// usage_limited, operator_stopped, or verdict_withheld.
type NoHopReason struct {
	Reason string `json:"reason"`
	Passes int    `json:"passes"`
}

// Stages is what chain_funnels reads besides run history; no response
// carries it.
type Stages struct {
	Chains     [][]string // as DispatchConfig.Chains returns them
	LabelRules []string   // the rules a stage label selects
	Live       []LivePass // passes a running session, a queued retry, or a budget hold holds now
}

// LivePass is a pass held outside finished runs. A pass with an empty ChainID
// or RuleName is ignored.
type LivePass struct {
	ChainID      string
	RuleName     string
	PreviousRule string // the rule whose hop reached the pass; empty when none did
	BudgetHeld   bool   // a budget hold; otherwise a running session or a queued retry
}

// SelfReview reports the self-review aggregation. MeanIterations is
// null when RunsWithMetadata is 0. Present only on the full schema tier.
type SelfReview struct {
	RunsWithMetadata int            `json:"runs_with_metadata"`
	ByFinalVerdict   []VerdictCount `json:"by_final_verdict"`
	CapReachedRuns   int            `json:"cap_reached_runs"`
	MeanIterations   *float64       `json:"mean_iterations"`
	UnparsedRuns     int            `json:"unparsed_runs"`
}

// VerdictCount is one entry of [SelfReview.ByFinalVerdict].
type VerdictCount struct {
	Verdict string `json:"verdict"`
	Runs    int    `json:"runs"`
}

// SkillCallReport counts the skill calls of the counted runs. Report.SkillCalls
// is nil when the database lacks the skills column. UnrecordedRuns counts the
// runs whose row records no skill calls; BySkill is never nil.
type SkillCallReport struct {
	UnrecordedRuns int          `json:"unrecorded_runs"`
	BySkill        []SkillCount `json:"by_skill"`
}

// SkillCount is one entry of [SkillCallReport.BySkill]. Runs counts the runs
// that called the skill at least once, Calls every call, and FailedCalls the
// calls that ended in a tool error.
type SkillCount struct {
	Skill       string `json:"skill"`
	Runs        int    `json:"runs"`
	Calls       int    `json:"calls"`
	FailedCalls int    `json:"failed_calls"`
}
