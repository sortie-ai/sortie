//go:build unix

package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
)

// ceilingAgentKind declares incremental usage arrival so the run is graded by
// a usage-reporting adapter rather than the silent fixture kind's default.
const ceilingAgentKind = "qualification-e2e-ceiling-fixture"

func init() {
	registry.Agents.RegisterWithMeta(ceilingAgentKind, func(map[string]any) (domain.AgentAdapter, error) {
		return nil, fmt.Errorf("%s is resolved through AgentAdapterByKind, never constructed from the registry", ceilingAgentKind)
	}, registry.AgentMeta{
		RequiresCommand:  true,
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionSessionTotal,
	})
}

// ceilingSessions is raised above the one session the run opens so the
// session budget cannot withhold a second dispatch and mask the token ceiling.
const (
	ceilingTokens   = 100
	turnSpendTokens = 250
	ceilingSessions = 8
)

// spendingAgent reports a run-cumulative spend, then holds the turn open until
// its context ends, so the ceiling stop is what ends it.
type spendingAgent struct {
	*fakeAgent
	starts atomic.Int64
}

func newSpendingAgent() *spendingAgent {
	return &spendingAgent{fakeAgent: newFakeAgent()}
}

func (a *spendingAgent) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	a.starts.Add(1)
	return a.fakeAgent.StartSession(ctx, params)
}

func (a *spendingAgent) RunTurn(ctx context.Context, _ domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	params.OnEvent(domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: time.Now().UTC(),
		Usage: domain.TokenUsage{
			InputTokens:  turnSpendTokens / 2,
			OutputTokens: turnSpendTokens - turnSpendTokens/2,
			TotalTokens:  turnSpendTokens,
		},
	})
	<-ctx.Done()
	return domain.TurnResult{}, &domain.AgentError{
		Kind:    domain.ErrTurnFailed,
		Message: "the turn was stopped before it finished",
		Err:     ctx.Err(),
	}
}

func ceilingHarness(t *testing.T) (*Harness, *spendingAgent) {
	t.Helper()

	agent := newSpendingAgent()
	harness := NewHarnessWithAgent(t, agent, "sortie-qualification-spending-agent --session-fixture", ceilingAgentKind, Budgets{
		MaxTokens:   ceilingTokens,
		MaxSessions: ceilingSessions,
		Observation: 30 * time.Second,
	})
	return harness, agent
}

func awaitRunHistory(t *testing.T, harness *Harness) []persistence.RunHistory {
	t.Helper()

	deadline := time.Now().Add(harness.Observation())
	for {
		rows, err := harness.store.QueryRunHistoryByIssue(context.Background(), issueID)
		if err != nil {
			t.Fatalf("query run history: %v", err)
		}
		if len(rows) > 0 {
			return rows
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no run history row for %s within %s; the run never ended", issueIdentifier, harness.Observation())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitTokenBudgetHold polls until the issue is held out of dispatch by the
// token budget. Reaching it proves a poll tick ran after the stop, so the
// no-redispatch assertions stand on a loop that could have dispatched again.
func awaitTokenBudgetHold(t *testing.T, harness *Harness) orchestrator.SnapshotBudgetEntry {
	t.Helper()

	deadline := time.Now().Add(harness.Observation())
	for {
		snapshot, err := harness.orchestrator.SnapshotFunc()()
		if err != nil {
			t.Fatalf("runtime snapshot: %v", err)
		}
		for _, entry := range snapshot.BudgetExhausted {
			if entry.IssueID == issueID {
				return entry
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("issue %s never entered the budget hold within %s", issueIdentifier, harness.Observation())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTokenCeilingStopsTheRunAndDispatchesNoOther(t *testing.T) {
	harness, agent := ceilingHarness(t)

	cfg := harness.manager.Config().Agent
	if cfg.MaxTokens != ceilingTokens {
		t.Fatalf("configured max_tokens = %d, want the finite ceiling %d", cfg.MaxTokens, ceilingTokens)
	}
	if cfg.MaxSessions <= 1 {
		t.Fatalf("configured max_sessions = %d, want room for a further dispatch so the token ceiling is what withholds it", cfg.MaxSessions)
	}

	StartWorkflow(t, harness)

	rows := awaitRunHistory(t, harness)
	if len(rows) != 1 {
		t.Fatalf("run history holds %d rows, want the one run the ceiling stopped", len(rows))
	}
	row := rows[0]

	if row.Status != "budget_stopped" {
		t.Errorf("run status = %q, want %q: the ceiling's own stop is recorded as its own status", row.Status, "budget_stopped")
	}
	if row.Error == nil {
		t.Fatalf("run status %q carries no error, want the token ceiling stop reason", row.Status)
	}
	if !strings.Contains(*row.Error, "token ceiling reached") {
		t.Errorf("run error = %q, want it to name the token ceiling as the reason the run ended", *row.Error)
	}
	if !strings.Contains(*row.Error, fmt.Sprintf("of %d tokens", ceilingTokens)) {
		t.Errorf("run error = %q, want it to name the ceiling %d the run crossed", *row.Error, ceilingTokens)
	}
	if !row.TokensMeasured || row.TotalTokens < ceilingTokens {
		t.Errorf("run recorded %d token(s) measured=%t, want a measured total of at least the ceiling %d",
			row.TotalTokens, row.TokensMeasured, ceilingTokens)
	}

	hold := awaitTokenBudgetHold(t, harness)
	if hold.Reason != "token_budget" {
		t.Errorf("budget hold reason = %q, want %q", hold.Reason, "token_budget")
	}
	if hold.UsedTokens == nil || *hold.UsedTokens < ceilingTokens {
		t.Errorf("budget hold used tokens = %v, want at least the ceiling %d", hold.UsedTokens, ceilingTokens)
	}

	// The issue is still active; a loop that did not hold it would redispatch
	// within this window, which at 20ms ticks covers many.
	time.Sleep(600 * time.Millisecond)

	if starts := agent.starts.Load(); starts != 1 {
		t.Errorf("agent sessions started = %d, want 1: nothing may be dispatched after the ceiling stop", starts)
	}
	after, err := harness.store.QueryRunHistoryByIssue(context.Background(), issueID)
	if err != nil {
		t.Fatalf("query run history: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("run history holds %d rows after the stop, want 1: nothing may be dispatched after the ceiling stop", len(after))
	}
}
