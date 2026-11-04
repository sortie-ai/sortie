package orchestrator

import (
	"maps"
	"math"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

const turnsIssue = "ISSUE-TURNS"

func turnsSession(t *testing.T) *State {
	t.Helper()

	state := exitState(t, turnsIssue, nil)
	state.Rates = fixtureRates(t, 10)
	entry := state.Running[turnsIssue]
	entry.DispatchID = "dispatch-turns"
	entry.AgentKind = "k"
	return state
}

func feedTurnEvents(state *State, events ...domain.AgentEvent) {
	for _, event := range events {
		HandleAgentEvent(state, turnsIssue, event, discardLogger(), nil)
	}
}

func turnToolResult(failed bool) domain.AgentEvent {
	return domain.AgentEvent{Type: domain.EventToolResult, Timestamp: baseTime, ToolError: failed}
}

func turnUsage(model string, cumulative domain.TokenUsage) domain.AgentEvent {
	return domain.AgentEvent{Type: domain.EventTokenUsage, Timestamp: baseTime, Model: model, Usage: cumulative}
}

func turnEnd(kind domain.AgentEventType, at time.Time) domain.AgentEvent {
	return domain.AgentEvent{Type: kind, Timestamp: at}
}

func exitTurnsSession(t *testing.T, state *State, result WorkerResult) storage.RunHistory {
	t.Helper()

	store := &mockExitStore{}
	result.IssueID, result.Identifier, result.ExitKind, result.AgentAdapter = turnsIssue, turnsIssue+"-ident", WorkerExitNormal, "k"

	HandleWorkerExit(state, result, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	return store.runHistories[0]
}

func TestRunTurns_SumToRun(t *testing.T) {
	t.Parallel()

	exitAt := baseTime.Add(60 * time.Second)
	ends := []time.Time{
		baseTime.Add(10 * time.Second),
		baseTime.Add(20 * time.Second).In(time.FixedZone("plus2", 2*3600)),
		baseTime.Add(30 * time.Second),
	}
	wantModels := []map[string]storage.ModelTokens{
		{"m1": {InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200}},
		{"m1": {InputTokens: 2000, OutputTokens: 700, CacheReadTokens: 400}},
		{
			"m2": {InputTokens: 800, OutputTokens: 300, CacheWriteTokens: 100},
			"m1": {InputTokens: 100, OutputTokens: 40},
		},
	}
	wantToolCalls := []int{1, 2, 1}
	wantErrors := []int{0, 2, 0}

	tests := []struct {
		name        string
		endLastTurn bool
		wantLastEnd time.Time
		wantLive    int
	}{
		{name: "trailing usage lands on the last ended turn", endLastTurn: true, wantLastEnd: ends[2], wantLive: 3},
		{name: "unended last turn becomes an exit-time record", wantLastEnd: exitAt, wantLive: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := turnsSession(t)
			feedTurnEvents(state,
				turnToolResult(false),
				turnUsage("m1", domain.TokenUsage{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500, CacheReadTokens: 200}),
				turnEnd(domain.EventTurnCompleted, ends[0]),

				turnToolResult(true),
				turnToolResult(false),
				turnUsage("m1", domain.TokenUsage{InputTokens: 3000, OutputTokens: 1200, TotalTokens: 4200, CacheReadTokens: 600}),
				turnEnd(domain.EventTurnFailed, ends[1]),

				turnToolResult(false),
				turnUsage("m2", domain.TokenUsage{InputTokens: 3800, OutputTokens: 1500, TotalTokens: 5300, CacheReadTokens: 600, CacheWriteTokens: 100}),
			)
			if tt.endLastTurn {
				feedTurnEvents(state, turnEnd(domain.EventTurnCompleted, ends[2]))
			}

			live := state.RunningTurns.Sessions()
			if len(live) != 1 || len(live[0].Turns) != tt.wantLive {
				t.Fatalf("RunningTurns.Sessions() = %+v, want one session with %d turns", live, tt.wantLive)
			}

			run := exitTurnsSession(t, state, WorkerResult{
				Usage:         domain.TokenUsage{InputTokens: 3900, OutputTokens: 1540, TotalTokens: 5440, CacheReadTokens: 600, CacheWriteTokens: 100},
				UsageMeasured: true,
				ModelName:     "m1",
				TurnsStarted:  3,
			})

			if left := state.RunningTurns.Sessions(); len(left) != 0 {
				t.Errorf("RunningTurns.Sessions() after exit = %+v, want none", left)
			}
			if len(run.Turns) != 3 {
				t.Fatalf("RunHistory.Turns has %d records, want 3", len(run.Turns))
			}
			if run.EstimatedCostUSD == nil || *run.EstimatedCostUSD <= 0 {
				t.Fatalf("RunHistory.EstimatedCostUSD = %v, want a positive cost", run.EstimatedCostUSD)
			}

			summed := make(map[string]storage.ModelTokens)
			summedCost := 0.0
			for i, turn := range run.Turns {
				wantEnd := ends[i]
				if i == 2 {
					wantEnd = tt.wantLastEnd
				}
				if turn.Seq != i+1 || !turn.EndedAt.Equal(wantEnd) || turn.EndedAt.Location() != time.UTC {
					t.Errorf("Turns[%d] (Seq, EndedAt) = (%d, %v), want (%d, %v in UTC)", i, turn.Seq, turn.EndedAt, i+1, wantEnd)
				}
				if turn.ToolCalls != wantToolCalls[i] || turn.Errors != wantErrors[i] {
					t.Errorf("Turns[%d] (ToolCalls, Errors) = (%d, %d), want (%d, %d)", i, turn.ToolCalls, turn.Errors, wantToolCalls[i], wantErrors[i])
				}
				if !turn.TokensMeasured || !maps.Equal(turn.Models, wantModels[i]) {
					t.Errorf("Turns[%d] (TokensMeasured, Models) = (%v, %v), want (true, %v)", i, turn.TokensMeasured, turn.Models, wantModels[i])
				}
				for model, tokens := range turn.Models {
					sum := summed[model]
					sum.InputTokens += tokens.InputTokens
					sum.OutputTokens += tokens.OutputTokens
					sum.CacheReadTokens += tokens.CacheReadTokens
					sum.CacheWriteTokens += tokens.CacheWriteTokens
					summed[model] = sum
				}
				if turn.EstimatedCostUSD == nil {
					t.Errorf("Turns[%d].EstimatedCostUSD = nil, want a cost", i)
					continue
				}
				summedCost += *turn.EstimatedCostUSD
				classes := turn.InputCostUSD + turn.OutputCostUSD + turn.CacheReadCostUSD + turn.CacheWriteCostUSD
				if math.Abs(classes-*turn.EstimatedCostUSD) > 1e-6 {
					t.Errorf("Turns[%d] class costs sum to %v, want %v", i, classes, *turn.EstimatedCostUSD)
				}
			}
			if !maps.Equal(summed, run.ModelUsage) {
				t.Errorf("Turns per-model tokens sum to %v, want RunHistory.ModelUsage %v", summed, run.ModelUsage)
			}
			if math.Abs(summedCost-*run.EstimatedCostUSD) > 1e-6 {
				t.Errorf("Turns costs sum to %v, want RunHistory.EstimatedCostUSD %v", summedCost, *run.EstimatedCostUSD)
			}
		})
	}
}

func TestRunTurns_Unmeasured(t *testing.T) {
	t.Parallel()

	state := turnsSession(t)
	feedTurnEvents(state,
		turnToolResult(true),
		turnEnd(domain.EventTurnFailed, baseTime.Add(10*time.Second)),
		turnToolResult(false),
		turnEnd(domain.EventTurnCompleted, baseTime.Add(20*time.Second)),
	)

	run := exitTurnsSession(t, state, WorkerResult{TurnsStarted: 2})

	wantToolCalls := []int{1, 1}
	wantErrors := []int{2, 0}
	if len(run.Turns) != 2 {
		t.Fatalf("RunHistory.Turns has %d records, want 2", len(run.Turns))
	}
	for i, turn := range run.Turns {
		if turn.TokensMeasured || len(turn.Models) != 0 || turn.EstimatedCostUSD != nil {
			t.Errorf("Turns[%d] (TokensMeasured, Models, EstimatedCostUSD) = (%v, %v, %v), want (false, none, nil)", i, turn.TokensMeasured, turn.Models, turn.EstimatedCostUSD)
		}
		if turn.ToolCalls != wantToolCalls[i] || turn.Errors != wantErrors[i] {
			t.Errorf("Turns[%d] (ToolCalls, Errors) = (%d, %d), want (%d, %d)", i, turn.ToolCalls, turn.Errors, wantToolCalls[i], wantErrors[i])
		}
	}
}

func TestRunTurns_NextTurnStartBeforeEndingKeepsUnendedTurn(t *testing.T) {
	t.Parallel()

	state := turnsSession(t)
	firstEnd := baseTime.Add(10 * time.Second)
	feedTurnEvents(state, turnUsage("m1", domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}))
	state.Running[turnsIssue].TurnCount = 2
	feedTurnEvents(state,
		turnEnd(domain.EventTurnCompleted, firstEnd),
		turnToolResult(false),
		turnUsage("m1", domain.TokenUsage{InputTokens: 300, OutputTokens: 90, TotalTokens: 390}),
	)

	run := exitTurnsSession(t, state, WorkerResult{TurnsStarted: 2})

	if len(run.Turns) != 2 || !run.Turns[0].EndedAt.Equal(firstEnd) || run.Turns[1].ToolCalls != 1 {
		t.Fatalf("RunHistory.Turns = %+v, want the first ended at %v and the unended second made at exit with its tool call", run.Turns, firstEnd)
	}
}
