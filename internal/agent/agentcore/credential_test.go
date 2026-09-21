package agentcore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

type scriptedCredentialAdapter struct {
	startFn func(ctx context.Context, params domain.StartSessionParams) (domain.Session, error)
	runFn   func(ctx context.Context) (domain.TurnResult, error)
	stopFn  func(ctx context.Context) error

	calls []string
}

var _ domain.AgentAdapter = (*scriptedCredentialAdapter)(nil)

func (a *scriptedCredentialAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	a.calls = append(a.calls, "start")
	if a.startFn != nil {
		return a.startFn(ctx, params)
	}
	return domain.Session{ID: "sess-verify"}, nil
}

func (a *scriptedCredentialAdapter) RunTurn(ctx context.Context, _ domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
	a.calls = append(a.calls, "run")
	if a.runFn != nil {
		return a.runFn(ctx)
	}
	return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
}

func (a *scriptedCredentialAdapter) StopSession(ctx context.Context, _ domain.Session) error {
	a.calls = append(a.calls, "stop")
	if a.stopFn != nil {
		return a.stopFn(ctx)
	}
	return nil
}

func TestVerifyCredential_Verdict(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection reset by peer")
	cancelledStart := errors.New("adapter-specific start cancellation")
	cancelledRun := errors.New("adapter-specific cancellation")
	notFound := &domain.AgentError{Kind: domain.ErrAgentNotFound, Message: "no binary"}
	startTimeout := &domain.AgentError{Kind: domain.ErrResponseTimeout, Message: "timed out"}
	connFailed := ConnectionFailedError()

	const boundMessage = "the agent runtime did not complete a credential verification request within 20 ms"

	tests := []struct {
		name      string
		start     func(ctx context.Context, cancel context.CancelFunc) error
		run       func(ctx context.Context, cancel context.CancelFunc) (domain.TurnResult, error)
		turnBound time.Duration

		wantUnchanged error
		wantNil       bool
		wantMessage   string
		wantCause     error
	}{
		{
			name:          "ctx ended during StartSession returns its error unchanged",
			start:         func(_ context.Context, cancel context.CancelFunc) error { cancel(); return cancelledStart },
			wantUnchanged: cancelledStart,
		},
		{
			name: "ctx ended during RunTurn returns its error unchanged",
			run: func(_ context.Context, cancel context.CancelFunc) (domain.TurnResult, error) {
				cancel()
				return domain.TurnResult{}, cancelledRun
			},
			wantUnchanged: cancelledRun,
		},
		{
			name: "ctx ended during RunTurn with a nil error must not report success",
			run: func(_ context.Context, cancel context.CancelFunc) (domain.TurnResult, error) {
				cancel()
				return domain.TurnResult{}, nil
			},
			wantUnchanged: context.Canceled,
		},
		{
			name: "turn bound elapsed",
			run: func(ctx context.Context, _ context.CancelFunc) (domain.TurnResult, error) {
				<-ctx.Done()
				return domain.TurnResult{}, ctx.Err()
			},
			turnBound:   20 * time.Millisecond,
			wantMessage: boundMessage,
		},
		{
			name: "turn bound is checked before a nil RunTurn with a non-completed exit",
			run: func(ctx context.Context, _ context.CancelFunc) (domain.TurnResult, error) {
				<-ctx.Done()
				return domain.TurnResult{ExitReason: domain.EventTurnCancelled}, nil
			},
			turnBound:   20 * time.Millisecond,
			wantMessage: boundMessage,
		},
		{
			name: "ssh connection failure returned unchanged",
			run: func(context.Context, context.CancelFunc) (domain.TurnResult, error) {
				return domain.TurnResult{}, connFailed
			},
			wantUnchanged: sshutil.ErrConnectionFailed,
		},
		{
			name: "non-retryable kind returned unchanged",
			run: func(context.Context, context.CancelFunc) (domain.TurnResult, error) {
				return domain.TurnResult{}, notFound
			},
			wantUnchanged: notFound,
		},
		{
			name: "credential_unverified from RunTurn returned unchanged",
			run: func(context.Context, context.CancelFunc) (domain.TurnResult, error) {
				return domain.TurnResult{}, CredentialUnverifiedError("already unverified", cause)
			},
			wantMessage: "the agent runtime did not complete a credential verification request: already unverified",
		},
		{
			name:          "other StartSession error returned unchanged",
			start:         func(context.Context, context.CancelFunc) error { return startTimeout },
			wantUnchanged: startTimeout,
		},
		{
			name:        "turn bound elapsed during StartSession",
			start:       func(ctx context.Context, _ context.CancelFunc) error { <-ctx.Done(); return ctx.Err() },
			turnBound:   20 * time.Millisecond,
			wantMessage: boundMessage,
		},
		{
			name:    "turn completed passes",
			wantNil: true,
		},
		{
			name: "turn_incomplete passes",
			run: func(context.Context, context.CancelFunc) (domain.TurnResult, error) {
				return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnIncomplete, Message: "no report"}
			},
			wantNil: true,
		},
		{
			name: "other RunTurn error carries its message and cause, never its kind",
			run: func(context.Context, context.CancelFunc) (domain.TurnResult, error) {
				return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "the request failed", Err: cause}
			},
			wantMessage: "the agent runtime did not complete a credential verification request: the request failed",
			wantCause:   cause,
		},
		{
			name: "nil RunTurn with a non-completed exit",
			run: func(context.Context, context.CancelFunc) (domain.TurnResult, error) {
				return domain.TurnResult{ExitReason: domain.EventTurnCancelled}, nil
			},
			wantMessage: "the agent runtime did not complete a credential verification request: the request ended as turn_cancelled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			adapter := &scriptedCredentialAdapter{}
			if tt.start != nil {
				adapter.startFn = func(ctx context.Context, _ domain.StartSessionParams) (domain.Session, error) {
					return domain.Session{}, tt.start(ctx, cancel)
				}
			}
			if tt.run != nil {
				adapter.runFn = func(ctx context.Context) (domain.TurnResult, error) { return tt.run(ctx, cancel) }
			}

			_, err := VerifyCredential(ctx, adapter, CredentialVerification{TurnBound: tt.turnBound})

			switch {
			case tt.wantNil:
				if err != nil {
					t.Fatalf("VerifyCredential() error = %v, want nil", err)
				}
			case tt.wantUnchanged != nil:
				if !errors.Is(err, tt.wantUnchanged) {
					t.Fatalf("VerifyCredential() error = %v, want %v unchanged", err, tt.wantUnchanged)
				}
				if ae, ok := errors.AsType[*domain.AgentError](err); ok && ae.Kind == domain.ErrCredentialUnverified {
					t.Errorf("VerifyCredential() error kind = %q, want the adapter's own kind", ae.Kind)
				}
			default:
				ae, ok := errors.AsType[*domain.AgentError](err)
				if !ok || ae.Kind != domain.ErrCredentialUnverified {
					t.Fatalf("VerifyCredential() error = %#v, want a credential_unverified *domain.AgentError", err)
				}
				if ae.Message != tt.wantMessage {
					t.Errorf("AgentError.Message = %q, want %q", ae.Message, tt.wantMessage)
				}
				if strings.Contains(err.Error(), string(domain.ErrTurnFailed)) {
					t.Errorf("VerifyCredential() error = %q, must not render the adapter's own kind", err)
				}
				if tt.wantCause != nil && !errors.Is(err, tt.wantCause) {
					t.Errorf("VerifyCredential() error = %v, want it to wrap %v", err, tt.wantCause)
				}
			}
		})
	}
}

func TestVerifyCredential_Lifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		startErr  error
		run       func(context.Context) (domain.TurnResult, error)
		stopErr   error
		wantPanic bool
		wantErr   bool
		wantCalls []string
	}{
		{
			name:      "success",
			wantCalls: []string{"start", "request", "run", "stop"},
		},
		{
			name: "RunTurn error",
			run: func(context.Context) (domain.TurnResult, error) {
				return domain.TurnResult{}, errors.New("boom")
			},
			wantErr:   true,
			wantCalls: []string{"start", "request", "run", "stop"},
		},
		{
			name:      "failed StartSession runs neither OnRequest nor StopSession",
			startErr:  errors.New("start failed"),
			wantErr:   true,
			wantCalls: []string{"start"},
		},
		{
			name:      "RunTurn panic stops the session and propagates",
			run:       func(context.Context) (domain.TurnResult, error) { panic("run turn exploded") },
			wantPanic: true,
			wantCalls: []string{"start", "request", "run", "stop"},
		},
		{
			name:      "StopSession failure changes nothing else",
			stopErr:   errors.New("stop failed"),
			wantCalls: []string{"start", "request", "run", "stop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter := &scriptedCredentialAdapter{
				runFn:  tt.run,
				stopFn: func(context.Context) error { return tt.stopErr },
			}
			if tt.startErr != nil {
				adapter.startFn = func(context.Context, domain.StartSessionParams) (domain.Session, error) {
					return domain.Session{}, tt.startErr
				}
			}

			var err error
			panicked := func() (panicked bool) {
				defer func() { panicked = recover() != nil }()
				_, err = VerifyCredential(context.Background(), adapter, CredentialVerification{
					OnRequest: func() { adapter.calls = append(adapter.calls, "request") },
				})
				return false
			}()

			if panicked != tt.wantPanic {
				t.Errorf("panicked = %v, want %v", panicked, tt.wantPanic)
			}
			if !tt.wantPanic && (err != nil) != tt.wantErr {
				t.Errorf("VerifyCredential() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(adapter.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", adapter.calls, tt.wantCalls)
			}
		})
	}
}

func TestVerifyCredential_ForcesVerificationSessionFields(t *testing.T) {
	t.Parallel()

	var got domain.StartSessionParams
	var stopCtxErr error
	adapter := &scriptedCredentialAdapter{
		startFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
			got = params
			return domain.Session{ID: "sess-verify"}, nil
		},
		stopFn: func(ctx context.Context) error {
			stopCtxErr = ctx.Err()
			return nil
		},
	}

	_, err := VerifyCredential(context.Background(), adapter, CredentialVerification{
		Session: domain.StartSessionParams{
			ResumeSessionID: "resume-123",
			MCPConfigPath:   "/ws/.sortie/mcp.json",
		},
	})
	if err != nil {
		t.Fatalf("VerifyCredential() error = %v, want nil", err)
	}

	if !got.CredentialVerification || got.ResumeSessionID != "" || got.MCPConfigPath != "" {
		t.Errorf("StartSessionParams = {CredentialVerification: %v, ResumeSessionID: %q, MCPConfigPath: %q}, want {true, \"\", \"\"}",
			got.CredentialVerification, got.ResumeSessionID, got.MCPConfigPath)
	}
	if stopCtxErr != nil {
		t.Errorf("StopSession context.Err() = %v, want nil with no StopBound", stopCtxErr)
	}
}

func TestCredentialErrorBuilders(t *testing.T) {
	t.Parallel()

	cause := errors.New("underlying cause")
	tests := []struct {
		name        string
		err         *domain.AgentError
		wantMessage string
	}{
		{
			name:        "refused",
			err:         CredentialRefusedError("API key is invalid.", cause),
			wantMessage: "the agent runtime refused its credential: API key is invalid.",
		},
		{
			name:        "absent",
			err:         CredentialAbsentError("whoami exited with status 1", cause),
			wantMessage: "the agent runtime reports no usable credential: whoami exited with status 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.err.Kind != domain.ErrCredentialUnverified || tt.err.Message != tt.wantMessage {
				t.Errorf("error = {%q, %q}, want {%q, %q}", tt.err.Kind, tt.err.Message, domain.ErrCredentialUnverified, tt.wantMessage)
			}
			if !errors.Is(tt.err, cause) {
				t.Errorf("error does not wrap its cause")
			}
		})
	}
}
