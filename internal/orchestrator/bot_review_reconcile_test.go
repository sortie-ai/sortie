package orchestrator

import (
	"context"
	"errors"
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
			},
		},
		{
			name: "well-formed bot_usernames coerces to []string",
			rc: config.ReactionConfig{
				Extra: map[string]any{
					"bot_usernames": []any{"dependabot", "renovate-bot"},
				},
			},
			want: BotReviewReactionConfig{
				Escalation:           "label",
				EscalationLabel:      "needs-human",
				PollIntervalMS:       60000,
				MaxContinuationTurns: 5,
				BotUsernames:         []string{"dependabot", "renovate-bot"},
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
			},
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
	escalateBotReviewFailure(state, params, botPending, 5, botData, discardLogger(), context.Background(), metrics)
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
	escalateBotReviewFailure(state, params, botPending, 5, botData, discardLogger(), context.Background(), metrics)
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
	escalateBotReviewFailure(state, params, botPending, 5, botData, discardLogger(), context.Background(), metrics)
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
	escalateBotReviewFailure(state, params, botPending, 5, botData, discardLogger(), context.Background(), metrics)
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
	escalateBotReviewFailure(state, params, botPending, 5, botData, discardLogger(), context.Background(), metrics)
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
