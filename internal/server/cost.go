package server

import (
	"math"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/pricing"
)

// Cost states served on running rows. An estimate is exact only in the
// estimated state; partial is a lower bound.
const (
	costStatusEstimated  = "estimated"
	costStatusUnpriced   = "unpriced"
	costStatusUnmeasured = "unmeasured"
	costStatusPartial    = "partial"
)

// servedCost returns a recorded or live figure as it is served. An empty
// status is a run recorded without a price, which reads as unpriced rather
// than as a guessed cost.
func servedCost(cost *float64, status string) (*float64, string) {
	if status == "" {
		return nil, costStatusUnpriced
	}
	return cost, status
}

func liveCostOf(e orchestrator.SnapshotRunningEntry) (cost *float64, status string) {
	return servedCost(e.EstimatedCostUSD, e.CostStatus)
}

// sessionCostPart is the running session's share of an issue's cost. The
// zero value is no session.
type sessionCostPart struct {
	cost   *float64
	status string
	spent  bool
}

func sessionPartOf(e orchestrator.SnapshotRunningEntry) sessionCostPart {
	cost, status := liveCostOf(e)
	spent := e.AgentInputTokens != 0 || e.AgentOutputTokens != 0 || e.AgentTotalTokens != 0 ||
		e.CacheReadTokens != 0 || e.CacheWriteTokens != 0
	return sessionCostPart{cost: cost, status: status, spent: spent}
}

// issueCost folds the recorded cost of an issue's completed sessions and its
// running session into one cost and state. The result is estimated only when
// usedComplete says the priced parts cover the whole spend, and partial when
// it is a lower bound. A part that spent nothing prices nothing, so a running
// session at zero tokens neither makes an unpriced issue partial nor a fully
// priced one unpriced.
func issueCost(completed *orchestrator.SnapshotIssueUsage, usedTokens *int64, usedComplete bool, session sessionCostPart) (cost *float64, status string) {
	if usedTokens == nil {
		return nil, costStatusUnmeasured
	}

	total, folded := foldIssueCost(completed, session, usedComplete)
	switch folded {
	case pricing.CostUnpriced, pricing.CostUnmeasured:
		return nil, string(folded)
	}
	return &total, string(folded)
}

// foldIssueCost folds the issue's completed sessions and its running session
// through [pricing.IssueCost].
func foldIssueCost(completed *orchestrator.SnapshotIssueUsage, session sessionCostPart, usedComplete bool) (float64, pricing.CostStatus) {
	return pricing.IssueCost(recordedOf(completed), pricing.SessionCost{
		USD:    session.cost,
		Status: pricing.CostStatus(session.status),
		Spent:  session.spent,
	}, usedComplete)
}

// issueCostUSD returns the priced sum of the issue's completed sessions and
// its running session, nil for none, which the next outcomes' money gate
// compares. Completeness moves the status, never the sum.
func issueCostUSD(completed *orchestrator.SnapshotIssueUsage, running *orchestrator.SnapshotRunningEntry) float64 {
	var session sessionCostPart
	if running != nil {
		session = sessionPartOf(*running)
	}
	usd, _ := foldIssueCost(completed, session, false)
	return usd
}

// recordedOf maps the cost aggregates of an issue's completed sessions, nil
// for none, onto the input of [pricing.IssueCost].
func recordedOf(completed *orchestrator.SnapshotIssueUsage) pricing.RecordedCost {
	if completed == nil {
		return pricing.RecordedCost{}
	}
	return pricing.RecordedCost{
		USD:      completed.RecordedCostUSD,
		Priced:   completed.PricedSessions,
		Partial:  completed.PartialSessions,
		Unpriced: completed.UnpricedSessions,
	}
}

// activeCostTotal sums the live estimates of the running sessions priced
// estimated or partial, and counts the sessions priced unpriced. anySet is
// false when no session priced, distinguishing that from a priced total of
// zero.
func activeCostTotal(running []orchestrator.SnapshotRunningEntry) (total float64, anySet bool, unpriced int) {
	for _, e := range running {
		cost, status := liveCostOf(e)
		switch status {
		case costStatusEstimated, costStatusPartial:
			if cost != nil {
				total += *cost
				anySet = true
			}
		case costStatusUnpriced:
			unpriced++
		}
	}
	return total, anySet, unpriced
}

// FormatCost formats a USD cost value as a string with two decimal places
// and comma thousand separators; see [pricing.FormatUSD].
func FormatCost(v float64) string {
	return pricing.FormatUSD(v)
}

// FormatCostCut formats a USD cost value as [FormatCost] does, truncated
// toward zero to the cent so a lower bound never reads above the spend it
// bounds.
//
// The cut reads the shortest decimal form of the value, not v*100, because
// binary floating point puts 0.29*100 below 29 and would drop a cent the value
// holds.
func FormatCostCut(usd float64) string {
	whole, fraction, _ := strings.Cut(strconv.FormatFloat(math.Abs(usd), 'f', -1, 64), ".")
	cents, _ := strconv.ParseInt(whole+(fraction + "00")[:2], 10, 64)
	if usd < 0 {
		cents = -cents
	}
	return pricing.FormatUSD(float64(cents) / 100)
}
