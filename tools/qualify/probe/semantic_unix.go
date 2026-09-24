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

	"github.com/sortie-ai/sortie/tools/qualify/eval"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/procgroup"
)

// permissionAcceptedNotice and permissionNoOptionNotice mirror
// agentcore.DecideHumanRequest's two notice strings; a change on the
// adapter side must be followed here.
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

// launchOutcomeFor maps a native launch's error onto the closed
// LaunchOutcome set: nil is completed, errNativeLaunchFailed is
// launch_failed, anything else is run_failed.
func launchOutcomeFor(err error) evidence.LaunchOutcome {
	switch {
	case err == nil:
		return evidence.LaunchOutcomeCompleted
	case errors.Is(err, errNativeLaunchFailed):
		return evidence.LaunchOutcomeLaunchFailed
	default:
		return evidence.LaunchOutcomeRunFailed
	}
}

// newNativeLaunchRecord builds the journal's launch record for one
// native case launch. seededSessionID is empty when the surface states
// no seed_args.
func newNativeLaunchRecord(argv []string, coords Coordinates, promptID, seededSessionID string, err error) evidence.LaunchRecord {
	return evidence.LaunchRecord{
		Argv:            argv,
		AuthEnvNames:    coords.AuthEnvNames,
		Model:           coords.Model,
		PromptID:        promptID,
		SeededSessionID: seededSessionID,
		Outcome:         launchOutcomeFor(err),
	}
}

// launchProtocolOneTurn launches one protocol session with a fresh
// workspace and runs a single turn with prompt, recording the turn's
// UsageMeasured reading into fixture's usage tracker.
func launchProtocolOneTurn(t *testing.T, coords Coordinates, fixture *sharedFixture, prompt string) (sessionID string, err error) {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, "", "")
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

// launchNativeOneTurn launches surface's native entry point with
// prompt, seeding a fresh identifier through seed_args when the surface
// states one; seededID is empty otherwise.
func launchNativeOneTurn(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface, prompt string) (output, seededID string, err error) {
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

// recordUnrecognizedTerminal appends output's terminal envelope to
// fixture's unrecognized set when the recognizer cannot resolve it to a
// known outcome; a no-op otherwise.
func recordUnrecognizedTerminal(coords Coordinates, fixture *sharedFixture, surface evidence.Surface, output string) {
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
func induceSuccess(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) gradedObservation {
	t.Helper()
	prompt := coords.Profile.ProbePrompts[promptKeySuccess]
	if surface == evidence.SurfaceProtocol {
		sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
		if err != nil {
			return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the success turn did not complete", SessionID: sessionID})
		}
		return transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the turn completed with no error", SessionID: sessionID, EvidencePath: evidence.SemanticEvidencePath(surface)})
	}
	output, seededID, launchErr := launchNativeOneTurn(t, coords, fixture, surface, prompt)
	argv, _ := coords.Profile.EntryArgs(surface, coords.Model, "", prompt)
	launch := newNativeLaunchRecord(argv, coords, string(evidence.InputDispositionSuccess), seededID, launchErr)
	streams := boundStreamCapture(output)
	obs, found := eval.Recognize(coords.Profile, surface, evidence.CaseSuccess, launch, streams)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	return recognizerGraded(obs, launch, streams)
}

// runtimeFailureModel is a model identifier no backend resolves, so
// the runtime rejects the launch before any request reaches a model,
// yielding a turn-level error rather than an ambiguous exit code.
const runtimeFailureModel = "sortie-qualification-refused-model"

// induceRuntimeFailure induces the runtime_failure case, graded from
// domain.ErrResponseError on the protocol surface and from the
// recognizer's error member on a native one.
func induceRuntimeFailure(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) gradedObservation {
	t.Helper()
	coords.Model = runtimeFailureModel
	prompt := coords.Profile.ProbePrompts[promptKeySuccess]
	if surface == evidence.SurfaceProtocol {
		sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
		var agentErr *domain.AgentError
		switch {
		case errors.As(err, &agentErr) && agentErr.Kind == domain.ErrResponseError:
			return transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the turn ended with a protocol-level error response", SessionID: sessionID, EvidencePath: evidence.SemanticEvidencePath(surface)})
		case err == nil:
			return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the turn completed instead of failing", SessionID: sessionID})
		default:
			return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the turn failed for a reason other than a protocol-level error response", SessionID: sessionID})
		}
	}
	output, seededID, launchErr := launchNativeOneTurn(t, coords, fixture, surface, prompt)
	argv, _ := coords.Profile.EntryArgs(surface, coords.Model, "", prompt)
	launch := newNativeLaunchRecord(argv, coords, string(evidence.InputDispositionRuntimeFailure), seededID, launchErr)
	streams := boundStreamCapture(output)
	obs, found := eval.Recognize(coords.Profile, surface, evidence.CaseRuntimeFailure, launch, streams)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	return recognizerGraded(obs, launch, streams)
}

// induceRefusalPair induces the runtime_refusal disposition case and
// derives its non_retryable_refusal retry peer from the same run.
func induceRefusalPair(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) (disposition, retry gradedObservation) {
	t.Helper()
	prompt := coords.Profile.ProbePrompts[promptKeyRuntimeRefusal]
	if surface == evidence.SurfaceProtocol {
		sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
		var agentErr *domain.AgentError
		var obs evidence.Observation
		switch {
		case errors.As(err, &agentErr) && agentErr.Kind == domain.ErrTurnRefused:
			obs = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the turn ended with a reported refusal", SessionID: sessionID, EvidencePath: evidence.SemanticEvidencePath(surface)}
		case err == nil:
			obs = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the turn completed instead of refusing", SessionID: sessionID}
		default:
			obs = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the turn failed for a reason other than a reported refusal", SessionID: sessionID}
		}
		disposition = transportGraded(obs)
	} else {
		output, seededID, launchErr := launchNativeOneTurn(t, coords, fixture, surface, prompt)
		argv, _ := coords.Profile.EntryArgs(surface, coords.Model, "", prompt)
		launch := newNativeLaunchRecord(argv, coords, string(evidence.InputDispositionRuntimeRefusal), seededID, launchErr)
		streams := boundStreamCapture(output)
		obs, found := eval.Recognize(coords.Profile, surface, evidence.CaseRuntimeRefusal, launch, streams)
		if !found {
			recordUnrecognizedTerminal(coords, fixture, surface, output)
		}
		disposition = recognizerGraded(obs, launch, streams)
	}
	retryObs := evidence.Observation{Grade: disposition.obs.Grade, Outcome: disposition.obs.Outcome, Detail: disposition.obs.Detail, SessionID: disposition.obs.SessionID, EvidencePath: disposition.obs.EvidencePath}
	retry = composedGraded(retryObs)
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

// awaitProbeUnderTurn folds every snapshot into owned. ended=true
// means the turn finished first, the one case no signal can be sent.
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

// The protocol surface needs its context cancelled to reach the
// cancelled terminal; signaling its process group instead surfaces as
// a port exit. A native surface takes one SIGINT once the cancellation
// probe's marker appears.
func induceCancellation(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) gradedObservation {
	t.Helper()
	if surface == evidence.SurfaceProtocol {
		return induceProtocolCancellation(t, coords, fixture,
			"the turn ended in cancellation after its own context was cancelled",
			"the turn did not end in cancellation after its own context was cancelled")
	}
	return induceNativeAsyncSignal(t, coords, fixture, surface, "cancellation", syscall.SIGINT,
		evidence.CaseCancellation, string(evidence.InputDispositionCancellation))
}

// induceProtocolCancellation runs the cancellation probe under a
// cancellable context, cancels once the probe is observed running, and
// grades the result against domain.ErrTurnCancelled.
func induceProtocolCancellation(t *testing.T, coords Coordinates, fixture *sharedFixture, matchDetail, missDetail string) gradedObservation {
	t.Helper()
	prompt := promptNamingProbe(fixture, "cancellation")
	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"})
	}
	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, "")
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "the induction session failed to start"})
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
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the turn ended before the probe was observed running", SessionID: session.ID})
	}
	if !observed {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the probe was never observed running under this launch", SessionID: session.ID})
	}

	cancel()
	runErr := <-done

	var agentErr *domain.AgentError
	switch {
	case errors.As(runErr, &agentErr) && agentErr.Kind == domain.ErrTurnCancelled:
		return transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: matchDetail, SessionID: session.ID, EvidencePath: evidence.SemanticEvidencePath(evidence.SurfaceProtocol)})
	case runErr == nil:
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: missDetail, SessionID: session.ID})
	default:
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: missDetail, SessionID: session.ID})
	}
}

// induceRetryableTransport induces the retryable_transport case: once
// the transport probe's started marker appears, its process group is
// SIGKILLed.
func induceRetryableTransport(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) gradedObservation {
	t.Helper()
	return induceAsyncSignalCase(t, coords, fixture, surface, "transport", syscall.SIGKILL,
		domain.ErrPortExit, evidence.CaseRetryableTransport,
		"the launch reported transport loss after one SIGKILL to its group",
		"the launch did not report transport loss after one SIGKILL to its group")
}

// induceAsyncSignalCase shares the async-launch, marker-wait,
// exactly-one-signal shape cancellation and retryable_transport follow,
// differing only in probe, signal, and outcome.
func induceAsyncSignalCase(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface, probeName string, sig syscall.Signal, wantKind domain.AgentErrorKind, wantCase evidence.Case, matchDetail, missDetail string) gradedObservation {
	t.Helper()
	if surface == evidence.SurfaceProtocol {
		return induceProtocolAsyncSignal(t, coords, fixture, probeName, sig, wantKind, matchDetail, missDetail)
	}
	return induceNativeAsyncSignal(t, coords, fixture, surface, probeName, sig, wantCase, string(evidence.InputRetryableTransport))
}

// induceProtocolAsyncSignal runs the named probe under a protocol
// session, sends sig to its process group once the probe is observed
// running, and grades against wantKind.
func induceProtocolAsyncSignal(t *testing.T, coords Coordinates, fixture *sharedFixture, probeName string, sig syscall.Signal, wantKind domain.AgentErrorKind, matchDetail, missDetail string) gradedObservation {
	t.Helper()
	prompt := promptNamingProbe(fixture, probeName)
	probePath := fixture.probePath(probeName)
	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"})
	}
	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, "")
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "the induction session failed to start"})
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
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the turn ended before the probe was observed running", SessionID: session.ID})
	}
	if !observed {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the probe was never observed running under this launch", SessionID: session.ID})
	}

	killProtocolSession(t, session, sig)
	runErr := <-done

	var agentErr *domain.AgentError
	switch {
	case errors.As(runErr, &agentErr) && agentErr.Kind == wantKind:
		return transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: matchDetail, SessionID: session.ID, EvidencePath: evidence.SemanticEvidencePath(evidence.SurfaceProtocol)})
	case runErr == nil:
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: missDetail, SessionID: session.ID})
	default:
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: missDetail, SessionID: session.ID})
	}
}

// induceNativeAsyncSignal launches surface's native entry point, sends
// sig to its process group once the named probe is observed running,
// and grades from eval.Recognize's transport-loss reading.
func induceNativeAsyncSignal(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface, probeName string, sig syscall.Signal, wantCase evidence.Case, promptID string) gradedObservation {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(surface, coords.Model, "", promptNamingProbe(fixture, probeName))
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "could not resolve the native entry point"})
	}
	workspace := fixture.newLaunchWorkspace(t)
	launch, err := startBoundedLaunch(coords.CommandPath, argv, workspace, fixture.env, fixture.ownership())
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the native launch did not start"})
	}
	defer launch.cancel()

	if !awaitProbeExecution(fixture.ownership(), fixture.probePath(probeName), launch.pgid, time.Now().Add(fixture.observationBound())) {
		if _, _, ended := launch.terminate(); ended {
			procgroup.AwaitAbsence(t, launch.pgid)
		}
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the probe was never observed running under this launch"})
	}

	_ = signalProcessGroup(launch.pgid, sig)
	// The signal is a request: a runtime that ignores it or a descendant
	// holding the streams open would hold this wait past every bound, so
	// the group comes down when this wait expires.
	result, output, ended := launch.await(fixture.signalBound())
	if !ended {
		result, output, ended = launch.terminate()
		if !ended {
			return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the launch did not end within its own bound after the signal"})
		}
	}
	waitErr := result.WaitErr
	procgroup.AwaitAbsence(t, launch.pgid)

	fixture.recordNativeOutput(surface, output)
	launchRecord := newNativeLaunchRecord(argv, coords, promptID, "", waitErr)
	streams := boundStreamCapture(output)
	obs, found := eval.Recognize(coords.Profile, surface, wantCase, launchRecord, streams)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	return recognizerGraded(obs, launchRecord, streams)
}

// induceProtocolLimit induces limit_reached with an oversize filler
// sized from clientProtocolMaxLineBytesMirror, graded from the
// runtime's own token or request limit error.
func induceProtocolLimit(t *testing.T, coords Coordinates, fixture *sharedFixture) gradedObservation {
	t.Helper()
	fillerSize := clientProtocolMaxLineBytesMirror - limitReachedMargin
	prompt := strings.Repeat("a", fillerSize) + "\n\nReply with exactly SORTIE_LIMIT_PROBE_DONE."
	sessionID, err := launchProtocolOneTurn(t, coords, fixture, prompt)
	var agentErr *domain.AgentError
	switch {
	case errors.As(err, &agentErr) && (agentErr.Kind == domain.ErrTurnTokenLimit || agentErr.Kind == domain.ErrTurnRequestLimit):
		return transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the oversize turn ended at the runtime's own token or request limit", SessionID: sessionID, EvidencePath: evidence.SemanticEvidencePath(evidence.SurfaceProtocol)})
	case err == nil:
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "the oversize turn completed normally instead of reaching a limit", SessionID: sessionID})
	default:
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the oversize turn failed for a reason other than reaching a limit", SessionID: sessionID})
	}
}

// induceToolServer runs on its own permissive-posture launch, separate
// from the asking-posture launch: its usable arm needs the probe's
// marker present, which asking-posture rows need absent.
func induceToolServer(t *testing.T, coords Coordinates, fixture *sharedFixture) gradedObservation {
	t.Helper()

	dir := t.TempDir()
	callRecordPath := dir + "/calls.jsonl"
	scriptPath := agenttest.FakeRuntime(t, dir, "mcp-server", mcpToolServerScenario, mcpToolServerParams{RecordPath: callRecordPath})
	mcpConfigPath := writeToolServerMCPConfig(t, dir, scriptPath)
	policyPath := writeToolServerPolicy(t, dir, coords.Profile)

	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, policyPath, "")
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"})
	}

	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, mcpConfigPath)
	if err != nil {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the tool-server induction session failed to start"})
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
		return transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the declared tool server recorded a call and the turn consumed it", SessionID: sessionID})
	case runErr != nil:
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the tool-server induction turn did not complete", SessionID: sessionID})
	case calledAnyTool:
		return transportGraded(evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "the turn completed a tool call that never reached the declared server", SessionID: sessionID})
	default:
		return transportGraded(evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "the turn completed without calling any tool, so the declared server was never reached", SessionID: sessionID})
	}
}

// inducePermissionPolicyHumanInput drives one asking-posture launch and
// derives permission, policy, and human_input rows from it, staying
// unauthorized since every usable arm needs the marker absent.
func inducePermissionPolicyHumanInput(t *testing.T, coords Coordinates, fixture *sharedFixture) (permission, policy, humanInput gradedObservation) {
	t.Helper()

	dir := t.TempDir()
	callRecordPath := dir + "/calls.jsonl"
	scriptPath := agenttest.FakeRuntime(t, dir, "mcp-server", mcpToolServerScenario, mcpToolServerParams{RecordPath: callRecordPath})
	mcpConfigPath := writeToolServerMCPConfig(t, dir, scriptPath)

	argv, ok := coords.Profile.AskingArgs(evidence.SurfaceProtocol, coords.Model, "", "")
	if !ok {
		obs := evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "the profile states no asking posture for the protocol surface"}
		g := transportGraded(obs)
		return g, g, g
	}

	workspace := fixture.newLaunchWorkspace(t)
	adapter, session, err := startInductionSession(t, coords, argv, workspace, mcpConfigPath)
	if err != nil {
		obs := evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the asking-posture induction session failed to start"}
		g := transportGraded(obs)
		return g, g, g
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
	permissionObs, policyObs, humanInputObs := gradePermissionLaunch(signals)
	return transportGraded(permissionObs), transportGraded(policyObs), transportGraded(humanInputObs)
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

func gradePermissionLaunch(sig permissionLaunchSignals) (permission, policy, humanInput evidence.Observation) {
	switch {
	case sig.noOptionNotice && sig.endedRequiringInput:
		permission = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the request offered no refusing option and the attempt ended requiring human input", SessionID: sig.sessionID}
	case sig.noOptionNotice:
		permission = evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "the request offered no refusing option and the attempt did not end", SessionID: sig.sessionID}
	case sig.continuableNotice && sig.endedRequiringInput:
		permission = evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "a continuable refusal was transmitted and the attempt ended anyway", SessionID: sig.sessionID}
	case sig.continuableNotice:
		permission = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "a continuable refusal was transmitted and the turn stayed open", SessionID: sig.sessionID}
	case sig.turnFailed:
		permission = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the turn did not complete", SessionID: sig.sessionID}
	default:
		permission = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "no permission request was raised under this posture", SessionID: sig.sessionID}
	}

	refusalAnswered := sig.continuableNotice || sig.noOptionNotice
	switch {
	case refusalAnswered && !sig.toolRecorded:
		policy = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the refusal was answered and the marker stayed absent", SessionID: sig.sessionID}
	case refusalAnswered:
		policy = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the refusal was answered but the tool ran anyway", SessionID: sig.sessionID}
	default:
		policy = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "no refusal was answered", SessionID: sig.sessionID}
	}

	humanInputPath := evidence.SemanticEvidencePath(evidence.SurfaceProtocol)
	switch {
	case sig.endedRequiringInput && sig.noOptionNotice:
		humanInput = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the request offered no refusing option and the attempt ended requiring human input", SessionID: sig.sessionID, EvidencePath: humanInputPath}
	case sig.endedRequiringInput && sig.continuableNotice:
		humanInput = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "a continuable refusal was transmitted and the attempt still ended requiring human input", SessionID: sig.sessionID, EvidencePath: humanInputPath}
	case sig.endedRequiringInput:
		humanInput = evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the turn ended requiring human input with no permission request to attribute it to", SessionID: sig.sessionID, EvidencePath: humanInputPath}
	case sig.turnFailed:
		humanInput = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeRuntimeFailed, Detail: "the turn did not complete, so no human-input case was induced", SessionID: sig.sessionID}
	case sig.noOptionNotice:
		humanInput = evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: "the request offered no refusing option and the turn went on without ending requiring human input", SessionID: sig.sessionID, EvidencePath: humanInputPath}
	case sig.continuableNotice:
		humanInput = evidence.Observation{Grade: evidence.GradeNotApplicable, Outcome: evidence.OutcomeNotApplicable, Detail: "the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose", SessionID: sig.sessionID, EvidencePath: humanInputPath}
	default:
		humanInput = evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "no request of either class was raised under this posture", SessionID: sig.sessionID}
	}

	return permission, policy, humanInput
}

// induceHumanInputNative induces the native surface's own human_input
// case: one turn under asking_args naming the failing probe.
func induceHumanInputNative(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) gradedObservation {
	t.Helper()
	argv, ok := coords.Profile.AskingArgs(surface, coords.Model, "", promptNamingProbe(fixture, "failing"))
	if !ok {
		return transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "the profile states no asking posture for this surface"})
	}
	workspace := fixture.newLaunchWorkspace(t)
	output, launchErr := launchNativeProbe(t, coords.CommandPath, argv, workspace, fixture.env, fixture.ownership())
	fixture.recordNativeOutput(surface, output)
	marker := probeStarted(workspace)
	launch := newNativeLaunchRecord(argv, coords, string(evidence.InputRetryHumanInput), "", launchErr)
	launch.ProbeMarker = marker
	streams := boundStreamCapture(output)
	obs, found := eval.Recognize(coords.Profile, surface, evidence.CaseHumanInput, launch, streams)
	if !found {
		recordUnrecognizedTerminal(coords, fixture, surface, output)
	}
	return recognizerGraded(obs, launch, streams)
}
