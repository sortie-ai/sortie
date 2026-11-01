package orchestrator

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
	"github.com/sortie-ai/sortie/internal/waittest"
)

type flakyBlockerResolver struct {
	reads       atomic.Int32
	onFirstRead func()
}

func (r *flakyBlockerResolver) NeedsRead(domain.Issue) bool { return true }

func (r *flakyBlockerResolver) Resolve(_ context.Context, issue domain.Issue) (domain.Issue, error) {
	if r.reads.Add(1) == 1 {
		r.onFirstRead()
		return issue, &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "blocker read failed"}
	}
	return issue, nil
}

func newOnceHarness(t *testing.T, tune func(*config.ServiceConfig)) *runHarness {
	t.Helper()
	cfg := runConfig(t, map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}})
	cfg.Polling.IntervalMS = 20
	if tune != nil {
		tune(&cfg)
	}
	h := newRunHarness(t, cfg, "kind-a")
	h.build()
	h.o.once = newOnceState()
	return h
}

func runUntilSettled(t *testing.T, h *runHarness) OnceResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.o.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("log: %s", h.logs.String())
		}
	})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waittest.Receive(t, done, "Run return")
	return h.o.OnceResult()
}

func startedWorkspaces(h *runHarness) []string {
	var names []string
	for _, sessions := range h.started() {
		for _, params := range sessions {
			names = append(names, filepath.Base(params.WorkspacePath))
		}
	}
	slices.Sort(names)
	return names
}

func failFirstAttempt(h *runHarness, issue domain.Issue, afterFailure func()) {
	h.tracker.issues[issue.ID] = issue
	var turns atomic.Int32
	h.recorders["kind-a"].adapter.runTurnFn = func(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
		if turns.Add(1) == 1 {
			if afterFailure != nil {
				afterFailure()
			}
			return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnFailed}
		}
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
	}
}

func setWindowUsage(h *runHarness, usd float64) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.windowUsage = storage.IssueTokenUsage{RecordedCostUSD: usd, PricedSessions: 1, Sessions: 1}
}

func TestOnce_RunReturnsWhenTheWorkSettles(t *testing.T) {
	t.Parallel()

	first, second, late := candidate("id-1", "A-1"), candidate("id-2", "A-2"), candidate("id-3", "A-3")
	withCeiling := func(cfg *config.ServiceConfig) { cfg.Agent.MaxDailyCostUSD = 5 }
	tests := []struct {
		name          string
		tune          func(*config.ServiceConfig)
		candidates    []domain.Issue
		setup         func(*runHarness)
		wantStarted   []string
		want          OnceResult
		wantOutcomes  []OnceOutcome
		wantRetryKept bool
	}{
		{name: "an empty poll"},
		{name: "one succeeded issue", candidates: []domain.Issue{first}, wantStarted: []string{"A-1"}, want: OnceResult{Issues: 1}},
		{
			name:         "a failed attempt whose retry succeeds",
			candidates:   []domain.Issue{first},
			setup:        func(h *runHarness) { failFirstAttempt(h, first, nil) },
			wantStarted:  []string{"A-1", "A-1"},
			want:         OnceResult{Issues: 1},
			wantOutcomes: []OnceOutcome{{Identifier: "A-1", Outcome: RunOutcomeSucceeded}},
		},
		{
			name:        "two candidates under one slot",
			tune:        func(cfg *config.ServiceConfig) { cfg.Agent.MaxConcurrentAgents = 1 },
			candidates:  []domain.Issue{first, second},
			wantStarted: []string{"A-1", "A-2"},
			want:        OnceResult{Issues: 2},
		},
		{
			name:       "a candidate no session can free a slot for",
			tune:       func(cfg *config.ServiceConfig) { cfg.Agent.MaxConcurrentByState = map[string]int{"to do": 0} },
			candidates: []domain.Issue{first},
			want:       OnceResult{NotRun: 1},
		},
		{
			name:        "a candidate whose blocker read fails once",
			candidates:  []domain.Issue{first},
			setup:       func(h *runHarness) { h.o.blockerResolver = &flakyBlockerResolver{onFirstRead: func() {}} },
			wantStarted: []string{"A-1"},
			want:        OnceResult{Issues: 1},
		},
		{
			name:       "a candidate only a later fetch returns",
			candidates: []domain.Issue{first},
			setup: func(h *runHarness) {
				h.o.blockerResolver = &flakyBlockerResolver{onFirstRead: func() {
					h.tracker.mu.Lock()
					defer h.tracker.mu.Unlock()
					h.tracker.candidates = append(h.tracker.candidates, late)
				}}
			},
			wantStarted: []string{"A-1"},
			want:        OnceResult{Issues: 1},
		},
		{
			name:       "a spend ceiling hold at the poll",
			tune:       withCeiling,
			candidates: []domain.Issue{first},
			setup:      func(h *runHarness) { setWindowUsage(h, 5) },
			want:       OnceResult{Issues: 1, NotSucceeded: []string{"A-1"}},
		},
		{
			name:       "a weekly spend ceiling hold at the poll",
			tune:       func(cfg *config.ServiceConfig) { cfg.Agent.MaxWeeklyCostUSD = 5 },
			candidates: []domain.Issue{first},
			setup:      func(h *runHarness) { setWindowUsage(h, 5) },
			want:       OnceResult{Issues: 1, NotSucceeded: []string{"A-1"}},
		},
		{
			name:          "a retry that fires under a spend ceiling hold",
			tune:          withCeiling,
			candidates:    []domain.Issue{first},
			setup:         func(h *runHarness) { failFirstAttempt(h, first, func() { setWindowUsage(h, 5) }) },
			wantStarted:   []string{"A-1"},
			want:          OnceResult{Issues: 1, NotSucceeded: []string{"A-1"}},
			wantRetryKept: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newOnceHarness(t, tt.tune)
			h.tracker.candidates = tt.candidates
			if tt.setup != nil {
				tt.setup(h)
			}

			got := runUntilSettled(t, h)

			if tt.wantOutcomes != nil && !slices.Equal(got.Outcomes, tt.wantOutcomes) {
				t.Errorf("OnceResult().Outcomes = %+v, want %+v", got.Outcomes, tt.wantOutcomes)
			}
			got.Outcomes = nil
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("OnceResult() = %+v, want %+v", got, tt.want)
			}
			if started := startedWorkspaces(h); !slices.Equal(started, tt.wantStarted) {
				t.Errorf("sessions started in workspaces = %v, want %v", started, tt.wantStarted)
			}
			if tt.wantRetryKept {
				h.store.mu.Lock()
				defer h.store.mu.Unlock()
				if len(h.store.savedRetries) == 0 || len(h.store.deletedRetryIDs) != 0 {
					t.Errorf("persisted retry rows saved, deleted = %d, %v, want a saved row and no deletion", len(h.store.savedRetries), h.store.deletedRetryIDs)
				}
			}
		})
	}
}

func TestOnce_SignalDuringASessionInterrupts(t *testing.T) {
	t.Parallel()

	h := newOnceHarness(t, nil)
	h.tracker.candidates = []domain.Issue{candidate("id-1", "A-1")}
	h.recorders["kind-a"].adapter.runTurnFn = func(ctx context.Context, _ domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
		<-ctx.Done()
		return domain.TurnResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		h.o.Run(ctx)
		close(done)
	}()
	h.waitForSessions(1)

	cancel()
	<-done

	if got := h.o.OnceResult(); !got.Interrupted {
		t.Errorf("OnceResult().Interrupted after a signal during a session = %v, want true", got.Interrupted)
	}
}

func TestOnce_TickStartsNoReactionPass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		once      bool
		wantCalls int
	}{
		{"once mode", true, 0},
		{"long-running mode", false, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newOnceHarness(t, nil)
			if !tt.once {
				h.o.once = nil
			}
			scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
			h.o.scmAdapter = scm
			h.o.reviewConfig = defaultReviewConfig()
			h.state.PendingReactions[ReactionKey("ISS-R", ReactionKindReview)] = newReviewPendingEntry("ISS-R", 10)
			h.state.Claimed["ISS-R"] = struct{}{}

			h.o.handleTick(context.Background())

			if scm.calls != tt.wantCalls {
				t.Errorf("FetchPendingReviews calls after a tick (once=%v) = %d, want %d", tt.once, scm.calls, tt.wantCalls)
			}
		})
	}
}
