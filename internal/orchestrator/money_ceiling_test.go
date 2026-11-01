package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

func TestEnforceInFlightMoneyCeiling_StopAndExit(t *testing.T) {
	t.Parallel()

	const issueID = "MONEY-1"
	// fixtureRates(t, 10) prices kind "k" at $10 per million input tokens, so
	// 250_000 input tokens estimate $2.50 of session cost.
	recorded3 := storage.IssueTokenUsage{RecordedCostUSD: 3, PricedSessions: 1, Sessions: 1}
	recorded1 := storage.IssueTokenUsage{RecordedCostUSD: 1, PricedSessions: 1, Sessions: 1}

	tests := []struct {
		name        string
		baseline    *storage.IssueTokenUsage
		read        tokenStoreResponse
		inputTokens int64
		wantSource  string
		wantError   string
	}{
		{
			name:        "confirming read keeps the issue at the ceiling",
			baseline:    &recorded3,
			read:        tokenStoreResponse{usage: recorded3},
			inputTokens: 250_000,
			wantSource:  tokenSumConfirmedRead,
			wantError:   "money ceiling reached: used $5.50 of $5.00",
		},
		{
			name:        "confirming read puts the issue back under the ceiling",
			baseline:    &recorded3,
			read:        tokenStoreResponse{usage: recorded1},
			inputTokens: 250_000,
		},
		{
			name:        "failed read with the session estimate below the ceiling lets the run continue",
			baseline:    &recorded3,
			read:        tokenStoreResponse{err: errors.New("db unavailable")},
			inputTokens: 250_000,
		},
		{
			name:        "failed read with the session estimate at the ceiling still stops",
			read:        tokenStoreResponse{err: errors.New("db unavailable")},
			inputTokens: 500_000,
			wantSource:  tokenSumSessionSpendAlone,
			wantError:   "money ceiling reached: used $5.00 of $5.00",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cancelCalls int
			state := exitState(t, issueID, nil)
			state.MaxCostUSD = 5
			state.Rates = fixtureRates(t, 10)
			entry := state.Running[issueID]
			entry.AgentKind = "k"
			entry.IssueUsage = tt.baseline
			entry.CeilingCancelFunc = func() { cancelCalls++ }
			store := &fakeTokenStore{responses: []tokenStoreResponse{tt.read}}

			event := domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{InputTokens: tt.inputTokens, TotalTokens: tt.inputTokens}}
			HandleAgentEvent(state, issueID, event, discardLogger(), &spyMetrics{})
			enforceInFlightMoneyCeiling(context.Background(), state, issueID, event, store, discardLogger())

			wantStop := tt.wantSource != ""
			request := entry.CeilingStopRequest
			if (request != nil) != wantStop {
				t.Fatalf("enforceInFlightMoneyCeiling(%d input tokens) CeilingStopRequest = %+v, want stop %v", tt.inputTokens, request, wantStop)
			}
			wantCancels := 0
			if wantStop {
				wantCancels = 1
			}
			if cancelCalls != wantCancels {
				t.Errorf("CeilingCancelFunc called %d times, want %d", cancelCalls, wantCancels)
			}
			if !wantStop {
				return
			}
			if request.Reason != budgetReasonMoney || request.SumSource != tt.wantSource {
				t.Errorf("CeilingStopRequest = {Reason %q, SumSource %q}, want {%q, %q}", request.Reason, request.SumSource, budgetReasonMoney, tt.wantSource)
			}

			exitStore := &mockExitStore{}
			HandleWorkerExit(state, WorkerResult{
				IssueID:          issueID,
				Identifier:       issueID + "-ident",
				ExitKind:         WorkerExitCancelled,
				StoppedByCeiling: true,
				AgentAdapter:     "k",
				WorkspacePath:    "/tmp/ws",
			}, defaultExitParams(t, exitStore))

			if len(exitStore.runHistories) != 1 {
				t.Fatalf("AppendRunHistory called %d times, want 1", len(exitStore.runHistories))
			}
			run := exitStore.runHistories[0]
			if run.Status != "budget_stopped" {
				t.Errorf("RunHistory.Status = %q, want %q", run.Status, "budget_stopped")
			}
			if run.Error == nil || *run.Error != tt.wantError {
				t.Errorf("RunHistory.Error = %v, want %q", run.Error, tt.wantError)
			}
		})
	}
}
