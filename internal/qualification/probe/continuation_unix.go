//go:build unix

package probe

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// recallEvidence is the state one continuation recall attempt reached,
// before it is narrowed to the closed detail set a record may carry.
type recallEvidence uint8

const (
	// recallAttemptFailed: the recall turn never completed.
	recallAttemptFailed recallEvidence = iota

	// recallAnswered: the new answer carried the nonce the seed turn
	// alone was told.
	recallAnswered

	// recallSameSessionSilent: completed in the seed's session and
	// answered without that nonce.
	recallSameSessionSilent

	// recallDeclined: completed in the seed's session and declined to
	// answer.
	recallDeclined

	// recallFreshSession: completed in a session other than the seed's.
	recallFreshSession

	// recallSessionUnobserved: completed without reporting which session
	// it ran in.
	recallSessionUnobserved
)

// recallObservation narrows one recall attempt to the record the
// evidence vocabulary admits. Only a new answer carrying the nonce is
// functional recall; every other completed attempt reports the session
// it observed or none, never an identifier this run generated.
func recallObservation(evidence recallEvidence, seedSessionID, actualSessionID string) qualification.Observation {
	switch evidence {
	case recallAnswered:
		return qualification.Observation{
			Grade:     qualification.GradeUsable,
			Outcome:   qualification.OutcomePass,
			Detail:    qualification.RecallConfirmedSameSession,
			SessionID: seedSessionID,
		}
	case recallFreshSession:
		return qualification.Observation{
			Grade:     qualification.GradeGap,
			Outcome:   qualification.OutcomePass,
			Detail:    qualification.RecallFreshFallback,
			SessionID: actualSessionID,
		}
	case recallSameSessionSilent:
		return qualification.Observation{
			Grade:     qualification.GradeGap,
			Outcome:   qualification.OutcomePass,
			Detail:    qualification.RecallSameSessionWithoutRecall,
			SessionID: actualSessionID,
		}
	case recallDeclined:
		return qualification.Observation{
			Grade:     qualification.GradeNotObserved,
			Outcome:   qualification.OutcomeFixtureInductionFailed,
			Detail:    qualification.RecallDeclined,
			SessionID: actualSessionID,
		}
	case recallAttemptFailed, recallSessionUnobserved:
	}
	return qualification.Observation{
		Grade:   qualification.GradeNotObserved,
		Outcome: qualification.OutcomeRuntimeFailed,
		Detail:  qualification.RecallUnobservedActual,
	}
}

// answerCarriesNonce reports whether text carries nonce. An empty nonce
// never matches: a fixture that seeded nothing cannot have its memory
// confirmed by every answer a runtime gives.
func answerCarriesNonce(text, nonce string) bool {
	return nonce != "" && strings.Contains(text, nonce)
}

// declineMarkers are the phrases an answer carries when it states
// unwillingness to answer. Some safety training reads the recall prompt
// as a credential-replay request and answers with a policy statement.
var declineMarkers = []string{
	"i will not",
	"i won't",
	"i am not going to",
	"i'm not going to",
	"i am not designed to",
	"i'm not designed to",
	"i am not supposed to",
	"i'm not supposed to",
	"i am not comfortable",
	"i'm not comfortable",
	"i would rather not",
	"i'd rather not",
	"i decline",
}

// historyMarkers are the words an answer uses to speak about the turns
// before it. An answer reporting on its own history says something
// about the session it ran in, so the grade it earns stands.
var historyMarkers = []string{
	"prior",
	"previous",
	"earlier",
	"first message",
	"no record",
	"no memory",
}

// answerDeclinesRecall reports whether text states unwillingness to
// answer the recall prompt. It is deliberately narrow: a false positive
// would publish a runtime that really lost the session as one nobody
// measured, hiding a shortfall the operator feels.
func answerDeclinesRecall(text string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(text, "\u2019", "'"))
	for _, marker := range historyMarkers {
		if strings.Contains(normalized, marker) {
			return false
		}
	}
	return slices.ContainsFunc(declineMarkers, func(marker string) bool {
		return strings.Contains(normalized, marker)
	})
}

// nativeAnswerChannels resolves the text channels one native launch
// answered on, one per member path. A streaming runtime writes the same
// member once per delta, so a channel answers only when read whole and
// in order. Excluded are members the recognizer names for something
// other than the turn's text, the launch's echo of a prompt this run
// supplied, and text written outside a structured record.
func nativeAnswerChannels(recognizer qualification.Recognizer, output string, prompts ...string) []string {
	channels := map[string]*strings.Builder{}
	machinery := recognizerMachineryMembers(recognizer)
	for _, record := range decodeLaunchRecords(output) {
		accumulateAnswerText(record, "", machinery, prompts, channels)
	}
	texts := make([]string, 0, len(channels))
	for _, channel := range channels {
		texts = append(texts, channel.String())
	}
	return texts
}

func nativeAnswerCarriesNonce(recognizer qualification.Recognizer, output, nonce string, prompts ...string) bool {
	if nonce == "" {
		return false
	}
	return slices.ContainsFunc(nativeAnswerChannels(recognizer, output, prompts...), func(channel string) bool {
		return answerCarriesNonce(channel, nonce)
	})
}

func nativeAnswerDeclinesRecall(recognizer qualification.Recognizer, output string, prompts ...string) bool {
	return slices.ContainsFunc(nativeAnswerChannels(recognizer, output, prompts...), answerDeclinesRecall)
}

// recognizerMachineryMembers names the members recognizer resolves for
// something other than the turn's text: the discriminator, the outcome
// members, and the heads of the identifier, model-request, and token
// paths. A name is matched at any depth, so a nested terminal excludes
// the same members as a flat one.
func recognizerMachineryMembers(recognizer qualification.Recognizer) map[string]bool {
	members := map[string]bool{}
	for _, name := range append([]string{recognizer.Locator.DiscriminatorKey, recognizer.StatusMember}, recognizer.ErrorMembers...) {
		if name != "" {
			members[name] = true
		}
	}
	heads := [][]string{recognizer.SessionIDPath, recognizer.ModelRequestPath}
	for _, path := range recognizer.TokenPaths {
		heads = append(heads, path.Path)
	}
	for _, path := range heads {
		if len(path) > 0 {
			members[path[0]] = true
		}
	}
	return members
}

// accumulateAnswerText appends value's text to the channel each member
// path names, descending through objects and list elements, which share
// their member's channel since a split message is written across them.
func accumulateAnswerText(value any, path string, machinery map[string]bool, prompts []string, channels map[string]*strings.Builder) {
	switch typed := value.(type) {
	case map[string]any:
		for member, nested := range typed {
			if machinery[member] {
				continue
			}
			accumulateAnswerText(nested, path+"."+member, machinery, prompts, channels)
		}
	case []any:
		for _, element := range typed {
			accumulateAnswerText(element, path, machinery, prompts, channels)
		}
	case string:
		if echoesAPrompt(typed, prompts) {
			return
		}
		channel, open := channels[path]
		if !open {
			channel = &strings.Builder{}
			channels[path] = channel
		}
		channel.WriteString(typed)
	}
}

// echoesAPrompt reports whether text is a launch's echo of one of the
// prompts this run supplied, which answers nothing.
func echoesAPrompt(text string, prompts []string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	for _, prompt := range prompts {
		if trimmed == strings.TrimSpace(prompt) {
			return true
		}
	}
	return false
}

// decodeLaunchRecords decodes one launch's captured output into the JSON
// objects it carries, the way the recognizer reads them: newline-
// delimited objects first, otherwise a stream from the first brace on.
func decodeLaunchRecords(output string) []map[string]any {
	var records []map[string]any
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err == nil {
			records = append(records, record)
		}
	}
	if len(records) > 0 {
		return records
	}
	brace := strings.Index(output, "{")
	if brace < 0 {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(output[brace:]))
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			return records
		}
		records = append(records, record)
	}
}

// induceProtocolContinuation drives the protocol surface's continuation
// seed and recall through the adapter-level resume oracle, grading the
// recall on the answer it produced.
func induceProtocolContinuation(t *testing.T, coords Coordinates, fixture *sharedFixture) (seed, recall qualification.Observation) {
	t.Helper()

	unmetRecall := qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: qualification.RecallPreconditionUnmet}

	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		seed := qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "could not resolve the protocol entry point"}
		return seed, unmetRecall
	}
	adapter, err := clientprotocol.NewClientProtocolAdapter(nil)
	if err != nil {
		seed := qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "could not construct the induction adapter"}
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
		seed := qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "the seed session failed to start"}
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
	if seedErr != nil {
		seed := qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeRuntimeFailed, Detail: "the seed turn did not complete", SessionID: firstSession.ID}
		return seed, recallObservation(recallAttemptFailed, firstSession.ID, "")
	}
	seed = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the seed turn completed and left history", SessionID: firstSession.ID}

	awaitMinuteBoundary(createdAt)

	recallPrompt := coords.Profile.ProbePrompts[promptKeyContinuationRecall]
	secondSession, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     launchConfig,
		ResumeSessionID: firstSession.ID,
	})
	if err != nil {
		return seed, recallObservation(recallAttemptFailed, firstSession.ID, "")
	}
	// Registered with the run rather than stopped here, so the
	// process-cleanup reading stops and measures this session while the
	// run is still being graded.
	if err := fixture.registerSession(adapter, secondSession); err != nil {
		_ = adapter.StopSession(context.Background(), secondSession)
		t.Fatalf("account for the continuation induction's recall session: %v", err)
	}
	t.Cleanup(func() {
		if err := fixture.stopOpenSessions(context.Background()); err != nil {
			t.Errorf("stop the continuation induction's recall session: %v", err)
		}
		assertSessionGroupAbsent(t, secondSession)
	})

	// A load replays the prior conversation, and chunks the adapter
	// queued while no turn ran are flushed into this turn's events. Only
	// text this turn produced can answer it, so replayed chunks are told
	// apart by the moment the adapter observed them.
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

	switch {
	case recallErr != nil || recallResult.ExitReason != domain.EventTurnCompleted:
		recall = recallObservation(recallAttemptFailed, firstSession.ID, secondSession.ID)
	case secondSession.ID != firstSession.ID:
		recall = recallObservation(recallFreshSession, firstSession.ID, secondSession.ID)
	case answerCarriesNonce(answer.String(), fixture.nonce):
		recall = recallObservation(recallAnswered, firstSession.ID, secondSession.ID)
	case answerDeclinesRecall(answer.String()):
		recall = recallObservation(recallDeclined, firstSession.ID, secondSession.ID)
	default:
		recall = recallObservation(recallSameSessionSilent, firstSession.ID, secondSession.ID)
	}
	return seed, recall
}

// induceNativeContinuation drives one native surface's continuation
// seed and recall. The recall is skipped when the surface states no
// resume_args or no identifier exists to name; a surface stating
// resume_args without seed_args resumes from the identifier its
// recognizer read out of the seed's terminal.
func induceNativeContinuation(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) (seed, recall qualification.Observation) {
	t.Helper()

	entry := coords.Profile.EntryPoints[surface]
	correlationID := newRFC4122V4(t)
	workspace := fixture.newLaunchWorkspace(t)

	seedPrompt := strings.ReplaceAll(coords.Profile.ProbePrompts[promptKeyContinuationSeed], "{nonce}", fixture.nonce)
	seedArgv, err := coords.Profile.EntryArgs(surface, coords.Model, "", seedPrompt)
	if err != nil {
		seed = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: "could not resolve the native entry point"}
		recall = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: qualification.RecallPreconditionUnmet}
		return seed, recall
	}
	seedArgv = append(seedArgv, substituteSessionID(entry.SeedArgs, correlationID)...)

	seedOutput, seedLaunchErr := launchNativeProbe(t, coords.CommandPath, seedArgv, workspace, fixture.env, fixture.ownership())
	fixture.recordNativeOutput(surface, seedOutput)
	seedTerminal, seedTransportLoss, seedFound := nativeTerminal(coords.Profile, surface, seedOutput, seedLaunchErr)
	if !seedFound {
		recordUnrecognizedTerminal(coords, fixture, surface, seedOutput)
	}

	seedSessionID := nativeSessionID(coords, surface, seedOutput)

	if seedFound && !seedTerminal.Error {
		seed = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the seed launch completed a turn that left history", SessionID: seedSessionID}
	} else {
		seed = nativeFailureObservation(seedSessionID, seedTransportLoss, qualification.OutcomeRuntimeFailed, "the seed launch did not complete a turn")
	}

	// A resume names the identifier the runtime reported, otherwise the
	// one the seed launch was told to use. A surface with no seed_args
	// carried none.
	resumeSessionID := seedSessionID
	if resumeSessionID == "" && len(entry.SeedArgs) > 0 {
		resumeSessionID = correlationID
	}
	if len(entry.ResumeArgs) == 0 || resumeSessionID == "" {
		recall = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomePrerequisiteFailed, Detail: qualification.RecallPreconditionUnmet}
		return seed, recall
	}

	recallPrompt := coords.Profile.ProbePrompts[promptKeyContinuationRecall]
	recallArgv, err := coords.Profile.EntryArgs(surface, coords.Model, "", recallPrompt)
	if err != nil {
		return seed, recallObservation(recallAttemptFailed, seedSessionID, "")
	}
	recallArgv = append(recallArgv, substituteSessionID(entry.ResumeArgs, resumeSessionID)...)

	recallOutput, recallLaunchErr := launchNativeProbe(t, coords.CommandPath, recallArgv, workspace, fixture.env, fixture.ownership())
	fixture.recordNativeOutput(surface, recallOutput)
	recallTerminal, _, recallFound := nativeTerminal(coords.Profile, surface, recallOutput, recallLaunchErr)
	if !recallFound {
		recordUnrecognizedTerminal(coords, fixture, surface, recallOutput)
	}

	recognizer := coords.Profile.Recognizers[surface]
	recallSessionID := nativeSessionID(coords, surface, recallOutput)

	switch {
	case !recallFound || recallTerminal.Error:
		recall = recallObservation(recallAttemptFailed, seedSessionID, recallSessionID)
	case seedSessionID != "" && recallSessionID != "" && recallSessionID != seedSessionID:
		recall = recallObservation(recallFreshSession, seedSessionID, recallSessionID)
	case nativeAnswerCarriesNonce(recognizer, recallOutput, fixture.nonce, recallPrompt, seedPrompt):
		recall = recallObservation(recallAnswered, seedSessionID, recallSessionID)
	case seedSessionID != "" && recallSessionID != "" && nativeAnswerDeclinesRecall(recognizer, recallOutput, recallPrompt, seedPrompt):
		recall = recallObservation(recallDeclined, seedSessionID, recallSessionID)
	case seedSessionID != "" && recallSessionID != "":
		recall = recallObservation(recallSameSessionSilent, seedSessionID, recallSessionID)
	default:
		recall = recallObservation(recallSessionUnobserved, seedSessionID, recallSessionID)
	}
	return seed, recall
}
