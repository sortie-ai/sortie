package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/registry"
)

type fakeSpendStore struct {
	byStart map[int64]persistence.IssueTokenUsage
	err     error
	calls   int
}

func (f *fakeSpendStore) UsageCompletedBetween(_ context.Context, since, _ time.Time) (persistence.IssueTokenUsage, error) {
	f.calls++
	return f.byStart[since.Unix()], f.err
}

func utcDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func TestSpendWindowBounds(t *testing.T) {
	t.Parallel()

	utcPlus3 := time.FixedZone("UTC+3", 3*60*60)
	utcPlus2 := time.FixedZone("UTC+2", 2*60*60)
	leapNoon := time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC)
	newYearsEve := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	tests := []struct {
		name      string
		window    SpendWindow
		now       time.Time
		wantStart time.Time
		wantEnd   time.Time
	}{
		{"daily one nanosecond before midnight", SpendWindowDaily, utcDate(2026, 3, 14).Add(-time.Nanosecond), utcDate(2026, 3, 13), utcDate(2026, 3, 14)},
		{"daily at midnight", SpendWindowDaily, utcDate(2026, 3, 14), utcDate(2026, 3, 14), utcDate(2026, 3, 15)},
		{"daily reads an offset instant in UTC", SpendWindowDaily, time.Date(2026, 3, 14, 1, 30, 0, 0, utcPlus3), utcDate(2026, 3, 13), utcDate(2026, 3, 14)},
		{"daily on a leap day", SpendWindowDaily, leapNoon, utcDate(2028, 2, 29), utcDate(2028, 3, 1)},
		{"daily on 31 December", SpendWindowDaily, newYearsEve, utcDate(2026, 12, 31), utcDate(2027, 1, 1)},
		{"weekly on the last nanosecond of a Sunday across a month end", SpendWindowWeekly, utcDate(2026, 11, 2).Add(-time.Nanosecond), utcDate(2026, 10, 26), utcDate(2026, 11, 2)},
		{"weekly at Monday midnight", SpendWindowWeekly, utcDate(2026, 11, 2), utcDate(2026, 11, 2), utcDate(2026, 11, 9)},
		{"weekly reads Monday locally as Sunday in UTC", SpendWindowWeekly, time.Date(2026, 11, 2, 1, 0, 0, 0, utcPlus2), utcDate(2026, 10, 26), utcDate(2026, 11, 2)},
		{"weekly on 31 December", SpendWindowWeekly, time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), utcDate(2026, 12, 28), utcDate(2027, 1, 4)},
		{"weekly on 1 January in ISO week 53", SpendWindowWeekly, utcDate(2027, 1, 1), utcDate(2026, 12, 28), utcDate(2027, 1, 4)},
		{"monthly one nanosecond before the first", SpendWindowMonthly, utcDate(2026, 11, 1).Add(-time.Nanosecond), utcDate(2026, 10, 1), utcDate(2026, 11, 1)},
		{"monthly on the first", SpendWindowMonthly, utcDate(2026, 11, 1), utcDate(2026, 11, 1), utcDate(2026, 12, 1)},
		{"monthly reads an offset instant in UTC", SpendWindowMonthly, time.Date(2026, 11, 1, 1, 0, 0, 0, utcPlus3), utcDate(2026, 10, 1), utcDate(2026, 11, 1)},
		{"monthly on a leap day", SpendWindowMonthly, leapNoon, utcDate(2028, 2, 1), utcDate(2028, 3, 1)},
		{"monthly on 31 December", SpendWindowMonthly, newYearsEve, utcDate(2026, 12, 1), utcDate(2027, 1, 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			start, end := spendWindowBounds(tt.window, tt.now)

			if !start.Equal(tt.wantStart) || !end.Equal(tt.wantEnd) {
				t.Errorf("spendWindowBounds(%q, %v) = [%v, %v), want [%v, %v)", tt.window, tt.now, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestEvaluateSpendCeiling(t *testing.T) {
	t.Parallel()

	defaultNow := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	floatSum := 0.7
	floatSum += 0.1
	recorded := func(usd float64) persistence.IssueTokenUsage {
		return persistence.IssueTokenUsage{RecordedCostUSD: usd, PricedSessions: 1, Sessions: 1}
	}
	live := func(inputTokens int64, measured bool) *RunningEntry {
		return &RunningEntry{AgentKind: "k", UsageArrival: registry.UsageArrivalIncremental, CarriesTokenFigure: true, UsageMeasured: measured, AgentInputTokens: inputTokens, AgentTotalTokens: inputTokens}
	}

	tests := []struct {
		name         string
		now          time.Time
		window       SpendWindow
		daily        float64
		weekly       float64
		monthly      float64
		rows         persistence.IssueTokenUsage
		running      *RunningEntry
		then         func(*spendCeilingParams)
		wantSkip     SkipReason
		wantUsed     float64
		wantComplete bool
	}{
		{name: "recorded costs equal to the ceiling hold", daily: 5, rows: recorded(5), wantSkip: SkipDailySpendCeiling, wantUsed: 5, wantComplete: true},
		{name: "float sum of 0.7 and 0.1 holds against 0.8", daily: 0.8, rows: recorded(floatSum), wantSkip: SkipDailySpendCeiling, wantUsed: floatSum, wantComplete: true},
		{name: "one cent below the ceiling does not hold", daily: 5, rows: recorded(4.99), wantUsed: 4.99, wantComplete: true},
		{name: "running estimate carries the window to the ceiling", daily: 5.5, rows: recorded(3), running: live(250_000, true), wantSkip: SkipDailySpendCeiling, wantUsed: 5.5, wantComplete: true},
		{name: "raising the ceiling above the spend releases", daily: 5, rows: recorded(5), then: func(p *spendCeilingParams) { p.Ceilings[SpendWindowDaily] = 6 }, wantUsed: 5, wantComplete: true},
		{name: "setting the ceiling to zero releases", daily: 5, rows: recorded(5), then: func(p *spendCeilingParams) { p.Ceilings[SpendWindowDaily] = 0 }},
		{name: "a now in the next window releases", daily: 5, rows: recorded(5), then: func(p *spendCeilingParams) { p.Now = p.Now.Add(24 * time.Hour) }, wantComplete: true},
		{name: "a failed read keeps the last result", daily: 5, rows: recorded(5), then: func(p *spendCeilingParams) { p.Store.(*fakeSpendStore).err = errors.New("db down") }, wantSkip: SkipDailySpendCeiling, wantUsed: 5, wantComplete: true},
		{name: "an unpriced row leaves the spend unchanged and incomplete", daily: 5, rows: persistence.IssueTokenUsage{RecordedCostUSD: 3, PricedSessions: 1, Sessions: 2, UnpricedSessions: 1}, wantUsed: 3},
		{name: "an unmeasured row leaves the spend unchanged and incomplete", daily: 5, rows: persistence.IssueTokenUsage{RecordedCostUSD: 3, PricedSessions: 1, Sessions: 2, UnmeasuredSessions: 1}, wantUsed: 3},
		{name: "a running session that reported nothing keeps the spend complete", daily: 5, rows: recorded(3), running: live(0, false), wantUsed: 3, wantComplete: true},
		{name: "a running session that carries no token figure leaves the spend incomplete", daily: 5, rows: recorded(3), running: &RunningEntry{AgentKind: "k", UsageArrival: registry.UsageArrivalTurnEnd}, wantUsed: 3},
		{name: "no setting reads nothing", rows: recorded(5)},
		{name: "both windows holding return the monthly reason", window: SpendWindowMonthly, daily: 5, monthly: 5, rows: recorded(5), wantSkip: SkipMonthlySpendCeiling, wantUsed: 5, wantComplete: true},
		{name: "a weekly ceiling at the week's spend holds", window: SpendWindowWeekly, weekly: 5, rows: recorded(5), wantSkip: SkipWeeklySpendCeiling, wantUsed: 5, wantComplete: true},
		{name: "Monday midnight releases the weekly hold", now: time.Date(2026, 11, 1, 23, 59, 59, 999_999_999, time.UTC), window: SpendWindowWeekly, weekly: 5, rows: recorded(5), then: func(p *spendCeilingParams) { p.Now = utcDate(2026, 11, 2) }, wantComplete: true},
		{name: "daily and weekly resetting together return the weekly reason", now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), window: SpendWindowWeekly, daily: 5, weekly: 5, rows: recorded(5), wantSkip: SkipWeeklySpendCeiling, wantUsed: 5, wantComplete: true},
		{name: "weekly resetting before monthly returns the weekly reason", now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), window: SpendWindowWeekly, weekly: 5, monthly: 5, rows: recorded(5), wantSkip: SkipWeeklySpendCeiling, wantUsed: 5, wantComplete: true},
		{name: "monthly resetting after weekly returns the monthly reason", now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), window: SpendWindowMonthly, weekly: 5, monthly: 5, rows: recorded(5), wantSkip: SkipMonthlySpendCeiling, wantUsed: 5, wantComplete: true},
		{name: "all three resetting together return the monthly reason", now: time.Date(2027, 1, 31, 12, 0, 0, 0, time.UTC), window: SpendWindowMonthly, daily: 5, weekly: 5, monthly: 5, rows: recorded(5), wantSkip: SkipMonthlySpendCeiling, wantUsed: 5, wantComplete: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now, window := tt.now, tt.window
			if now.IsZero() {
				now = defaultNow
			}
			if window == "" {
				window = SpendWindowDaily
			}
			ceilings := map[SpendWindow]float64{SpendWindowDaily: tt.daily, SpendWindowWeekly: tt.weekly, SpendWindowMonthly: tt.monthly}
			state := NewState(1000, 1, 0, nil, AgentTotals{})
			state.Rates = fixtureRates(t, 10)
			if tt.running != nil {
				state.Running["R-1"] = tt.running
			}
			store := &fakeSpendStore{byStart: map[int64]persistence.IssueTokenUsage{}}
			for w, ceiling := range ceilings {
				if start, _ := spendWindowBounds(w, now); ceiling > 0 {
					store.byStart[start.Unix()] = tt.rows
				}
			}
			params := spendCeilingParams{Store: store, Ceilings: ceilings, Rates: state.Rates, Now: now, Logger: discardLogger()}

			got := evaluateSpendCeiling(state, params)
			if tt.then != nil {
				tt.then(&params)
				got = evaluateSpendCeiling(state, params)
			}

			status, _ := findSpendStatus(state.SpendCeilings, window)
			wantHolding := tt.wantSkip != SkipNone
			if tt.daily == 0 && tt.weekly == 0 && tt.monthly == 0 && (store.calls != 0 || state.SpendCeilings != nil) {
				t.Errorf("evaluateSpendCeiling() with no setting = %d reads, %v, want 0 reads, nil", store.calls, state.SpendCeilings)
			}
			if got != tt.wantSkip || status.UsedUSD != tt.wantUsed || status.UsedComplete != tt.wantComplete || status.Holding != wantHolding {
				t.Errorf("evaluateSpendCeiling() = %q with %s (UsedUSD, UsedComplete, Holding) = (%v, %v, %v), want %q with (%v, %v, %v)",
					got, window, status.UsedUSD, status.UsedComplete, status.Holding, tt.wantSkip, tt.wantUsed, tt.wantComplete, wantHolding)
			}
		})
	}
}

func TestSpendWindowDefs_EveryWindowReadsItsOwnSetting(t *testing.T) {
	for _, def := range spendWindowDefs {
		if def.setting != "agent.max_"+string(def.window)+"_cost_usd" || def.reason != SkipReason(def.window+"_spend_ceiling") || def.ceiling == nil || def.bounds == nil {
			t.Errorf("spendWindowDefs row %q = (%q, %q, ceiling set %t, bounds set %t), want its setting and reason named after the window, with both functions",
				def.window, def.setting, def.reason, def.ceiling != nil, def.bounds != nil)
		}
		key := strings.TrimPrefix(def.setting, "agent.")
		envName := "SORTIE_" + strings.ToUpper(strings.ReplaceAll(def.setting, ".", "_"))

		for _, source := range []string{"front matter", "environment"} {
			t.Run(string(def.window)+" via "+source, func(t *testing.T) {
				raw := map[string]any{}
				if source == "front matter" {
					raw["agent"] = map[string]any{key: 7}
				} else {
					t.Setenv(envName, "7")
				}

				cfg, err := config.NewServiceConfig(raw)
				if err != nil {
					t.Fatalf("NewServiceConfig() error = %v", err)
				}

				for window, got := range spendCeilingsOf(cfg.Agent) {
					want := 0.0
					if window == def.window {
						want = 7
					}
					if got != want {
						t.Errorf("spendCeilingsOf()[%q] with only %s set via %s = %v, want %v", window, def.setting, source, got, want)
					}
				}
				for _, w := range config.ValidateFrontMatter(raw, cfg) {
					if w.Check == "unknown_sub_key" || w.Check == "type_mismatch" {
						t.Errorf("ValidateFrontMatter() warning = %+v, want none for %s", w, def.setting)
					}
				}
			})
		}
	}
}

func TestEvaluateSpendCeiling_IndependentOfRatesAndRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "sortie.db")
	openStore := func() *persistence.Store {
		store, err := persistence.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("persistence.Open(%q): %v", path, err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatalf("store.Migrate: %v", err)
		}
		return store
	}
	evaluate := func(store spendStore, rates *pricing.Rates) SpendCeilingStatus {
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		evaluateSpendCeiling(state, spendCeilingParams{Store: store, Ceilings: map[SpendWindow]float64{SpendWindowDaily: 3}, Rates: rates, Now: now, Logger: discardLogger()})
		if len(state.SpendCeilings) != 1 {
			t.Fatalf("evaluateSpendCeiling() left %d windows, want 1", len(state.SpendCeilings))
		}
		return state.SpendCeilings[0]
	}
	first := openStore()
	_, err := first.AppendRunHistory(context.Background(), persistence.RunHistory{
		IssueID: "I-1", Identifier: "I-1", Attempt: 1, AgentAdapter: "k", Workspace: "/tmp/ws/I-1",
		StartedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), CompletedAt: now.Add(-time.Hour).Format(time.RFC3339),
		Status: "succeeded", TotalTokens: 100, TokensMeasured: true, EstimatedCostUSD: new(3.0), CostStatus: "estimated",
	})
	if err != nil {
		t.Fatalf("AppendRunHistory: %v", err)
	}
	want := evaluate(first, fixtureRates(t, 10))
	if want.UsedUSD != 3 || !want.UsedComplete || !want.Holding {
		t.Fatalf("evaluateSpendCeiling() baseline (UsedUSD, UsedComplete, Holding) = (%v, %v, %v), want (3, true, true)", want.UsedUSD, want.UsedComplete, want.Holding)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	reopened := openStore()

	for name, got := range map[string]SpendCeilingStatus{
		"an edited rate table": evaluate(reopened, fixtureRates(t, 99)),
		"the reopened store":   evaluate(reopened, fixtureRates(t, 10)),
	} {
		if got != want {
			t.Errorf("evaluateSpendCeiling() after %s = %+v, want %+v", name, got, want)
		}
	}
}

func TestHandleTick_SpendCeilingHoldsOnlyWhatIsOtherwiseStartable(t *testing.T) {
	t.Parallel()

	cfg := runConfig(t, map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}})
	cfg.Agent.MaxDailyCostUSD = 5
	spy := &spyMetrics{}
	h := newRunHarness(t, cfg, "kind-a")
	h.metrics = spy
	h.store.windowUsage = persistence.IssueTokenUsage{RecordedCostUSD: 5, PricedSessions: 1, Sessions: 1}
	blocked := candidate("id-2", "A-2")
	blocked.BlockedBy = []domain.BlockerRef{{ID: "b", State: "To Do"}}
	h.tracker.candidates = []domain.Issue{candidate("id-1", "A-1"), blocked}
	h.build()

	h.o.handleTick(context.Background())

	if started := h.started(); len(started) != 0 || len(h.state.Running) != 0 {
		t.Errorf("sessions, Running after a tick under the hold = %v, %v, want none", started, h.state.Running)
	}
	spy.mu.Lock()
	holds := slices.Sorted(slices.Values(spy.candidateHolds))
	spy.mu.Unlock()
	if want := []string{string(SkipBlockedBy), string(SkipDailySpendCeiling)}; !slices.Equal(holds, want) {
		t.Errorf("IncCandidateHolds reasons = %v, want %v", holds, want)
	}

	cfg.Agent.MaxDailyCostUSD = 10
	h.wm.setConfig(cfg)
	h.o.handleTick(context.Background())

	if started := h.waitForSessions(1); len(started["kind-a"]) != 1 {
		t.Errorf("sessions started after the ceiling was raised = %v, want the unblocked candidate dispatched", started)
	}
}

func TestHandleRetryTimer_ReleaseHeld(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		release      bool
		wantReleased bool
		wantKept     bool
	}{
		{"flag off re-arms the retry", false, false, true},
		{"flag on releases the claim", true, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "ISS-SPEND"
			state := frozenRetryState(id, RetryEntry{Attempt: 3, Error: "agent exited"})
			store := &mockRetryStore{windowUsage: persistence.IssueTokenUsage{RecordedCostUSD: 5, PricedSessions: 1, Sessions: 1}}
			params := defaultRetryParams(t, store, &mockRetryTracker{fetchedIssue: candidateIssue(id, id, "To Do")})
			params.SpendCeilings = map[SpendWindow]float64{SpendWindowDaily: 5}
			params.PollIntervalMS = 60_000
			params.ReleaseHeld = tt.release
			t.Cleanup(func() { stopRetryTimer(state, id) })

			got := HandleRetryTimer(state, id, params)

			_, claimed := state.Claimed[id]
			_, queued := state.RetryAttempts[id]
			if got != tt.wantReleased || claimed != tt.wantKept || queued != tt.wantKept {
				t.Errorf("HandleRetryTimer(ReleaseHeld=%v) = %v with claim, queued retry = %v, %v, want %v with %v, %v",
					tt.release, got, claimed, queued, tt.wantReleased, tt.wantKept, tt.wantKept)
			}
			if len(store.savedEntries) != 0 || len(store.deletedIssueID) != 0 {
				t.Errorf("persisted retry rows saved, deleted = %d, %v, want none touched", len(store.savedEntries), store.deletedIssueID)
			}
		})
	}
}
