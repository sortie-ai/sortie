package credentialtest

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// earlyExitConformingAdapter's StartSession drives a real fake runtime
// through [agentcore.ExitedEarly] and [agentcore.EarlyExit.Report], so
// its error carries a genuine *agentcore.EarlyExitError rather than a
// hand-built lookalike.
type earlyExitConformingAdapter struct {
	runtimePath string
}

var _ domain.AgentAdapter = (*earlyExitConformingAdapter)(nil)

func newEarlyExitConformingAdapter(t *testing.T) *earlyExitConformingAdapter {
	t.Helper()
	dir := t.TempDir()
	runtimePath := agenttest.FakeRuntime(t, dir, "agent", agenttest.OutputScenario, agenttest.Output{
		Stderr:   earlyExitConformanceLine + "\n",
		ExitCode: 2,
	})
	return &earlyExitConformingAdapter{runtimePath: runtimePath}
}

func (a *earlyExitConformingAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target := agentcore.LaunchTarget{Command: a.runtimePath, WorkspacePath: params.WorkspacePath}
	stderr := procutil.NewTailBuffer(agentcore.EarlyExitCaptureBytes)
	result, err := procutil.RunCapture(exec.CommandContext(ctx, a.runtimePath), procutil.DefaultStopGrace, procutil.CaptureParams{Stderr: stderr})
	if err != nil {
		return domain.Session{}, err
	}
	return domain.Session{}, agentcore.ExitedEarly(target, result).Report(stderr.Collector(nil))
}

func (a *earlyExitConformingAdapter) RunTurn(context.Context, domain.Session, domain.RunTurnParams) (domain.TurnResult, error) {
	return domain.TurnResult{}, nil
}

func (a *earlyExitConformingAdapter) StopSession(context.Context, domain.Session) error { return nil }

// earlyExitNegativeControlAdapter's StartSession returns a port_exit
// error carrying [earlyExitConformanceMessage] but no
// *agentcore.EarlyExitError in its chain, the shape
// [requireEarlyExitReport] must reject.
type earlyExitNegativeControlAdapter struct{}

var _ domain.AgentAdapter = (*earlyExitNegativeControlAdapter)(nil)

func (a *earlyExitNegativeControlAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{}, &domain.AgentError{
		Kind:    domain.ErrPortExit,
		Message: earlyExitConformanceMessage,
		Err:     errors.New("exit status 2"),
	}
}

func (a *earlyExitNegativeControlAdapter) RunTurn(context.Context, domain.Session, domain.RunTurnParams) (domain.TurnResult, error) {
	return domain.TurnResult{}, nil
}

func (a *earlyExitNegativeControlAdapter) StopSession(context.Context, domain.Session) error {
	return nil
}

func TestRequireEarlyExitReport_ReporterSeam(t *testing.T) {
	t.Parallel()

	conforming := newEarlyExitConformingAdapter(t)
	_, conformingErr := conforming.StartSession(context.Background(), domain.StartSessionParams{WorkspacePath: t.TempDir()})

	negativeControl := &earlyExitNegativeControlAdapter{}
	_, negativeControlErr := negativeControl.StartSession(context.Background(), domain.StartSessionParams{})

	tests := []struct {
		name     string
		err      error
		wantFail bool
	}{
		{name: "conforming fixture reaches agentcore.EarlyExit.Report", err: conformingErr},
		{name: "negative control fixture carries no *agentcore.EarlyExitError", err: negativeControlErr, wantFail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			requireEarlyExitReport(reporter, tt.err)

			if failed := len(reporter.errors) > 0; failed != tt.wantFail {
				t.Errorf("requireEarlyExitReport() failures = %v, want failure %v", reporter.errors, tt.wantFail)
			}
		})
	}
}

func TestRequireEarlyExitReport_ExportedWrapper(t *testing.T) {
	t.Parallel()

	conforming := newEarlyExitConformingAdapter(t)
	_, conformingErr := conforming.StartSession(context.Background(), domain.StartSessionParams{WorkspacePath: t.TempDir()})
	RequireEarlyExitReport(t, conformingErr)

	credentialUnverifiedErr := &domain.AgentError{Kind: domain.ErrCredentialUnverified, Message: "whoami reports no signed-in account"}
	reporter := &fakeReporter{}
	requireEarlyExitReport(reporter, credentialUnverifiedErr)
	if len(reporter.errors) == 0 {
		t.Error("requireEarlyExitReport() did not fail for a credential_unverified error, want RequireEarlyExitReport to reject it")
	}
}
