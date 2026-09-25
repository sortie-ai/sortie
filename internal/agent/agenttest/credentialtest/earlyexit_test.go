package credentialtest

import (
	"context"
	"errors"
	"log/slog"
	"os"
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

// alwaysSucceedsAdapter's StartSession and RunTurn both always succeed,
// ignoring whatever runtime params.AgentConfig.Command names: the
// negative control AssertEarlyExitReport must reject, since neither
// launch ever fails.
type alwaysSucceedsAdapter struct{}

var _ domain.AgentAdapter = (*alwaysSucceedsAdapter)(nil)

func (a *alwaysSucceedsAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{}, nil
}

func (a *alwaysSucceedsAdapter) RunTurn(context.Context, domain.Session, domain.RunTurnParams) (domain.TurnResult, error) {
	return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
}

func (a *alwaysSucceedsAdapter) StopSession(context.Context, domain.Session) error { return nil }

// forkPerTurnFixtureAdapter wraps the real fork-per-turn skeleton, so a
// launch driven through it against a real runtime path produces a
// genuine *agentcore.EarlyExitError chain rather than a hand-built
// lookalike, for every one of AssertEarlyExitReport's three launch
// shapes: a verification request, a working session, and one through
// SSH.
type forkPerTurnFixtureAdapter struct {
	session *agentcore.ForkPerTurnSession
}

var _ domain.AgentAdapter = (*forkPerTurnFixtureAdapter)(nil)

func (a *forkPerTurnFixtureAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}
	a.session = agentcore.NewForkPerTurnSession(&target, agentcore.ForkPerTurnHooks{
		BuildArgs:    func(int, string) []string { return nil },
		ParseLine:    func([]byte, func(domain.AgentEvent), string) (any, error) { return nil, nil },
		GetUsage:     func() (domain.TokenUsage, bool) { return domain.TokenUsage{}, false },
		GetSessionID: func() string { return "" },
		OnFinalize: func(emit func(domain.AgentEvent), _ any, exitCode int, _ []string, earlyExit *domain.AgentError) (domain.TurnResult, *domain.AgentError) {
			return agentcore.FinalizeTurn(emit, slog.Default(), agentcore.TurnEvidence{
				ExitObserved: true,
				ExitCode:     exitCode,
				EarlyExit:    earlyExit,
			}, agentcore.TurnMeta{})
		},
	}, slog.Default(), 0)
	return domain.Session{}, nil
}

func (a *forkPerTurnFixtureAdapter) RunTurn(ctx context.Context, _ domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	onEvent := params.OnEvent
	if onEvent == nil {
		onEvent = func(domain.AgentEvent) {}
	}
	return a.session.RunTurn(ctx, params.Prompt, onEvent)
}

func (a *forkPerTurnFixtureAdapter) StopSession(context.Context, domain.Session) error { return nil }

// runAssertEarlyExitReport drives AssertEarlyExitReport against a
// stand-in *testing.T obtained from new(testing.T) rather than from
// t.Run, so it has no parent: its own Fail() never reaches the calling
// test's failure state, letting a deliberately-failing fixture be
// driven through the real, exported entry point without reddening the
// package. AssertEarlyExitReport's own t.Setenv("PATH", ...) call
// registers its restore as a Cleanup that a parentless *testing.T never
// runs, so the caller must save and restore PATH itself.
func runAssertEarlyExitReport(kind string, adapter domain.AgentAdapter) bool {
	origPath, hadPath := os.LookupEnv("PATH")
	defer func() {
		if hadPath {
			os.Setenv("PATH", origPath) //nolint:errcheck // best-effort restore
		} else {
			os.Unsetenv("PATH") //nolint:errcheck // best-effort restore
		}
	}()

	stand := new(testing.T)
	AssertEarlyExitReport(stand, kind, adapter, domain.AgentConfig{})
	return !stand.Failed()
}

// Neither test below calls t.Parallel(): runAssertEarlyExitReport
// mutates the process-wide PATH variable through AssertEarlyExitReport's
// own t.Setenv call, so two of these racing each other would corrupt
// PATH, exactly the hazard AssertEarlyExitReport's own doc comment
// warns a parallel caller into.

func TestAssertEarlyExitReport_NegativeControls(t *testing.T) {
	if runAssertEarlyExitReport(credentialTestKindWithCommand, &earlyExitNegativeControlAdapter{}) {
		t.Error("AssertEarlyExitReport() passed for a hand-built error with no *agentcore.EarlyExitError, want it to fail")
	}

	if runAssertEarlyExitReport(credentialTestKindWithCommand, &alwaysSucceedsAdapter{}) {
		t.Error("AssertEarlyExitReport() passed for an adapter that never fails, want it to fail")
	}
}

func TestAssertEarlyExitReport_Passes(t *testing.T) {
	if !runAssertEarlyExitReport(credentialTestKindWithCommand, &forkPerTurnFixtureAdapter{}) {
		t.Error("AssertEarlyExitReport() failed for an adapter whose RunTurn returns a report agentcore built, want it to pass")
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
