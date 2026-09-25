package credentialtest

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// earlyExitConformanceLine is the fixed line every early-exit
// conformance fixture writes to standard error before it exits 2.
const earlyExitConformanceLine = "error: unexpected argument '--sortie-unknown-switch' found (sortie early-exit conformance)"

// earlyExitConformanceSwitch is the switch the conformance runtime
// rejects, appended to config.Command so every launch that carries the
// configured command's fixed argument prefix fails, while a launch
// that bypasses it (a version canary execing target.Command directly)
// succeeds.
const earlyExitConformanceSwitch = "--sortie-unknown-switch"

// earlyExitConformanceMessage is the exact [*domain.AgentError] message
// every kind wired to [agentcore.EarlyExit.Report] must produce for the
// conformance runtime.
const earlyExitConformanceMessage = "the agent runtime exited before responding: exit status 2"

// AssertEarlyExitReport fails t unless kind is registered with
// [registry.AgentMeta.RequiresCommand] true, and three launches driven
// through adapter with config each fail with the shared early-exit
// report: a verification session through [agentcore.VerifyCredential],
// a working session, and a working session routed through a stand-in
// ssh placed first on PATH. A working session is
// [domain.AgentAdapter.StartSession] followed, when it succeeds, by
// its first [domain.AgentAdapter.RunTurn]. The runtime is a fake
// writing [earlyExitConformanceLine] to standard error and exiting 2
// for a launch that carries [earlyExitConformanceSwitch]; any other
// launch writes nothing and exits 0. A launch that succeeds is stopped
// and fails t.
//
// AssertEarlyExitReport sets PATH through [testing.T.Setenv], so its
// caller must not run it in parallel with a sibling test.
func AssertEarlyExitReport(t *testing.T, kind string, adapter domain.AgentAdapter, config domain.AgentConfig) {
	t.Helper()

	meta, registered := registry.Agents.Meta(kind)
	if !registered {
		t.Errorf("kind %q is not registered", kind)
		return
	}
	if !meta.RequiresCommand {
		t.Errorf("kind %q: RequiresCommand is false, so AssertEarlyExitReport does not apply", kind)
		return
	}

	sshDir := t.TempDir()
	agenttest.FakeRuntime(t, sshDir, "ssh", agenttest.OutputScenario, agenttest.Output{
		Stderr:   earlyExitConformanceLine + "\n",
		ExitCode: 2,
	})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	binDir := t.TempDir()
	runtimePath := agenttest.FakeRuntime(t, binDir, "agent", agenttest.OutputScenario, agenttest.Output{
		Stderr:   earlyExitConformanceLine + "\n",
		ExitCode: 2,
		WhenArg:  earlyExitConformanceSwitch,
	})

	baseConfig := config
	baseConfig.Command = runtimePath + " " + earlyExitConformanceSwitch

	verifyParams := domain.StartSessionParams{
		WorkspacePath:          t.TempDir(),
		AgentConfig:            baseConfig,
		CredentialVerification: true,
	}
	_, verifyErr := agentcore.VerifyCredential(context.Background(), adapter, agentcore.CredentialVerification{
		Session: verifyParams,
		Issue:   domain.Issue{},
	})
	assertEarlyExitReportOutcome(t, kind+": verification session", verifyErr)

	workingParams := domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   baseConfig,
	}
	assertEarlyExitWorkingSession(t, kind+": working session", adapter, workingParams)

	remoteParams := domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   baseConfig,
		SSHHost:       "user@sortie-conformance-host",
	}
	assertEarlyExitWorkingSession(t, kind+": working session through ssh", adapter, remoteParams)
}

// assertEarlyExitWorkingSession drives one working session through
// adapter (StartSession, then, on success, its first RunTurn) and
// fails t unless it fails with the early-exit report. A session that
// starts is stopped, by runWorking itself, so the fake runtime it
// holds does not leak whether or not the turn also failed.
func assertEarlyExitWorkingSession(t *testing.T, name string, adapter domain.AgentAdapter, params domain.StartSessionParams) {
	t.Helper()

	err := runWorking(context.Background(), adapter, params, 0, 0)
	if err == nil {
		t.Errorf("case %q: StartSession and RunTurn against the conformance runtime unexpectedly both succeeded", name)
		return
	}
	assertEarlyExitReportOutcome(t, name, err)
}

// RequireEarlyExitReport fails t unless err is a port_exit
// [*domain.AgentError] whose chain holds an
// [*agentcore.EarlyExitError] with non-empty [agentcore.EarlyExitError.Status]
// and [agentcore.EarlyExitError.Output].
func RequireEarlyExitReport(t *testing.T, err error) {
	t.Helper()
	requireEarlyExitReport(t, err)
}

// earlyExitReporter lets a package-internal test observe failures,
// which a *testing.T cannot do without failing itself.
type earlyExitReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

func requireEarlyExitReport(t earlyExitReporter, err error) {
	t.Helper()

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Errorf("error %v is not a port_exit *domain.AgentError", err)
		return
	}
	var earlyExitErr *agentcore.EarlyExitError
	if !errors.As(agentErr.Err, &earlyExitErr) {
		t.Errorf("error %v does not chain an *agentcore.EarlyExitError", err)
		return
	}
	if earlyExitErr.Status() == "" {
		t.Errorf("error %v carries an *agentcore.EarlyExitError with an empty Status()", err)
	}
	if earlyExitErr.Output() == "" {
		t.Errorf("error %v carries an *agentcore.EarlyExitError with an empty Output()", err)
	}
}

// assertEarlyExitReportOutcome fails t unless err is the exact
// early-exit report [AssertEarlyExitReport]'s conformance runtime
// produces: a port_exit *domain.AgentError with message
// [earlyExitConformanceMessage], whose chain holds an
// *agentcore.EarlyExitError with Status "exit status 2" and Output
// equal to [earlyExitConformanceLine].
func assertEarlyExitReportOutcome(t *testing.T, name string, err error) {
	t.Helper()

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Errorf("case %q: error %v is not a port_exit *domain.AgentError", name, err)
		return
	}
	if agentErr.Message != earlyExitConformanceMessage {
		t.Errorf("case %q: message %q, want %q", name, agentErr.Message, earlyExitConformanceMessage)
	}
	var earlyExitErr *agentcore.EarlyExitError
	if !errors.As(agentErr.Err, &earlyExitErr) {
		t.Errorf("case %q: error %v does not chain an *agentcore.EarlyExitError", name, err)
		return
	}
	if earlyExitErr.Status() != "exit status 2" {
		t.Errorf("case %q: Status() = %q, want %q", name, earlyExitErr.Status(), "exit status 2")
	}
	if earlyExitErr.Output() != earlyExitConformanceLine {
		t.Errorf("case %q: Output() = %q, want %q", name, earlyExitErr.Output(), earlyExitConformanceLine)
	}
}
