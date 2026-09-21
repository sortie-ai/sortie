//go:build unix

package probe

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/domain"

	"github.com/sortie-ai/sortie/tools/qualify/eval"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

// newRFC4122V4 generates a random v4 UUID, the probe's correlation
// identifier for a launch, handed to a runtime under seed_args. It is
// never reported as an identifier a runtime itself produced.
func newRFC4122V4(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate continuation identifier: %v", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func induceProtocolContinuation(t *testing.T, coords Coordinates, fixture *sharedFixture) (seed, recall gradedObservation) {
	t.Helper()

	unmetRecall := transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: evidence.RecallPreconditionUnmet})

	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		seed := transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"})
		return seed, unmetRecall
	}
	adapter, err := clientprotocol.NewClientProtocolAdapter(nil)
	if err != nil {
		seed := transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "could not construct the induction adapter"})
		return seed, unmetRecall
	}

	fullCommand := append([]string{coords.CommandPath}, argv...)
	launchConfig := domain.AgentConfig{
		Kind:           "agent-client-protocol",
		Command:        strings.Join(fullCommand, " "),
		ReadTimeoutMS:  30000,
		TurnTimeoutMS:  int(continuationInductionTurnBound / time.Millisecond),
		StallTimeoutMS: 60000,
	}

	workspace := fixture.newLaunchWorkspace(t)
	firstSession, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   launchConfig,
	})
	if err != nil {
		seed := transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "the seed session failed to start"})
		return seed, unmetRecall
	}
	createdAt := time.Now().UTC()

	seedPrompt := strings.ReplaceAll(coords.Profile.ProbePrompts[promptKeyContinuationSeed], "{nonce}", fixture.nonce)
	seedResult, seedErr := adapter.RunTurn(context.Background(), firstSession, domain.RunTurnParams{
		Prompt:  seedPrompt,
		OnEvent: func(domain.AgentEvent) {},
	})
	fixture.usage.observe(firstSession.ID, seedResult.UsageMeasured)
	if stopErr := adapter.StopSession(context.Background(), firstSession); stopErr != nil {
		t.Errorf("stop the continuation induction's seed session: %v", stopErr)
	}
	assertSessionGroupAbsent(t, firstSession)

	seedOutcome := evidence.LaunchOutcomeCompleted
	if seedErr != nil {
		seedOutcome = evidence.LaunchOutcomeRunFailed
	}
	seedLaunch := evidence.LaunchRecord{Outcome: seedOutcome, SeededSessionID: firstSession.ID, Argv: argv, AuthEnvNames: coords.AuthEnvNames, Model: coords.Model, PromptID: promptKeyContinuationSeed}
	seedStreams := evidence.StreamCapture{Retention: evidence.StreamRetentionFull}
	seedObs := eval.RecognizeContinuationSeed(coords.Profile, evidence.SurfaceProtocol, seedLaunch, seedStreams)
	seedObs.SessionID = firstSession.ID
	seed = recognizerGraded(seedObs, seedLaunch, seedStreams)
	if seedErr != nil {
		recallLaunch := evidence.LaunchRecord{Outcome: evidence.LaunchOutcomeRunFailed, PriorSessionID: firstSession.ID}
		recallStreams := evidence.StreamCapture{Retention: evidence.StreamRetentionFull}
		return seed, recognizerGraded(eval.RecognizeContinuationRecall(coords.Profile, evidence.SurfaceProtocol, recallLaunch, recallStreams), recallLaunch, recallStreams)
	}

	awaitMinuteBoundary(createdAt)

	recallPrompt := coords.Profile.ProbePrompts[promptKeyContinuationRecall]
	secondSession, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     launchConfig,
		ResumeSessionID: firstSession.ID,
	})
	if err != nil {
		recallLaunch := evidence.LaunchRecord{Outcome: evidence.LaunchOutcomeLaunchFailed, PriorSessionID: firstSession.ID}
		recallStreams := evidence.StreamCapture{Retention: evidence.StreamRetentionFull}
		return seed, recognizerGraded(eval.RecognizeContinuationRecall(coords.Profile, evidence.SurfaceProtocol, recallLaunch, recallStreams), recallLaunch, recallStreams)
	}
	// Registered with the run rather than stopped here, so the
	// process-cleanup reading stops and measures this session while the
	// run is still being graded.
	if err := fixture.registerSession(adapter, secondSession); err != nil {
		_ = adapter.StopSession(context.Background(), secondSession)
		t.Fatalf("account for the continuation induction's recall session: %v", err)
	}
	t.Cleanup(func() {
		if _, err := fixture.stopOpenSessions(context.Background()); err != nil {
			t.Errorf("stop the continuation induction's recall session: %v", err)
		}
		assertSessionGroupAbsent(t, secondSession)
	})

	// A load replays the prior conversation into this turn's events, so
	// replayed chunks are told apart from new ones by the moment the
	// adapter observed them.
	startedAt := time.Now().UTC()
	var answer strings.Builder
	recallResult, recallErr := adapter.RunTurn(context.Background(), secondSession, domain.RunTurnParams{
		Prompt: recallPrompt,
		OnEvent: func(ev domain.AgentEvent) {
			if ev.Type != domain.EventNotification || !ev.Timestamp.After(startedAt) {
				return
			}
			answer.WriteString(ev.Message)
		},
	})
	fixture.usage.observe(secondSession.ID, recallResult.UsageMeasured)

	outcome := evidence.LaunchOutcomeCompleted
	if recallErr != nil || recallResult.ExitReason != domain.EventTurnCompleted {
		outcome = evidence.LaunchOutcomeRunFailed
	}
	recallLaunch := evidence.LaunchRecord{
		Outcome:         outcome,
		Nonce:           fixture.nonce,
		PriorSessionID:  firstSession.ID,
		SeededSessionID: secondSession.ID,
	}
	recallStreams := boundStreamCapture(answer.String())
	recall = recognizerGraded(eval.RecognizeContinuationRecall(coords.Profile, evidence.SurfaceProtocol, recallLaunch, recallStreams), recallLaunch, recallStreams)
	return seed, recall
}

// induceNativeContinuation skips recall with no resume_args or
// identifier. With resume_args but no seed_args, it resumes from the
// identifier the recognizer read out of the seed's terminal.
func induceNativeContinuation(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) (seed, recall gradedObservation) {
	t.Helper()

	entry := coords.Profile.EntryPoints[surface]
	correlationID := newRFC4122V4(t)
	workspace := fixture.newLaunchWorkspace(t)

	seedPrompt := strings.ReplaceAll(coords.Profile.ProbePrompts[promptKeyContinuationSeed], "{nonce}", fixture.nonce)
	seedArgv, err := coords.Profile.EntryArgs(surface, coords.Model, "", seedPrompt)
	if err != nil {
		seed = transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "could not resolve the native entry point"})
		recall = transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: evidence.RecallPreconditionUnmet})
		return seed, recall
	}
	seedArgv = append(seedArgv, substituteSessionID(entry.SeedArgs, correlationID)...)

	seedOutput, seedLaunchErr := launchNativeProbe(t, coords.CommandPath, seedArgv, workspace, fixture.env, fixture.ownership())
	fixture.recordNativeOutput(surface, seedOutput)

	seedLaunch := newNativeLaunchRecord(seedArgv, coords, promptKeyContinuationSeed, correlationID, seedLaunchErr)
	seedLaunch.Nonce = fixture.nonce
	seedStreams := boundStreamCapture(seedOutput)
	seedObs := eval.RecognizeContinuationSeed(coords.Profile, surface, seedLaunch, seedStreams)
	if !eval.RecognizedTerminal(coords.Profile, surface, seedLaunch, seedStreams) {
		recordUnrecognizedTerminal(coords, fixture, surface, seedOutput)
	}
	seed = recognizerGraded(seedObs, seedLaunch, seedStreams)
	seedSessionID := seedObs.SessionID

	// A resume names the identifier the runtime reported, otherwise the
	// one the seed launch was told to use. A surface with no seed_args
	// carried none.
	resumeSessionID := seedSessionID
	if resumeSessionID == "" && len(entry.SeedArgs) > 0 {
		resumeSessionID = correlationID
	}
	if len(entry.ResumeArgs) == 0 || resumeSessionID == "" {
		recall = transportGraded(evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: evidence.RecallPreconditionUnmet})
		return seed, recall
	}

	recallPrompt := coords.Profile.ProbePrompts[promptKeyContinuationRecall]
	recallArgv, err := coords.Profile.EntryArgs(surface, coords.Model, "", recallPrompt)
	if err != nil {
		recallLaunch := evidence.LaunchRecord{Outcome: evidence.LaunchOutcomeLaunchFailed, PriorSessionID: seedSessionID}
		recall = transportGraded(eval.RecognizeContinuationRecall(coords.Profile, surface, recallLaunch, evidence.StreamCapture{Retention: evidence.StreamRetentionFull}))
		return seed, recall
	}
	recallArgv = append(recallArgv, substituteSessionID(entry.ResumeArgs, resumeSessionID)...)

	recallOutput, recallLaunchErr := launchNativeProbe(t, coords.CommandPath, recallArgv, workspace, fixture.env, fixture.ownership())
	fixture.recordNativeOutput(surface, recallOutput)

	recallLaunch := newNativeLaunchRecord(recallArgv, coords, promptKeyContinuationRecall, "", recallLaunchErr)
	recallLaunch.Nonce = fixture.nonce
	recallLaunch.PriorSessionID = seedSessionID
	recallStreams := boundStreamCapture(recallOutput)
	recallObs := eval.RecognizeContinuationRecall(coords.Profile, surface, recallLaunch, recallStreams)
	if !eval.RecognizedTerminal(coords.Profile, surface, recallLaunch, recallStreams) {
		recordUnrecognizedTerminal(coords, fixture, surface, recallOutput)
	}
	recall = recognizerGraded(recallObs, recallLaunch, recallStreams)
	return seed, recall
}
