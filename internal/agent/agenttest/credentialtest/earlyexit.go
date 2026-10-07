package credentialtest

import (
	"context"
	"errors"
	"os"
	"strings"
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

// conformanceRuntimeVersion is what the conformance runtime reports to a
// version query, so a kind that reads its runtime's version before a
// launch reaches the launch the case is about instead of failing on the
// query. Kinds differ in the versions they accept, so a kind that gates
// on a version range must accept this one.
const conformanceRuntimeVersion = "2.0.18"

// earlyExitConformanceMessage is the exact [*domain.AgentError] message
// every kind wired to [agentcore.EarlyExit.Report] must produce for the
// conformance runtime.
const earlyExitConformanceMessage = "the agent runtime exited before responding: exit status 2"

// emptyCommandMessage is the [*domain.AgentError] message
// [agentcore.ResolveLaunchTarget] reports for a zero command with no
// default.
const emptyCommandMessage = "agent command is empty or whitespace-only"

// unreadableConformanceLine is the fixed line the fourth conformance
// case's runtime writes to standard output: text no structured-output
// decoder accepts, so a kind never counts it as a response.
const unreadableConformanceLine = "Usage: sortie-early-exit-conformance [options]"

// AssertEarlyExitReport fails t unless kind is registered with
// [registry.AgentMeta.RequiresCommand] true, and four launches driven
// through adapter with config each fail with the shared early-exit
// report: a verification session through [agentcore.VerifyCredential],
// a working session, a working session routed through a stand-in ssh
// placed first on PATH, and a working session whose runtime writes one
// line no structured-output decoder accepts. Each of the four runs
// twice, with the command as [domain.AgentConfig.Command] and as
// [domain.AgentConfig.CommandArgv], so a kind that reads the command
// around [agentcore.ResolveLaunchTarget] fails. A further pair of
// launches gives the kind a zero command: a kind with a
// [registry.AgentMeta.DefaultCommand] must launch it and fail with the
// early-exit report, and a kind with none must fail with the empty
// command error. A working session is
// [domain.AgentAdapter.StartSession] followed, when it succeeds, by
// its first [domain.AgentAdapter.RunTurn]. The first three cases'
// runtime is a fake writing [earlyExitConformanceLine] to standard
// error and exiting 2 for a launch that carries
// [earlyExitConformanceSwitch]; any other launch writes nothing and
// exits 0. A launch that succeeds is stopped and fails t.
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
		Version:  conformanceRuntimeVersion,
	})

	unreadableDir := t.TempDir()
	unreadablePath := agenttest.FakeRuntime(t, unreadableDir, "agent-unreadable", agenttest.OutputScenario, agenttest.Output{
		Stdout:   unreadableConformanceLine + "\n",
		Stderr:   earlyExitConformanceLine + "\n",
		ExitCode: 2,
		WhenArg:  earlyExitConformanceSwitch,
		Version:  conformanceRuntimeVersion,
	})

	for _, form := range commandForms {
		formKind := kind + " (" + form.name + ")"
		baseConfig := form.apply(config, runtimePath)

		verifyParams := domain.StartSessionParams{
			WorkspacePath:          t.TempDir(),
			AgentConfig:            baseConfig,
			CredentialVerification: true,
		}
		_, verifyErr := agentcore.VerifyCredential(context.Background(), adapter, agentcore.CredentialVerification{
			Session: verifyParams,
			Issue:   domain.Issue{},
		})
		assertEarlyExitReportOutcome(t, formKind+": verification session", verifyErr)

		workingParams := domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   baseConfig,
		}
		assertEarlyExitWorkingSession(t, formKind+": working session", adapter, workingParams)

		remoteParams := domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   baseConfig,
			SSHHost:       "user@sortie-conformance-host",
		}
		assertEarlyExitWorkingSession(t, formKind+": working session through ssh", adapter, remoteParams)

		assertUnreadableLineConformance(t, formKind, adapter, form.apply(config, unreadablePath))
	}

	assertZeroCommandLaunch(t, kind, adapter, config, meta.DefaultCommand)
}

type commandForm struct {
	name  string
	apply func(config domain.AgentConfig, runtimePath string) domain.AgentConfig
}

var commandForms = []commandForm{
	{
		name: "string command",
		apply: func(config domain.AgentConfig, runtimePath string) domain.AgentConfig {
			config.Command = runtimePath + " " + earlyExitConformanceSwitch
			config.CommandArgv = nil
			return config
		},
	},
	{
		name: "list command",
		apply: func(config domain.AgentConfig, runtimePath string) domain.AgentConfig {
			config.Command = ""
			config.CommandArgv = []string{runtimePath, earlyExitConformanceSwitch}
			return config
		},
	},
}

// assertZeroCommandLaunch gives kind no command. A non-empty
// defaultCommand names the runtime the launch must reach, which a fake
// of that name placed first on PATH makes fail with the early-exit
// report; an empty one must fail with the empty command error.
func assertZeroCommandLaunch(t *testing.T, kind string, adapter domain.AgentAdapter, config domain.AgentConfig, defaultCommand string) {
	t.Helper()

	config.Command = ""
	config.CommandArgv = nil
	name := kind + ": zero command"

	if strings.TrimSpace(defaultCommand) == "" {
		err := runWorking(context.Background(), adapter, domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   config,
		}, 0, 0)
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) || agentErr.Message != emptyCommandMessage {
			t.Errorf("case %q: error %v, want an *domain.AgentError with message %q", name, err, emptyCommandMessage)
		}
		return
	}

	defaultDir := t.TempDir()
	agenttest.FakeRuntime(t, defaultDir, strings.Fields(defaultCommand)[0], agenttest.OutputScenario, agenttest.Output{
		Stderr:   earlyExitConformanceLine + "\n",
		ExitCode: 2,
		Version:  conformanceRuntimeVersion,
	})
	t.Setenv("PATH", defaultDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, verifyErr := agentcore.VerifyCredential(context.Background(), adapter, agentcore.CredentialVerification{
		Session: domain.StartSessionParams{
			WorkspacePath:          t.TempDir(),
			AgentConfig:            config,
			CredentialVerification: true,
		},
		Issue: domain.Issue{},
	})
	assertEarlyExitReportOutcome(t, name+": verification session", verifyErr)

	assertEarlyExitWorkingSession(t, name+": working session", adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   config,
	})
}

// assertUnreadableLineConformance drives a working session with
// unreadableConfig, whose command names a fake runtime that writes
// [unreadableConformanceLine] on standard output, then
// [earlyExitConformanceLine] on standard error, then exits 2, and fails
// t unless the session fails with the shared early-exit report.
func assertUnreadableLineConformance(t *testing.T, kind string, adapter domain.AgentAdapter, unreadableConfig domain.AgentConfig) {
	t.Helper()

	name := kind + ": unreadable standard-output line"

	err := runWorking(context.Background(), adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   unreadableConfig,
	}, 0, 0)
	if err == nil {
		t.Errorf("case %q: StartSession and RunTurn against the conformance runtime unexpectedly both succeeded", name)
		return
	}
	assertEarlyExitReportOutcome(t, name, err)
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
