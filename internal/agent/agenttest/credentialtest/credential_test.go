package credentialtest

import (
	"context"
	"errors"
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
