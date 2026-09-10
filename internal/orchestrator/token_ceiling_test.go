package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
)

// tokenStoreResponse is one scripted reply for [fakeTokenStore].
type tokenStoreResponse struct {
	usage persistence.IssueTokenUsage
	err   error
}

// fakeTokenStore is a scriptable [issueTokenStore] double. Each call to
// TokenUsageByIssue consumes the next response in the script; the final
// response repeats once the script is exhausted. calls records every
// issueID the double was invoked with, in order.
type fakeTokenStore struct {
	responses []tokenStoreResponse
	calls     []string
}

func (f *fakeTokenStore) TokenUsageByIssue(_ context.Context, issueID string) (persistence.IssueTokenUsage, error) {
	f.calls = append(f.calls, issueID)
	idx := len(f.calls) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	if idx < 0 {
		return persistence.IssueTokenUsage{}, nil
	}
	return f.responses[idx].usage, f.responses[idx].err
}

// failingIssueTokenStore fails the test the instant TokenUsageByIssue is
// called, so a test asserting "no store read" cannot pass by accident.
type failingIssueTokenStore struct {
	t *testing.T
}

func (f *failingIssueTokenStore) TokenUsageByIssue(_ context.Context, issueID string) (persistence.IssueTokenUsage, error) {
	f.t.Helper()
	f.t.Fatalf("TokenUsageByIssue(%q) called, want no store read", issueID)
	return persistence.IssueTokenUsage{}, nil
}

// textLogger returns a *slog.Logger that renders to a [lockedBuf] in
// text format, so a test can assert on rendered message text and
// attributes.
func textLogger() (*lockedBuf, *slog.Logger) {
	lb := &lockedBuf{}
	return lb, slog.New(slog.NewTextHandler(lb, nil))
}

func TestFreezeIssueTokenBaseline(t *testing.T) {
	t.Parallel()

	t.Run("arrival reports no figure emits the cannot-bound warning and still reads the baseline", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-1"] = &RunningEntry{
			Identifier:   "ISS-1-ident",
			AgentKind:    "mock",
			UsageArrival: registry.UsageArrivalNone,
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 42}}}}

		freezeIssueTokenBaseline(context.Background(), state, "ISS-1", store, logger)

		if got := strings.Count(lb.String(), "token ceiling cannot bound this run"); got != 1 {
			t.Errorf(`freezeIssueTokenBaseline log contains %d "token ceiling cannot bound this run" records, want 1`, got)
		}
		if len(store.calls) != 1 || store.calls[0] != "ISS-1" {
			t.Errorf("TokenUsageByIssue calls = %v, want exactly one call for %q", store.calls, "ISS-1")
		}
		if got := state.Running["ISS-1"].IssueTokensCompleted; got != 42 {
			t.Errorf("entry.IssueTokensCompleted = %d, want 42 (baseline read still attempted)", got)
		}
	})

	t.Run("baseline read failure proceeds and warns, leaving IssueTokensCompleted at zero", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-2"] = &RunningEntry{
			Identifier:   "ISS-2-ident",
			AgentKind:    "mock",
			UsageArrival: registry.UsageArrivalIncremental,
		}
		wantErr := errors.New("boom")
		store := &fakeTokenStore{responses: []tokenStoreResponse{{err: wantErr}}}

		freezeIssueTokenBaseline(context.Background(), state, "ISS-2", store, logger)

		if got := strings.Count(lb.String(), "prior token spend unknown, token ceiling bounds this session only"); got != 1 {
			t.Errorf(`freezeIssueTokenBaseline log contains %d "prior token spend unknown..." records, want 1`, got)
		}
		if strings.Contains(lb.String(), "token ceiling cannot bound this run") {
			t.Error("freezeIssueTokenBaseline logged the cannot-bound warning for an arrival that reports a figure")
		}
		if got := state.Running["ISS-2"].IssueTokensCompleted; got != 0 {
			t.Errorf("entry.IssueTokensCompleted = %d, want 0 after a failed baseline read", got)
		}
	})

	t.Run("MaxTokens zero performs no store read and emits no record", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		state := NewState(5000, 4, 0, nil, AgentTotals{})
		state.Running["ISS-3"] = &RunningEntry{
			Identifier:   "ISS-3-ident",
			AgentKind:    "mock",
			UsageArrival: registry.UsageArrivalNone,
		}
		store := &failingIssueTokenStore{t: t}

		freezeIssueTokenBaseline(context.Background(), state, "ISS-3", store, logger)

		if got := state.Running["ISS-3"].IssueTokensCompleted; got != 0 {
			t.Errorf("entry.IssueTokensCompleted = %d, want 0 (untouched when MaxTokens is 0)", got)
		}
	})

	t.Run("poll lane and retry lane freeze the same baseline given the same state and store", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-POLL"] = &RunningEntry{Identifier: "ISS-POLL-ident", AgentKind: "mock", UsageArrival: registry.UsageArrivalIncremental}
		state.Running["ISS-RETRY"] = &RunningEntry{Identifier: "ISS-RETRY-ident", AgentKind: "mock", UsageArrival: registry.UsageArrivalIncremental}
		store := &fakeTokenStore{responses: []tokenStoreResponse{
			{usage: persistence.IssueTokenUsage{TotalTokens: 777}},
			{usage: persistence.IssueTokenUsage{TotalTokens: 777}},
		}}

		// The poll-lane dispatch and the retry-lane dispatch both call
		// freezeIssueTokenBaseline directly, with no lane-specific
		// branch inside it; calling it twice with equivalent inputs
		// must produce equivalent baselines.
		freezeIssueTokenBaseline(context.Background(), state, "ISS-POLL", store, logger)
		freezeIssueTokenBaseline(context.Background(), state, "ISS-RETRY", store, logger)

		pollGot := state.Running["ISS-POLL"].IssueTokensCompleted
		retryGot := state.Running["ISS-RETRY"].IssueTokensCompleted
		if pollGot != 777 || retryGot != 777 || pollGot != retryGot {
			t.Errorf("poll-lane IssueTokensCompleted = %d, retry-lane = %d, want both 777", pollGot, retryGot)
		}
	})
}

func TestFreezeIssueTokenBaselineRecordsOnePerDispatch(t *testing.T) {
	t.Parallel()

	// A run whose arrival reports no figure is unbounded whatever the
	// baseline read did, so the two records must never both fire: the
	// second one would tell the operator the ceiling bounds a session
	// the first one just said it cannot bound.
	lb, logger := textLogger()
	state := NewState(5000, 4, 100, nil, AgentTotals{})
	state.Running["ISS-BOTH"] = &RunningEntry{
		Identifier:   "ISS-BOTH-ident",
		AgentKind:    "mock",
		UsageArrival: registry.UsageArrivalNone,
	}
	readErr := errors.New("db unavailable")
	store := &fakeTokenStore{responses: []tokenStoreResponse{{err: readErr}}}

	freezeIssueTokenBaseline(context.Background(), state, "ISS-BOTH", store, logger)

	if strings.Contains(lb.String(), "prior token spend unknown") {
		t.Errorf("logged that the ceiling bounds this session for a run it had just called unboundable:\n%s", lb.String())
	}
	line := lineWith(t, lb.String(), "token ceiling cannot bound this run")
	if !strings.Contains(line, "error=") {
		t.Errorf("the one record does not carry the read failure it absorbed:\n%s", line)
	}
	if len(store.calls) != 1 {
		t.Errorf("TokenUsageByIssue calls = %v, want exactly one", store.calls)
	}
}

func TestEnforceInFlightTokenCeiling(t *testing.T) {
	t.Parallel()

	// driveEvent mirrors the orchestrator's own agentEventCh case:
	// HandleAgentEvent applies the usage delta before
	// enforceInFlightTokenCeiling evaluates it, and the latter must
	// never be exercised out of that order.
	driveEvent := func(state *State, issueID string, usage domain.TokenUsage, store issueTokenStore, metrics domain.Metrics, log *slog.Logger) {
		event := domain.AgentEvent{Type: domain.EventTokenUsage, Usage: usage}
		HandleAgentEvent(state, issueID, event, log, metrics)
		enforceInFlightTokenCeiling(context.Background(), state, issueID, event, store, metrics, log)
	}

	t.Run("reaching the ceiling cancels the run while in flight", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-STOP"] = &RunningEntry{
			Identifier: "ISS-STOP-ident",
			CancelFunc: func() { cancelCalls++ },
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 0}}}}

		driveEvent(state, "ISS-STOP", domain.TokenUsage{TotalTokens: 60}, store, spy, logger)
		if state.Running["ISS-STOP"].TokenCeilingStopped {
			t.Fatal("run stopped below the ceiling (60 < 100)")
		}

		driveEvent(state, "ISS-STOP", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)

		entry := state.Running["ISS-STOP"]
		if !entry.TokenCeilingStopped {
			t.Fatal("entry.TokenCeilingStopped = false, want true once the ceiling is reached")
		}
		if cancelCalls != 1 {
			t.Errorf("entry.CancelFunc called %d times, want 1", cancelCalls)
		}
		if got := strings.Count(lb.String(), "run stopped by token ceiling"); got != 1 {
			t.Errorf(`log contains %d "run stopped by token ceiling" records, want 1`, got)
		}
		if len(spy.runsStoppedByBudget) != 1 || spy.runsStoppedByBudget[0] != budgetReasonToken {
			t.Errorf("IncRunsStoppedByBudget calls = %v, want exactly one %q", spy.runsStoppedByBudget, budgetReasonToken)
		}

		line := lineWith(t, lb.String(), "run stopped by token ceiling")
		for _, want := range []string{"sum_source=" + tokenSumConfirmedRead, "unmeasured_sessions="} {
			if !strings.Contains(line, want) {
				t.Errorf("stop record taken on a confirming read is missing %s:\n%s", want, line)
			}
		}
	})

	t.Run("events after the stop produce no additional record or counter increment", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-LATCH"] = &RunningEntry{
			Identifier: "ISS-LATCH-ident",
			CancelFunc: func() { cancelCalls++ },
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 0}}}}

		driveEvent(state, "ISS-LATCH", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)
		if !state.Running["ISS-LATCH"].TokenCeilingStopped {
			t.Fatal("setup failed: run did not stop on the first over-ceiling event")
		}
		callsAfterStop := len(store.calls)

		driveEvent(state, "ISS-LATCH", domain.TokenUsage{TotalTokens: 300}, store, spy, logger)
		driveEvent(state, "ISS-LATCH", domain.TokenUsage{TotalTokens: 450}, store, spy, logger)

		if len(store.calls) != callsAfterStop {
			t.Errorf("TokenUsageByIssue called %d more time(s) after the latch, want 0", len(store.calls)-callsAfterStop)
		}
		if cancelCalls != 1 {
			t.Errorf("entry.CancelFunc called %d times total, want 1", cancelCalls)
		}
		if got := strings.Count(lb.String(), "run stopped by token ceiling"); got != 1 {
			t.Errorf(`log contains %d "run stopped by token ceiling" records after 3 events, want 1`, got)
		}
		if len(spy.runsStoppedByBudget) != 1 {
			t.Errorf("IncRunsStoppedByBudget called %d times, want 1", len(spy.runsStoppedByBudget))
		}
	})

	t.Run("MaxTokens zero performs no store read on any event", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		state := NewState(5000, 4, 0, nil, AgentTotals{})
		state.Running["ISS-NOCEIL"] = &RunningEntry{Identifier: "ISS-NOCEIL-ident", AgentTotalTokens: 1_000_000}
		store := &failingIssueTokenStore{t: t}

		enforceInFlightTokenCeiling(context.Background(), state, "ISS-NOCEIL",
			domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 1_000_000}},
			store, &domain.NoopMetrics{}, logger)
	})

	t.Run("an event carrying no usage component performs no store read", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-NOUSAGE"] = &RunningEntry{Identifier: "ISS-NOUSAGE-ident", AgentTotalTokens: 1_000_000}
		store := &failingIssueTokenStore{t: t}

		enforceInFlightTokenCeiling(context.Background(), state, "ISS-NOUSAGE",
			domain.AgentEvent{Type: domain.EventNotification},
			store, &domain.NoopMetrics{}, logger)
	})

	t.Run("a failing confirming read lets the run continue and warns once, and a later succeeding read still evaluates", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-FAILREAD"] = &RunningEntry{
			Identifier: "ISS-FAILREAD-ident",
			CancelFunc: func() { cancelCalls++ },
			// The frozen baseline is what carries this issue over the
			// ceiling; the session's own spend stays under it, so the
			// failed read is genuinely the only evidence available.
			IssueTokensCompleted: 80,
		}
		queryErr := errors.New("db unavailable")
		store := &fakeTokenStore{responses: []tokenStoreResponse{
			{err: queryErr},
			{err: queryErr},
			{usage: persistence.IssueTokenUsage{TotalTokens: 80}},
		}}

		driveEvent(state, "ISS-FAILREAD", domain.TokenUsage{TotalTokens: 25}, store, spy, logger)
		driveEvent(state, "ISS-FAILREAD", domain.TokenUsage{TotalTokens: 26}, store, spy, logger)

		entry := state.Running["ISS-FAILREAD"]
		if entry.TokenCeilingStopped {
			t.Fatal("entry.TokenCeilingStopped = true, want false while every confirming read fails")
		}
		if got := strings.Count(lb.String(), "in-flight token ceiling check failed, run continues"); got != 1 {
			t.Errorf(`log contains %d "in-flight token ceiling check failed, run continues" records across 2 failing reads, want 1`, got)
		}
		if cancelCalls != 0 {
			t.Errorf("entry.CancelFunc called %d times, want 0 while reads keep failing", cancelCalls)
		}

		driveEvent(state, "ISS-FAILREAD", domain.TokenUsage{TotalTokens: 27}, store, spy, logger)

		if !entry.TokenCeilingStopped {
			t.Fatal("entry.TokenCeilingStopped = false, want true once the confirming read succeeds and the sum is at the ceiling")
		}
		if cancelCalls != 1 {
			t.Errorf("entry.CancelFunc called %d times after recovery, want 1", cancelCalls)
		}
	})

	t.Run("a failing read still stops a session whose own spend reached the ceiling", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-OUTAGE"] = &RunningEntry{
			Identifier: "ISS-OUTAGE-ident",
			CancelFunc: func() { cancelCalls++ },
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{err: errors.New("db unavailable")}}}

		driveEvent(state, "ISS-OUTAGE", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)

		entry := state.Running["ISS-OUTAGE"]
		if !entry.TokenCeilingStopped {
			t.Fatal("entry.TokenCeilingStopped = false; a session whose own spend reached the ceiling needs no read to prove the breach")
		}
		if cancelCalls != 1 {
			t.Errorf("entry.CancelFunc called %d times, want 1", cancelCalls)
		}
		if len(spy.runsStoppedByBudget) != 1 || spy.runsStoppedByBudget[0] != budgetReasonToken {
			t.Errorf("IncRunsStoppedByBudget calls = %v, want exactly one %q", spy.runsStoppedByBudget, budgetReasonToken)
		}
		if strings.Contains(lb.String(), "in-flight token ceiling check failed, run continues") {
			t.Error(`logged "run continues" for a run it stopped`)
		}

		line := lineWith(t, lb.String(), "run stopped by token ceiling")
		if !strings.Contains(line, "sum_source="+tokenSumSessionSpendAlone) {
			t.Errorf("stop record does not name how the sum was established:\n%s", line)
		}
		if strings.Contains(line, "unmeasured_sessions=") {
			t.Errorf("stop record reports an unmeasured count no read supplied:\n%s", line)
		}
	})

	t.Run("a confirming read below the ceiling replaces IssueTokensCompleted and leaves the run running", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 1000, nil, AgentTotals{})
		state.Running["ISS-BELOW"] = &RunningEntry{
			Identifier:           "ISS-BELOW-ident",
			IssueTokensCompleted: 900, // a stale, overestimated frozen baseline
			CancelFunc:           func() { cancelCalls++ },
		}
		// The pre-filter (stale 900 + this session's 150 = 1050) crosses
		// the ceiling, but the confirming read reports the issue's true
		// completed sum is only 800, so 800 + 150 = 950 stays under it.
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 800}}}}

		driveEvent(state, "ISS-BELOW", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)

		entry := state.Running["ISS-BELOW"]
		if entry.TokenCeilingStopped {
			t.Error("entry.TokenCeilingStopped = true, want false (950 < 1000 ceiling)")
		}
		if entry.IssueTokensCompleted != 800 {
			t.Errorf("entry.IssueTokensCompleted = %d, want 800 (replaced by the confirming read)", entry.IssueTokensCompleted)
		}
		if cancelCalls != 0 {
			t.Errorf("entry.CancelFunc called %d times, want 0", cancelCalls)
		}
	})

	t.Run("used_tokens on the stop record equals the confirming read's sum plus AgentTotalTokens", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		state := NewState(5000, 4, 50, nil, AgentTotals{})
		state.Running["ISS-MATH"] = &RunningEntry{Identifier: "ISS-MATH-ident"}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 20}}}}

		driveEvent(state, "ISS-MATH", domain.TokenUsage{TotalTokens: 80}, store, spy, logger)

		if !state.Running["ISS-MATH"].TokenCeilingStopped {
			t.Fatal("setup failed: run did not stop")
		}
		// entry.AgentTotalTokens is 80 (this session's cumulative
		// figure); the confirming read reports a completed sum of 20;
		// used_tokens on the log record must be their sum, 100.
		if !strings.Contains(lb.String(), "used_tokens=100") {
			t.Errorf("log = %q, want it to contain used_tokens=100 (confirming sum 20 + AgentTotalTokens 80)", lb.String())
		}
	})
}

// lineWith returns the single rendered log line carrying msg, failing
// the test when the record is absent or repeated.
func lineWith(t *testing.T, rendered, msg string) string {
	t.Helper()
	var found []string
	for line := range strings.SplitSeq(strings.TrimSpace(rendered), "\n") {
		if strings.Contains(line, msg) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("log contains %d records for %q, want exactly 1:\n%s", len(found), msg, rendered)
	}
	return found[0]
}

// TestTokenCeilingRecordsCarryIssueContext pins the identifying
// attributes on every record this file emits. Both entry points take an
// unscoped logger and derive the issue-scoped one themselves, so a
// record's shape cannot depend on the caller: the poll lane and the
// event loop hand over the orchestrator's base logger, and the retry
// lane hands over its own. A record naming neither the issue nor the
// session cannot be acted on in a deployment running several agents.
func TestTokenCeilingRecordsCarryIssueContext(t *testing.T) {
	t.Parallel()

	const (
		issueID    = "ISS-CTX"
		identifier = "ISS-CTX-ident"
		sessionID  = "sess-42"
	)

	newState := func(arrival registry.UsageArrival, completedTokens, sessionTokens int64) *State {
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running[issueID] = &RunningEntry{
			Identifier:           identifier,
			SessionID:            sessionID,
			AgentKind:            "mock",
			UsageArrival:         arrival,
			IssueTokensCompleted: completedTokens,
			AgentTotalTokens:     sessionTokens,
			CancelFunc:           func() {},
		}
		return state
	}
	usageEvent := domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 150}}

	tests := []struct {
		name    string
		message string
		emit    func(*slog.Logger)
	}{
		{
			name:    "dispatch under an arrival that reports no figure",
			message: "token ceiling cannot bound this run",
			emit: func(log *slog.Logger) {
				store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{}}}}
				freezeIssueTokenBaseline(context.Background(), newState(registry.UsageArrivalNone, 0, 0), issueID, store, log)
			},
		},
		{
			name:    "dispatch whose baseline read fails",
			message: "prior token spend unknown, token ceiling bounds this session only",
			emit: func(log *slog.Logger) {
				store := &fakeTokenStore{responses: []tokenStoreResponse{{err: errors.New("boom")}}}
				freezeIssueTokenBaseline(context.Background(), newState(registry.UsageArrivalIncremental, 0, 0), issueID, store, log)
			},
		},
		{
			name:    "confirming read fails over the pre-filter",
			message: "in-flight token ceiling check failed, run continues",
			emit: func(log *slog.Logger) {
				store := &fakeTokenStore{responses: []tokenStoreResponse{{err: errors.New("boom")}}}
				state := newState(registry.UsageArrivalIncremental, 80, 25)
				enforceInFlightTokenCeiling(context.Background(), state, issueID, usageEvent, store, &spyMetrics{}, log)
			},
		},
		{
			name:    "the stop itself",
			message: "run stopped by token ceiling",
			emit: func(log *slog.Logger) {
				store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 0}}}}
				state := newState(registry.UsageArrivalIncremental, 0, 150)
				enforceInFlightTokenCeiling(context.Background(), state, issueID, usageEvent, store, &spyMetrics{}, log)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lb, logger := textLogger()
			tt.emit(logger)

			line := lineWith(t, lb.String(), tt.message)
			for _, want := range []string{
				"issue_id=" + issueID,
				"issue_identifier=" + identifier,
				"session_id=" + sessionID,
			} {
				if !strings.Contains(line, want) {
					t.Errorf("record %q is missing %s:\n%s", tt.message, want, line)
				}
			}
		})
	}
}
