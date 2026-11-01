package runstats

import (
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

const (
	stageResultAdvanced = "advanced"
	stageResultPartial  = "partial"
	stageResultCeiling  = "ceiling"
	stageResultFailed   = "failed"
)

const (
	reasonRunFailed       = "run_failed"
	reasonNeedsPerson     = "needs_person"
	reasonBudgetHeld      = "budget_held"
	reasonUsageLimited    = "usage_limited"
	reasonOperatorStopped = "operator_stopped"
	reasonVerdictWithheld = "verdict_withheld"
)

// noHopReasonOrder is the order in which equally frequent reasons are listed.
var noHopReasonOrder = []string{
	reasonRunFailed,
	reasonNeedsPerson,
	reasonBudgetHeld,
	reasonUsageLimited,
	reasonOperatorStopped,
	reasonVerdictWithheld,
}

// passKey identifies a pass on one stage: the runs of a chain that share a
// rule.
type passKey struct {
	chainID string
	rule    string
}

// passAccum is a pass's folded runs on one stage.
type passAccum struct {
	previous string
	seconds  float64
	timed    bool

	// Fields of the latest deciding run.
	status      string
	stageResult string
	blocked     bool
	withheld    bool
}

// foldPass folds row into the pass of its chain and rule. A reaction run
// takes part only with a hop result; without one it counts in by_rule alone.
// Rows arrive in ascending id order, so the last folded run decides.
func (a *Aggregator) foldPass(row storage.RunStatsRow) {
	if row.ChainID == "" || row.RuleName == "" {
		return
	}
	stageRun := row.ReactionKind == ""
	if !stageRun && row.StageResult == "" {
		return
	}
	key := passKey{chainID: row.ChainID, rule: row.RuleName}
	p, ok := a.passes[key]
	if !ok {
		p = &passAccum{previous: row.StagePrevious}
		a.passes[key] = p
	}
	p.status = row.Status
	p.stageResult = row.StageResult
	p.blocked = row.AgentBlocked
	p.withheld = row.HandoffWithheld
	if !stageRun {
		return
	}
	if seconds, ok := duration(row); ok {
		p.seconds += seconds
		p.timed = true
	}
}

// funnelDefinition is the merge of the configured chains that share a stage.
type funnelDefinition struct {
	stages  []string
	entries map[string]bool
}

// funnelDefinitions merges chains, in configuration order, into one
// definition per set of chains that share a stage. A chain joining a
// definition adds its first rule as an entry and inserts the rules before its
// first shared rule immediately before that rule.
func funnelDefinitions(chains [][]string) []*funnelDefinition {
	var defs []*funnelDefinition
	for _, chain := range chains {
		if len(chain) == 0 {
			continue
		}
		def := definitionSharing(defs, chain)
		if def == nil {
			defs = append(defs, &funnelDefinition{
				stages:  slices.Clone(chain),
				entries: map[string]bool{chain[0]: true},
			})
			continue
		}
		def.entries[chain[0]] = true
		for j, rule := range chain {
			if at := slices.Index(def.stages, rule); at >= 0 {
				def.stages = slices.Insert(def.stages, at, chain[:j]...)
				break
			}
		}
	}
	return defs
}

// definitionSharing returns the first definition holding any rule of chain.
func definitionSharing(defs []*funnelDefinition, chain []string) *funnelDefinition {
	for _, def := range defs {
		for _, rule := range chain {
			if slices.Contains(def.stages, rule) {
				return def
			}
		}
	}
	return nil
}

// stageKeys indexes every pass the report counts by its stage: the folded
// passes and the live ones.
type stageKeys struct {
	chainsByRule map[string][]string
	held         map[passKey]bool
	budget       map[passKey]bool
	livePrevious map[passKey]string
}

// newStageKeys builds the single set of pass keys, so a pass that finished
// runs and a live pass share counts once.
func (a *Aggregator) newStageKeys(live []LivePass) stageKeys {
	keys := stageKeys{
		chainsByRule: make(map[string][]string),
		held:         make(map[passKey]bool),
		budget:       make(map[passKey]bool),
		livePrevious: make(map[passKey]string),
	}
	seen := make(map[passKey]bool)
	add := func(key passKey) {
		if seen[key] {
			return
		}
		seen[key] = true
		keys.chainsByRule[key.rule] = append(keys.chainsByRule[key.rule], key.chainID)
	}
	for key := range a.passes {
		add(key)
	}
	for _, pass := range live {
		if pass.ChainID == "" || pass.RuleName == "" {
			continue
		}
		key := passKey{chainID: pass.ChainID, rule: pass.RuleName}
		if _, ok := keys.livePrevious[key]; !ok {
			keys.livePrevious[key] = pass.PreviousRule
		}
		if pass.BudgetHeld {
			keys.budget[key] = true
		} else {
			keys.held[key] = true
		}
		add(key)
	}
	return keys
}

// funnelReport builds chain_funnels. Live passes count only while the range
// reaches the report's own time.
func (a *Aggregator) funnelReport(generatedAt time.Time, until *time.Time) []Funnel {
	funnels := []Funnel{}
	if len(a.stages.Chains) == 0 || !a.caps.FunnelBreakdown() {
		return funnels
	}
	var live []LivePass
	if until == nil || until.After(generatedAt) {
		live = a.stages.Live
	}
	keys := a.newStageKeys(live)
	labelRules := make(map[string]bool, len(a.stages.LabelRules))
	for _, rule := range a.stages.LabelRules {
		labelRules[rule] = true
	}

	for _, def := range funnelDefinitions(a.stages.Chains) {
		funnel := Funnel{Stages: make([]FunnelStage, 0, len(def.stages))}
		for i, rule := range def.stages {
			stage := FunnelStage{
				Rule:         rule,
				Entry:        def.entries[rule],
				Last:         i == len(def.stages)-1,
				NoHopReasons: []NoHopReason{},
			}
			if group, ok := a.byRule[rule]; ok {
				stage.Runs = group.runs
			}
			a.fillStage(&stage, keys, labelRules)
			funnel.Stages = append(funnel.Stages, stage)
		}
		funnels = append(funnels, funnel)
	}
	return funnels
}

// fillStage counts every pass that reached stage.Rule into stage.
func (a *Aggregator) fillStage(stage *FunnelStage, keys stageKeys, labelRules map[string]bool) {
	var samples []float64
	reasons := make(map[string]int)
	for _, chainID := range keys.chainsByRule[stage.Rule] {
		key := passKey{chainID: chainID, rule: stage.Rule}
		p := a.passes[key]
		stage.Entered++

		previous := keys.livePrevious[key]
		if p != nil {
			previous = p.previous
		}
		switch {
		case previous != "":
			stage.Arrivals.Hop++
		case labelRules[stage.Rule]:
			stage.Arrivals.Label++
		default:
			stage.Arrivals.Match++
		}

		reason := stage.countOutcome(p, keys.held[key], keys.budget[key])
		if reason != "" {
			reasons[reason]++
		}
		if p != nil && p.timed {
			samples = append(samples, p.seconds)
		}
	}

	for _, reason := range noHopReasonOrder {
		if reasons[reason] > 0 {
			stage.NoHopReasons = append(stage.NoHopReasons, NoHopReason{Reason: reason, Passes: reasons[reason]})
		}
	}
	slices.SortStableFunc(stage.NoHopReasons, func(x, y NoHopReason) int { return y.Passes - x.Passes })

	if len(samples) > 0 {
		slices.Sort(samples)
		stage.MedianSeconds = new(percentile(samples, 50))
	}
}

// countOutcome counts one pass into stage.Outcomes by the first matching
// row of the outcome table and returns the no-hop reason, empty for any
// other outcome. p is nil for a pass only a live record supplies.
func (stage *FunnelStage) countOutcome(p *passAccum, held, budget bool) string {
	var latest passAccum
	if p != nil {
		latest = *p
	}
	switch {
	case held:
		stage.Outcomes.InStage++
	case latest.stageResult == stageResultAdvanced || latest.stageResult == stageResultPartial:
		if stage.Last {
			stage.Outcomes.HandedOff++
		} else {
			stage.Outcomes.Advanced++
		}
	case latest.stageResult == stageResultCeiling:
		stage.Outcomes.Ceiling++
	case latest.stageResult == stageResultFailed:
		stage.Outcomes.HopFailed++
	case latest.status == "succeeded" && latest.blocked:
		return stage.noHop(reasonNeedsPerson)
	case latest.status == "succeeded" && stage.Last:
		stage.Outcomes.HandedOff++
	case budget:
		return stage.noHop(reasonBudgetHeld)
	case latest.status == "succeeded":
		return stage.noHop(reasonOperatorStopped)
	case latest.status == "failed" && latest.withheld:
		return stage.noHop(reasonVerdictWithheld)
	case latest.status == "needs_person":
		return stage.noHop(reasonNeedsPerson)
	case latest.status == "budget_stopped":
		return stage.noHop(reasonBudgetHeld)
	case latest.status == "usage_limited":
		return stage.noHop(reasonUsageLimited)
	case latest.status == "cancelled":
		return stage.noHop(reasonOperatorStopped)
	default:
		return stage.noHop(reasonRunFailed)
	}
	return ""
}

// noHop counts a no-hop outcome and returns its reason.
func (stage *FunnelStage) noHop(reason string) string {
	stage.Outcomes.NoHop++
	return reason
}
