package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func snapshotRetryRow(t *testing.T, snapshot func() RuntimeSnapshotResult, issueID string) SnapshotRetryEntry {
	t.Helper()

	snap := snapshot()
	i := slices.IndexFunc(snap.Retrying, func(r SnapshotRetryEntry) bool { return r.IssueID == issueID })
	if i < 0 {
		return SnapshotRetryEntry{}
	}
	return snap.Retrying[i]
}

func TestRetryNow(t *testing.T) {
	t.Parallel()

	const (
		retryID         = "retry-1"
		retryIdentifier = "RETRY-1"
		otherID         = "other-1"
		seededAttempt   = 2
	)

	tests := []struct {
		name         string
		identifier   string
		reactionKind string
		issueState   string
		otherRunning bool
		wantStatus   RetryNowStatus
		wantStarted  bool
		wantPaused   bool
		wantError    string
	}{
		{
			name: "free slot starts a session", identifier: retryIdentifier, issueState: "To Do",
			wantStatus: RetryNowDue, wantStarted: true,
		},
		{
			name: "paused retry stays paused", identifier: retryIdentifier, reactionKind: ReactionKindReview, issueState: "QA Review",
			wantStatus: RetryNowDue, wantPaused: true,
		},
		{
			name: "busy slot keeps the retry waiting", identifier: retryIdentifier, issueState: "To Do", otherRunning: true,
			wantStatus: RetryNowDue, wantError: "no available orchestrator slots",
		},
		{
			name: "unknown identifier changes nothing", identifier: "NOPE-1", issueState: "To Do",
			wantStatus: RetryNowNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := defaultWorkerConfig(t.TempDir())
			cfg.Tracker.Kind = "mock"
			cfg.Tracker.HandoffState = "Ready For Review"
			cfg.Polling = config.PollingConfig{IntervalMS: int(time.Hour / time.Millisecond)}
			cfg.Agent.MaxConcurrentAgents = 1

			var started sync.Map
			agent := &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					started.Store(filepath.Base(params.WorkspacePath), true)
					return domain.Session{ID: "sess-1"}, nil
				},
				runTurnFn: func(ctx context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					<-ctx.Done()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCancelled}, ctx.Err()
				},
			}
			tracker := &candidateTrackerAdapter{
				mockTrackerAdapter: &mockTrackerAdapter{
					fetchIssueByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
						return domain.Issue{ID: id, Identifier: retryIdentifier, Title: "retry", State: tt.issueState}, nil
					},
				},
				fetchCandidatesFn: func(context.Context) ([]domain.Issue, error) {
					if !tt.otherRunning {
						return nil, nil
					}
					return []domain.Issue{{ID: otherID, Identifier: "OTHER-1", Title: "other", State: "To Do"}}, nil
				},
			}

			pf := passingPreflightRegistries()
			pf.ReloadWorkflow = func() error { return nil }
			pf.ConfigFunc = func() config.ServiceConfig { return cfg }

			state := NewState(cfg.Polling.IntervalMS, 1, 0, nil, AgentTotals{})
			o := NewOrchestrator(OrchestratorParams{
				State:           state,
				Logger:          discardLogger(),
				TrackerAdapter:  tracker,
				AgentAdapter:    agent,
				WorkflowManager: &stubWorkflowManager{config: cfg, template: mustParseTemplate(t, "do {{ .issue.identifier }}")},
				Store:           &stubStore{},
				PreflightParams: pf,
			})
			state.Claimed[retryID] = struct{}{}
			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:             retryID,
				Identifier:          retryIdentifier,
				Attempt:             seededAttempt,
				DelayMS:             time.Hour.Milliseconds(),
				Error:               "worker exited: boom",
				ReactionKind:        tt.reactionKind,
				ContinuationContext: map[string]any{"review_comments": map[string]any{"count": 1}},
			}, o.onRetryFire)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				o.Run(ctx)
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})

			snapshot := o.SnapshotFunc()
			sessionStarted := func(identifier string) func() error {
				return func() error {
					if _, ok := started.Load(identifier); !ok {
						return fmt.Errorf("no session started for %q", identifier)
					}
					return nil
				}
			}
			if tt.otherRunning {
				waittest.Until(t, sessionStarted("OTHER-1"))
				waittest.Until(t, func() error {
					snap := snapshot()
					if len(snap.Running) != 1 {
						return fmt.Errorf("running = %d, want 1", len(snap.Running))
					}
					return nil
				})
			}
			var before SnapshotRetryEntry
			waittest.Until(t, func() error {
				before = snapshotRetryRow(t, snapshot, retryID)
				if before.IssueID == "" {
					return fmt.Errorf("no retry row for %q published yet", retryID)
				}
				return nil
			})

			result := o.RetryNowFunc()(ctx, tt.identifier)

			if result.Status != tt.wantStatus {
				t.Fatalf("RetryNowFunc()(%q).Status = %d, want %d", tt.identifier, result.Status, tt.wantStatus)
			}
			if tt.wantStarted {
				waittest.Until(t, sessionStarted(retryIdentifier))
				return
			}

			waittest.Until(t, func() error {
				row := snapshotRetryRow(t, snapshot, retryID)
				if row.Paused != tt.wantPaused || (tt.wantError != "" && row.Error != tt.wantError) {
					return fmt.Errorf("retry row Paused = %t, Error = %q, want Paused = %t, Error = %q", row.Paused, row.Error, tt.wantPaused, tt.wantError)
				}
				return nil
			})
			after := snapshotRetryRow(t, snapshot, retryID)
			if _, ok := started.Load(retryIdentifier); ok {
				t.Errorf("session started for %q, want none", retryIdentifier)
			}
			if tt.wantStatus == RetryNowNotFound {
				if !reflect.DeepEqual(after, before) {
					t.Errorf("retry row after unknown identifier = %+v, want %+v", after, before)
				}
				return
			}
			if after.Attempt != seededAttempt {
				t.Errorf("retry row Attempt = %d, want %d", after.Attempt, seededAttempt)
			}
			if drift := after.DueAtMS - before.DueAtMS; drift < -1000 || drift > 1000 {
				t.Errorf("retry row DueAtMS = %d, want %d (within 1000ms)", after.DueAtMS, before.DueAtMS)
			}
		})
	}
}
