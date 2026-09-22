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

type tokenStoreResponse struct {
	usage persistence.IssueTokenUsage
	err   error
}

// fakeTokenStore replays responses in order; the last response repeats
// once the script is exhausted.
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

// failingIssueTokenStore fails the test if read, so a "no store read"
// assertion cannot pass by accident.
type failingIssueTokenStore struct {
	t *testing.T
}

func (f *failingIssueTokenStore) TokenUsageByIssue(_ context.Context, issueID string) (persistence.IssueTokenUsage, error) {
	f.t.Helper()
	f.t.Fatalf("TokenUsageByIssue(%q) called, want no store read", issueID)
	return persistence.IssueTokenUsage{}, nil
}

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

	// The two records must never both fire: a run whose arrival reports
	// no figure is unbounded regardless of the baseline read, so the
	// session-bounded record would contradict the cannot-bound one.
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

// driveEvent mirrors the agentEventCh case: HandleAgentEvent applies the
// usage delta before enforceInFlightTokenCeiling evaluates it.
func driveEvent(state *State, issueID string, usage domain.TokenUsage, store issueTokenStore, metrics domain.Metrics, log *slog.Logger) {
	event := domain.AgentEvent{Type: domain.EventTokenUsage, Usage: usage}
	HandleAgentEvent(state, issueID, event, log, metrics)
	enforceInFlightTokenCeiling(context.Background(), state, issueID, event, store, log)
}

func TestEnforceInFlightTokenCeiling(t *testing.T) {
	t.Parallel()

	t.Run("reaching the ceiling records a stop request and cancels the worker context, without counting or logging a stop", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-STOP"] = &RunningEntry{
			Identifier:             "ISS-STOP-ident",
			TokenCeilingCancelFunc: func() { cancelCalls++ },
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 0}}}}

		driveEvent(state, "ISS-STOP", domain.TokenUsage{TotalTokens: 60}, store, spy, logger)
		if state.Running["ISS-STOP"].TokenCeilingStopRequest != nil {
			t.Fatal("run stopped below the ceiling (60 < 100)")
		}

		driveEvent(state, "ISS-STOP", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)

		entry := state.Running["ISS-STOP"]
		request := entry.TokenCeilingStopRequest
		if request == nil {
			t.Fatal("entry.TokenCeilingStopRequest = nil, want non-nil once the ceiling is reached")
		}
		if cancelCalls != 1 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times, want 1", cancelCalls)
		}
		if request.BudgetTokens != 100 {
			t.Errorf("request.BudgetTokens = %d, want 100", request.BudgetTokens)
		}
		if request.SumSource != tokenSumConfirmedRead {
			t.Errorf("request.SumSource = %q, want %q", request.SumSource, tokenSumConfirmedRead)
		}
		if request.Usage == nil {
			t.Error("request.Usage = nil, want the confirming read")
		}
		if len(spy.runsStoppedByBudget) != 0 {
			t.Errorf("IncRunsStoppedByBudget calls = %v, want none: the in-flight lane never counts a stop", spy.runsStoppedByBudget)
		}
		if strings.Contains(lb.String(), "run stopped by token ceiling") {
			t.Errorf(`log contains "run stopped by token ceiling", want no such record from the in-flight lane:%s`, lb.String())
		}
	})

	t.Run("a stop request taken on a confirming read carries every count behind an incomplete sum", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		spy := &spyMetrics{}
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-UNACC"] = &RunningEntry{
			Identifier:             "ISS-UNACC-ident",
			TokenCeilingCancelFunc: func() {},
		}
		// Every completed session was measured, but the sum still omits
		// two unaccounted turns.
		store := &fakeTokenStore{responses: []tokenStoreResponse{
			{usage: persistence.IssueTokenUsage{TotalTokens: 40, Sessions: 1, UnaccountedTurns: 2}},
		}}

		driveEvent(state, "ISS-UNACC", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)

		request := state.Running["ISS-UNACC"].TokenCeilingStopRequest
		if request == nil {
			t.Fatal("entry.TokenCeilingStopRequest = nil, want non-nil once the ceiling is reached")
		}
		if request.Usage == nil {
			t.Fatal("request.Usage = nil, want the confirming read")
		}
		if request.Usage.UnaccountedTurns != 2 {
			t.Errorf("request.Usage.UnaccountedTurns = %d, want 2", request.Usage.UnaccountedTurns)
		}
	})

	t.Run("events after the stop request produce no additional store read or cancel call", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-LATCH"] = &RunningEntry{
			Identifier:             "ISS-LATCH-ident",
			TokenCeilingCancelFunc: func() { cancelCalls++ },
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 0}}}}

		driveEvent(state, "ISS-LATCH", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)
		firstRequest := state.Running["ISS-LATCH"].TokenCeilingStopRequest
		if firstRequest == nil {
			t.Fatal("setup failed: run did not record a stop request on the first over-ceiling event")
		}
		callsAfterStop := len(store.calls)

		driveEvent(state, "ISS-LATCH", domain.TokenUsage{TotalTokens: 300}, store, spy, logger)
		driveEvent(state, "ISS-LATCH", domain.TokenUsage{TotalTokens: 450}, store, spy, logger)

		if len(store.calls) != callsAfterStop {
			t.Errorf("TokenUsageByIssue called %d more time(s) after the latch, want 0", len(store.calls)-callsAfterStop)
		}
		if cancelCalls != 1 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times total, want 1", cancelCalls)
		}
		if state.Running["ISS-LATCH"].TokenCeilingStopRequest != firstRequest {
			t.Error("TokenCeilingStopRequest changed after the latch, want the same request the first crossing recorded")
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
			store, logger)
	})

	t.Run("an event carrying no usage component performs no store read", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-NOUSAGE"] = &RunningEntry{Identifier: "ISS-NOUSAGE-ident", AgentTotalTokens: 1_000_000}
		store := &failingIssueTokenStore{t: t}

		enforceInFlightTokenCeiling(context.Background(), state, "ISS-NOUSAGE",
			domain.AgentEvent{Type: domain.EventNotification},
			store, logger)
	})

	t.Run("a failing confirming read lets the run continue and warns once, and a later succeeding read still evaluates", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-FAILREAD"] = &RunningEntry{
			Identifier:             "ISS-FAILREAD-ident",
			TokenCeilingCancelFunc: func() { cancelCalls++ },
			// The frozen baseline, not this session's own spend, carries
			// the issue over the ceiling, so the failed read is the only
			// evidence available.
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
		if entry.TokenCeilingStopRequest != nil {
			t.Fatal("entry.TokenCeilingStopRequest = non-nil, want nil while every confirming read fails")
		}
		if got := strings.Count(lb.String(), "in-flight token ceiling check failed, run continues"); got != 1 {
			t.Errorf(`log contains %d "in-flight token ceiling check failed, run continues" records across 2 failing reads, want 1`, got)
		}
		if cancelCalls != 0 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times, want 0 while reads keep failing", cancelCalls)
		}

		driveEvent(state, "ISS-FAILREAD", domain.TokenUsage{TotalTokens: 27}, store, spy, logger)

		if entry.TokenCeilingStopRequest == nil {
			t.Fatal("entry.TokenCeilingStopRequest = nil, want non-nil once the confirming read succeeds and the sum is at the ceiling")
		}
		if cancelCalls != 1 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times after recovery, want 1", cancelCalls)
		}
	})

	t.Run("a failing read still records a stop request for a session whose own spend reached the ceiling", func(t *testing.T) {
		t.Parallel()

		lb, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-OUTAGE"] = &RunningEntry{
			Identifier:             "ISS-OUTAGE-ident",
			TokenCeilingCancelFunc: func() { cancelCalls++ },
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{err: errors.New("db unavailable")}}}

		driveEvent(state, "ISS-OUTAGE", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)

		entry := state.Running["ISS-OUTAGE"]
		request := entry.TokenCeilingStopRequest
		if request == nil {
			t.Fatal("entry.TokenCeilingStopRequest = nil; a session whose own spend reached the ceiling needs no read to prove the breach")
		}
		if cancelCalls != 1 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times, want 1", cancelCalls)
		}
		if request.SumSource != tokenSumSessionSpendAlone {
			t.Errorf("request.SumSource = %q, want %q", request.SumSource, tokenSumSessionSpendAlone)
		}
		if request.Usage != nil {
			t.Errorf("request.Usage = %v, want nil: a session-spend-alone stop supplies no read", request.Usage)
		}
		if strings.Contains(lb.String(), "in-flight token ceiling check failed, run continues") {
			t.Error(`logged "run continues" for a run whose stop was recorded`)
		}
	})

	t.Run("a confirming read below the ceiling replaces IssueTokensCompleted and leaves the run running", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 1000, nil, AgentTotals{})
		state.Running["ISS-BELOW"] = &RunningEntry{
			Identifier:             "ISS-BELOW-ident",
			IssueTokensCompleted:   900, // a stale, overestimated frozen baseline
			TokenCeilingCancelFunc: func() { cancelCalls++ },
		}
		// Pre-filter (stale 900 + 150 = 1050) crosses the ceiling, but
		// the confirming read's true 800 + 150 = 950 stays under it.
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 800}}}}

		driveEvent(state, "ISS-BELOW", domain.TokenUsage{TotalTokens: 150}, store, spy, logger)

		entry := state.Running["ISS-BELOW"]
		if entry.TokenCeilingStopRequest != nil {
			t.Error("entry.TokenCeilingStopRequest = non-nil, want nil (950 < 1000 ceiling)")
		}
		if entry.IssueTokensCompleted != 800 {
			t.Errorf("entry.IssueTokensCompleted = %d, want 800 (replaced by the confirming read)", entry.IssueTokensCompleted)
		}
		if cancelCalls != 0 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times, want 0", cancelCalls)
		}
	})

	t.Run("none arrival at the ceiling performs no store read, records no request, and cancels nothing", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		spy := &spyMetrics{}
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-NONE-CEIL"] = &RunningEntry{
			Identifier:             "ISS-NONE-CEIL-ident",
			UsageArrival:           registry.UsageArrivalNone,
			IssueTokensCompleted:   100,
			TokenCeilingCancelFunc: func() { cancelCalls++ },
		}
		store := &failingIssueTokenStore{t: t}

		enforceInFlightTokenCeiling(context.Background(), state, "ISS-NONE-CEIL",
			domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 1}},
			store, logger)

		entry := state.Running["ISS-NONE-CEIL"]
		if entry.TokenCeilingStopRequest != nil {
			t.Error("entry.TokenCeilingStopRequest = non-nil, want nil (none arrival)")
		}
		if cancelCalls != 0 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times, want 0", cancelCalls)
		}
		if len(spy.runsStoppedByBudget) != 0 {
			t.Errorf("IncRunsStoppedByBudget calls = %v, want none", spy.runsStoppedByBudget)
		}
	})

	t.Run("incremental arrival at the ceiling reads and records a stop request for the same event", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		var cancelCalls int
		state := NewState(5000, 4, 100, nil, AgentTotals{})
		state.Running["ISS-INC-CEIL"] = &RunningEntry{
			Identifier:             "ISS-INC-CEIL-ident",
			UsageArrival:           registry.UsageArrivalIncremental,
			IssueTokensCompleted:   100,
			TokenCeilingCancelFunc: func() { cancelCalls++ },
		}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 100}}}}

		enforceInFlightTokenCeiling(context.Background(), state, "ISS-INC-CEIL",
			domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 1}},
			store, logger)

		entry := state.Running["ISS-INC-CEIL"]
		if entry.TokenCeilingStopRequest == nil {
			t.Fatal("entry.TokenCeilingStopRequest = nil, want non-nil (incremental arrival at the ceiling)")
		}
		if cancelCalls != 1 {
			t.Errorf("entry.TokenCeilingCancelFunc called %d times, want 1", cancelCalls)
		}
		if len(store.calls) != 1 {
			t.Errorf("TokenUsageByIssue calls = %v, want exactly one", store.calls)
		}
	})

	t.Run("UsedTokens on the stop request equals the confirming read's sum plus AgentTotalTokens", func(t *testing.T) {
		t.Parallel()

		_, logger := textLogger()
		spy := &spyMetrics{}
		state := NewState(5000, 4, 50, nil, AgentTotals{})
		state.Running["ISS-MATH"] = &RunningEntry{Identifier: "ISS-MATH-ident"}
		store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 20}}}}

		driveEvent(state, "ISS-MATH", domain.TokenUsage{TotalTokens: 80}, store, spy, logger)

		request := state.Running["ISS-MATH"].TokenCeilingStopRequest
		if request == nil {
			t.Fatal("setup failed: run did not record a stop request")
		}
		// UsedTokens = confirming read 20 + AgentTotalTokens 80 = 100.
		if request.UsedTokens != 100 {
			t.Errorf("request.UsedTokens = %d, want 100 (confirming sum 20 + AgentTotalTokens 80)", request.UsedTokens)
		}
	})
}

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

func TestEnforceInFlightTokenCeiling_TurnEndPairSingleRequest(t *testing.T) {
	t.Parallel()

	_, logger := textLogger()
	spy := &spyMetrics{}
	var cancelCalls int
	s1 := domain.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 5}

	state := NewState(5000, 4, 100, nil, AgentTotals{})
	state.Running["ISS-TE"] = &RunningEntry{
		Identifier:             "ISS-TE-ident",
		TokenCeilingCancelFunc: func() { cancelCalls++ },
	}
	store := &fakeTokenStore{responses: []tokenStoreResponse{{usage: persistence.IssueTokenUsage{TotalTokens: 0}}}}

	usageEvent := domain.AgentEvent{Type: domain.EventTokenUsage, Usage: s1}
	HandleAgentEvent(state, "ISS-TE", usageEvent, logger, spy)
	enforceInFlightTokenCeiling(context.Background(), state, "ISS-TE", usageEvent, store, logger)

	entry := state.Running["ISS-TE"]
	firstRequest := entry.TokenCeilingStopRequest
	if firstRequest == nil {
		t.Fatal("entry.TokenCeilingStopRequest = nil, want non-nil after the token_usage event alone (S1.TotalTokens exceeds the ceiling)")
	}
	if cancelCalls != 1 {
		t.Fatalf("entry.TokenCeilingCancelFunc called %d times after the token_usage event, want 1", cancelCalls)
	}

	terminalEvent := domain.AgentEvent{Type: domain.EventTurnCompleted, Usage: s1}
	HandleAgentEvent(state, "ISS-TE", terminalEvent, logger, spy)
	enforceInFlightTokenCeiling(context.Background(), state, "ISS-TE", terminalEvent, store, logger)

	if cancelCalls != 1 {
		t.Errorf("entry.TokenCeilingCancelFunc called %d times total, want 1 (the terminal event produces no second request)", cancelCalls)
	}
	if entry.TokenCeilingStopRequest != firstRequest {
		t.Error("TokenCeilingStopRequest changed on the terminal event, want the same request the first event recorded")
	}
}

// TestTokenCeilingRecordsCarryIssueContext pins the identifying
// attributes on the in-flight lane's own records: every entry point
// takes an unscoped logger and scopes it itself, and a record naming
// neither issue nor session cannot be acted on when several agents run
// at once.
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
			Identifier:             identifier,
			SessionID:              sessionID,
			AgentKind:              "mock",
			UsageArrival:           arrival,
			IssueTokensCompleted:   completedTokens,
			AgentTotalTokens:       sessionTokens,
			TokenCeilingCancelFunc: func() {},
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
				enforceInFlightTokenCeiling(context.Background(), state, issueID, usageEvent, store, log)
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

func TestHandleWorkerExit_CeilingMeasuredNothing(t *testing.T) {
	t.Parallel()

	const message = "run reported no token usage, token ceiling could not bound it"

	tests := []struct {
		name             string
		maxTokens        int
		arrival          registry.UsageArrival
		entryMeasured    bool
		resultMeasured   bool
		wantRecord       bool
		wantUsageArrival string
	}{
		{
			name:             "declared arrival, ceiling set, nothing recorded",
			maxTokens:        500_000,
			arrival:          registry.UsageArrivalTurnEnd,
			wantRecord:       true,
			wantUsageArrival: "turn_end",
		},
		{
			name:       "no ceiling configured",
			maxTokens:  0,
			arrival:    registry.UsageArrivalTurnEnd,
			wantRecord: false,
		},
		{
			name:          "figures recorded on the entry",
			maxTokens:     500_000,
			arrival:       registry.UsageArrivalTurnEnd,
			entryMeasured: true,
			wantRecord:    false,
		},
		{
			name:           "figures recorded only on the worker result",
			maxTokens:      500_000,
			arrival:        registry.UsageArrivalTurnEnd,
			resultMeasured: true,
			wantRecord:     false,
		},
		{
			name:       "arrival reports no figure, already reported at dispatch",
			maxTokens:  500_000,
			arrival:    registry.UsageArrivalNone,
			wantRecord: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "CEIL-EXIT"
			lb, logger := textLogger()
			state := exitState(t, issueID, nil)
			state.MaxTokens = tt.maxTokens
			entry := state.Running[issueID]
			entry.AgentKind = "transport"
			entry.SessionID = "sess-ceil"
			entry.UsageArrival = tt.arrival
			entry.UsageMeasured = tt.entryMeasured

			params := defaultExitParams(t, &mockExitStore{})
			params.Logger = logger

			HandleWorkerExit(state, WorkerResult{
				IssueID:       issueID,
				Identifier:    issueID + "-ident",
				ExitKind:      WorkerExitNormal,
				AgentAdapter:  "transport",
				UsageMeasured: tt.resultMeasured,
			}, params)

			rendered := lb.String()
			if !tt.wantRecord {
				if strings.Contains(rendered, message) {
					t.Errorf("log contains %q, want no such record:\n%s", message, rendered)
				}
				return
			}

			line := lineWith(t, rendered, message)
			for _, want := range []string{
				"issue_id=" + issueID,
				"issue_identifier=" + issueID + "-ident",
				"session_id=sess-ceil",
				"agent_kind=transport",
				"usage_arrival=" + tt.wantUsageArrival,
				"budget_tokens=500000",
				"level=WARN",
			} {
				if !strings.Contains(line, want) {
					t.Errorf("record %q is missing %s:\n%s", message, want, line)
				}
			}
		})
	}
}
