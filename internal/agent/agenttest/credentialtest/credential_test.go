package credentialtest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Prefixed so the fixture kinds cannot collide with a real adapter kind.
const (
	credentialTestKindWithCommand    = "agenttest-credential-with-command"
	credentialTestKindWithoutCommand = "agenttest-credential-without-command"
)

func credentialTestConstructor(map[string]any) (domain.AgentAdapter, error) {
	return nil, errors.New("credentialtest: fixture kind has no real adapter")
}

func init() {
	registry.Agents.RegisterWithMeta(credentialTestKindWithCommand, credentialTestConstructor, registry.AgentMeta{
		RequiresCommand: true,
	})
	registry.Agents.RegisterWithMeta(credentialTestKindWithoutCommand, credentialTestConstructor, registry.AgentMeta{
		RequiresCommand: false,
	})
}

type fakeReporter struct {
	errors []string
}

func (f *fakeReporter) Helper() {}

func (f *fakeReporter) Errorf(format string, args ...any) {
	f.errors = append(f.errors, format)
}

type scriptedAdapter struct {
	startErr error
	runErr   error
}

var _ domain.AgentAdapter = (*scriptedAdapter)(nil)

func (a *scriptedAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	if a.startErr != nil {
		return domain.Session{}, a.startErr
	}
	return domain.Session{ID: "sess-verify"}, nil
}

func (a *scriptedAdapter) RunTurn(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
	if a.runErr != nil {
		return domain.TurnResult{}, a.runErr
	}
	return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
}

func (a *scriptedAdapter) StopSession(context.Context, domain.Session) error { return nil }

func verifiedAdapter() domain.AgentAdapter {
	return &scriptedAdapter{}
}

func unverifiedAdapter() domain.AgentAdapter {
	return &scriptedAdapter{runErr: &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "refused"}}
}

func sshFailedAdapter() domain.AgentAdapter {
	return &scriptedAdapter{runErr: &domain.AgentError{Kind: domain.ErrPortExit, Message: "ssh connection failed", Err: sshutil.ErrConnectionFailed}}
}

type callTrackingAdapter struct {
	startErr error
	runErr   error
	stopErr  error
	calls    []string
}

var _ domain.AgentAdapter = (*callTrackingAdapter)(nil)

func (a *callTrackingAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	a.calls = append(a.calls, "start")
	if a.startErr != nil {
		return domain.Session{}, a.startErr
	}
	return domain.Session{ID: "sess-working"}, nil
}

func (a *callTrackingAdapter) RunTurn(context.Context, domain.Session, domain.RunTurnParams) (domain.TurnResult, error) {
	a.calls = append(a.calls, "run")
	if a.runErr != nil {
		return domain.TurnResult{}, a.runErr
	}
	return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
}

func (a *callTrackingAdapter) StopSession(context.Context, domain.Session) error {
	a.calls = append(a.calls, "stop")
	return a.stopErr
}

func TestRunWorkingLive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		adapter   *callTrackingAdapter
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "success runs start, run, and stop",
			adapter:   &callTrackingAdapter{},
			wantCalls: []string{"start", "run", "stop"},
		},
		{
			name:      "StartSession error skips RunTurn and StopSession",
			adapter:   &callTrackingAdapter{startErr: errors.New("start failed")},
			wantErr:   errors.New("start failed"),
			wantCalls: []string{"start"},
		},
		{
			name:      "RunTurn error is returned and the session is still stopped",
			adapter:   &callTrackingAdapter{runErr: errors.New("run failed")},
			wantErr:   errors.New("run failed"),
			wantCalls: []string{"start", "run", "stop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := RunWorkingLive(tt.adapter, domain.StartSessionParams{})

			if (err == nil) != (tt.wantErr == nil) {
				t.Fatalf("RunWorkingLive() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && err.Error() != tt.wantErr.Error() {
				t.Errorf("RunWorkingLive() error = %q, want %q", err.Error(), tt.wantErr.Error())
			}
			if !slices.Equal(tt.adapter.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", tt.adapter.calls, tt.wantCalls)
			}
		})
	}
}

func TestRequireRefused(t *testing.T) {
	t.Parallel()

	conforming := newEarlyExitConformingAdapter(t)
	_, earlyExitErr := conforming.StartSession(context.Background(), domain.StartSessionParams{WorkspacePath: t.TempDir()})

	tests := []struct {
		name     string
		err      error
		wantFail bool
	}{
		{name: "credential_unverified is accepted", err: &domain.AgentError{Kind: domain.ErrCredentialUnverified, Message: "refused"}},
		{name: "an early-exit report is accepted", err: earlyExitErr},
		{name: "an unrelated error is rejected", err: &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "unrelated"}, wantFail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stand := new(testing.T)
			RequireRefused(stand, tt.err)

			if stand.Failed() != tt.wantFail {
				t.Errorf("RequireRefused(%v) failed = %v, want %v", tt.err, stand.Failed(), tt.wantFail)
			}
		})
	}
}

func TestAssertCredentialVerification(t *testing.T) {
	t.Parallel()

	verified := CredentialVerificationCase{Name: "verified", Adapter: verifiedAdapter(), Want: WantVerified}
	unverified := CredentialVerificationCase{Name: "unverified", Adapter: unverifiedAdapter(), Want: WantUnverified}
	sshFailed := CredentialVerificationCase{Name: "ssh failed", Adapter: sshFailedAdapter(), Want: WantSSHConnectionFailed}
	mismatched := CredentialVerificationCase{Name: "mismatched", Adapter: unverifiedAdapter(), Want: WantVerified}

	tests := []struct {
		name     string
		kind     string
		cases    []CredentialVerificationCase
		wantFail bool
	}{
		{name: "all three outcomes match", kind: credentialTestKindWithCommand, cases: []CredentialVerificationCase{verified, unverified, sshFailed}},
		{name: "no ssh case needed without a command", kind: credentialTestKindWithoutCommand, cases: []CredentialVerificationCase{verified, unverified}},
		{name: "unregistered kind", kind: "agenttest-credential-does-not-exist", cases: []CredentialVerificationCase{verified, unverified}, wantFail: true},
		{name: "missing ssh case with a command", kind: credentialTestKindWithCommand, cases: []CredentialVerificationCase{verified, unverified}, wantFail: true},
		{name: "outcome mismatch", kind: credentialTestKindWithCommand, cases: []CredentialVerificationCase{mismatched, unverified, sshFailed}, wantFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertCredentialVerification(reporter, tt.kind, tt.cases)

			if failed := len(reporter.errors) > 0; failed != tt.wantFail {
				t.Errorf("assertCredentialVerification() failures = %v, want failure %v", reporter.errors, tt.wantFail)
			}
		})
	}
}
