package orchestrator

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- bot-review metrics spy ---

// botReviewMetricsSpy records bot-review-specific metric calls.
type botReviewMetricsSpy struct {
	domain.NoopMetrics
	botReviewChecks      map[string]int
	botReviewEscalations map[string]int
}

func newBotReviewMetricsSpy() *botReviewMetricsSpy {
	return &botReviewMetricsSpy{
		botReviewChecks:      make(map[string]int),
		botReviewEscalations: make(map[string]int),
	}
}

func (s *botReviewMetricsSpy) IncBotReviewChecks(result string) { s.botReviewChecks[result]++ }
func (s *botReviewMetricsSpy) IncBotReviewEscalations(action string) {
	s.botReviewEscalations[action]++
}

// --- bot-review test helpers ---

// botReviewBaseTime is a fixed reference time for bot-review reconcile tests.
var botReviewBaseTime = time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)

// makeBotReviewPendingEntry builds a PendingReaction for bot-review tests.
func makeBotReviewPendingEntry(t *testing.T, issueID string, prNumber int) *PendingReaction {
	t.Helper()
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindBotReview,
		CreatedAt:  botReviewBaseTime,
		KindData: &BotReviewReactionData{
			PRNumber: prNumber,
			Owner:    "owner",
			Repo:     "repo",
			Branch:   "feature/bot-fix",
			SHA:      "sha123",
		},
	}
}

// stateWithBotReviewReaction creates a State with one bot-review PendingReaction.
func stateWithBotReviewReaction(t *testing.T, issueID string, prNumber int) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindBotReview)
	s.PendingReactions[rkey] = makeBotReviewPendingEntry(t, issueID, prNumber)
	s.Claimed[issueID] = struct{}{}
	return s
}

// defaultBotReviewConfig returns a BotReviewReactionConfig with sensible defaults.
func defaultBotReviewConfig() BotReviewReactionConfig {
	return BotReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		PollIntervalMS:       60000,
		MaxContinuationTurns: 5,
	}
}

// botReviewParams returns ReconcileParams wired for bot-review reconcile tests.
func botReviewParams(store *reviewReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter) ReconcileParams {
	return ReconcileParams{
		TrackerAdapter:      tracker,
		SCMAdapter:          scm,
		BotReviewConfig:     defaultBotReviewConfig(),
		BotReviewConfigured: true,
		Store:               store,
		OnRetryFire:         noopRetryFire,
		Ctx:                 context.Background(),
		Logger:              discardLogger(),
		NowFunc:             func() time.Time { return botReviewBaseTime },
	}
}

// --- BuildBotReviewReactionConfig tests (6.2 → R4) ---

func TestBuildBotReviewReactionConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rc      config.ReactionConfig
		want    BotReviewReactionConfig
		wantErr bool
	}{
		{
			name: "defaults: MaxContinuationTurns=5, PollIntervalMS=60000, Escalation=label, EscalationLabel=needs-human",
			rc:   config.ReactionConfig{},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       60000,
				MaxContinuationTurns: 5,
				BotUsernames:         nil,
				WatchWindowMS:        reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "explicit valid escalation comment",
			rc:   config.ReactionConfig{Escalation: "comment"},
			want: BotReviewReactionConfig{
				Escalation:           "comment",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       60000,
				MaxContinuationTurns: 5,
				WatchWindowMS:        reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "well-formed bot_usernames coerces to []string",
			rc: config.ReactionConfig{
				Extra: map[string]any{
					"bot_usernames": []any{"houndci-bot"},
				},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       60000,
				MaxContinuationTurns: 5,
				BotUsernames:         []string{"houndci-bot"},
				WatchWindowMS:        reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "poll_interval_ms < 30000 errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"poll_interval_ms": 29999},
			},
			wantErr: true,
		},
		{
			name: "poll_interval_ms exactly 30000 is valid",
			rc: config.ReactionConfig{
				Extra: map[string]any{"poll_interval_ms": 30000},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       30000,
				MaxContinuationTurns: 5,
				WatchWindowMS:        reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "poll_interval_ms as int64 is valid",
			rc: config.ReactionConfig{
				Extra: map[string]any{"poll_interval_ms": int64(45000)},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       45000,
				MaxContinuationTurns: 5,
				WatchWindowMS:        reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "poll_interval_ms as whole float64 is valid",
			rc: config.ReactionConfig{
				Extra: map[string]any{"poll_interval_ms": float64(90000)},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       90000,
				MaxContinuationTurns: 5,
				WatchWindowMS:        reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "poll_interval_ms non-numeric type errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"poll_interval_ms": "fast"},
			},
			wantErr: true,
		},
		{
			name: "poll_interval_ms fractional float64 errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"poll_interval_ms": float64(30000.5)},
			},
			wantErr: true,
		},
		{
			name: "poll_interval_ms infinite float64 errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"poll_interval_ms": math.Inf(1)},
			},
			wantErr: true,
		},
		{
			name: "max_continuation_turns valid override",
			rc: config.ReactionConfig{
				Extra: map[string]any{"max_continuation_turns": 8},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       60000,
				MaxContinuationTurns: 8,
				WatchWindowMS:        reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "max_continuation_turns non-numeric type errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"max_continuation_turns": "many"},
			},
			wantErr: true,
		},
		{
			name: "max_continuation_turns <= 0 errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"max_continuation_turns": 0},
			},
			wantErr: true,
		},
		{
			name: "max_continuation_turns negative errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"max_continuation_turns": -1},
			},
			wantErr: true,
		},
		{
			name:    "bad escalation value errors",
			rc:      config.ReactionConfig{Escalation: "webhook"},
			wantErr: true,
		},
		{
			name: "non-string bot_usernames element errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{
					"bot_usernames": []any{"valid-bot", 42},
				},
			},
			wantErr: true,
		},
		{
			name: "non-list value under bot_usernames errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{
					"bot_usernames": "not-a-list",
				},
			},
			wantErr: true,
		},
		{
			name: "watch_window_ms override",
			rc: config.ReactionConfig{
				Extra: map[string]any{"watch_window_ms": 600000},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       60000,
				MaxContinuationTurns: 5,
				WatchWindowMS:        600000,
			},
		},
		{
			name: "watch_window_ms zero disables the bound",
			rc: config.ReactionConfig{
				Extra: map[string]any{"watch_window_ms": 0},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       60000,
				MaxContinuationTurns: 5,
				WatchWindowMS:        0,
			},
		},
		{
			name: "watch_window_ms negative errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"watch_window_ms": -1},
			},
			wantErr: true,
		},
		{
			name: "watch_window_ms above ceiling errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS) + 1},
			},
			wantErr: true,
		},
		{
			name: "watch_window_ms non-numeric errors",
			rc: config.ReactionConfig{
				Extra: map[string]any{"watch_window_ms": "soon"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := BuildBotReviewReactionConfig(tt.rc)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("BuildBotReviewReactionConfig(%+v) = %+v, want error", tt.rc, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildBotReviewReactionConfig(%+v) unexpected error: %v", tt.rc, err)
			}
			if got.MaxContinuationTurns != tt.want.MaxContinuationTurns {
				t.Errorf("BuildBotReviewReactionConfig MaxContinuationTurns = %d, want %d",
					got.MaxContinuationTurns, tt.want.MaxContinuationTurns)
			}
			if got.PollIntervalMS != tt.want.PollIntervalMS {
				t.Errorf("BuildBotReviewReactionConfig PollIntervalMS = %d, want %d",
					got.PollIntervalMS, tt.want.PollIntervalMS)
			}
			if got.Escalation != tt.want.Escalation {
				t.Errorf("BuildBotReviewReactionConfig Escalation = %q, want %q",
					got.Escalation, tt.want.Escalation)
			}
			if got.EscalationLabel != tt.want.EscalationLabel {
				t.Errorf("BuildBotReviewReactionConfig EscalationLabel = %q, want %q",
					got.EscalationLabel, tt.want.EscalationLabel)
			}
			if got.WatchWindowMS != tt.want.WatchWindowMS {
				t.Errorf("BuildBotReviewReactionConfig WatchWindowMS = %d, want %d",
					got.WatchWindowMS, tt.want.WatchWindowMS)
			}
			if len(got.BotUsernames) != len(tt.want.BotUsernames) {
				t.Errorf("BuildBotReviewReactionConfig BotUsernames = %v, want %v",
					got.BotUsernames, tt.want.BotUsernames)
			} else {
				for i, name := range got.BotUsernames {
					if name != tt.want.BotUsernames[i] {
						t.Errorf("BuildBotReviewReactionConfig BotUsernames[%d] = %q, want %q",
							i, name, tt.want.BotUsernames[i])
					}
				}
			}
		})
	}
}

// TestBuildBotReviewReactionConfig_ReviewDefaultsUnchanged verifies that the bot-review
// defaults (MaxContinuationTurns=5, PollIntervalMS=60000) are independent of the review
// kind defaults (MaxContinuationTurns=3, PollIntervalMS=120000). R4.
func TestBuildBotReviewReactionConfig_ReviewDefaultsUnchanged(t *testing.T) {
	t.Parallel()

	botGot, err := BuildBotReviewReactionConfig(config.ReactionConfig{})
	if err != nil {
		t.Fatalf("BuildBotReviewReactionConfig: %v", err)
	}
	reviewGot, err := BuildReviewReactionConfig(config.ReactionConfig{})
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}

	if botGot.MaxContinuationTurns != 5 {
		t.Errorf("bot-review MaxContinuationTurns = %d, want 5", botGot.MaxContinuationTurns)
	}
	if botGot.PollIntervalMS != 60000 {
		t.Errorf("bot-review PollIntervalMS = %d, want 60000", botGot.PollIntervalMS)
	}
	if reviewGot.MaxContinuationTurns != 3 {
		t.Errorf("review MaxContinuationTurns = %d, want 3", reviewGot.MaxContinuationTurns)
	}
	if reviewGot.PollIntervalMS != 120000 {
		t.Errorf("review PollIntervalMS = %d, want 120000", reviewGot.PollIntervalMS)
	}
}

// --- reconcileBotReviewComments tests (6.3 → R5, R6) ---

func TestReconcileBotReviewComments_NilAdapter(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-1", 42)
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	params := botReviewParams(store, nil, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("BOT-1", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed with nil SCMAdapter; want no-op")
	}
	if len(metrics.botReviewChecks) != 0 {
		t.Error("IncBotReviewChecks called with nil adapter; want no calls")
	}
}

func TestReconcileBotReviewComments_NotConfigured(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-NC", 42)
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)
	params.BotReviewConfigured = false

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("BOT-NC", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed when BotReviewConfigured=false; want no-op")
	}
	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 (not configured)", scm.botCalls)
	}
}

func TestReconcileBotReviewComments_PollThrottle(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-THROTT", 10)
	rkey := ReactionKey("BOT-THROTT", ReactionKindBotReview)
	state.PendingReactions[rkey].PendingRetryAt = botReviewBaseTime.Add(5 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped on poll throttle; want re-enqueued")
	}
	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 (throttled)", scm.botCalls)
	}
}

func TestReconcileBotReviewComments_SCMFetchError_ReEnqueues(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-ERR", 10)
	rkey := ReactionKey("BOT-ERR", ReactionKindBotReview)
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botErr: errors.New("upstream timeout")}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on SCM fetch error; want re-enqueued")
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1 after first error", entry.PendingAttempts)
	}
	if !entry.PendingRetryAt.After(botReviewBaseTime) {
		t.Error("PendingRetryAt not in future after SCM error; want backoff applied")
	}
	// PendingRetryAt must be >= max(backoff, pollInterval): the poll interval is 60s.
	minExpected := botReviewBaseTime.Add(60 * time.Second)
	if entry.PendingRetryAt.Before(minExpected) {
		t.Errorf("PendingRetryAt = %v, want >= pollInterval from now (%v)",
			entry.PendingRetryAt, minExpected)
	}
	if metrics.botReviewChecks["error"] != 1 {
		t.Errorf(`IncBotReviewChecks("error") = %d, want 1`, metrics.botReviewChecks["error"])
	}
}

func TestReconcileBotReviewComments_NoActionableComments(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-NOACT", 10)
	rkey := ReactionKey("BOT-NOACT", ReactionKindBotReview)
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: []domain.ReviewComment{}}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with no actionable comments; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["BOT-NOACT"]; ok {
		t.Error("retry scheduled with no actionable comments; want none")
	}
}

func TestReconcileBotReviewComments_AllCommentsOutdated(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-OUT", 10)
	rkey := ReactionKey("BOT-OUT", ReactionKindBotReview)
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{
		botComments: []domain.ReviewComment{
			{ID: "b1", Outdated: true},
			{ID: "b2", Outdated: true},
		},
	}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with all outdated comments; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["BOT-OUT"]; ok {
		t.Error("retry scheduled with all outdated comments; want none")
	}
}

// TestReconcileBotReviewComments_ImmediateDispatch_NoDebounce verifies R5: bot-review
// dispatches on the same tick it detects actionable comments, with no debounce gate.
// The comment timestamp can be brand-new (seconds ago) and dispatch still happens.
func TestReconcileBotReviewComments_ImmediateDispatch_NoDebounce(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-IMM", 10)
	rkey := ReactionKey("BOT-IMM", ReactionKindBotReview)

	// Comment submitted just 2 seconds ago — within any plausible debounce window.
	// The review path would NOT dispatch yet; the bot-review path MUST dispatch immediately.
	freshComment := []domain.ReviewComment{
		{ID: "bot-fresh", Body: "lint: line too long", SubmittedAt: botReviewBaseTime.Add(-2 * time.Second)},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: freshComment}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Must have dispatched: pending entry consumed.
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after dispatch; want consumed (no debounce)")
	}
	if _, ok := state.RetryAttempts["BOT-IMM"]; !ok {
		t.Fatal("retry not scheduled after bot-review dispatch; want scheduled")
	}
	retry := state.RetryAttempts["BOT-IMM"]
	if retry.ReactionKind != ReactionKindBotReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindBotReview)
	}
	if retry.ContinuationContext == nil {
		t.Error("RetryEntry.ContinuationContext is nil; want bot_review_comments map")
	}
	if _, ok := retry.ContinuationContext["bot_review_comments"]; !ok {
		t.Error("ContinuationContext missing bot_review_comments key")
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.botReviewChecks["dispatched"] != 1 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 1`, metrics.botReviewChecks["dispatched"])
	}
}

// TestReconcileBotReviewComments_FingerprintChurn verifies R6: changing the set of
// actionable comment IDs produces a new fingerprint, resets the dispatched flag,
// and triggers re-dispatch on the next tick.
func TestReconcileBotReviewComments_FingerprintChurn(t *testing.T) {
	t.Parallel()

	// First tick: comment set {c1, c2}. Fingerprint stored and dispatched.
	firstSet := []domain.ReviewComment{
		{ID: "c1", Body: "fix 1"},
		{ID: "c2", Body: "fix 2"},
	}
	firstFP := buildReviewFingerprint(firstSet)

	// Second tick: comment set {c1, c2, c3} — churned.
	secondSet := []domain.ReviewComment{
		{ID: "c1", Body: "fix 1"},
		{ID: "c2", Body: "fix 2"},
		{ID: "c3", Body: "fix 3"},
	}

	// Store returns the OLD fingerprint as dispatched, but the new set produces a different one.
	store := &reviewReconcileStore{
		getFingerprintResult:     firstFP,
		getFingerprintDispatched: true,
	}

	state := stateWithBotReviewReaction(t, "BOT-CHURN", 20)
	rkey := ReactionKey("BOT-CHURN", ReactionKindBotReview)
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: secondSet}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// New fingerprint != stored dispatched fingerprint → must dispatch.
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after fingerprint churn dispatch; want consumed")
	}
	if _, ok := state.RetryAttempts["BOT-CHURN"]; !ok {
		t.Fatal("retry not scheduled after fingerprint churn; want scheduled")
	}
	if metrics.botReviewChecks["dispatched"] != 1 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 1 after fingerprint churn`,
			metrics.botReviewChecks["dispatched"])
	}
}

// TestReconcileBotReviewComments_UnchangedDispatchedFingerprint verifies that an unchanged
// dispatched fingerprint causes re-enqueue WITHOUT dispatch.
func TestReconcileBotReviewComments_UnchangedDispatchedFingerprint(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{
		{ID: "stable-1", Body: "fix this"},
	}
	fp := buildReviewFingerprint(comments)

	store := &reviewReconcileStore{
		getFingerprintResult:     fp,
		getFingerprintDispatched: true,
	}

	state := stateWithBotReviewReaction(t, "BOT-SAME", 30)
	rkey := ReactionKey("BOT-SAME", ReactionKindBotReview)
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: comments}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Same dispatched fingerprint → re-enqueue, no dispatch.
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped for same dispatched fingerprint; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["BOT-SAME"]; ok {
		t.Error("retry scheduled for same dispatched fingerprint; want none")
	}
	if metrics.botReviewChecks["dispatched"] != 0 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 0 (same fingerprint)`,
			metrics.botReviewChecks["dispatched"])
	}
}

// TestReconcileBotReviewComments_KindDataTypeMismatch verifies that a KindData with
// the wrong concrete type is skipped with an error log and does not panic.
func TestReconcileBotReviewComments_KindDataTypeMismatch(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey("BOT-MISMATCH", ReactionKindBotReview)
	state.PendingReactions[rkey] = &PendingReaction{
		IssueID:    "BOT-MISMATCH",
		Identifier: "BOT-MISMATCH-ident",
		Kind:       ReactionKindBotReview,
		CreatedAt:  botReviewBaseTime,
		KindData:   &ReviewReactionData{PRNumber: 1}, // wrong type
	}

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)

	// Must not panic.
	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Entry consumed (deleted, not re-enqueued) after type mismatch skip.
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after KindData mismatch; want skipped (deleted)")
	}
	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 (mismatch short-circuits)", scm.botCalls)
	}
}

// TestReconcileBotReviewComments_SkipsNonBotReviewEntries verifies that entries
// for other reaction kinds are not processed by the bot-review reconcile pass.
func TestReconcileBotReviewComments_SkipsNonBotReviewEntries(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	// Add a review-kind entry that must remain untouched.
	reviewKey := ReactionKey("BOT-CROSS", ReactionKindReview)
	state.PendingReactions[reviewKey] = &PendingReaction{
		IssueID:    "BOT-CROSS",
		Identifier: "BOT-CROSS-ident",
		Kind:       ReactionKindReview,
		CreatedAt:  botReviewBaseTime,
		KindData:   &ReviewReactionData{PRNumber: 5},
	}

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[reviewKey]; !ok {
		t.Error("review PendingReactions entry removed by bot-review reconcile; want untouched")
	}
	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 (no bot-review entries)", scm.botCalls)
	}
}

// --- escalateBotReviewFailure tests (6.4 → R7, R10) ---

// TestEscalateBotReviewFailure_CrossKindIsolation verifies R7 and R10:
// a PR with both a review slot and a bot-review slot — after bot-review escalation:
// - the bot-review PendingReaction is deleted
// - the review slot remains untouched
// - the merge slot remains untouched (if present)
// - state.Claimed[issueID] is NOT cleared
// - state.ReactionAttempts for bot-review is NOT reset.
func TestEscalateBotReviewFailure_CrossKindIsolation(t *testing.T) {
	t.Parallel()

	issueID := "ISO-1"
	state := NewState(5000, 4, nil, AgentTotals{})

	// Seed: review slot.
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[reviewKey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Kind:       ReactionKindReview,
		CreatedAt:  botReviewBaseTime,
		KindData:   &ReviewReactionData{PRNumber: 77},
	}

	// Seed: merge (auto-merge) slot.
	mergeKey := ReactionKey(issueID, ReactionKindAutoMerge)
	state.PendingReactions[mergeKey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Kind:       ReactionKindAutoMerge,
		CreatedAt:  botReviewBaseTime,
		KindData:   &AutoMergeReactionData{PRNumber: 77},
	}

	// Seed: bot-review slot at cap.
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID,
		Attempt:    1,
		Kind:       ReactionKindBotReview,
		CreatedAt:  botReviewBaseTime,
		KindData: &BotReviewReactionData{
			PRNumber: 77,
			Owner:    "owner",
			Repo:     "repo",
			Branch:   "feature/iso",
		},
	}
	state.PendingReactions[botKey] = botPending
	state.Claimed[issueID] = struct{}{}
	state.ReactionAttempts[botKey] = 5 // at cap

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	// bot-review slot deleted.
	if _, ok := state.PendingReactions[botKey]; ok {
		t.Error("bot-review PendingReactions entry still present after escalation; want deleted")
	}
	// review slot untouched.
	if _, ok := state.PendingReactions[reviewKey]; !ok {
		t.Error("review PendingReactions entry removed by bot-review escalation; want untouched")
	}
	// merge slot untouched.
	if _, ok := state.PendingReactions[mergeKey]; !ok {
		t.Error("merge PendingReactions entry removed by bot-review escalation; want untouched")
	}
	// Claim untouched.
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("state.Claimed[issueID] cleared by bot-review escalation; want untouched")
	}
	// ReactionAttempts counter for bot-review is NOT cleared.
	if state.ReactionAttempts[botKey] != 5 {
		t.Errorf("ReactionAttempts[%s] = %d, want 5 (residual counter preserved)", botKey, state.ReactionAttempts[botKey])
	}
	// Fingerprint delete was called.
	if store.deleteFingerprintCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1", store.deleteFingerprintCalls)
	}
}

// TestEscalateBotReviewFailure_LabelAction verifies the "label" escalation branch.
func TestEscalateBotReviewFailure_LabelAction(t *testing.T) {
	t.Parallel()

	issueID := "ESC-LABEL-1"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 10, Owner: "o", Repo: "r"},
	}
	state.PendingReactions[botKey] = botPending
	state.Claimed[issueID] = struct{}{}

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 for label escalation", tracker.addLabelCalled)
	}
	if tracker.commentIssueCalls != 0 {
		t.Errorf("CommentIssue calls = %d, want 0 for label escalation", tracker.commentIssueCalls)
	}
	if metrics.botReviewEscalations["label"] != 1 {
		t.Errorf(`IncBotReviewEscalations("label") = %d, want 1`, metrics.botReviewEscalations["label"])
	}
}

// TestEscalateBotReviewFailure_CommentAction verifies the "comment" escalation branch.
func TestEscalateBotReviewFailure_CommentAction(t *testing.T) {
	t.Parallel()

	issueID := "ESC-COMMENT-1"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 20, Owner: "o", Repo: "r"},
	}
	state.PendingReactions[botKey] = botPending

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "comment",
		EscalationLabel:      "needs-human",
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.commentIssueCalls != 1 {
		t.Errorf("CommentIssue calls = %d, want 1 for comment escalation", tracker.commentIssueCalls)
	}
	if tracker.addLabelCalled != 0 {
		t.Errorf("AddLabel calls = %d, want 0 for comment escalation", tracker.addLabelCalled)
	}
	if metrics.botReviewEscalations["comment"] != 1 {
		t.Errorf(`IncBotReviewEscalations("comment") = %d, want 1`, metrics.botReviewEscalations["comment"])
	}
}

// TestEscalateBotReviewFailure_EmptyEscalation verifies that empty-string escalation
// falls back to the comment action (same as "comment").
func TestEscalateBotReviewFailure_EmptyEscalation(t *testing.T) {
	t.Parallel()

	issueID := "ESC-EMPTY-1"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 30, Owner: "o", Repo: "r"},
	}
	state.PendingReactions[botKey] = botPending

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "", // empty → comment branch
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.commentIssueCalls != 1 {
		t.Errorf("CommentIssue calls = %d, want 1 for empty escalation (comment branch)", tracker.commentIssueCalls)
	}
}

// TestEscalateBotReviewFailure_NilTrackerAdapter verifies the no-op path when
// TrackerAdapter is nil: no panic, fingerprint still deleted.
func TestEscalateBotReviewFailure_NilTrackerAdapter(t *testing.T) {
	t.Parallel()

	issueID := "ESC-NOTRACKER"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 40},
	}
	state.PendingReactions[botKey] = botPending

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	params := botReviewParams(store, nil, nil)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)

	// Must not panic with nil TrackerAdapter.
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	// bot-review slot still removed.
	if _, ok := state.PendingReactions[botKey]; ok {
		t.Error("bot-review PendingReactions entry still present after nil-tracker escalation; want deleted")
	}
	// Fingerprint delete called.
	if store.deleteFingerprintCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1", store.deleteFingerprintCalls)
	}
}

// TestReconcileBotReviewComments_TurnCapEscalates verifies that when
// ReactionAttempts >= MaxContinuationTurns, escalateBotReviewFailure is called
// via the reconcile loop and ONLY the bot-review slot is cleaned up (R7, R10).
func TestReconcileBotReviewComments_TurnCapEscalates(t *testing.T) {
	t.Parallel()

	issueID := "BOT-CAP"
	state := NewState(5000, 4, nil, AgentTotals{})

	// Sibling review slot that must survive.
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[reviewKey] = &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &ReviewReactionData{PRNumber: 99},
	}

	// bot-review slot at cap.
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	state.PendingReactions[botKey] = makeBotReviewPendingEntry(t, issueID, 99)
	state.Claimed[issueID] = struct{}{}
	state.ReactionAttempts[botKey] = 5 // == MaxContinuationTurns

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	tracker := &reviewTrackerStub{}
	params := botReviewParams(store, scm, tracker)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	// bot-review slot consumed.
	if _, ok := state.PendingReactions[botKey]; ok {
		t.Error("bot-review PendingReactions entry still present after turn cap; want deleted")
	}
	// SCM must not have been called (cap check before fetch).
	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 (cap check before fetch)", scm.botCalls)
	}
	// Claim must still be set (only terminal cleanup releases it).
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("state.Claimed[issueID] cleared by bot-review escalation; want preserved (R10)")
	}
	// Sibling review slot must be untouched.
	if _, ok := state.PendingReactions[reviewKey]; !ok {
		t.Error("review PendingReactions entry removed by bot-review escalation; want untouched (R7)")
	}
	// ReactionAttempts for bot-review is NOT cleared.
	if state.ReactionAttempts[botKey] != 5 {
		t.Errorf("ReactionAttempts[%s] = %d, want 5 (residual counter preserved, R10)", botKey, state.ReactionAttempts[botKey])
	}
}

// botReviewErrTrackerStub is a TrackerAdapter whose AddLabel and CommentIssue
// return a configurable error, used to exercise the escalation error paths.
type botReviewErrTrackerStub struct {
	addLabelErr     error
	commentErr      error
	addLabelCalls   int
	commentCalls    int
	lastLabel       string
	lastCommentText string
}

var _ domain.TrackerAdapter = (*botReviewErrTrackerStub)(nil)

func (s *botReviewErrTrackerStub) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (s *botReviewErrTrackerStub) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (s *botReviewErrTrackerStub) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (s *botReviewErrTrackerStub) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *botReviewErrTrackerStub) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *botReviewErrTrackerStub) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (s *botReviewErrTrackerStub) TransitionIssue(_ context.Context, _, _ string) error {
	return nil
}
func (s *botReviewErrTrackerStub) CommentIssue(_ context.Context, _, text string) error {
	s.commentCalls++
	s.lastCommentText = text
	return s.commentErr
}
func (s *botReviewErrTrackerStub) AddLabel(_ context.Context, _, label string) error {
	s.addLabelCalls++
	s.lastLabel = label
	return s.addLabelErr
}

// TestReconcileBotReviewComments_TTLDrop verifies that a bot-review pending
// entry older than BotReviewPendingTTL is dropped without dispatch or fetch.
func TestReconcileBotReviewComments_TTLDrop(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-TTL", 10)
	rkey := ReactionKey("BOT-TTL", ReactionKindBotReview)
	// CreatedAt is botReviewBaseTime; advance now well past the TTL.
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)
	params.BotReviewPendingTTL = 1 * time.Minute
	params.NowFunc = func() time.Time { return botReviewBaseTime.Add(2 * time.Minute) }

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry present after TTL drop; want removed")
	}
	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 (TTL drop before fetch)", scm.botCalls)
	}
	if _, ok := state.RetryAttempts["BOT-TTL"]; ok {
		t.Error("retry scheduled after TTL drop; want none")
	}
}

// TestReconcileBotReviewComments_DropOnAgeReleasesCounter verifies that the
// drop-on-age branch also deletes the bot-review reaction attempt counter,
// leaves a sibling kind's counter and the claim untouched, and performs
// no retry or fingerprint store call.
func TestReconcileBotReviewComments_DropOnAgeReleasesCounter(t *testing.T) {
	t.Parallel()

	issueID := "BOT-AGE-1"
	state := stateWithBotReviewReaction(t, issueID, 10)
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	state.ReactionAttempts[botKey] = 2
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.ReactionAttempts[reviewKey] = 9
	delete(state.Claimed, issueID)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)
	params.BotReviewPendingTTL = 1 * time.Minute
	params.NowFunc = func() time.Time { return botReviewBaseTime.Add(2 * time.Minute) }

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[botKey]; ok {
		t.Error("PendingReactions[bot-review] present after drop-on-age; want removed")
	}
	if _, ok := state.ReactionAttempts[botKey]; ok {
		t.Error("ReactionAttempts[bot-review] present after drop-on-age; want removed")
	}
	if state.ReactionAttempts[reviewKey] != 9 {
		t.Errorf("ReactionAttempts[review] = %d, want 9 (untouched)", state.ReactionAttempts[reviewKey])
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("Claimed present after drop-on-age; want absent")
	}
	if len(store.deletedIssueIDs) != 0 {
		t.Errorf("DeleteRetryEntry calls = %d, want 0", len(store.deletedIssueIDs))
	}
	if store.upsertFingerprintCalls != 0 || store.getFingerprintCalls != 0 || store.deleteFingerprintCalls != 0 {
		t.Errorf("fingerprint calls = upsert:%d get:%d delete:%d, want all 0",
			store.upsertFingerprintCalls, store.getFingerprintCalls, store.deleteFingerprintCalls)
	}
	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 (TTL drop before fetch)", scm.botCalls)
	}
}

// TestReconcileBotReviewComments_WatchWindowZeroNeverDrops verifies that a
// BotReviewPendingTTL of 0 (watch_window_ms: 0) never drops an entry on age,
// however old it is.
func TestReconcileBotReviewComments_WatchWindowZeroNeverDrops(t *testing.T) {
	t.Parallel()

	rkey := ReactionKey("BOT-WW0", ReactionKindBotReview)
	state := stateWithBotReviewReaction(t, "BOT-WW0", 10)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)
	params.BotReviewPendingTTL = 0
	params.NowFunc = func() time.Time { return botReviewBaseTime.Add(365 * 24 * time.Hour) }

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with BotReviewPendingTTL=0; want never dropped on age")
	}
}

// TestReconcileBotReviewComments_WatchWindowNonDefaultTakesEffect verifies
// that a configured window other than the default threshold actually gates
// the drop: an entry older than the configured window is dropped with its
// attempt counter released, and one younger survives.
func TestReconcileBotReviewComments_WatchWindowNonDefaultTakesEffect(t *testing.T) {
	t.Parallel()

	t.Run("older than configured window is dropped and counter released", func(t *testing.T) {
		t.Parallel()

		rkey := ReactionKey("BOT-WWN-OLD", ReactionKindBotReview)
		state := stateWithBotReviewReaction(t, "BOT-WWN-OLD", 10)
		state.ReactionAttempts[rkey] = 2

		store := &reviewReconcileStore{}
		metrics := newBotReviewMetricsSpy()
		scm := &mockSCMAdapter{}
		params := botReviewParams(store, scm, nil)
		params.BotReviewPendingTTL = 5 * time.Minute
		params.NowFunc = func() time.Time { return botReviewBaseTime.Add(6 * time.Minute) }

		reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions entry present past configured 5m window; want dropped")
		}
		if _, ok := state.ReactionAttempts[rkey]; ok {
			t.Error("ReactionAttempts present past configured 5m window; want released")
		}
	})

	t.Run("younger than configured window survives", func(t *testing.T) {
		t.Parallel()

		rkey := ReactionKey("BOT-WWN-NEW", ReactionKindBotReview)
		state := stateWithBotReviewReaction(t, "BOT-WWN-NEW", 10)

		store := &reviewReconcileStore{}
		metrics := newBotReviewMetricsSpy()
		scm := &mockSCMAdapter{}
		params := botReviewParams(store, scm, nil)
		params.BotReviewPendingTTL = 5 * time.Minute
		params.NowFunc = func() time.Time { return botReviewBaseTime.Add(4 * time.Minute) }

		reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[rkey]; !ok {
			t.Error("PendingReactions entry dropped inside configured 5m window; want kept")
		}
	})
}

// TestReconcileBotReviewComments_WatchWindowElapsedLogsRenamedAttribute pins
// the drop-on-age log record: message text and the window_ms attribute name
// (renamed from ttl_ms). A regression that reverts the rename or the wording
// must fail this test.
func TestReconcileBotReviewComments_WatchWindowElapsedLogsRenamedAttribute(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-WWLOG", 10)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)
	params.BotReviewPendingTTL = 30 * time.Minute
	params.NowFunc = func() time.Time { return botReviewBaseTime.Add(31 * time.Minute) }
	log, buf := logCapture()

	reconcileBotReviewComments(state, params, log, context.Background(), metrics)

	output := buf.String()
	const msg = "bot review watch window elapsed, dropping"
	assertLogLineHasIntAttr(t, output, msg, "window_ms", int(30*time.Minute/time.Millisecond))
	if strings.Contains(output, "ttl_ms") {
		t.Errorf("log output contains stale attribute %q: %s", "ttl_ms", output)
	}
	if strings.Contains(output, "exceeded ttl") {
		t.Errorf("log output contains stale message wording %q: %s", "exceeded ttl", output)
	}
}

// TestReconcileBotReviewComments_ZeroPollIntervalUsesFallback verifies that a
// zero PollIntervalMS falls back to the default backoff base when re-enqueuing
// a pending entry with no actionable comments.
func TestReconcileBotReviewComments_ZeroPollIntervalUsesFallback(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-ZEROPOLL", 10)
	rkey := ReactionKey("BOT-ZEROPOLL", ReactionKindBotReview)
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: []domain.ReviewComment{}}
	params := botReviewParams(store, scm, nil)
	params.BotReviewConfig.PollIntervalMS = 0 // forces fallback to reviewPendingBackoffBase

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped with zero poll interval; want re-enqueued")
	}
	wantRetryAt := botReviewBaseTime.Add(reviewPendingBackoffBase)
	if !entry.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v (fallback backoff base)", entry.PendingRetryAt, wantRetryAt)
	}
}

// TestReconcileBotReviewComments_UpsertFingerprintError_StillDispatches verifies
// that an UpsertReactionFingerprint failure is logged but does not prevent
// dispatch when actionable comments exist.
func TestReconcileBotReviewComments_UpsertFingerprintError_StillDispatches(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-UPFP", 10)
	rkey := ReactionKey("BOT-UPFP", ReactionKindBotReview)
	store := &reviewReconcileStore{
		upsertFingerprintErr: errors.New("upsert failed"),
	}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: []domain.ReviewComment{{ID: "u1", Body: "fix me"}}}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after dispatch despite upsert error; want consumed")
	}
	if _, ok := state.RetryAttempts["BOT-UPFP"]; !ok {
		t.Error("retry not scheduled despite actionable comments; upsert error must not block dispatch")
	}
	if metrics.botReviewChecks["dispatched"] != 1 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 1`, metrics.botReviewChecks["dispatched"])
	}
}

// TestReconcileBotReviewComments_GetFingerprintError_ProceedsWithoutDedup
// verifies that a GetReactionFingerprint failure is logged but dispatch still
// proceeds without the dedup short-circuit.
func TestReconcileBotReviewComments_GetFingerprintError_ProceedsWithoutDedup(t *testing.T) {
	t.Parallel()

	state := stateWithBotReviewReaction(t, "BOT-GETFP", 10)
	rkey := ReactionKey("BOT-GETFP", ReactionKindBotReview)
	store := &reviewReconcileStore{
		getFingerprintErr: errors.New("get failed"),
	}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: []domain.ReviewComment{{ID: "g1", Body: "fix me"}}}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after dispatch despite get-fingerprint error; want consumed")
	}
	if _, ok := state.RetryAttempts["BOT-GETFP"]; !ok {
		t.Error("retry not scheduled; get-fingerprint error must fall through to dispatch")
	}
	if metrics.botReviewChecks["dispatched"] != 1 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 1`, metrics.botReviewChecks["dispatched"])
	}
}

// TestEscalateBotReviewFailure_LabelDefaultsWhenEmpty verifies that the "label"
// escalation uses the "needs-human" default when EscalationLabel is empty.
func TestEscalateBotReviewFailure_LabelDefaultsWhenEmpty(t *testing.T) {
	t.Parallel()

	issueID := "ESC-LABEL-DEFAULT"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 10, Owner: "o", Repo: "r"},
	}
	state.PendingReactions[botKey] = botPending

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &botReviewErrTrackerStub{}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "", // empty → defaults to needs-human
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalls != 1 {
		t.Fatalf("AddLabel calls = %d, want 1", tracker.addLabelCalls)
	}
	if tracker.lastLabel != "needs-human" {
		t.Errorf("AddLabel label = %q, want %q (default)", tracker.lastLabel, "needs-human")
	}
	if metrics.botReviewEscalations["label"] != 1 {
		t.Errorf(`IncBotReviewEscalations("label") = %d, want 1`, metrics.botReviewEscalations["label"])
	}
}

// TestEscalateBotReviewFailure_LabelActionError verifies that a failing AddLabel
// records the "error" escalation metric and still removes the bot-review slot.
func TestEscalateBotReviewFailure_LabelActionError(t *testing.T) {
	t.Parallel()

	issueID := "ESC-LABEL-ERR"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 11, Owner: "o", Repo: "r"},
	}
	state.PendingReactions[botKey] = botPending

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &botReviewErrTrackerStub{addLabelErr: errors.New("label api failed")}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalls != 1 {
		t.Fatalf("AddLabel calls = %d, want 1", tracker.addLabelCalls)
	}
	if metrics.botReviewEscalations["error"] != 1 {
		t.Errorf(`IncBotReviewEscalations("error") = %d, want 1 on AddLabel failure`, metrics.botReviewEscalations["error"])
	}
	if metrics.botReviewEscalations["label"] != 0 {
		t.Errorf(`IncBotReviewEscalations("label") = %d, want 0 on AddLabel failure`, metrics.botReviewEscalations["label"])
	}
	if _, ok := state.PendingReactions[botKey]; ok {
		t.Error("bot-review slot still present after label-error escalation; want removed")
	}
}

// TestEscalateBotReviewFailure_CommentActionError verifies that a failing
// CommentIssue records the "error" escalation metric.
func TestEscalateBotReviewFailure_CommentActionError(t *testing.T) {
	t.Parallel()

	issueID := "ESC-COMMENT-ERR"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 21, Owner: "o", Repo: "r"},
	}
	state.PendingReactions[botKey] = botPending

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &botReviewErrTrackerStub{commentErr: errors.New("comment api failed")}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "comment",
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.commentCalls != 1 {
		t.Fatalf("CommentIssue calls = %d, want 1", tracker.commentCalls)
	}
	if metrics.botReviewEscalations["error"] != 1 {
		t.Errorf(`IncBotReviewEscalations("error") = %d, want 1 on CommentIssue failure`, metrics.botReviewEscalations["error"])
	}
	if metrics.botReviewEscalations["comment"] != 0 {
		t.Errorf(`IncBotReviewEscalations("comment") = %d, want 0 on CommentIssue failure`, metrics.botReviewEscalations["comment"])
	}
}

// TestEscalateBotReviewFailure_DeleteFingerprintError verifies that a failing
// DeleteReactionFingerprint during escalation is logged but does not panic and
// the bot-review slot is still removed.
func TestEscalateBotReviewFailure_DeleteFingerprintError(t *testing.T) {
	t.Parallel()

	issueID := "ESC-DELFP-ERR"
	state := NewState(5000, 4, nil, AgentTotals{})
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	botPending := &PendingReaction{
		IssueID:   issueID,
		Kind:      ReactionKindBotReview,
		CreatedAt: botReviewBaseTime,
		KindData:  &BotReviewReactionData{PRNumber: 31, Owner: "o", Repo: "r"},
	}
	state.PendingReactions[botKey] = botPending

	store := &reviewReconcileStore{deleteFingerprintErr: errors.New("delete failed")}
	metrics := newBotReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	params := botReviewParams(store, &mockSCMAdapter{}, tracker)
	params.BotReviewConfig = BotReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		MaxContinuationTurns: 5,
		PollIntervalMS:       60000,
	}

	botData := botPending.KindData.(*BotReviewReactionData)

	// Must not panic despite the delete error.
	escalateBotReviewFailure(state, params, botPending, 5, EscalationTriggerBudget, botData, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if store.deleteFingerprintCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1", store.deleteFingerprintCalls)
	}
	if _, ok := state.PendingReactions[botKey]; ok {
		t.Error("bot-review slot still present after delete-fingerprint error; want removed")
	}
}

// --- coexistence with the review reaction kind ---

// TestReconcileBotReviewComments_CoexistsWithReview verifies how the review
// and bot-review passes interact when both kinds are pending for the same
// issue in one reconcile cycle.
//
// The per-kind tracking state stays independent: each pass advances only its
// own fingerprint row and attempt counter and consumes only its own pending
// slot. The retry slot is shared: RetryAttempts is keyed by issue ID alone,
// so the later bot-review pass finds the review retry occupying the slot and
// defers to it instead of overwriting it. The deferral leaves the review
// continuation as the incumbent and re-enqueues the bot-review pending entry
// unconsumed, so bot-review re-detects on the next tick once the slot frees.
func TestReconcileBotReviewComments_CoexistsWithReview(t *testing.T) {
	t.Parallel()

	issueID := "BOT-COEXIST"
	state := NewState(5000, 4, nil, AgentTotals{})
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[reviewKey] = newReviewPendingEntry(issueID, 55)
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	state.PendingReactions[botKey] = makeBotReviewPendingEntry(t, issueID, 55)
	state.Claimed[issueID] = struct{}{}

	scm := &mockSCMAdapter{
		comments: []domain.ReviewComment{
			{ID: "human-1", Body: "please rename this variable", SubmittedAt: botReviewBaseTime.Add(-5 * time.Minute)},
		},
		botComments: []domain.ReviewComment{
			{ID: "bot-1", Body: "lint: line too long", SubmittedAt: botReviewBaseTime.Add(-2 * time.Second)},
		},
	}
	store := newStatefulFingerprintStore()
	reviewMetrics := newReviewMetricsSpy()
	botMetrics := newBotReviewMetricsSpy()
	params := ReconcileParams{
		SCMAdapter:          scm,
		ReviewConfig:        defaultReviewConfig(),
		BotReviewConfig:     defaultBotReviewConfig(),
		BotReviewConfigured: true,
		Store:               store,
		OnRetryFire:         noopRetryFire,
		Ctx:                 context.Background(),
		Logger:              discardLogger(),
		NowFunc:             func() time.Time { return botReviewBaseTime },
	}

	reconcileReviewComments(state, params, discardLogger(), context.Background(), reviewMetrics)

	// The review pass schedules its continuation retry in the issue-ID-keyed
	// slot, becoming the incumbent the bot-review pass defers to below.
	reviewRetry, reviewScheduled := state.RetryAttempts[issueID]
	if !reviewScheduled {
		t.Fatal("retry not scheduled after review dispatch; want scheduled")
	}
	if reviewRetry.ReactionKind != ReactionKindReview {
		t.Errorf("review RetryEntry.ReactionKind = %q, want %q", reviewRetry.ReactionKind, ReactionKindReview)
	}
	if _, ok := reviewRetry.ContinuationContext["review_comments"]; !ok {
		t.Error("review RetryEntry.ContinuationContext missing review_comments key")
	}
	reviewAttempt := reviewRetry.Attempt

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), botMetrics)

	// The retry slot still holds the review incumbent, untouched by the
	// deferring bot-review pass.
	botRetry, stillScheduled := state.RetryAttempts[issueID]
	if !stillScheduled {
		t.Fatal("retry entry removed from slot; want review incumbent preserved")
	}
	if botRetry != reviewRetry {
		t.Error("RetryAttempts[issueID] replaced; want the original review entry preserved")
	}
	if botRetry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryAttempts[issueID].ReactionKind = %q, want %q (review incumbent survives)", botRetry.ReactionKind, ReactionKindReview)
	}
	if botRetry.Attempt != reviewAttempt {
		t.Errorf("RetryAttempts[issueID].Attempt = %d, want %d (unchanged by deferral)", botRetry.Attempt, reviewAttempt)
	}
	if len(state.RetryAttempts) != 1 {
		t.Errorf("len(RetryAttempts) = %d, want 1 (bot-review defers instead of taking a second slot)", len(state.RetryAttempts))
	}

	if reviewMetrics.reviewChecks["dispatched"] != 1 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, reviewMetrics.reviewChecks["dispatched"])
	}
	if botMetrics.botReviewChecks["dispatched"] != 0 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 0 (deferred, not dispatched)`, botMetrics.botReviewChecks["dispatched"])
	}
	if state.ReactionAttempts[reviewKey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", reviewKey, state.ReactionAttempts[reviewKey])
	}
	if _, ok := state.ReactionAttempts[botKey]; ok {
		t.Errorf("ReactionAttempts[%s] present, want absent (deferral does not advance the counter)", botKey)
	}

	if _, ok := state.PendingReactions[reviewKey]; ok {
		t.Error("review PendingReactions entry still present; want consumed by its own dispatch")
	}
	botPending, botReenqueued := state.PendingReactions[botKey]
	if !botReenqueued {
		t.Fatal("bot-review PendingReactions entry missing; want re-enqueued by the deferral")
	}
	if botPending.CreatedAt != botReviewBaseTime {
		t.Errorf("bot-review PendingReactions.CreatedAt = %v, want %v (refreshed to the tick's now)", botPending.CreatedAt, botReviewBaseTime)
	}

	reviewFingerprint := store.fingerprints[reviewKey].fingerprint
	if reviewFingerprint == "" {
		t.Error("review fingerprint row is empty; want a non-empty hash after review dispatch")
	}
	// The fingerprint upsert is per-tick bookkeeping that runs before the
	// arbitration decision, so it still records the observed comment set;
	// only the dispatched flag distinguishes a deferral from a dispatch.
	if store.fingerprints[botKey].dispatched {
		t.Error("bot-review fingerprint marked dispatched; want undispatched after a deferral")
	}

	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("state.Claimed[issueID] cleared during deferral; want preserved")
	}
}

// --- Triage gate integration ---

// botReviewTriageParams returns botReviewParams wired with a real
// workspace and the given triage script, so reactionTriageGate actually
// starts a subprocess for the pass's actionable comment set.
func botReviewTriageParams(store *reviewReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter, workspaceRoot, script string) ReconcileParams {
	params := botReviewParams(store, scm, tracker)
	params.WorkspaceRoot = workspaceRoot
	params.BotReviewConfig.Triage = config.ReactionTriageConfig{Script: script, TimeoutMS: 5000}
	return params
}

// actionableBotReviewComments returns one non-outdated bot comment.
func actionableBotReviewComments() []domain.ReviewComment {
	return []domain.ReviewComment{{ID: "bc-1", Body: "fix this"}}
}

// runBotReviewTriageToCompletion drives a pass that starts a triage run
// for issueID, waits for the subprocess to finish, then resets the
// entry's PendingRetryAt to the past so the next pass is immediately
// due.
func runBotReviewTriageToCompletion(t *testing.T, state *State, params ReconcileParams, rkey string, metrics domain.Metrics) {
	t.Helper()
	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)
	entry, ok := state.PendingReactions[rkey]
	if !ok || entry.Triage == nil {
		t.Fatalf("PendingReactions[%s] = %+v, want a started triage run", rkey, entry)
	}
	waitTriageRunDone(t, entry.Triage)
	entry.PendingRetryAt = time.Time{}
}

// TestReconcileBotReviewComments_Triage_NoConfig_BehavesAsPinned
// verifies that a bot-review reaction with no triage block dispatches
// exactly as the pinned revision.
func TestReconcileBotReviewComments_Triage_NoConfig_BehavesAsPinned(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-OFF"
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)
	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
	params := botReviewParams(store, scm, nil)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a scheduled continuation, want dropped (pinned behavior)")
	}
	if metrics.botReviewChecks["dispatched"] != 1 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 1`, metrics.botReviewChecks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
}

// TestReconcileBotReviewComments_Triage_WaitsWithoutProviderCall
// verifies that while a triage run is in flight, the pass re-enqueues
// without making a provider call and without incrementing
// PendingAttempts.
func TestReconcileBotReviewComments_Triage_WaitsWithoutProviderCall(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-WAIT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-wait", func() {})

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
	params := botReviewTriageParams(store, scm, nil, root, handledScript)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if scm.botCalls != 0 {
		t.Errorf("FetchBotReviewComments calls = %d, want 0 while a triage run is in flight", scm.botCalls)
	}
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped while waiting on triage, want re-enqueued")
	}
	if entry.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0 (waiting is not a fetch error)", entry.PendingAttempts)
	}
}

// TestReconcileBotReviewComments_Triage_Handled verifies that a handled
// disposition marks the fingerprint dispatched and re-enqueues the
// entry with the poll interval, without spending a continuation or
// incrementing IncBotReviewChecks("dispatched").
func TestReconcileBotReviewComments_Triage_Handled(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-HANDLED"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
	params := botReviewTriageParams(store, scm, nil, root, handledScript)

	runBotReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if !entry.PendingRetryAt.After(botReviewBaseTime) {
		t.Errorf("PendingRetryAt = %v, want after %v (re-enqueued with the poll interval)", entry.PendingRetryAt, botReviewBaseTime)
	}
	if state.ReactionAttempts[rkey] != 0 {
		t.Errorf("ReactionAttempts[%s] = %d, want 0 (a handled verdict must not spend a continuation)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.botReviewChecks["dispatched"] != 0 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 0 on a handled pass`, metrics.botReviewChecks["dispatched"])
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileBotReviewComments_Triage_Escalate verifies that an
// escalate disposition invokes escalateBotReviewFailure with
// EscalationTriggerTriage.
func TestReconcileBotReviewComments_Triage_Escalate(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-ESCALATE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &ciTrackerStub{}
	scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
	params := botReviewTriageParams(store, scm, tracker, root, escalateTriageScript)

	runBotReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (triage escalation uses the kind's own escalation action)", tracker.addLabelCalled)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (matches a budget escalation)")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileBotReviewComments_Triage_DispatchAgent_ProceedsNormally
// verifies that a dispatch-agent disposition falls through to the
// existing dispatch block, incrementing IncBotReviewChecks("dispatched").
func TestReconcileBotReviewComments_Triage_DispatchAgent_ProceedsNormally(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-DISPATCH"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
	params := botReviewTriageParams(store, scm, nil, root, dispatchAgentTriageScript)

	runBotReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if metrics.botReviewChecks["dispatched"] != 1 {
		t.Errorf(`IncBotReviewChecks("dispatched") = %d, want 1`, metrics.botReviewChecks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 for dispatch-agent", store.markDispatchedCalls)
	}
}

// TestReconcileBotReviewComments_Triage_CancelOnTTLDrop verifies that an
// in-flight triage run is cancelled before the entry is dropped on TTL
// elapse.
func TestReconcileBotReviewComments_Triage_CancelOnTTLDrop(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-DROP"
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)
	spy := &triageCancelSpy{}
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-drop", spy.cancel)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := botReviewParams(store, scm, nil)
	params.BotReviewPendingTTL = 1 * time.Minute
	params.NowFunc = func() time.Time { return botReviewBaseTime.Add(2 * time.Minute) }

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if spy.calls() != 1 {
		t.Errorf("Cancel called %d times, want 1 (the in-flight run must not outlive the dropped entry)", spy.calls())
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the TTL, want dropped")
	}
}

// TestReconcileBotReviewComments_Triage_RepeatedHandled_StillAgesOut
// pins the bound bot-review carries and ci does not: the TTL is
// measured from the entry's creation, so a succession of handled
// answers still ages the entry out once that TTL elapses.
func TestReconcileBotReviewComments_Triage_RepeatedHandled_StillAgesOut(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-AGESOUT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
	params := botReviewTriageParams(store, scm, nil, root, handledScript)
	params.BotReviewPendingTTL = 1 * time.Minute

	runBotReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics) // applies: handled

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Fatal("PendingReactions entry dropped right after a handled verdict, want retained until TTL")
	}

	// Advance past the TTL and confirm it still drops despite the
	// retained handled outcome.
	params.NowFunc = func() time.Time { return botReviewBaseTime.Add(2 * time.Minute) }
	state.PendingReactions[rkey].PendingRetryAt = time.Time{}

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the TTL despite a handled verdict, want dropped")
	}
}

// TestReconcileBotReviewComments_Triage_EpisodeCloseClearsHandledForNextEpisode
// pins cancelReactionTriage's detach-on-close contract: a memoized
// handled verdict from one episode must not survive into the next.
// Without it, a later episode that recomputes the identical
// actionable-comment fingerprint (the same bot comment reappearing
// after a push cleared it) would replay the stale handled outcome from
// memory instead of running the newly configured command, silently
// suppressing the real verdict.
func TestReconcileBotReviewComments_Triage_EpisodeCloseClearsHandledForNextEpisode(t *testing.T) {
	t.Parallel()

	const issueID = "BOT-TRIAGE-EPISODE-CLOSE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithBotReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindBotReview)

	store := &reviewReconcileStore{}
	metrics := newBotReviewMetricsSpy()
	tracker := &ciTrackerStub{}
	scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
	params := botReviewTriageParams(store, scm, tracker, root, handledScript)

	// Episode 1: one actionable bot comment; the command answers
	// handled, memoizing the verdict on pending.Triage rather than
	// re-running it on the next pass over the same comment set.
	runBotReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if entry.Triage == nil {
		t.Fatal("PendingReaction.Triage cleared inside its own episode, want the memoized handle retained")
	}
	entry.PendingRetryAt = time.Time{}

	// The episode closes: the bot comment set goes empty, taking the
	// no-actionable-comments branch that must cancel the retained
	// handle before re-enqueueing.
	scm.botComments = nil

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok = state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped when the episode closed, want re-enqueued")
	}
	if entry.Triage != nil {
		t.Fatal("PendingReaction.Triage survived the episode close, want detached so a later identical comment set cannot replay it")
	}
	entry.PendingRetryAt = time.Time{}

	// Episode 2: the same bot comment reappears, recomputing the
	// identical fingerprint. The command now answers escalate; a
	// cleared handle must run it fresh rather than replay episode 1's
	// memoized handled verdict.
	scm.botComments = actionableBotReviewComments()
	params.BotReviewConfig.Triage = config.ReactionTriageConfig{Script: escalateTriageScript, TimeoutMS: 5000}

	runBotReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (the replayed handled verdict from episode 1 must not suppress the fresh escalate)", tracker.addLabelCalled)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (the replayed handled verdict from episode 1 must not suppress it)")
	}
	if store.markDispatchedCalls != 2 {
		t.Errorf("MarkReactionDispatched calls = %d, want 2 (one real verdict per episode: handled, then escalate)", store.markDispatchedCalls)
	}
}
