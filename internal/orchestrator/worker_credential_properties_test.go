package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/prompt"
)

type credentialPropertyAdapter struct {
	verifyRunFn func(ctx context.Context, params domain.RunTurnParams) (domain.TurnResult, error)
	workRunFn   func(params domain.RunTurnParams) (domain.TurnResult, error)

	mu                  sync.Mutex
	workingStartCalls   int
	workingRunCalls     int
	workingStartParams  domain.StartSessionParams
	verifyWorkspacePath string
}

type credentialPropertySessionMeta struct{}

var _ domain.AgentAdapter = (*credentialPropertyAdapter)(nil)

func (a *credentialPropertyAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if params.CredentialVerification {
		a.mu.Lock()
		a.verifyWorkspacePath = params.WorkspacePath
		a.mu.Unlock()
		return domain.Session{ID: "sess-verify", Internal: &credentialPropertySessionMeta{}}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workingStartCalls++
	a.workingStartParams = params
	return domain.Session{ID: "sess-work"}, nil
}

func (a *credentialPropertyAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if _, verification := session.Internal.(*credentialPropertySessionMeta); verification {
		if a.verifyRunFn != nil {
			return a.verifyRunFn(ctx, params)
		}
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
	}
	a.mu.Lock()
	a.workingRunCalls++
	a.mu.Unlock()
	if a.workRunFn != nil {
		return a.workRunFn(params)
	}
	return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
}

func (a *credentialPropertyAdapter) StopSession(context.Context, domain.Session) error { return nil }

func (a *credentialPropertyAdapter) working() (start, run int, params domain.StartSessionParams) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workingStartCalls, a.workingRunCalls, a.workingStartParams
}

func (a *credentialPropertyAdapter) workspacePath() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verifyWorkspacePath
}

func runCredentialWorker(t *testing.T, ctx context.Context, cfg config.ServiceConfig, adapter domain.AgentAdapter, onEvent func(domain.AgentEvent)) WorkerResult {
	t.Helper()
	cfg.Agent.MaxTurns = 1
	ec := newExitCapture()
	RunWorkerAttempt(ctx, workerTestIssue(), nil, WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           adapter,
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
		ResumeSessionID:        "resume-abc",
		OnEvent: func(_ string, e domain.AgentEvent) {
			if onEvent != nil {
				onEvent(e)
			}
		},
		OnExit: ec.onExit,
		Logger: discardLogger(),
	})
	return ec.waitResult(t)
}

func TestRunWorkerAttempt_FailedCredentialStepReachesNoWorkingSession(t *testing.T) {
	t.Parallel()

	adapter := &credentialPropertyAdapter{
		verifyRunFn: func(context.Context, domain.RunTurnParams) (domain.TurnResult, error) {
			return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "the model rejected the request"}
		},
	}

	result := runCredentialWorker(t, context.Background(), defaultWorkerConfig(t.TempDir()), adapter, nil)

	if result.ExitKind != WorkerExitError {
		t.Errorf("WorkerResult.ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
	}
	if result.Error == nil || !strings.HasPrefix(result.Error.Error(), "agent session start: ") || !strings.Contains(result.Error.Error(), "credential_unverified") {
		t.Errorf("WorkerResult.Error = %v, want a credential_unverified error with the %q prefix a failed working StartSession reports", result.Error, "agent session start: ")
	}
	if start, run, _ := adapter.working(); start != 0 || run != 0 {
		t.Errorf("working StartSession/RunTurn calls = (%d, %d), want (0, 0)", start, run)
	}
}

func TestRunWorkerAttempt_PassingCredentialStepStartsTheWorkingSession(t *testing.T) {
	t.Parallel()

	adapter := &credentialPropertyAdapter{}

	result := runCredentialWorker(t, context.Background(), defaultWorkerConfig(t.TempDir()), adapter, nil)

	if result.ExitKind != WorkerExitNormal {
		t.Fatalf("WorkerResult.ExitKind = %q, want %q (error: %v)", result.ExitKind, WorkerExitNormal, result.Error)
	}
	start, run, params := adapter.working()
	if start != 1 || run != 1 {
		t.Errorf("working StartSession/RunTurn calls = (%d, %d), want (1, 1)", start, run)
	}
	if params.CredentialVerification || params.ResumeSessionID != "resume-abc" {
		t.Errorf("working StartSessionParams = {CredentialVerification: %v, ResumeSessionID: %q}, want {false, %q}", params.CredentialVerification, params.ResumeSessionID, "resume-abc")
	}
}

// TestRunWorkerAttempt_HandoffEvidenceBaselineCapturedBeforeCredentialVerification
// proves the baseline is read before credential verification touches the
// workspace: the verification turn below only turns the workspace into a
// Git repository once it runs, so a baseline captured afterward would find
// a repository where a baseline captured beforehand finds none.
func TestRunWorkerAttempt_HandoffEvidenceBaselineCapturedBeforeCredentialVerification(t *testing.T) {
	t.Parallel()

	adapter := &credentialPropertyAdapter{}
	adapter.verifyRunFn = func(context.Context, domain.RunTurnParams) (domain.TurnResult, error) {
		initGitRepo(t, adapter.workspacePath())
		return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
	}

	result := runCredentialWorker(t, context.Background(), defaultWorkerConfig(t.TempDir()), adapter, nil)

	if result.HandoffEvidenceBaselineError == nil {
		t.Error("WorkerResult.HandoffEvidenceBaselineError = nil, want a non-Git-workspace error: the baseline must be captured before credential verification can turn the workspace into a Git repository")
	}
}

func TestRunWorkerAttempt_CredentialStepBounds(t *testing.T) {
	t.Parallel()

	silent := func(ctx context.Context, _ domain.RunTurnParams) (domain.TurnResult, error) {
		<-ctx.Done()
		return domain.TurnResult{}, ctx.Err()
	}
	chatty := func(ctx context.Context, params domain.RunTurnParams) (domain.TurnResult, error) {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return domain.TurnResult{}, ctx.Err()
			case <-ticker.C:
				params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC(), Message: "still working"})
			}
		}
	}

	tests := []struct {
		name          string
		turnTimeoutMS int
		// cancelAfter stands in for stall detection cancelling the
		// running entry's context; zero never cancels.
		cancelAfter    time.Duration
		run            func(context.Context, domain.RunTurnParams) (domain.TurnResult, error)
		wantExit       WorkerExitKind
		wantUnverified bool
	}{
		{
			name:          "stall cancellation wins over the turn bound",
			turnTimeoutMS: 5000,
			cancelAfter:   50 * time.Millisecond,
			run:           silent,
			wantExit:      WorkerExitCancelled,
		},
		{
			name:           "turn bound ends a verification that keeps sending events",
			turnTimeoutMS:  100,
			run:            chatty,
			wantExit:       WorkerExitError,
			wantUnverified: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := defaultWorkerConfig(t.TempDir())
			cfg.Agent.TurnTimeoutMS = tt.turnTimeoutMS
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelAfter > 0 {
				time.AfterFunc(tt.cancelAfter, cancel)
			}
			adapter := &credentialPropertyAdapter{verifyRunFn: tt.run}
			limit := time.Duration(tt.turnTimeoutMS)*time.Millisecond + stopSessionDeadline(cfg) + 2*time.Second
			if tt.cancelAfter > 0 {
				limit = 2 * time.Second
			}

			startedAt := time.Now()
			result := runCredentialWorker(t, ctx, cfg, adapter, nil)

			if elapsed := time.Since(startedAt); elapsed > limit {
				t.Errorf("RunWorkerAttempt took %v, want at most %v", elapsed, limit)
			}
			if result.ExitKind != tt.wantExit {
				t.Fatalf("WorkerResult.ExitKind = %q, want %q (error: %v)", result.ExitKind, tt.wantExit, result.Error)
			}
			agentErr, isAgentErr := errors.AsType[*domain.AgentError](result.Error)
			gotUnverified := isAgentErr && agentErr.Kind == domain.ErrCredentialUnverified
			if gotUnverified != tt.wantUnverified {
				t.Errorf("WorkerResult.Error = %v, credential_unverified = %v, want %v", result.Error, gotUnverified, tt.wantUnverified)
			}
			wantBound := fmt.Sprintf("did not complete a credential verification request within %d ms", tt.turnTimeoutMS)
			if tt.wantUnverified && !strings.Contains(agentErr.Message, wantBound) {
				t.Errorf("AgentError.Message = %q, want it to contain %q", agentErr.Message, wantBound)
			}
			if start, run, _ := adapter.working(); start != 0 || run != 0 {
				t.Errorf("working StartSession/RunTurn calls = (%d, %d), want (0, 0)", start, run)
			}
		})
	}
}

func TestRunWorkerAttempt_RelayedVerificationEventsCarryOnlyTheVerificationShape(t *testing.T) {
	t.Parallel()

	adapter := &credentialPropertyAdapter{
		verifyRunFn: func(_ context.Context, params domain.RunTurnParams) (domain.TurnResult, error) {
			params.OnEvent(domain.AgentEvent{
				Type:           domain.EventTokenUsage,
				Timestamp:      time.Now().UTC(),
				SessionID:      "raw-session-id",
				AgentPID:       "raw-pid",
				Usage:          domain.TokenUsage{InputTokens: 80, OutputTokens: 20, TotalTokens: 100},
				Model:          "raw-model",
				Message:        "raw message",
				ToolName:       "raw-tool",
				ToolDurationMS: 999,
				RateLimits:     map[string]any{"limit": int64(5)},
			})
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: time.Now().UTC(),
				SessionID: "raw-session-id-2",
				Message:   "some notification text",
				ToolName:  "raw-tool-2",
			})
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		},
	}

	var received []domain.AgentEvent
	result := runCredentialWorker(t, context.Background(), defaultWorkerConfig(t.TempDir()), adapter, func(e domain.AgentEvent) {
		received = append(received, e)
	})
	if result.ExitKind != WorkerExitNormal {
		t.Fatalf("WorkerResult.ExitKind = %q, want %q (error: %v)", result.ExitKind, WorkerExitNormal, result.Error)
	}

	if len(received) != 3 {
		t.Fatalf("deps.OnEvent received %d events, want 3 (the verification start notification and the two relayed events): %+v", len(received), received)
	}
	for i, e := range received {
		if e.Type != domain.EventTokenUsage && e.Type != domain.EventNotification {
			t.Errorf("received[%d].Type = %q, want token_usage or notification", i, e.Type)
		}
		if e.Message != "verifying the agent credential" {
			t.Errorf("received[%d].Message = %q, want %q", i, e.Message, "verifying the agent credential")
		}
		if e.SessionID != "" || e.AgentPID != "" || e.ToolName != "" || e.ToolDurationMS != 0 || e.RateLimits != nil {
			t.Errorf("received[%d] = %+v, want no SessionID, AgentPID, tool field or rate limits", i, e)
		}
	}
	if received[1].Usage.TotalTokens != 100 || received[1].Model != "raw-model" {
		t.Errorf("received[1] usage = (%d, %q), want the token_usage event's own (100, %q)", received[1].Usage.TotalTokens, received[1].Model, "raw-model")
	}
}

func TestRunWorkerAttempt_TokenColumnsEqualVerificationPlusWorking(t *testing.T) {
	t.Parallel()

	verifyUsage := domain.TokenUsage{InputTokens: 80, OutputTokens: 20, TotalTokens: 100}
	workUsage := domain.TokenUsage{InputTokens: 150, OutputTokens: 50, TotalTokens: 200}
	reportUsage := func(params domain.RunTurnParams, usage domain.TokenUsage) (domain.TurnResult, error) {
		params.OnEvent(domain.AgentEvent{Type: domain.EventTokenUsage, Timestamp: time.Now().UTC(), Usage: usage, Model: "m"})
		return domain.TurnResult{SessionID: "sess", ExitReason: domain.EventTurnCompleted, Usage: usage, UsageMeasured: true}, nil
	}
	adapter := &credentialPropertyAdapter{
		verifyRunFn: func(_ context.Context, params domain.RunTurnParams) (domain.TurnResult, error) {
			return reportUsage(params, verifyUsage)
		},
		workRunFn: func(params domain.RunTurnParams) (domain.TurnResult, error) { return reportUsage(params, workUsage) },
	}

	result := runCredentialWorker(t, context.Background(), defaultWorkerConfig(t.TempDir()), adapter, nil)
	if result.ExitKind != WorkerExitNormal {
		t.Fatalf("WorkerResult.ExitKind = %q, want %q (error: %v)", result.ExitKind, WorkerExitNormal, result.Error)
	}

	want := domain.TokenUsage{
		InputTokens:  verifyUsage.InputTokens + workUsage.InputTokens,
		OutputTokens: verifyUsage.OutputTokens + workUsage.OutputTokens,
		TotalTokens:  verifyUsage.TotalTokens + workUsage.TotalTokens,
	}
	got := domain.TokenUsage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens, TotalTokens: result.Usage.TotalTokens}
	if got != want {
		t.Errorf("WorkerResult.Usage = %+v, want %+v", got, want)
	}
	if result.APIRequestCount < 2 || !result.UsageMeasured {
		t.Errorf("WorkerResult = {APIRequestCount: %d, UsageMeasured: %v}, want at least 2 requests counted and usage measured", result.APIRequestCount, result.UsageMeasured)
	}
}
