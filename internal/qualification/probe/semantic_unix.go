//go:build unix

package probe

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// permissionAcceptedNotice and permissionNoOptionNotice mirror the two
// operator-facing notice strings agentcore.DecideHumanRequest emits for
// a permission request under this client's unattended refusal posture.
// A change to either constant on the adapter side must be followed here.
const (
	permissionAcceptedNotice = "refused a permission request because this run is unattended and no one can approve it"
	permissionNoOptionNotice = "the agent needs a permission this unattended run cannot grant"
)

// clientProtocolMaxLineBytesMirror mirrors clientprotocol's unexported
// connection line bound; a change to the adapter's bound must be
// followed here.
const clientProtocolMaxLineBytesMirror = 10 * 1024 * 1024

// limitReachedMargin reserves room below clientProtocolMaxLineBytesMirror
// for the marker instruction and the JSON-RPC envelope the oversize
// filler travels inside.
const limitReachedMargin = 4096

// launchProtocolOneTurn launches one protocol session with a fresh
// workspace and runs a single turn with prompt, recording the turn's
// UsageMeasured reading into fixture's usage tracker.
func launchProtocolOneTurn(t *testing.T, coords Coordinates, fixture *sharedFixture, prompt string) (sessionID string, err error) {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return "", err
	}
	adapter, session, err := startInductionSession(t, coords, argv, fixture.newLaunchWorkspace(t), "")
	if err != nil {
		return session.ID, err
	}
	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  prompt,
		OnEvent: func(domain.AgentEvent) {},
	})
	fixture.usage.observe(session.ID, result.UsageMeasured)
	return session.ID, runErr
}

// launchNativeOneTurn launches surface's native entry point in a fresh
// workspace with prompt, under a fresh identifier passed through
// seed_args when surface states one. It returns the combined output,
// the identifier supplied (empty when surface states no seed_args), and
// any launch error.
func launchNativeOneTurn(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface, prompt string) (output, seededID string, err error) {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(surface, coords.Model, "", prompt)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errNativeLaunchFailed, err)
	}
	if seedArgs := coords.Profile.EntryPoints[surface].SeedArgs; len(seedArgs) > 0 {
		seededID = newRFC4122V4(t)
		argv = append(argv, substituteSessionID(seedArgs, seededID)...)
	}
	output, launchErr := launchNativeProbe(t, coords.CommandPath, argv, fixture.newLaunchWorkspace(t), fixture.env, fixture.ownership())
	fixture.recordNativeOutput(surface, output)
	return output, seededID, launchErr
}

func substituteSessionID(args []string, sessionID string) []string {
	out := make([]string, len(args))
	replacer := strings.NewReplacer("{session_id}", sessionID)
	for i, arg := range args {
		out[i] = replacer.Replace(arg)
	}
	return out
}

func killProtocolSession(t *testing.T, session domain.Session, sig syscall.Signal) {
	t.Helper()
	pid, err := strconv.Atoi(session.AgentPID)
	if err != nil {
		t.Fatalf("session agent_pid %q did not parse as a process id: %v", session.AgentPID, err)
	}
	_ = signalProcessGroup(pid, sig)
}

// promptNamingProbe builds the instruction an induction turn hands the
// runtime to run one of the shared fixture's probe executables.
func promptNamingProbe(fixture *sharedFixture, probeName string) string {
	return fmt.Sprintf("Run the shell command %q now and report what happens.", fixture.probePath(probeName))
}

// nativeSessionID resolves surface's session identifier from a native
// launch's output through the profile's session_id_path selector. It
// reports the empty string, never a generated one, when the profile
// states no such path or the terminal carries no value there.
func nativeSessionID(coords Coordinates, surface qualification.Surface, output string) string {
	recognizer, ok := coords.Profile.Recognizers[surface]
	if !ok {
		return ""
	}
	sessionID, ok := recognizer.SessionID(output)
	if !ok {
		return ""
	}
	return sessionID
}

// resolveNativeSessionID confirms surface's session identifier for one
// native launch: through nativeSessionID's selector first, otherwise by
// seededID occurring literally in output. It reports the empty string,
// never seededID unconfirmed, so an identifier this run never observed
// is not recorded.
func resolveNativeSessionID(coords Coordinates, surface qualification.Surface, seededID, output string) string {
	if sessionID := nativeSessionID(coords, surface, output); sessionID != "" {
		return sessionID
	}
	if seededID != "" && strings.Contains(output, seededID) {
		return seededID
	}
	return ""
}

// recordUnrecognizedTerminal appends output's located terminal envelope
// to fixture's unrecognized set when the recognizer locates one it
// cannot resolve to a known outcome. It is a no-op when none was
// located.
func recordUnrecognizedTerminal(coords Coordinates, fixture *sharedFixture, surface qualification.Surface, output string) {
	recognizer, ok := coords.Profile.Recognizers[surface]
	if !ok {
		return
	}
	terminal, ok := recognizer.RawTerminal(output)
	if !ok {
		return
	}
	fixture.recordUnrecognized(terminal)
}

// induceSuccess induces the success case: RunTurn (or the native
// terminal) reports a clean end of turn.
func induceSuccess(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) qualification.Observation {
	t.Helper()
	prompt := coords.Profile.ProbePrompts[promptKeySuccess]
	if surface == qualification.SurfaceProtocol {
		sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
		if err != nil {
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the success turn did not complete", SessionID: sessionID}
		}
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the turn completed with no error", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
	}
	output, seededID, launchErr := launchNativeOneTurn(t, coords, fixture, surface, prompt)
	terminal, transportLoss, found := nativeTerminal(coords.Profile, surface, output, launchErr)
	sessionID := resolveNativeSessionID(coords, surface, seededID, output)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	if found && !terminal.Error && terminal.EndTurn {
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the recognized terminal reported end of turn", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
	}
	return nativeFailureObservation(sessionID, transportLoss, qualification.OutcomeFixtureInductionFailed, "no recognized end-of-turn terminal was observed")
}

// runtimeFailureModel is a model identifier no backend resolves, so the
// launch is rejected in the runtime's own error path before any request
// reaches a model. This yields a turn-level error rather than a child
// process's exit code the runtime may narrate as an ordinary tool
// result.
const runtimeFailureModel = "sortie-qualification-refused-model"

// induceRuntimeFailure induces the runtime_failure case, graded from
// domain.ErrResponseError on the protocol surface and from the
// recognizer's error member on a native one.
func induceRuntimeFailure(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) qualification.Observation {
	t.Helper()
	coords.Model = runtimeFailureModel
	prompt := coords.Profile.ProbePrompts[promptKeySuccess]
	if surface == qualification.SurfaceProtocol {
		sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
		var agentErr *domain.AgentError
		switch {
		case errors.As(err, &agentErr) && agentErr.Kind == domain.ErrResponseError:
			return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the turn ended with a protocol-level error response", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
		case err == nil:
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the turn completed instead of failing", SessionID: sessionID}
		default:
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the turn failed for a reason other than a protocol-level error response", SessionID: sessionID}
		}
	}
	output, seededID, launchErr := launchNativeOneTurn(t, coords, fixture, surface, prompt)
	terminal, transportLoss, found := nativeTerminal(coords.Profile, surface, output, launchErr)
	sessionID := resolveNativeSessionID(coords, surface, seededID, output)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	if found && terminal.Error {
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the recognized terminal carried an error member", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
	}
	return nativeFailureObservation(sessionID, transportLoss, qualification.OutcomeFixtureInductionFailed, "no recognized error terminal was observed")
}

// induceRefusalPair induces the runtime_refusal disposition case and
// derives its non_retryable_refusal retry peer from the same run.
func induceRefusalPair(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) (disposition, retry qualification.Observation) {
	t.Helper()
	prompt := coords.Profile.ProbePrompts[promptKeyRuntimeRefusal]
	if surface == qualification.SurfaceProtocol {
		sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
		var agentErr *domain.AgentError
		switch {
		case errors.As(err, &agentErr) && agentErr.Kind == domain.ErrTurnRefused:
			disposition = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the turn ended with a reported refusal", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
		case err == nil:
			disposition = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the turn completed instead of refusing", SessionID: sessionID}
		default:
			disposition = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the turn failed for a reason other than a reported refusal", SessionID: sessionID}
		}
	} else {
		output, seededID, launchErr := launchNativeOneTurn(t, coords, fixture, surface, prompt)
		terminal, transportLoss, found := nativeTerminal(coords.Profile, surface, output, launchErr)
		sessionID := resolveNativeSessionID(coords, surface, seededID, output)
		if !found {
			recordUnrecognizedTerminal(coords, fixture, surface, output)
		}
		switch {
		case found && !terminal.Error && terminal.Case == qualification.CaseRuntimeRefusal:
			disposition = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the recognized terminal reported a refusal status", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
		default:
			disposition = nativeFailureObservation(sessionID, transportLoss, qualification.OutcomeFixtureInductionFailed, "no recognized refusal terminal was observed")
		}
	}
	retry = qualification.Observation{Grade: disposition.Grade, Outcome: disposition.Outcome, Detail: disposition.Detail, SessionID: disposition.SessionID, EvidencePath: disposition.EvidencePath}
	return disposition, retry
}

func sessionProcessGroup(t *testing.T, session domain.Session) int {
	t.Helper()
	pgid, err := strconv.Atoi(session.AgentPID)
	if err != nil {
		t.Fatalf("session agent_pid %q did not parse as a process id: %v", session.AgentPID, err)
	}
	return pgid
}

// awaitProbeUnderTurn waits for the launch led by pgid to be observed
// running probePath while the turn is still in flight. It reports
// observed=false when the deadline passed, and ended=true when the turn
// finished first, the one case where no signal can be sent. Every
// snapshot is folded into owned.
func awaitProbeUnderTurn(owned *ownedDescendants, probePath string, pgid int, done <-chan error, bound time.Duration) (observed, ended bool) {
	deadline := time.Now().Add(bound)
	for {
		select {
		case <-done:
			return false, true
		default:
		}
		if snapshot, err := psSnapshot(); err == nil {
			owned.observe(snapshot)
			if launchRunsProgram(snapshot, pgid, filepath.Base(probePath)) {
				return true, false
			}
		}
		if time.Now().After(deadline) {
			return false, false
		}
		select {
		case <-done:
			return false, true
		case <-time.After(probeExecutionPollInterval):
		}
	}
}

// induceCancellation induces the cancellation case. On the protocol
// surface the adapter reaches its cancelled terminal only when the
// turn's context is cancelled, which sends the protocol's own cancel
// request; signaling the process group instead drops the connection and
// surfaces as a port exit. On a native surface the one-shot launch
// takes exactly one SIGINT once the cancellation probe's marker appears.
func induceCancellation(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) qualification.Observation {
	t.Helper()
	if surface == qualification.SurfaceProtocol {
		return induceProtocolCancellation(t, coords, fixture,
			"the turn ended in cancellation after its own context was cancelled",
			"the turn did not end in cancellation after its own context was cancelled")
	}
	return induceNativeAsyncSignal(t, coords, fixture, surface, "cancellation", syscall.SIGINT,
		qualification.CaseCancellation,
		"the launch ended in cancellation after one SIGINT to its group",
		"the launch did not end in cancellation after one SIGINT to its group")
}

// induceProtocolCancellation runs the cancellation probe under a
// cancellable context, cancels once the probe is observed running, and
// grades the result against domain.ErrTurnCancelled.
func induceProtocolCancellation(t *testing.T, coords Coordinates, fixture *sharedFixture, matchDetail, missDetail string) qualification.Observation {
	t.Helper()
	prompt := promptNamingProbe(fixture, "cancellation")
	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"}
	}
	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, "")
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "the induction session failed to start"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		result, runErr := adapter.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt:  prompt,
			OnEvent: func(domain.AgentEvent) {},
		})
		fixture.usage.observe(session.ID, result.UsageMeasured)
		done <- runErr
	}()

	observed, ended := awaitProbeUnderTurn(fixture.ownership(), fixture.probePath("cancellation"), sessionProcessGroup(t, session), done, fixture.observationBound())
	if ended {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the turn ended before the probe was observed running", SessionID: session.ID}
	}
	if !observed {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the probe was never observed running under this launch", SessionID: session.ID}
	}

	cancel()
	runErr := <-done

	var agentErr *domain.AgentError
	switch {
	case errors.As(runErr, &agentErr) && agentErr.Kind == domain.ErrTurnCancelled:
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: matchDetail, SessionID: session.ID, EvidencePath: qualification.SemanticEvidencePath(qualification.SurfaceProtocol)}
	case runErr == nil:
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: missDetail, SessionID: session.ID}
	default:
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: missDetail, SessionID: session.ID}
	}
}

// induceRetryableTransport induces the retryable_transport case: once
// the transport probe's started marker appears, its process group is
// SIGKILLed.
func induceRetryableTransport(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) qualification.Observation {
	t.Helper()
	return induceAsyncSignalCase(t, coords, fixture, surface, "transport", syscall.SIGKILL,
		domain.ErrPortExit, qualification.CaseRetryableTransport,
		"the launch reported transport loss after one SIGKILL to its group",
		"the launch did not report transport loss after one SIGKILL to its group")
}

// induceAsyncSignalCase shares the async-launch, marker-wait,
// exactly-one-signal shape cancellation and retryable_transport follow,
// differing only in probe, signal, and outcome.
func induceAsyncSignalCase(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface, probeName string, sig syscall.Signal, wantKind domain.AgentErrorKind, wantCase qualification.Case, matchDetail, missDetail string) qualification.Observation {
	t.Helper()
	if surface == qualification.SurfaceProtocol {
		return induceProtocolAsyncSignal(t, coords, fixture, probeName, sig, wantKind, matchDetail, missDetail)
	}
	return induceNativeAsyncSignal(t, coords, fixture, surface, probeName, sig, wantCase, matchDetail, missDetail)
}

// induceProtocolAsyncSignal runs the named probe under a protocol
// session, sends sig to its process group once the probe is observed
// running, and grades against wantKind.
func induceProtocolAsyncSignal(t *testing.T, coords Coordinates, fixture *sharedFixture, probeName string, sig syscall.Signal, wantKind domain.AgentErrorKind, matchDetail, missDetail string) qualification.Observation {
	t.Helper()
	prompt := promptNamingProbe(fixture, probeName)
	probePath := fixture.probePath(probeName)
	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"}
	}
	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, "")
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "the induction session failed to start"}
	}

	done := make(chan error, 1)
	go func() {
		result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  prompt,
			OnEvent: func(domain.AgentEvent) {},
		})
		fixture.usage.observe(session.ID, result.UsageMeasured)
		done <- runErr
	}()

	observed, ended := awaitProbeUnderTurn(fixture.ownership(), probePath, sessionProcessGroup(t, session), done, fixture.observationBound())
	if ended {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the turn ended before the probe was observed running", SessionID: session.ID}
	}
	if !observed {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the probe was never observed running under this launch", SessionID: session.ID}
	}

	killProtocolSession(t, session, sig)
	runErr := <-done

	var agentErr *domain.AgentError
	switch {
	case errors.As(runErr, &agentErr) && agentErr.Kind == wantKind:
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: matchDetail, SessionID: session.ID, EvidencePath: qualification.SemanticEvidencePath(qualification.SurfaceProtocol)}
	case runErr == nil:
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: missDetail, SessionID: session.ID}
	default:
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: missDetail, SessionID: session.ID}
	}
}

// induceNativeAsyncSignal launches surface's native entry point, sends
// sig to its process group once the named probe is observed running,
// and grades from the recognizer's transport-loss reading.
func induceNativeAsyncSignal(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface, probeName string, sig syscall.Signal, wantCase qualification.Case, matchDetail, missDetail string) qualification.Observation {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(surface, coords.Model, "", promptNamingProbe(fixture, probeName))
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "could not resolve the native entry point"}
	}
	workspace := fixture.newLaunchWorkspace(t)
	launch, err := startBoundedLaunch(coords.CommandPath, argv, workspace, fixture.env, fixture.ownership())
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the native launch did not start"}
	}
	defer launch.cancel()

	if !awaitProbeExecution(fixture.ownership(), fixture.probePath(probeName), launch.pgid, time.Now().Add(fixture.observationBound())) {
		if _, _, ended := launch.terminate(); ended {
			qualification.AwaitProcessGroupAbsence(t, launch.pgid)
		}
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the probe was never observed running under this launch"}
	}

	_ = signalProcessGroup(launch.pgid, sig)
	// The signal is a request: a runtime that ignores it or a descendant
	// holding the streams open would hold this wait past every bound, so
	// the group comes down when this wait expires.
	result, output, ended := launch.await(fixture.signalBound())
	if !ended {
		result, output, ended = launch.terminate()
		if !ended {
			return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the launch did not end within its own bound after the signal"}
		}
	}
	waitErr := result.WaitErr
	qualification.AwaitProcessGroupAbsence(t, launch.pgid)

	fixture.recordNativeOutput(surface, output)
	terminal, transportLoss, found := nativeTerminal(coords.Profile, surface, output, waitErr)
	sessionID := nativeSessionID(coords, surface, output)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	switch {
	case wantCase == qualification.CaseRetryableTransport && transportLoss:
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: matchDetail, SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
	case found && !terminal.Error && terminal.Case == wantCase:
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: matchDetail, SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
	default:
		return nativeFailureObservation(sessionID, transportLoss, qualification.OutcomeFixtureInductionFailed, missDetail)
	}
}

// induceProtocolLimit induces the limit_reached case on the protocol
// surface with an oversize inert filler sized from
// clientProtocolMaxLineBytesMirror, graded from the runtime's own token
// or request limit error.
func induceProtocolLimit(t *testing.T, coords Coordinates, fixture *sharedFixture) qualification.Observation {
	t.Helper()
	fillerSize := clientProtocolMaxLineBytesMirror - limitReachedMargin
	prompt := strings.Repeat("a", fillerSize) + "\n\nReply with exactly SORTIE_LIMIT_PROBE_DONE."
	sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
	var agentErr *domain.AgentError
	switch {
	case errors.As(err, &agentErr) && (agentErr.Kind == domain.ErrTurnTokenLimit || agentErr.Kind == domain.ErrTurnRequestLimit):
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the oversize turn ended at the runtime's own token or request limit", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(qualification.SurfaceProtocol)}
	case err == nil:
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "the oversize turn completed normally instead of reaching a limit", SessionID: sessionID}
	default:
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the oversize turn failed for a reason other than reaching a limit", SessionID: sessionID}
	}
}

// induceToolServer induces the tool-server delivery row on its own
// permissive-posture launch, authorized by a run-scoped policy file. It
// is induced separately from the asking-posture launch because this
// row's usable arm needs the probe's side-effect marker present while
// the asking-posture rows need it absent.
func induceToolServer(t *testing.T, coords Coordinates, fixture *sharedFixture) qualification.Observation {
	t.Helper()

	dir := t.TempDir()
	callRecordPath := dir + "/calls.jsonl"
	scriptPath := agenttest.FakeRuntime(t, dir, "mcp-server", mcpToolServerScenario, mcpToolServerParams{RecordPath: callRecordPath})
	mcpConfigPath := writeToolServerMCPConfig(t, dir, scriptPath)
	policyPath := writeToolServerPolicy(t, dir, coords.Profile)

	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, policyPath, "")
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"}
	}

	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, mcpConfigPath)
	if err != nil {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the tool-server induction session failed to start"}
	}

	prompt := strings.ReplaceAll(coords.Profile.ProbePrompts[promptKeyToolCall], "{tool}", probeToolName)
	var calledAnyTool bool
	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: prompt,
		OnEvent: func(ev domain.AgentEvent) {
			if ev.Type == domain.EventToolResult {
				calledAnyTool = true
			}
		},
	})
	fixture.usage.observe(session.ID, result.UsageMeasured)

	recorded, _ := fileHasContent(callRecordPath)
	sessionID := session.ID

	switch {
	case recorded:
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the declared tool server recorded a call and the turn consumed it", SessionID: sessionID}
	case runErr != nil:
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the tool-server induction turn did not complete", SessionID: sessionID}
	case calledAnyTool:
		return qualification.Observation{Grade: qualification.GradeGap, Outcome: qualification.OutcomePass, Detail: "the turn completed a tool call that never reached the declared server", SessionID: sessionID}
	default:
		return qualification.Observation{Grade: qualification.GradeGap, Outcome: qualification.OutcomePass, Detail: "the turn completed without calling any tool, so the declared server was never reached", SessionID: sessionID}
	}
}

// inducePermissionPolicyHumanInput drives one asking-posture protocol
// launch and derives three rows from it: permission handling, the
// policy precondition, and the protocol human_input case. The launch
// stays unauthorized, since every usable arm here needs the probe's
// side-effect marker absent; tool-server delivery is induced separately.
func inducePermissionPolicyHumanInput(t *testing.T, coords Coordinates, fixture *sharedFixture) (permission, policy, humanInput qualification.Observation) {
	t.Helper()

	dir := t.TempDir()
	callRecordPath := dir + "/calls.jsonl"
	scriptPath := agenttest.FakeRuntime(t, dir, "mcp-server", mcpToolServerScenario, mcpToolServerParams{RecordPath: callRecordPath})
	mcpConfigPath := writeToolServerMCPConfig(t, dir, scriptPath)

	argv, ok := coords.Profile.AskingArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if !ok {
		obs := qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "the profile states no asking posture for the protocol surface"}
		return obs, obs, obs
	}

	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, mcpConfigPath)
	if err != nil {
		obs := qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the asking-posture induction session failed to start"}
		return obs, obs, obs
	}

	prompt := strings.ReplaceAll(coords.Profile.ProbePrompts[promptKeyToolCall], "{tool}", probeToolName)
	var events []domain.AgentEvent
	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: prompt,
		OnEvent: func(ev domain.AgentEvent) {
			events = append(events, ev)
		},
	})
	fixture.usage.observe(session.ID, result.UsageMeasured)

	recorded, _ := fileHasContent(callRecordPath)

	var agentErr *domain.AgentError
	signals := permissionLaunchSignals{
		continuableNotice:   containsNotification(events, permissionAcceptedNotice),
		noOptionNotice:      containsNotification(events, permissionNoOptionNotice),
		toolRecorded:        recorded,
		turnFailed:          runErr != nil,
		endedRequiringInput: errors.As(runErr, &agentErr) && agentErr.Kind == domain.ErrTurnInputRequired,
		sessionID:           session.ID,
	}
	return gradePermissionLaunch(signals)
}

// permissionLaunchSignals is what one asking-posture launch observed
// that the three rows derived from it are graded on. It is a plain
// value so the grading table can be exercised without a paid launch.
type permissionLaunchSignals struct {
	// continuableNotice reports that the client transmitted a refusal in
	// the continuable form, leaving the runtime free to carry the turn
	// on by another route.
	continuableNotice bool
	// noOptionNotice reports that the request offered the client no
	// refusing option at all, the posture that must end the attempt.
	noOptionNotice bool
	toolRecorded   bool
	turnFailed     bool
	// endedRequiringInput reports that the turn's error was the
	// human-input-required outcome.
	endedRequiringInput bool
	sessionID           string
}

// gradePermissionLaunch derives the permission, policy-precondition and
// protocol human_input rows from one asking-posture launch's signals.
//
// Permission handling and human input are two obligations, not one: a
// request for consent to act is refused in the continuable form and the
// turn carries on, while a request addressed to a person ends the
// attempt. A continuable refusal is therefore the wanted behavior and
// never lowers the human-input row. Since this launch induces a
// permission request and nothing else, it can only confirm a human-input
// ending it did not itself provoke; every other shape leaves human input
// unmeasured rather than graded against the runtime.
func gradePermissionLaunch(sig permissionLaunchSignals) (permission, policy, humanInput qualification.Observation) {
	switch {
	case sig.noOptionNotice && sig.endedRequiringInput:
		permission = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the request offered no refusing option and the attempt ended requiring human input", SessionID: sig.sessionID}
	case sig.noOptionNotice:
		permission = qualification.Observation{Grade: qualification.GradeGap, Outcome: qualification.OutcomePass, Detail: "the request offered no refusing option and the attempt did not end", SessionID: sig.sessionID}
	case sig.continuableNotice && sig.endedRequiringInput:
		permission = qualification.Observation{Grade: qualification.GradeGap, Outcome: qualification.OutcomePass, Detail: "a continuable refusal was transmitted and the attempt ended anyway", SessionID: sig.sessionID}
	case sig.continuableNotice:
		permission = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "a continuable refusal was transmitted and the turn stayed open", SessionID: sig.sessionID}
	case sig.turnFailed:
		permission = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the turn did not complete", SessionID: sig.sessionID}
	default:
		permission = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "no permission request was raised under this posture", SessionID: sig.sessionID}
	}

	refusalAnswered := sig.continuableNotice || sig.noOptionNotice
	switch {
	case refusalAnswered && !sig.toolRecorded:
		policy = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the refusal was answered and the marker stayed absent", SessionID: sig.sessionID}
	case refusalAnswered:
		policy = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the refusal was answered but the tool ran anyway", SessionID: sig.sessionID}
	default:
		policy = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "no refusal was answered", SessionID: sig.sessionID}
	}

	switch {
	case sig.endedRequiringInput && !refusalAnswered:
		humanInput = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the turn ended requiring human input with no permission request to attribute it to", SessionID: sig.sessionID, EvidencePath: qualification.SemanticEvidencePath(qualification.SurfaceProtocol)}
	case refusalAnswered:
		humanInput = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "this posture induced a permission request, which is not a request addressed to a person, so no human-input case was induced", SessionID: sig.sessionID}
	case sig.turnFailed:
		humanInput = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the turn did not complete, so no human-input case was induced", SessionID: sig.sessionID}
	default:
		humanInput = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "no request of either class was raised under this posture", SessionID: sig.sessionID}
	}

	return permission, policy, humanInput
}

// induceHumanInputNative induces the native surface's own human_input
// case: one turn under asking_args naming the failing probe.
func induceHumanInputNative(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) qualification.Observation {
	t.Helper()
	argv, ok := coords.Profile.AskingArgs(surface, coords.Model, "", promptNamingProbe(fixture, "failing"))
	if !ok {
		return qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "the profile states no asking posture for this surface"}
	}
	workspace := fixture.newLaunchWorkspace(t)
	output, launchErr := launchNativeProbe(t, coords.CommandPath, argv, workspace, fixture.env, fixture.ownership())
	fixture.recordNativeOutput(surface, output)
	terminal, transportLoss, found := nativeTerminal(coords.Profile, surface, output, launchErr)
	marker := probeStarted(workspace)
	sessionID := nativeSessionID(coords, surface, output)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	switch {
	case found && !terminal.Error && terminal.Case == qualification.CaseHumanInput:
		return qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the recognized terminal reported a human-input requirement", SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
	case found:
		detail := "a recognized terminal ended the turn without running the probe"
		if marker {
			detail = "a recognized terminal ended the turn though the probe's marker file was present"
		}
		return qualification.Observation{Grade: qualification.GradeGap, Outcome: qualification.OutcomePass, Detail: detail, SessionID: sessionID, EvidencePath: qualification.SemanticEvidencePath(surface)}
	default:
		return nativeFailureObservation(sessionID, transportLoss, qualification.OutcomeFixtureInductionFailed, "the terminal was not recognized")
	}
}
