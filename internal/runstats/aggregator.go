package runstats

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// tier reports the report's schema_tier label for caps.
func tier(caps persistence.RunHistoryCapabilities) string {
	if caps.Full() {
		return "full"
	}
	return "base"
}

// label maps an empty dimension value to the sentinel a reader sees for
// a run that carries no adapter, rule, template, trigger, or (in principle)
// status.
func label(v string) string {
	if v == "" {
		return "<none>"
	}
	return v
}

// triggerLabel names a retry trigger group. A nil trigger was never
// recorded, which differs from the empty trigger of a run nothing retried.
func triggerLabel(t *string) string {
	if t == nil {
		return "<unknown>"
	}
	return label(*t)
}

// originLabel names the run origin group of row, first match wins. A
// non-empty trigger decides before the reaction kind, so an error or stall
// retry of a reaction run lands in the same group as in by_trigger. A
// stage_hop is decided by FirstAfterHop alone: a later dispatch into a hopped
// stage carries the same StagePrevious.
func originLabel(row persistence.RunStatsRow) string {
	switch {
	case row.Trigger == nil:
		return "<unknown>"
	case *row.Trigger != "":
		return *row.Trigger
	case row.ReactionKind != "":
		return row.ReactionKind
	case row.Status == "ci_failed":
		return "<none>"
	case row.FirstAfterHop:
		return "stage_hop"
	default:
		return "poll_tick"
	}
}

// outcome names the one report-wide count a run with the given status
// belongs to. A status no count names is a failure, as the orchestrator
// records an unknown exit kind as failed too.
type outcome int

const (
	outcomeSucceeded outcome = iota
	outcomeFailed
	outcomeStopped
	outcomeCancelled
)

// outcomeOf maps a run status to its outcome.
func outcomeOf(status string) outcome {
	switch status {
	case "succeeded":
		return outcomeSucceeded
	case "needs_person", "budget_stopped", "usage_limited":
		return outcomeStopped
	case "cancelled":
		return outcomeCancelled
	default:
		return outcomeFailed
	}
}

// duration computes row's duration in seconds from its stored RFC3339
// timestamps. ok is false when either timestamp fails to parse or the
// resulting difference is negative.
func duration(row persistence.RunStatsRow) (seconds float64, ok bool) {
	started, err := time.Parse(time.RFC3339, row.StartedAt)
	if err != nil {
		return 0, false
	}
	completed, err := time.Parse(time.RFC3339, row.CompletedAt)
	if err != nil {
		return 0, false
	}
	diff := completed.Sub(started).Seconds()
	if diff < 0 {
		return 0, false
	}
	return diff, true
}

// percentile returns the nearest-rank percentile p over sorted, which
// MUST already be sorted ascending. No interpolation is performed.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	idx := int(math.Ceil(p/100*float64(n))) - 1
	idx = max(0, min(idx, n-1))
	return sorted[idx]
}

// spentTokens reports whether row used any token, in any counter. A row that
// spent nothing is neither priced nor unpriced.
func spentTokens(row persistence.RunStatsRow) bool {
	return row.InputTokens != 0 || row.OutputTokens != 0 || row.TotalTokens != 0 ||
		row.CacheReadTokens != 0 || row.CacheWriteTokens != 0
}

// costOutcome is how one measured run, or one model's share of it, counts in
// the cost figures. A run counts in at most one of priced and unpriced, and
// partial is independent of both.
type costOutcome struct {
	priced   bool
	unpriced bool
	partial  bool
	cost     float64

	// agentUSD is the part of cost priced from an agent-reported amount;
	// agentPriced is false when no such part counts.
	agentUSD    float64
	agentPriced bool
}

// classifyCost classifies a measured run's cost for every figure the report
// serves, so the summary, the groups, the model shares, and the spend buckets
// cannot drift apart. A ci_failed run records a CI verdict and no agent
// session, so it is neither priced, unpriced, nor partial.
func classifyCost(status string, spent bool, cost *float64, costStatus string) costOutcome {
	if status == "ci_failed" {
		return costOutcome{}
	}
	var out costOutcome
	switch {
	case !spent:
	case cost == nil:
		out.unpriced = true
	default:
		out.priced = true
		out.cost = *cost
	}
	out.partial = costStatus == "partial"
	return out
}

// agentPricedOf sums the parts priced from an agent-reported amount.
// ok is false when no part is.
func agentPricedOf(parts []persistence.RunModelPart) (usd float64, ok bool) {
	for _, part := range parts {
		if part.PriceSource != agentPriceSource {
			continue
		}
		ok = true
		if part.EstimatedCostUSD != nil {
			usd += *part.EstimatedCostUSD
		}
	}
	return usd, ok
}

// agentPriceSource is the recorded price source of a part priced from an
// amount the agent reported.
const agentPriceSource = "agent"

// costAccum accumulates the folded [costOutcome] values of one figure.
type costAccum struct {
	costUSD          float64
	pricedRuns       int
	costUnpricedRuns int
	costPartialRuns  int
	agentPricedUSD   float64
	hasAgentPriced   bool
}

func (c *costAccum) add(out costOutcome) {
	if out.priced {
		c.costUSD += out.cost
		c.pricedRuns++
	}
	if out.agentPriced {
		c.agentPricedUSD += out.agentUSD
		c.hasAgentPriced = true
	}
	if out.unpriced {
		c.costUnpricedRuns++
	}
	if out.partial {
		c.costPartialRuns++
	}
}

// agentPriced returns the rounded agent-priced amount, or nil when no part
// was added.
func (c *costAccum) agentPriced() *float64 {
	if !c.hasAgentPriced {
		return nil
	}
	return new(round9(c.agentPricedUSD))
}

// provenanceAccum accumulates, for one figure, the runs whose token figure is
// a lower bound or missing.
type provenanceAccum struct {
	lowerBoundRuns int
	causes         persistence.UnmeasuredCauses
}

func (p *provenanceAccum) addRun(row persistence.RunStatsRow) {
	switch {
	case !row.TokensMeasured:
		p.causes.Add(persistence.UnmeasuredCauseOf(row.Status, row.UsageCompleteness, row.ReportedUnits), row.ReportedUnits)
	case row.UnaccountedTurns > 0:
		p.lowerBoundRuns++
	}
}

// modelShare is the part of a run credited to one model in by_model.
type modelShare struct {
	model      string
	tokens     Tokens
	spent      bool
	cost       *float64
	costStatus string
	fromPart   bool

	// priceSource is the part's recorded price source; empty for a share
	// that carries the run's own figures.
	priceSource string
}

// sharesOf splits row across the models it spent tokens on, one share per
// spent part with the part's recorded cost. A run with no spent part, whatever
// its age, is one share under [modelOf] carrying the run's own figures.
func sharesOf(row persistence.RunStatsRow) []modelShare {
	var shares []modelShare
	for _, part := range row.Parts {
		t := part.Tokens
		if t.InputTokens <= 0 && t.OutputTokens <= 0 && t.CacheReadTokens <= 0 && t.CacheWriteTokens <= 0 {
			continue
		}
		shares = append(shares, modelShare{
			model: part.Model,
			tokens: Tokens{
				Input:      t.InputTokens,
				Output:     t.OutputTokens,
				Total:      t.InputTokens + t.OutputTokens,
				CacheRead:  t.CacheReadTokens,
				CacheWrite: t.CacheWriteTokens,
			},
			spent:       true,
			cost:        part.EstimatedCostUSD,
			costStatus:  part.CostStatus,
			fromPart:    true,
			priceSource: part.PriceSource,
		})
	}
	if len(shares) > 0 {
		return shares
	}
	return []modelShare{{
		model: modelOf(row),
		tokens: Tokens{
			Input:      row.InputTokens,
			Output:     row.OutputTokens,
			Total:      row.TotalTokens,
			CacheRead:  row.CacheReadTokens,
			CacheWrite: row.CacheWriteTokens,
		},
		spent:      spentTokens(row),
		cost:       row.EstimatedCostUSD,
		costStatus: row.CostStatus,
	}}
}

// selfReviewAccum accumulates the self-review folding state across
// every row that carries non-nil review metadata.
type selfReviewAccum struct {
	runsWithMetadata int
	byVerdict        map[string]int
	capReachedRuns   int
	iterationSum     int
	unparsedRuns     int
}

// foldSelfReview unmarshals raw into a [domain.ReviewMetadata] and folds
// the result into acc. A value that fails to unmarshal increments acc's
// unparsed count and contributes to nothing else.
func foldSelfReview(raw string, acc *selfReviewAccum) {
	var metadata domain.ReviewMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		acc.unparsedRuns++
		return
	}

	acc.runsWithMetadata++
	verdict := metadata.FinalVerdict
	if verdict == "" {
		verdict = "none"
	}
	acc.byVerdict[verdict]++
	if metadata.CapReached {
		acc.capReachedRuns++
	}
	acc.iterationSum += metadata.TotalIterations
}

// groupAccum accumulates the folding state for one [Group] row
// before the derived figures are computed in [Aggregator.Report].
type groupAccum struct {
	name              string
	runs              int
	succeeded         int
	turnSum           int
	tokens            Tokens
	measuredRuns      int
	unmeasuredRuns    int
	succeededMeasured int
	durationSamples   []float64
	costAccum
	provenanceAccum
}

// chainAccum accumulates the by_chain figures a groupAccum does
// not hold. hopInvolved is set once any row of the chain has a previous
// stage or a hop result.
type chainAccum struct {
	issue       string
	stages      []string
	lastHop     string
	hopInvolved bool
}

// totalAccum accumulates the report-wide folding state.
type totalAccum struct {
	runs                 int
	succeeded            int
	failed               int
	stopped              int
	cancelled            int
	succeededTurns       int
	tokens               Tokens
	measuredRuns         int
	unmeasuredRuns       int
	succeededMeasured    int
	zeroDurationRuns     int
	durationExcludedRuns int
	succeededSamples     []float64
	costAccum
	provenanceAccum
}

// dayAccum accumulates the runs that completed within one UTC calendar day.
type dayAccum struct {
	runs       int
	unmeasured int
	costAccum
}

// withUncached returns t with Uncached derived from its Total and
// CacheRead sums.
func withUncached(t Tokens) Tokens {
	t.Uncached = domain.TokenUsage{TotalTokens: t.Total, CacheReadTokens: t.CacheRead}.Uncached()
	return t
}

// bumpGroup returns groups' entry for name, creating it on first
// use, and increments its run count and, when isSucceeded, its
// succeeded count.
func bumpGroup(groups map[string]*groupAccum, name string, isSucceeded bool) *groupAccum {
	g, ok := groups[name]
	if !ok {
		g = &groupAccum{name: name}
		groups[name] = g
	}
	g.runs++
	if isSucceeded {
		g.succeeded++
	}
	return g
}

// round1, round2, round4, and round9 round v to the named number of decimal
// places so the JSON report is byte-stable. Costs take round9, which keeps a
// sub-cent total from reading as zero.
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
func round9(v float64) float64 { return math.Round(v*1e9) / 1e9 }

// durationStats computes p50, p95, and the mean over samples, in
// seconds. All three are null when samples is empty. samples is sorted
// in place by a copy so the caller's slice is unmodified.
func durationStats(samples []float64) Duration {
	d := Duration{Samples: len(samples)}
	if len(samples) == 0 {
		return d
	}

	sorted := slices.Clone(samples)
	slices.Sort(sorted)

	var sum float64
	for _, s := range sorted {
		sum += s
	}

	d.P50 = new(percentile(sorted, 50))
	d.P95 = new(percentile(sorted, 95))
	d.Mean = new(round1(sum / float64(len(sorted))))
	return d
}

// compareGroups orders groups by descending runs, then ascending name.
func compareGroups(x, y Group) int {
	if x.Runs != y.Runs {
		return y.Runs - x.Runs
	}
	return strings.Compare(x.Name, y.Name)
}

// degradedSchemaWarning names the figures the report leaves out and states
// the remedy. It MUST NOT be called when caps is full.
//
// A partly migrated database is the common case, and on it the two lists
// differ: the report falls back to the basic figures wholesale, so it drops
// groups this database does carry as well as the ones it never recorded.
// Naming only the unrecorded groups would let a reader conclude the
// remaining figures are present when they are not.
func degradedSchemaWarning(caps persistence.RunHistoryCapabilities) string {
	groups := []struct {
		recorded bool
		name     string
	}{
		{caps.HasTurnsCompleted, "turns"},
		{caps.HasReviewMetadata, "self-review results"},
		{caps.HasRuleRouting, "dispatch-rule routing"},
		{caps.HasTokens, "tokens and cost"},
		{caps.HasTokenMeasurement, "which runs the coding agent could measure"},
		{caps.HasConfiguredSettings, "the model each run was configured with"},
		{caps.HasStageChains, "stage chains"},
		{caps.HasTrigger, "what started each run"},
		{caps.HasFirstAfterHop, "the first run after each stage hop"},
		{caps.HasAgentBlocked, "which runs required action"},
		{caps.HasSkills, "the skill calls of each run"},
	}

	var unrecorded, dropped []string
	for _, g := range groups {
		if g.recorded {
			dropped = append(dropped, g.name)
		} else {
			unrecorded = append(unrecorded, g.name)
		}
	}

	if len(dropped) == 0 {
		return fmt.Sprintf(
			"this database was written before sortie recorded %s, so the report leaves them out. "+
				"Run sortie once with this workflow to add them.",
			strings.Join(unrecorded, ", "))
	}
	return fmt.Sprintf(
		"this database was written before sortie recorded %s. The report falls back to run counts and "+
			"durations, so it also leaves out %s, which this database does carry. "+
			"Run sortie once with this workflow to get the full report.",
		strings.Join(unrecorded, ", "), strings.Join(dropped, ", "))
}

// provenanceLabel names the by_provenance group of row: the sources and
// strategies that settled its token figure, or a sentinel for a run recorded
// before provenance was kept or one with no source.
func provenanceLabel(row persistence.RunStatsRow) string {
	switch {
	case row.UsageCompleteness == "":
		return "<unknown>"
	case row.UsageProvenance == "":
		return "<none>"
	}
	pairs := persistence.ParseUsageProvenance(row.UsageProvenance)
	names := make([]string, len(pairs))
	for i, pair := range pairs {
		names[i] = pair.Source + ":" + pair.Strategy
	}
	return strings.Join(names, ",")
}

// modelOf names the model a run is grouped under: the one the runtime
// reported, else the configured one, because a run left to the runtime's
// default configures none.
func modelOf(row persistence.RunStatsRow) string {
	return cmp.Or(row.ReportedModel, row.ConfiguredModel)
}

// laterTiers lists, oldest first, the columns added after the full tier and
// what the report leaves out without each. The tiers are cumulative, so the
// first missing column also takes every later part of the report with it.
var laterTiers = []struct {
	has      func(persistence.RunHistoryCapabilities) bool
	recorded string
	leftOut  string
	// chainsOnly marks a part the report holds only for a workflow with stage
	// chains.
	chainsOnly bool
}{
	{has: func(c persistence.RunHistoryCapabilities) bool { return c.HasConfiguredSettings }, recorded: "the model of each run", leftOut: "the breakdown by model"},
	{has: func(c persistence.RunHistoryCapabilities) bool { return c.HasStageChains }, recorded: "stage chains", leftOut: "the breakdown by stage chain"},
	{has: func(c persistence.RunHistoryCapabilities) bool { return c.HasTrigger }, recorded: "what started each run", leftOut: "the breakdown by retry trigger"},
	{has: func(c persistence.RunHistoryCapabilities) bool { return c.HasFirstAfterHop }, recorded: "the first run after each stage hop", leftOut: "the breakdown by run origin"},
	{has: func(c persistence.RunHistoryCapabilities) bool { return c.HasAgentBlocked }, recorded: "which runs required action", leftOut: "the funnel of each stage chain", chainsOnly: true},
	{has: func(c persistence.RunHistoryCapabilities) bool { return c.HasRecordedCost }, recorded: "the cost of each run", leftOut: "cost"},
	{has: func(c persistence.RunHistoryCapabilities) bool { return c.HasSkills }, recorded: "the skill calls of each run", leftOut: "the breakdown by skill"},
}

// laterTiersWarning names every column of laterTiers the database lacks and
// every part of the report that leaves out, or returns "" when none is missing.
func laterTiersWarning(caps persistence.RunHistoryCapabilities, hasChains bool) string {
	var unrecorded, leftOut []string
	for _, tier := range laterTiers {
		if !tier.has(caps) {
			unrecorded = append(unrecorded, tier.recorded)
		}
		if len(unrecorded) > 0 && (!tier.chainsOnly || hasChains) {
			leftOut = append(leftOut, tier.leftOut)
		}
	}
	if len(unrecorded) == 0 {
		return ""
	}
	pronoun := "it"
	if len(leftOut) > 1 {
		pronoun = "them"
	}
	return fmt.Sprintf(
		"this database was written before sortie recorded %s, so the report leaves out %s. "+
			"Run sortie once with this workflow to add %s.",
		englishList(unrecorded), englishList(leftOut), pronoun)
}

func englishList(items []string) string {
	switch len(items) {
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

// Aggregator folds a stream of [persistence.RunStatsRow] values, via
// [Aggregator.Add], into the totals and per-dimension groups
// [Aggregator.Report] renders into a [Report]. It is not safe for concurrent
// use; a caller builds one per report.
type Aggregator struct {
	caps      persistence.RunHistoryCapabilities
	tierLabel string

	total totalAccum
	days  map[time.Time]*dayAccum // keyed by the UTC midnight starting the day

	byStatus     map[string]*groupAccum
	byAdapter    map[string]*groupAccum
	byRule       map[string]*groupAccum
	byTemplate   map[string]*groupAccum
	byModel      map[string]*groupAccum
	byChain      map[string]*groupAccum
	byTrigger    map[string]*groupAccum
	byOrigin     map[string]*groupAccum
	byProvenance map[string]*groupAccum
	chains       map[string]*chainAccum
	issueIDs     map[string]struct{}
	passes       map[passKey]*passAccum
	stages       Stages

	selfReview selfReviewAccum

	unrecordedRuns int
	bySkill        map[string]*SkillCount
}

// New constructs an [Aggregator] for caps. It reads each run's cost as
// recorded and prices nothing.
func New(caps persistence.RunHistoryCapabilities) *Aggregator {
	return &Aggregator{
		caps:         caps,
		tierLabel:    tier(caps),
		days:         make(map[time.Time]*dayAccum),
		byStatus:     make(map[string]*groupAccum),
		byAdapter:    make(map[string]*groupAccum),
		byRule:       make(map[string]*groupAccum),
		byTemplate:   make(map[string]*groupAccum),
		byModel:      make(map[string]*groupAccum),
		byChain:      make(map[string]*groupAccum),
		byTrigger:    make(map[string]*groupAccum),
		byOrigin:     make(map[string]*groupAccum),
		byProvenance: make(map[string]*groupAccum),
		chains:       make(map[string]*chainAccum),
		passes:       make(map[passKey]*passAccum),
		issueIDs:     make(map[string]struct{}),
		selfReview:   selfReviewAccum{byVerdict: make(map[string]int)},
		bySkill:      make(map[string]*SkillCount),
	}
}

// SetStages supplies the stage chains and live passes chain_funnels reads. A
// later call replaces an earlier one; it may run before or after [Aggregator.Add]
// and MUST precede [Aggregator.Report]. Without a call the report serves no
// funnels.
func (a *Aggregator) SetStages(stages Stages) {
	a.stages = stages
}

// Add folds one row into the aggregator. It is the visit callback
// [persistence.Store.ScanRunHistoryRange] calls once per row; it calls
// no Store method and always returns nil.
func (a *Aggregator) Add(row persistence.RunStatsRow) error {
	isSucceeded := row.Status == "succeeded"
	a.total.runs++
	a.issueIDs[row.IssueID] = struct{}{}
	switch outcomeOf(row.Status) {
	case outcomeSucceeded:
		a.total.succeeded++
	case outcomeStopped:
		a.total.stopped++
	case outcomeCancelled:
		a.total.cancelled++
	default:
		a.total.failed++
	}

	bumped := []*groupAccum{
		bumpGroup(a.byStatus, label(row.Status), isSucceeded),
		bumpGroup(a.byAdapter, label(row.AgentAdapter), isSucceeded),
	}

	seconds, hasDuration := duration(row)
	var cost costOutcome

	full := a.caps.Full()
	if full {
		bumped = append(bumped,
			bumpGroup(a.byRule, label(row.RuleName), isSucceeded),
			bumpGroup(a.byTemplate, label(row.TemplateID), isSucceeded),
		)
		if a.caps.ModelBreakdown() {
			a.foldModels(row, isSucceeded, seconds, hasDuration)
		}
		if a.caps.ChainBreakdown() && row.ChainID != "" {
			bumped = append(bumped, bumpGroup(a.byChain, row.ChainID, isSucceeded))
			a.foldChain(row)
		}
		if a.caps.FunnelBreakdown() {
			a.foldPass(row)
		}
		if a.caps.TriggerBreakdown() {
			bumped = append(bumped, bumpGroup(a.byTrigger, triggerLabel(row.Trigger), isSucceeded))
		}
		if a.caps.OriginBreakdown() {
			bumped = append(bumped, bumpGroup(a.byOrigin, originLabel(row), isSucceeded))
		}
		if a.caps.UsageProvenance() {
			bumped = append(bumped, bumpGroup(a.byProvenance, provenanceLabel(row), isSucceeded))
		}

		for _, g := range bumped {
			g.turnSum += row.TurnsCompleted
		}
		if isSucceeded {
			a.total.succeededTurns += row.TurnsCompleted
		}

		a.total.addRun(row)
		for _, g := range bumped {
			g.addRun(row)
		}

		if !row.TokensMeasured {
			a.total.unmeasuredRuns++
			for _, g := range bumped {
				g.unmeasuredRuns++
			}
		} else {
			for _, g := range bumped {
				g.tokens.Input += row.InputTokens
				g.tokens.Output += row.OutputTokens
				g.tokens.Total += row.TotalTokens
				g.tokens.CacheRead += row.CacheReadTokens
				g.tokens.CacheWrite += row.CacheWriteTokens
				g.measuredRuns++
				if isSucceeded {
					g.succeededMeasured++
				}
			}
			a.total.tokens.Input += row.InputTokens
			a.total.tokens.Output += row.OutputTokens
			a.total.tokens.Total += row.TotalTokens
			a.total.tokens.CacheRead += row.CacheReadTokens
			a.total.tokens.CacheWrite += row.CacheWriteTokens
			a.total.measuredRuns++
			if isSucceeded {
				a.total.succeededMeasured++
			}

			if a.caps.RecordedCost() {
				cost = classifyCost(row.Status, spentTokens(row), row.EstimatedCostUSD, row.CostStatus)
				if cost.priced {
					cost.agentUSD, cost.agentPriced = agentPricedOf(row.Parts)
				}
				a.total.add(cost)
				for _, g := range bumped {
					g.add(cost)
				}
			}
		}

		if row.ReviewMetadata != nil {
			foldSelfReview(*row.ReviewMetadata, &a.selfReview)
		}

		if a.caps.SkillBreakdown() {
			a.foldSkills(row)
		}
	}

	if a.caps.RecordedCost() {
		a.foldSpend(row, cost)
	}

	if !hasDuration {
		a.total.durationExcludedRuns++
		return nil
	}
	if seconds == 0 {
		a.total.zeroDurationRuns++
	}
	for _, g := range bumped {
		g.durationSamples = append(g.durationSamples, seconds)
	}
	if isSucceeded {
		a.total.succeededSamples = append(a.total.succeededSamples, seconds)
	}

	return nil
}

// foldSkills folds the skill calls row stores into the per-skill counts. A row
// that stores none, or a value that does not decode, counts as unrecorded.
func (a *Aggregator) foldSkills(row persistence.RunStatsRow) {
	var calls []domain.SkillCall
	if row.Skills == nil || json.Unmarshal([]byte(*row.Skills), &calls) != nil {
		a.unrecordedRuns++
		return
	}

	counted := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		entry, ok := a.bySkill[call.Skill]
		if !ok {
			entry = &SkillCount{Skill: call.Skill}
			a.bySkill[call.Skill] = entry
		}
		entry.Calls++
		if call.ToolError {
			entry.FailedCalls++
		}
		if _, seen := counted[call.Skill]; !seen {
			counted[call.Skill] = struct{}{}
			entry.Runs++
		}
	}
}

// foldModels folds row into the by_model group of every model share it
// carries. Only the share's tokens and cost reach a group, never the run's
// totals, so the rows of a multi-model run add up to the run.
func (a *Aggregator) foldModels(row persistence.RunStatsRow, isSucceeded bool, seconds float64, hasDuration bool) {
	for _, share := range sharesOf(row) {
		g := bumpGroup(a.byModel, label(share.model), isSucceeded)
		g.turnSum += row.TurnsCompleted
		if hasDuration {
			g.durationSamples = append(g.durationSamples, seconds)
		}
		g.addRun(row)
		if !row.TokensMeasured {
			g.unmeasuredRuns++
			continue
		}
		g.tokens.Input += share.tokens.Input
		g.tokens.Output += share.tokens.Output
		g.tokens.Total += share.tokens.Total
		g.tokens.CacheRead += share.tokens.CacheRead
		g.tokens.CacheWrite += share.tokens.CacheWrite
		g.measuredRuns++
		if isSucceeded {
			g.succeededMeasured++
		}
		if !a.caps.RecordedCost() {
			continue
		}
		status := share.costStatus
		// Turns the agent left unaccounted spent tokens no part records, so a
		// part's cost is a lower bound even when the part priced fully.
		if share.fromPart && status == "estimated" && row.UnaccountedTurns > 0 {
			status = "partial"
		}
		out := classifyCost(row.Status, share.spent, share.cost, status)
		if out.priced && share.priceSource == agentPriceSource {
			out.agentUSD, out.agentPriced = *share.cost, true
		}
		g.add(out)
	}
}

// utcDay returns the UTC midnight that starts the calendar day holding t.
func utcDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// foldSpend folds row into the accumulator of the UTC day it completed on. A
// run whose completion time does not parse joins no day. cost is the zero
// outcome for a run whose tokens were not measured.
func (a *Aggregator) foldSpend(row persistence.RunStatsRow, cost costOutcome) {
	completed, err := time.Parse(time.RFC3339, row.CompletedAt)
	if err != nil {
		return
	}
	day := utcDay(completed)
	d, ok := a.days[day]
	if !ok {
		d = &dayAccum{}
		a.days[day] = d
	}
	d.runs++
	if !row.TokensMeasured {
		d.unmeasured++
		return
	}
	d.add(cost)
}

// foldChain folds row into its chain's by_chain accumulator. Rows arrive
// in ascending id order, so the last non-empty value of a field is the
// latest one.
func (a *Aggregator) foldChain(row persistence.RunStatsRow) {
	c, ok := a.chains[row.ChainID]
	if !ok {
		c = &chainAccum{}
		a.chains[row.ChainID] = c
	}
	c.issue = cmp.Or(row.DisplayID, row.Identifier)
	if row.RuleName != "" && (len(c.stages) == 0 || c.stages[len(c.stages)-1] != row.RuleName) {
		c.stages = append(c.stages, row.RuleName)
	}
	if row.StageResult != "" {
		c.lastHop = row.StageResult
	}
	if row.StagePrevious != "" || row.StageResult != "" {
		c.hopInvolved = true
	}
}

// chainReport lists the chains where a hop was due or made, sorted like
// every other breakdown.
func (a *Aggregator) chainReport() []ChainGroup {
	out := []ChainGroup{}
	for id, c := range a.chains {
		if !c.hopInvolved {
			continue
		}
		out = append(out, ChainGroup{
			Group:   a.groupStat(a.byChain[id], true),
			Issue:   c.issue,
			Stages:  append([]string{}, c.stages...),
			LastHop: c.lastHop,
		})
	}
	slices.SortFunc(out, func(x, y ChainGroup) int {
		return compareGroups(x.Group, y.Group)
	})
	return out
}

// Report computes every derived figure exactly once from a's folded
// state and returns the complete [Report]. Its Warnings field holds the one
// warning for the capabilities a was built with, if any.
func (a *Aggregator) Report(
	generatedAt time.Time,
	workflowPath, dbPath string,
	since, until *time.Time,
) Report {
	full := a.caps.Full()

	rpt := Report{
		GeneratedAt:  generatedAt.Format(time.RFC3339),
		WorkflowPath: workflowPath,
		DBPath:       dbPath,
		SchemaTier:   a.tierLabel,
		Warnings:     a.composeWarnings(),
		Summary:      a.summary(full),
		Spend:        a.spend(generatedAt, since, until),
		ByStatus:     a.groupSlice(a.byStatus, full),
		ByAdapter:    a.groupSlice(a.byAdapter, full),
		ByRule:       []Group{},
		ByTemplate:   []Group{},
		ByModel:      []Group{},
		ByChain:      []ChainGroup{},
		ChainFunnels: a.funnelReport(generatedAt, until),
		ByTrigger:    []Group{},
		ByOrigin:     []Group{},
		ByProvenance: []Group{},
	}
	if since != nil {
		rpt.Since = new(since.UTC().Format(time.RFC3339))
	}
	if until != nil {
		rpt.Until = new(until.UTC().Format(time.RFC3339))
	}
	if full {
		rpt.ByRule = a.groupSlice(a.byRule, full)
		rpt.ByTemplate = a.groupSlice(a.byTemplate, full)
		if a.caps.ModelBreakdown() {
			rpt.ByModel = a.groupSlice(a.byModel, full)
		}
		if a.caps.ChainBreakdown() {
			rpt.ByChain = a.chainReport()
		}
		if a.caps.TriggerBreakdown() {
			rpt.ByTrigger = a.groupSlice(a.byTrigger, full)
		}
		if a.caps.OriginBreakdown() {
			rpt.ByOrigin = a.groupSlice(a.byOrigin, full)
		}
		if a.caps.UsageProvenance() {
			rpt.ByProvenance = a.groupSlice(a.byProvenance, full)
		}
		rpt.SelfReview = a.selfReviewReport()
	}
	if a.caps.SkillBreakdown() {
		rpt.SkillCalls = a.skillCallReport()
	}

	return rpt
}

// spendPeriods maps the length of a report's range to the period its spend is
// bucketed by; the first entry whose limit holds the range names it.
var spendPeriods = []struct {
	limit  time.Duration
	period string
}{
	{limit: 2160 * time.Hour, period: "day"},
}

// spendPeriod names the period a range from since to end is bucketed by. An
// open range, and one longer than every entry of spendPeriods, takes months.
func spendPeriod(since *time.Time, end time.Time) string {
	if since == nil {
		return "month"
	}
	for _, rule := range spendPeriods {
		if end.Sub(*since) <= rule.limit {
			return rule.period
		}
	}
	return "month"
}

// periodStart returns the UTC instant that starts the period holding t.
func periodStart(t time.Time, period string) time.Time {
	day := utcDay(t)
	if period == "month" {
		return day.AddDate(0, 0, 1-day.Day())
	}
	return day
}

// nextPeriod returns the start of the period after the one starting at start.
func nextPeriod(start time.Time, period string) time.Time {
	if period == "month" {
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

// spend lists the recorded cost of the folded runs per UTC period, every
// period from the one holding the range's first instant to the one holding its
// last, so a period without runs is listed with none. It is nil unless the
// database records cost.
func (a *Aggregator) spend(generatedAt time.Time, since, until *time.Time) *Spend {
	if !a.caps.RecordedCost() {
		return nil
	}
	end := generatedAt
	if until != nil {
		end = *until
	}
	period := spendPeriod(since, end)
	out := &Spend{Period: period, Buckets: []SpendBucket{}}
	if len(a.days) == 0 {
		return out
	}

	// Days are summed in ascending order so a bucket's cost does not depend
	// on map iteration order.
	days := slices.SortedFunc(maps.Keys(a.days), time.Time.Compare)
	sums := make(map[time.Time]*dayAccum)
	for _, day := range days {
		start := periodStart(day, period)
		sum, ok := sums[start]
		if !ok {
			sum = &dayAccum{}
			sums[start] = sum
		}
		d := a.days[day]
		sum.runs += d.runs
		sum.unmeasured += d.unmeasured
		sum.costUSD += d.costUSD
		sum.pricedRuns += d.pricedRuns
		sum.costUnpricedRuns += d.costUnpricedRuns
		sum.costPartialRuns += d.costPartialRuns
		sum.agentPricedUSD += d.agentPricedUSD
		sum.hasAgentPriced = sum.hasAgentPriced || d.hasAgentPriced
	}

	firstAt := days[0]
	if since != nil {
		firstAt = *since
	}
	lastAt := generatedAt
	if until != nil {
		lastAt = until.Add(-time.Nanosecond)
	}
	first := periodStart(firstAt, period)
	last := periodStart(lastAt, period)
	if latest := periodStart(days[len(days)-1], period); latest.After(last) {
		last = latest
	}

	for start := first; !start.After(last); start = nextPeriod(start, period) {
		bucket := SpendBucket{Start: start.Format(time.RFC3339)}
		if sum, ok := sums[start]; ok {
			bucket.Runs = sum.runs
			bucket.TokensUnmeasuredRuns = sum.unmeasured
			bucket.CostUnpricedRuns = sum.costUnpricedRuns
			bucket.CostPartialRuns = sum.costPartialRuns
			if sum.pricedRuns > 0 {
				bucket.CostUSD = new(round9(sum.costUSD))
			}
			bucket.CostLowerBound = bucket.CostUSD != nil &&
				(sum.costUnpricedRuns > 0 || sum.costPartialRuns > 0 || sum.unmeasured > 0)
			bucket.AgentPricedUSD = sum.agentPriced()
		}
		out.Buckets = append(out.Buckets, bucket)
	}
	return out
}

// composeWarnings returns the non-nil warning list: at most one warning,
// naming everything a's capabilities leave out.
func (a *Aggregator) composeWarnings() []string {
	if !a.caps.Full() {
		return []string{degradedSchemaWarning(a.caps)}
	}
	if warning := laterTiersWarning(a.caps, len(a.stages.Chains) > 0); warning != "" {
		return []string{warning}
	}
	return []string{}
}

// summary computes the report-wide [Summary].
func (a *Aggregator) summary(full bool) Summary {
	s := Summary{
		Runs:                 a.total.runs,
		Issues:               len(a.issueIDs),
		Succeeded:            a.total.succeeded,
		Failed:               a.total.failed,
		Stopped:              a.total.stopped,
		Cancelled:            a.total.cancelled,
		Duration:             durationStats(a.total.succeededSamples),
		ZeroDurationRuns:     a.total.zeroDurationRuns,
		DurationExcludedRuns: a.total.durationExcludedRuns,
		CostUnpricedRuns:     a.total.costUnpricedRuns,
		CostPartialRuns:      a.total.costPartialRuns,
		UnmeasuredUnits:      []string{},
	}
	if s.Runs > 0 {
		s.SuccessRate = round4(float64(s.Succeeded) / float64(s.Runs))
	}
	if full && s.Succeeded > 0 {
		s.MeanTurnsSucceeded = new(round2(float64(a.total.succeededTurns) / float64(s.Succeeded)))
	}
	if full {
		s.TokensUnmeasuredRuns = a.total.unmeasuredRuns
	}
	if full && a.total.measuredRuns > 0 {
		s.Tokens = new(withUncached(a.total.tokens))
	}
	if full && a.total.pricedRuns > 0 {
		cost := round9(a.total.costUSD)
		s.CostUSD = &cost
		if a.total.succeededMeasured > 0 {
			s.CostPerSucceededRunUSD = new(round9(cost / float64(a.total.succeededMeasured)))
		}
	}
	if full {
		flags := lowerBoundFlagsOf(s.Tokens, s.CostUSD, a.total.unmeasuredRuns, &a.total.costAccum, &a.total.provenanceAccum)
		s.TokensLowerBoundRuns = flags.tokensLowerBoundRuns
		s.TokensLowerBound = flags.tokensLowerBound
		s.CostLowerBound = flags.costLowerBound
		s.AgentPricedUSD = flags.agentPricedUSD
		s.UnmeasuredCause = flags.cause
		s.UnmeasuredUnits = flags.units
	}
	return s
}

// lowerBoundFlags are the lower-bound members every figure of the report
// serves, computed once here so no client derives them.
type lowerBoundFlags struct {
	tokensLowerBoundRuns int
	tokensLowerBound     bool
	costLowerBound       bool
	agentPricedUSD       *float64
	cause                *string
	units                []string
}

// lowerBoundFlagsOf derives the flags of one figure from its served tokens and
// cost and its folded counts.
func lowerBoundFlagsOf(tokens *Tokens, costUSD *float64, unmeasuredRuns int, c *costAccum, p *provenanceAccum) lowerBoundFlags {
	flags := lowerBoundFlags{
		tokensLowerBoundRuns: p.lowerBoundRuns,
		tokensLowerBound:     tokens != nil && (unmeasuredRuns > 0 || p.lowerBoundRuns > 0),
		costLowerBound:       costUSD != nil && (c.costUnpricedRuns > 0 || c.costPartialRuns > 0 || unmeasuredRuns > 0),
		agentPricedUSD:       c.agentPriced(),
	}
	cause, units := p.causes.Result()
	if cause != "" {
		flags.cause = new(string(cause))
	}
	flags.units = units
	return flags
}

// groupSlice converts groups into a [Group] slice, sorted by
// descending runs with ties broken by ascending name under byte
// comparison.
func (a *Aggregator) groupSlice(groups map[string]*groupAccum, full bool) []Group {
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, a.groupStat(g, full))
	}
	slices.SortFunc(out, compareGroups)
	return out
}

// groupStat computes the derived [Group] figures for one accumulated
// group.
func (a *Aggregator) groupStat(g *groupAccum, full bool) Group {
	stat := Group{
		Name:      g.name,
		Runs:      g.runs,
		Succeeded: g.succeeded,
		Duration:  durationStats(g.durationSamples),

		UnmeasuredUnits: []string{},
	}
	if g.runs > 0 {
		stat.SuccessRate = round4(float64(g.succeeded) / float64(g.runs))
	}
	if a.total.runs > 0 {
		stat.Share = round4(float64(g.runs) / float64(a.total.runs))
	}
	if full {
		stat.MeanTurns = new(round2(float64(g.turnSum) / float64(g.runs)))
		stat.TokensUnmeasuredRuns = g.unmeasuredRuns
		stat.CostUnpricedRuns = g.costUnpricedRuns
		stat.CostPartialRuns = g.costPartialRuns
		if g.measuredRuns > 0 {
			stat.Tokens = new(withUncached(g.tokens))
		}
		if g.pricedRuns > 0 {
			cost := round9(g.costUSD)
			stat.CostUSD = &cost
			if g.succeededMeasured > 0 {
				stat.CostPerSucceededRunUSD = new(round9(cost / float64(g.succeededMeasured)))
			}
		}
		flags := lowerBoundFlagsOf(stat.Tokens, stat.CostUSD, g.unmeasuredRuns, &g.costAccum, &g.provenanceAccum)
		stat.TokensLowerBoundRuns = flags.tokensLowerBoundRuns
		stat.TokensLowerBound = flags.tokensLowerBound
		stat.CostLowerBound = flags.costLowerBound
		stat.AgentPricedUSD = flags.agentPricedUSD
		stat.UnmeasuredCause = flags.cause
		stat.UnmeasuredUnits = flags.units
	}
	return stat
}

// skillCallReport computes the [SkillCallReport] from a's folded skill state.
// by_skill is sorted by descending runs, then by ascending skill in byte order.
func (a *Aggregator) skillCallReport() *SkillCallReport {
	report := &SkillCallReport{
		UnrecordedRuns: a.unrecordedRuns,
		BySkill:        make([]SkillCount, 0, len(a.bySkill)),
	}
	for _, entry := range a.bySkill {
		report.BySkill = append(report.BySkill, *entry)
	}
	slices.SortFunc(report.BySkill, func(x, y SkillCount) int {
		return cmp.Or(cmp.Compare(y.Runs, x.Runs), strings.Compare(x.Skill, y.Skill))
	})
	return report
}

// selfReviewReport computes the derived [SelfReview] from a's folded
// self-review state. by_final_verdict is sorted by ascending verdict.
func (a *Aggregator) selfReviewReport() *SelfReview {
	sr := &SelfReview{
		RunsWithMetadata: a.selfReview.runsWithMetadata,
		ByFinalVerdict:   make([]VerdictCount, 0, len(a.selfReview.byVerdict)),
		CapReachedRuns:   a.selfReview.capReachedRuns,
		UnparsedRuns:     a.selfReview.unparsedRuns,
	}
	if a.selfReview.runsWithMetadata > 0 {
		sr.MeanIterations = new(round2(float64(a.selfReview.iterationSum) / float64(a.selfReview.runsWithMetadata)))
	}
	for verdict, count := range a.selfReview.byVerdict {
		sr.ByFinalVerdict = append(sr.ByFinalVerdict, VerdictCount{Verdict: verdict, Runs: count})
	}
	slices.SortFunc(sr.ByFinalVerdict, func(x, y VerdictCount) int {
		return strings.Compare(x.Verdict, y.Verdict)
	})
	return sr
}
