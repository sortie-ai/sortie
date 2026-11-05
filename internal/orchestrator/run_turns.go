package orchestrator

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/storage"
)

// SessionTurns is a running session's turn records. The turns are priced as
// the run would be priced if it ended now.
type SessionTurns struct {
	DispatchID string
	IssueID    string
	Identifier string

	// DisplayID is the qualified form of Identifier; empty when Identifier is
	// display-ready.
	DisplayID  string
	IssueTitle string
	RuleName   string
	AgentKind  string

	// UsageCompleteness is [storage.UsageComplete], [storage.UsageLowerBound],
	// or [storage.UsageUnmeasured].
	UsageCompleteness string

	// CostStatus is a [pricing.CostStatus] value.
	CostStatus string

	// CostUSD is the run's cost if it ended now; nil means no cost figure.
	CostUSD *float64

	// AgentPricedUSD is the part of CostUSD priced from agent-reported
	// amounts; nil when no part is.
	AgentPricedUSD *float64

	// Turns is ascending by Seq.
	Turns []storage.RunTurn
}

// TurnLog holds the latest turn records of every running session that has
// one, by dispatch ID. It is safe for concurrent use.
type TurnLog struct {
	mu       sync.Mutex
	sessions map[string]SessionTurns // guarded by mu
}

// NewTurnLog returns an empty log.
func NewTurnLog() *TurnLog {
	return &TurnLog{sessions: make(map[string]SessionTurns)}
}

// Sessions returns a copy of every held session in ascending DispatchID
// order, which the caller may keep and modify.
func (l *TurnLog) Sessions() []SessionTurns {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	sessions := make([]SessionTurns, 0, len(l.sessions))
	for _, session := range l.sessions {
		session.Turns = cloneTurns(session.Turns)
		session.CostUSD = cloneCost(session.CostUSD)
		session.AgentPricedUSD = cloneCost(session.AgentPricedUSD)
		sessions = append(sessions, session)
	}
	slices.SortFunc(sessions, func(a, b SessionTurns) int {
		return cmp.Compare(a.DispatchID, b.DispatchID)
	})
	return sessions
}

// publish replaces the held value of session.DispatchID. The log keeps
// session.Turns, so the caller hands over a slice nothing else writes.
func (l *TurnLog) publish(session SessionTurns) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.sessions[session.DispatchID] = session
	l.mu.Unlock()
}

// release forgets a session's records.
func (l *TurnLog) release(dispatchID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.sessions, dispatchID)
	l.mu.Unlock()
}

// RunningTurns returns the turn records of every running session that has
// one. It performs no I/O and is safe to call from any goroutine.
func (o *Orchestrator) RunningTurns() []SessionTurns {
	return o.state.RunningTurns.Sessions()
}

func cloneCost(cost *float64) *float64 {
	if cost == nil {
		return nil
	}
	return new(*cost)
}

// agentPricedOf sums the parts of price priced from an agent-reported amount;
// nil when no part is. Parts add in model order so the sum does not depend on
// map iteration.
func agentPricedOf(price pricing.Price) *float64 {
	var usd float64
	found := false
	for _, model := range slices.Sorted(maps.Keys(price.Parts)) {
		part := price.Parts[model]
		if part.Source != pricing.SourceAgent {
			continue
		}
		found = true
		if part.CostUSD != nil {
			usd += *part.CostUSD
		}
	}
	if !found {
		return nil
	}
	return &usd
}

func cloneTurns(turns []storage.RunTurn) []storage.RunTurn {
	cloned := slices.Clone(turns)
	for i := range cloned {
		cloned[i].Models = maps.Clone(cloned[i].Models)
		if cost := cloned[i].EstimatedCostUSD; cost != nil {
			cloned[i].EstimatedCostUSD = new(*cost)
		}
	}
	return cloned
}

// modelDelta returns the tokens each model spent beyond baseline, class by
// class, for the models that spent in some class; nil when none did.
func modelDelta(totals, baseline map[string]domain.TokenUsage) map[string]storage.ModelTokens {
	var delta map[string]storage.ModelTokens
	for model, total := range totals {
		base := baseline[model]
		spent := storage.ModelTokens{
			InputTokens:      total.InputTokens - base.InputTokens,
			OutputTokens:     total.OutputTokens - base.OutputTokens,
			CacheReadTokens:  total.CacheReadTokens - base.CacheReadTokens,
			CacheWriteTokens: total.CacheWriteTokens - base.CacheWriteTokens,
		}
		if spent == (storage.ModelTokens{}) {
			continue
		}
		if delta == nil {
			delta = make(map[string]storage.ModelTokens, len(totals))
		}
		delta[model] = spent
	}
	return delta
}

// addModelTokens adds extra onto into class by class and returns the result,
// allocating when into is nil and extra is not empty.
func addModelTokens(into, extra map[string]storage.ModelTokens) map[string]storage.ModelTokens {
	for model, tokens := range extra {
		if into == nil {
			into = make(map[string]storage.ModelTokens, len(extra))
		}
		sum := into[model]
		sum.InputTokens += tokens.InputTokens
		sum.OutputTokens += tokens.OutputTokens
		sum.CacheReadTokens += tokens.CacheReadTokens
		sum.CacheWriteTokens += tokens.CacheWriteTokens
		into[model] = sum
	}
	return into
}

// turnSpends maps turn records to the spend the pricing component prices.
func turnSpends(turns []storage.RunTurn) []pricing.TurnSpend {
	spends := make([]pricing.TurnSpend, len(turns))
	for i, turn := range turns {
		spends[i].Measured = turn.TokensMeasured
		if len(turn.Models) == 0 {
			continue
		}
		spends[i].Parts = make(map[string]domain.TokenUsage, len(turn.Models))
		for model, tokens := range turn.Models {
			spends[i].Parts[model] = domain.TokenUsage{
				InputTokens:      tokens.InputTokens,
				OutputTokens:     tokens.OutputTokens,
				CacheReadTokens:  tokens.CacheReadTokens,
				CacheWriteTokens: tokens.CacheWriteTokens,
			}
		}
	}
	return spends
}

// setTurnPrice stores price on turn.
func setTurnPrice(turn *storage.RunTurn, price pricing.TurnPrice) {
	turn.EstimatedCostUSD = price.CostUSD
	turn.InputCostUSD = price.Classes.Input
	turn.OutputCostUSD = price.Classes.Output
	turn.CacheReadCostUSD = price.Classes.CacheRead
	turn.CacheWriteCostUSD = price.Classes.CacheWrite
}

// endTurn makes the record of the turn event ended and publishes the
// session's records. It runs after the event's own usage is applied, so the
// event's usage belongs to the turn it ends.
func endTurn(state *State, issueID string, entry *RunningEntry, event domain.AgentEvent) {
	turn := storage.RunTurn{
		Seq:       len(entry.turns) + 1,
		EndedAt:   event.Timestamp.UTC(),
		ToolCalls: entry.turnToolCalls,
		Errors:    entry.turnErrors,
	}
	if event.Type == domain.EventTurnFailed || event.Type == domain.EventTurnEndedWithError {
		turn.Errors++
	}
	if entry.UsageMeasured {
		turn.TokensMeasured = true
		turn.Models = modelDelta(entry.UsageByModel, entry.turnSpend.Parts)
	}
	entry.turns = append(entry.turns, turn)

	// applyUsageDelta writes UsageByModel in place, so a baseline sharing its
	// map would read as zero spend in every later difference.
	entry.turnSpend = spendOfRunningEntry(entry)
	entry.turnSpend.Parts = maps.Clone(entry.turnSpend.Parts)
	entry.turnSpend.Reported = slices.Clone(entry.turnSpend.Reported)
	entry.turnToolCalls, entry.turnErrors = 0, 0

	publishTurns(state, issueID, entry)
}

// publishTurns prices the session's records as the run would be priced if it
// ended now and swaps the view into the turn log. Pricing and copying finish
// before the log's lock is taken.
func publishTurns(state *State, issueID string, entry *RunningEntry) {
	if state.RunningTurns == nil {
		return
	}
	spend := entry.turnSpend
	price := pricing.PriceRun(spend, state.Rates)
	turns := cloneTurns(entry.turns)
	prices := pricing.PriceTurns(spend, price, turnSpends(turns), state.Rates)
	for i := range turns {
		setTurnPrice(&turns[i], prices[i])
	}
	state.RunningTurns.publish(SessionTurns{
		DispatchID:        entry.DispatchID,
		IssueID:           issueID,
		Identifier:        entry.Identifier,
		DisplayID:         entry.Issue.DisplayID,
		IssueTitle:        entry.Issue.Title,
		RuleName:          entry.RuleName,
		AgentKind:         entry.AgentKind,
		UsageCompleteness: storage.UsageCompletenessOf(spend.Measured, spend.UnaccountedTurns),
		CostStatus:        string(price.Status),
		CostUSD:           cloneCost(price.CostUSD),
		AgentPricedUSD:    agentPricedOf(price),
		Turns:             turns,
	})
}

// exitTurns returns the run's final turn records. Spend the records do not
// yet hold, and tool calls since the latest record, go to the last record; a
// turn the worker started without an applied end, and a run with spend or
// tool calls and no record, get one record ended at now. A run with neither
// has none.
func exitTurns(entry *RunningEntry, measured bool, turnsSeen int, now time.Time) []storage.RunTurn {
	turns := cloneTurns(entry.turns)
	var pending map[string]storage.ModelTokens
	if measured {
		pending = modelDelta(entry.UsageByModel, entry.turnSpend.Parts)
	}
	switch {
	case turnsSeen > len(turns) || (len(turns) == 0 && (pending != nil || entry.turnToolCalls > 0)):
		return append(turns, storage.RunTurn{
			Seq:            len(turns) + 1,
			EndedAt:        now.UTC(),
			TokensMeasured: measured,
			Models:         pending,
			ToolCalls:      entry.turnToolCalls,
			Errors:         entry.turnErrors,
		})
	case len(turns) > 0:
		last := &turns[len(turns)-1]
		last.ToolCalls += entry.turnToolCalls
		last.Errors += entry.turnErrors
		if measured {
			last.TokensMeasured = true
			last.Models = addModelTokens(last.Models, pending)
		}
	}
	return turns
}
