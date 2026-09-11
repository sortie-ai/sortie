package clientprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// resolveSessionOutcome bundles resolveSession's return values so a
// test can hand them across a goroutine boundary on one channel.
type resolveSessionOutcome struct {
	sessionID string
	err       *domain.AgentError
}

// runResolveSessionAsync starts resolveSession on its own goroutine
// with an empty workspace and no tool servers, neither of which any
// case in this file depends on, and returns the channel its outcome
// arrives on so the calling test can drive the simulated peer while
// the call is in flight.
func runResolveSessionAsync(ctx context.Context, state *sessionState, resumeID string, caps agentCapabilities) <-chan resolveSessionOutcome {
	ch := make(chan resolveSessionOutcome, 1)
	go func() {
		sessionID, err := resolveSession(ctx, state, resumeID, caps, "", nil)
		ch <- resolveSessionOutcome{sessionID: sessionID, err: err}
	}()
	return ch
}

// awaitResolveSessionOutcome waits for outcome on ch, failing t if it
// does not arrive within awaitTimeout.
func awaitResolveSessionOutcome(t *testing.T, ch <-chan resolveSessionOutcome) resolveSessionOutcome {
	t.Helper()
	select {
	case outcome := <-ch:
		return outcome
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for resolveSession to return")
		return resolveSessionOutcome{}
	}
}

// capsAdvertisingLoad returns handshake-advertised capabilities that
// select the session/load continuation route.
func capsAdvertisingLoad() agentCapabilities {
	loadSession := true
	return agentCapabilities{LoadSession: &loadSession}
}

// capsAdvertisingResume returns handshake-advertised capabilities that
// advertise session/resume and no session/load, so the resume route is
// the one chooseContinuationMethod selects.
func capsAdvertisingResume() agentCapabilities {
	return agentCapabilities{SessionCapabilities: &sessionCapabilities{Resume: &sessionResumeCapabilities{}}}
}

// firstOutboundMethod decodes the very first line the pump wrote and
// returns its method and raw id, failing t on a decode error. Unlike
// outboundReader.awaitMethod, which discards lines for any other
// method while scanning forward, this fails a test outright when an
// earlier, unwanted line was written first.
func firstOutboundMethod(t *testing.T, out *outboundReader) (method string, id json.RawMessage) {
	t.Helper()
	line := out.next(t)
	var h wireHeader
	if err := json.Unmarshal(line, &h); err != nil {
		t.Fatalf("decode first outbound line %s: %v", line, err)
	}
	return h.Method, h.ID
}

// assertSessionContinuationEntry marks state's session id known, runs
// one turn to completion, and fails t unless the once-per-session gap
// notice's mention of the session continuation label matches wantGap.
// A gap notice is guaranteed to fire regardless of the outcome under
// test, because a local launch's stage-one record already carries
// tokenCounts at gap.
func assertSessionContinuationEntry(t *testing.T, state *sessionState, out *outboundReader, inPw io.Writer, wantGap bool) {
	t.Helper()

	markSessionKnown(state)
	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	notice := firstNotification(t, events)
	gotGap := strings.Contains(notice.Message, capabilityLabelSessionContinuation)
	if gotGap != wantGap {
		t.Errorf("gap notice = %q, session continuation listed = %v, want %v", notice.Message, gotGap, wantGap)
	}
}

// TestChooseContinuationMethod confirms the fixed priority order:
// session/load first when advertised, session/resume only
// when load is not, and no continuation method when neither is.
func TestChooseContinuationMethod(t *testing.T) {
	t.Parallel()

	both := capsAdvertisingLoad()
	both.SessionCapabilities = capsAdvertisingResume().SessionCapabilities

	tests := []struct {
		name string
		caps agentCapabilities
		want continuationMethod
	}{
		{name: "both load and resume advertised selects load", caps: both, want: continuationLoad},
		{name: "only load advertised selects load", caps: capsAdvertisingLoad(), want: continuationLoad},
		{name: "only resume advertised selects resume", caps: capsAdvertisingResume(), want: continuationResume},
		{name: "neither advertised selects no continuation method", caps: agentCapabilities{}, want: continuationNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := chooseContinuationMethod(tt.caps)

			if got != tt.want {
				t.Errorf("chooseContinuationMethod(%+v) = %v, want %v", tt.caps, got, tt.want)
			}
		})
	}
}

// TestResolveSessionEmptyResumeIDSkipsContinuation confirms that an
// empty ResumeSessionID calls session/new alone, with no negative
// control and no continuation method ever sent, even though the
// handshake advertised one.
func TestResolveSessionEmptyResumeIDSkipsContinuation(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "", capsAdvertisingLoad())

	method, id := firstOutboundMethod(t, out)
	if method != methodSessionNew {
		t.Fatalf("first outbound method = %q, want %q: an empty ResumeSessionID must call session/new alone", method, methodSessionNew)
	}
	respondLine(t, inPw, id, newSessionResponse{SessionID: sessionId("brand-new")})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "brand-new" {
		t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "brand-new")
	}
}

// TestResolveSessionLoadUnimplementedLowersAndFallsBack confirms that
// a session/load call answered
// with the negative control's own error code lowers the
// sessionContinuation entry and falls back to a new session, without
// failing the run.
func TestResolveSessionLoadUnimplementedLowersAndFallsBack(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	respondErrorLine(t, inPw, loadID, jsonrpcMethodNotFound, "method not found")

	newID := out.awaitMethod(t, methodSessionNew)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil: a continuation the agent does not implement must not fail the run", outcome.err)
	}
	if outcome.sessionID != "fallback-session" {
		t.Errorf("resolveSession() session id = %q, want %q: the fallback session/new's own identifier", outcome.sessionID, "fallback-session")
	}

	assertSessionContinuationEntry(t, state, out, inPw, true)
}

// TestResolveSessionLoadRealFailureLowersAndFallsBack confirms that a
// session/load call answered with an error other than
// the negative control's own code is a real failure of that call, and
// it still lowers the sessionContinuation entry and falls back, the
// same as the unimplemented case, because the capability did not
// deliver either way.
func TestResolveSessionLoadRealFailureLowersAndFallsBack(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	respondErrorLine(t, inPw, loadID, -32000, "session store unavailable")

	newID := out.awaitMethod(t, methodSessionNew)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil: a real continuation failure must still fall back rather than fail the run", outcome.err)
	}
	if outcome.sessionID != "fallback-session" {
		t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "fallback-session")
	}

	assertSessionContinuationEntry(t, state, out, inPw, true)
}

// TestResolveSessionLoadSuccessNoReplayLowersAndFallsBack confirms
// that a session/load response carrying success is not enough on its
// own. With no chunk ever replayed for the loaded identifier, the
// pump's own bounded wait elapses, the entry is lowered, and this
// falls back to a new session exactly as a load error would.
func TestResolveSessionLoadSuccessNoReplayLowersAndFallsBack(t *testing.T) {
	t.Parallel()

	const noReplayReadTimeoutMS = 50

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: noReplayReadTimeoutMS}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	respondLine(t, inPw, loadID, loadSessionResponse{})

	newID := out.awaitMethod(t, methodSessionNew)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil: a load that replays nothing must not fail the run", outcome.err)
	}
	if outcome.sessionID != "fallback-session" {
		t.Errorf("resolveSession() session id = %q, want %q: success with no observed replay must not be trusted", outcome.sessionID, "fallback-session")
	}

	// resolveLoad's own bounded wait and the pump's own replay deadline
	// are both bound by the same read timeout but started microseconds
	// apart, so resolveLoad can return before the pump has actually
	// lowered the entry. A margin of several read timeouts gives the
	// pump's own deadline time to fire before the assertion turn below
	// reads the record, without the two racing directly against each
	// other.
	time.Sleep(5 * noReplayReadTimeoutMS * time.Millisecond)

	assertSessionContinuationEntry(t, state, out, inPw, true)
}

// TestResolveSessionLoadEarlyChunkDoesNotConfirmReplay confirms that a
// chunk observed before the session/load control message names the
// identifier being loaded cannot confirm a load that replays nothing:
// only a chunk observed for that identifier once the load call is
// actually in flight counts as replay.
func TestResolveSessionLoadEarlyChunkDoesNotConfirmReplay(t *testing.T) {
	t.Parallel()

	const noReplayReadTimeoutMS = 50

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: noReplayReadTimeoutMS}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"prior-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"arrived before the load call was even sent"}}}}`)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	respondLine(t, inPw, loadID, loadSessionResponse{})

	newID := out.awaitMethod(t, methodSessionNew)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "fallback-session" {
		t.Errorf("resolveSession() session id = %q, want %q: a chunk observed before the load call must not confirm the load", outcome.sessionID, "fallback-session")
	}

	time.Sleep(5 * noReplayReadTimeoutMS * time.Millisecond)

	assertSessionContinuationEntry(t, state, out, inPw, true)
}

// TestResolveSessionLoadSuccessWithReplayConfirms confirms the
// converse of the no-replay case: an agent_message_chunk observed for
// the loaded identifier before the pump's bounded wait elapses
// confirms the load, so resolveSession returns the loaded identifier
// itself and never falls back to session/new.
func TestResolveSessionLoadSuccessWithReplayConfirms(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"prior-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replayed from history"}}}}`)
	respondLine(t, inPw, loadID, loadSessionResponse{})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "prior-session" {
		t.Errorf("resolveSession() session id = %q, want %q: observed replay must confirm the loaded session", outcome.sessionID, "prior-session")
	}

	assertSessionContinuationEntry(t, state, out, inPw, false)
}

// TestResolveSessionResumeSuccessConfirmsWithoutReplay confirms that a
// successful session/resume response alone confirms continuation, with
// no replay requirement, so resolveSession returns the resumed
// identifier at once and the entry is never lowered.
func TestResolveSessionResumeSuccessConfirmsWithoutReplay(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingResume())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	resumeID := out.awaitMethod(t, methodSessionResume)
	respondLine(t, inPw, resumeID, resumeSessionResponse{})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "prior-session" {
		t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "prior-session")
	}

	assertSessionContinuationEntry(t, state, out, inPw, false)
}

// TestResolveSessionResumeFailureLowersAndFallsBack confirms a
// session/resume error, like a session/load error, lowers the entry
// and falls back to a new session rather than failing the run.
func TestResolveSessionResumeFailureLowersAndFallsBack(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingResume())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	resumeID := out.awaitMethod(t, methodSessionResume)
	respondErrorLine(t, inPw, resumeID, jsonrpcMethodNotFound, "method not found")

	newID := out.awaitMethod(t, methodSessionNew)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "fallback-session" {
		t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "fallback-session")
	}

	assertSessionContinuationEntry(t, state, out, inPw, true)
}

// TestResolveSessionNegativeControlStreamEndFails confirms that a
// stream end while the negative control is in flight fails resolveSession
// with domain.ErrPortExit, the same as any other loss of the
// subprocess, rather than being read as a method-not-found answer.
func TestResolveSessionNegativeControlStreamEndFails(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	out.awaitMethod(t, negativeControlMethod)
	if err := inPw.Close(); err != nil {
		t.Fatalf("close the simulated agent stream: %v", err)
	}

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err == nil {
		t.Fatal("resolveSession() error = nil, want an error for a stream end during the negative control")
	}
	if outcome.err.Kind != domain.ErrPortExit {
		t.Errorf("resolveSession() error kind = %q, want %q", outcome.err.Kind, domain.ErrPortExit)
	}
}

// TestResolveSessionCallTimeoutFallsBack confirms that a call that never
// answers within the connection's own read timeout, whether it is the
// negative control itself or the continuation method it gates, is
// treated as an unconfirmed continuation: resolveSession falls back to
// session/new in the same call and returns its identifier without
// failing the run, the same as an explicit error response would. A
// stream end is the one condition that still fails outright, pinned
// separately above.
func TestResolveSessionCallTimeoutFallsBack(t *testing.T) {
	t.Parallel()

	const shortReadTimeoutMS = 50

	tests := []struct {
		name               string
		caps               agentCapabilities
		answerProbe        bool
		continuationMethod string
	}{
		{name: "negative control never answers", caps: capsAdvertisingLoad(), answerProbe: false},
		{name: "session/load never answers", caps: capsAdvertisingLoad(), answerProbe: true, continuationMethod: methodSessionLoad},
		{name: "session/resume never answers", caps: capsAdvertisingResume(), answerProbe: true, continuationMethod: methodSessionResume},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: shortReadTimeoutMS}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)

			outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", tt.caps)

			probeID := out.awaitMethod(t, negativeControlMethod)
			if tt.answerProbe {
				respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")
				out.awaitMethod(t, tt.continuationMethod)
			}

			newID := out.awaitMethod(t, methodSessionNew)
			respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

			outcome := awaitResolveSessionOutcome(t, outcomeCh)
			if outcome.err != nil {
				t.Fatalf("resolveSession() error = %v, want nil: a call that never answers must fall back rather than fail the run", outcome.err)
			}
			if outcome.sessionID != "fallback-session" {
				t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "fallback-session")
			}

			assertSessionContinuationEntry(t, state, out, inPw, true)
		})
	}
}

// TestUnconfirmedLoadClearsProvisionalSessionIDForFallbackUpdate confirms
// that once a session/load falls back to a fresh session, the
// provisional identifier resolveLoad's own expectLoad control adopted
// is cleared before the fallback session/new is even sent: a
// session/update naming the fallback session, arriving before the
// definitive sessionID control message StartSession would publish
// after resolveSession returns, is queued for the next turn rather than
// dropped as foreign.
func TestUnconfirmedLoadClearsProvisionalSessionIDForFallbackUpdate(t *testing.T) {
	t.Parallel()

	const replayedMessage = "from the fallback session"

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	respondErrorLine(t, inPw, loadID, jsonrpcMethodNotFound, "method not found")

	newID := out.awaitMethod(t, methodSessionNew)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fallback-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+replayedMessage+`"}}}}`)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "fallback-session" {
		t.Fatalf("resolveSession() session id = %q, want %q", outcome.sessionID, "fallback-session")
	}

	state.itemCh <- pumpItem{control: &pumpControl{sessionID: outcome.sessionID}}

	var events []domain.AgentEvent
	turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	turnOutcome := awaitOutcome(t, turnCh)
	if turnOutcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", turnOutcome.err)
	}

	for _, ev := range events {
		if ev.Type == domain.EventNotification && ev.Message == replayedMessage {
			return
		}
	}
	t.Errorf("events = %+v, want the session/update observed before the fallback session's identifier was confirmed to have reached the turn", events)
}

// TestUnconfirmedLoadSuccessNoReplayClearsProvisionalSessionIDForFallbackUpdate
// confirms the no-replay counterpart of
// TestUnconfirmedLoadClearsProvisionalSessionIDForFallbackUpdate: a
// session/load response carrying success but no replayed chunk still
// clears the provisional identifier expectLoad adopted once the
// pump's own bounded wait elapses, so a session/update naming the
// fallback session, arriving once session/new has been answered,
// reaches the turn rather than being dropped as foreign.
func TestUnconfirmedLoadSuccessNoReplayClearsProvisionalSessionIDForFallbackUpdate(t *testing.T) {
	t.Parallel()

	const noReplayReadTimeoutMS = 50
	const replayedMessage = "from the fallback session"

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: noReplayReadTimeoutMS}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	respondLine(t, inPw, loadID, loadSessionResponse{})

	newID := out.awaitMethod(t, methodSessionNew)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fallback-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+replayedMessage+`"}}}}`)

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "fallback-session" {
		t.Fatalf("resolveSession() session id = %q, want %q", outcome.sessionID, "fallback-session")
	}

	state.itemCh <- pumpItem{control: &pumpControl{sessionID: outcome.sessionID}}

	var events []domain.AgentEvent
	turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	turnOutcome := awaitOutcome(t, turnCh)
	if turnOutcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", turnOutcome.err)
	}

	for _, ev := range events {
		if ev.Type == domain.EventNotification && ev.Message == replayedMessage {
			return
		}
	}
	t.Errorf("events = %+v, want the session/update observed once the fallback session/new was answered to have reached the turn", events)
}

// TestResolveLoadDefersUntilCreationMinuteElapses confirms the
// same-minute guard: a session/load for an identifier this process
// created in the current UTC minute does not reach the connection
// before the guard's clock leaves that minute. A fake clock placed a
// few milliseconds before the minute boundary keeps the whole test's
// wall-clock cost in the milliseconds, never the full minute. This test
// must fail if the prologue is removed from resolveLoad, since the
// guarded session/load would then reach the connection immediately,
// inside the check window below.
//
// It also covers, by way of assertSessionContinuationEntry, that once
// the deferred load is confirmed by observed replay, the
// sessionContinuation capability is not lowered, so the deferral
// itself is not treated as a failed observation.
func TestResolveLoadDefersUntilCreationMinuteElapses(t *testing.T) {
	t.Parallel()

	const deferralMS = 150
	boundary := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	fakeNow := boundary.Add(-deferralMS * time.Millisecond)

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	state.origins.now = func() time.Time { return fakeNow }
	state.origins.record("", "prior-session")

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	select {
	case line := <-out.ch:
		t.Fatalf("resolveLoad wrote %s to the connection before the guard's deferral elapsed", line)
	case <-time.After(deferralMS * time.Millisecond / 2):
	}

	loadID := out.awaitMethod(t, methodSessionLoad)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"prior-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replayed from history"}}}}`)
	respondLine(t, inPw, loadID, loadSessionResponse{})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "prior-session" {
		t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "prior-session")
	}

	assertSessionContinuationEntry(t, state, out, inPw, false)
}

// TestResolveLoadDeferralLogsDebugRecord confirms the deferral's
// exact log shape: a Debug record on state.logger with the pinned
// deferral message and exactly the two typed attributes session_id
// and wait_ms.
func TestResolveLoadDeferralLogsDebugRecord(t *testing.T) {
	t.Parallel()

	const deferralMS = 200
	boundary := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	fakeNow := boundary.Add(-deferralMS * time.Millisecond)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
	out := newOutboundReader(outPr)
	state.origins.now = func() time.Time { return fakeNow }
	state.origins.record("", "prior-session")

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	loadID := out.awaitMethod(t, methodSessionLoad)
	respondErrorLine(t, inPw, loadID, jsonrpcMethodNotFound, "method not found")

	newID := out.awaitMethod(t, methodSessionNew)
	respondLine(t, inPw, newID, newSessionResponse{SessionID: sessionId("fallback-session")})

	awaitResolveSessionOutcome(t, outcomeCh)

	logged := buf.String()
	if !strings.Contains(logged, "level=DEBUG") {
		t.Fatalf("log output = %q, want a level=DEBUG record for the deferral", logged)
	}
	if !strings.Contains(logged, "session load deferred past the session's creation minute") {
		t.Errorf("log output = %q, want the exact deferral message the spec pins", logged)
	}
	if !strings.Contains(logged, `session_id=prior-session`) {
		t.Errorf("log output = %q, want a session_id=prior-session attribute", logged)
	}
	if !strings.Contains(logged, "wait_ms=") {
		t.Errorf("log output = %q, want a wait_ms attribute", logged)
	}
}

// TestResolveLoadNoDeferralWhenNotInCreationMinute confirms that a
// session/load for an identifier with no ledger entry, and one recorded
// in an earlier UTC minute, both reach the connection with no delay the
// guard added.
func TestResolveLoadNoDeferralWhenNotInCreationMinute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setUp func(o *sessionOrigins)
	}{
		{
			name:  "no ledger entry for the identifier",
			setUp: func(o *sessionOrigins) {},
		},
		{
			name: "recorded in an earlier UTC minute",
			setUp: func(o *sessionOrigins) {
				earlier := time.Date(2026, 3, 4, 5, 4, 30, 0, time.UTC)
				o.now = func() time.Time { return earlier }
				o.record("", "prior-session")
				o.now = func() time.Time { return earlier.Add(90 * time.Second) }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const noDelayBudget = 200 * time.Millisecond

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)
			tt.setUp(state.origins)

			outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

			probeID := out.awaitMethod(t, negativeControlMethod)
			respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

			start := time.Now()
			line := out.next(t)
			if elapsed := time.Since(start); elapsed > noDelayBudget {
				t.Errorf("session/load reached the connection after %v, want under %v: the guard must not delay this case", elapsed, noDelayBudget)
			}
			var h wireHeader
			if err := json.Unmarshal(line, &h); err != nil {
				t.Fatalf("decode line %s: %v", line, err)
			}
			if h.Method != methodSessionLoad {
				t.Fatalf("first outbound method after the negative control = %q, want %q", h.Method, methodSessionLoad)
			}

			sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"prior-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replayed from history"}}}}`)
			respondLine(t, inPw, h.ID, loadSessionResponse{})

			outcome := awaitResolveSessionOutcome(t, outcomeCh)
			if outcome.err != nil {
				t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
			}
			if outcome.sessionID != "prior-session" {
				t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "prior-session")
			}
		})
	}
}

// TestResolveResumeIgnoresCreationLedger confirms that a session/resume
// reaches the connection with no delay the guard added, even when the
// ledger holds a current-minute entry for the same identifier. The
// guard is scoped to session/load alone.
func TestResolveResumeIgnoresCreationLedger(t *testing.T) {
	t.Parallel()

	const noDelayBudget = 200 * time.Millisecond
	deepInMinute := time.Date(2026, 3, 4, 5, 6, 30, 0, time.UTC)

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	state.origins.now = func() time.Time { return deepInMinute }
	state.origins.record("", "prior-session")

	outcomeCh := runResolveSessionAsync(context.Background(), state, "prior-session", capsAdvertisingResume())

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	start := time.Now()
	resumeID := out.awaitMethod(t, methodSessionResume)
	if elapsed := time.Since(start); elapsed > noDelayBudget {
		t.Errorf("session/resume reached the connection after %v, want under %v even though the ledger holds a current-minute entry for this identifier", elapsed, noDelayBudget)
	}
	respondLine(t, inPw, resumeID, resumeSessionResponse{})

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", outcome.err)
	}
	if outcome.sessionID != "prior-session" {
		t.Errorf("resolveSession() session id = %q, want %q", outcome.sessionID, "prior-session")
	}
}

// TestResolveLoadContextCancelDuringDeferralReturnsPortExit confirms
// that when the caller's context ends while the deferral is being
// spent, no session/load and no session/new reaches the connection,
// and resolveSession returns a domain.AgentError of kind
// domain.ErrPortExit.
func TestResolveLoadContextCancelDuringDeferralReturnsPortExit(t *testing.T) {
	t.Parallel()

	boundary := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	fakeNow := boundary.Add(-30 * time.Second)

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	state.origins.now = func() time.Time { return fakeNow }
	state.origins.record("", "prior-session")

	ctx, cancel := context.WithCancel(context.Background())
	outcomeCh := make(chan resolveSessionOutcome, 1)
	go func() {
		sessionID, err := resolveSession(ctx, state, "prior-session", capsAdvertisingLoad(), "", nil)
		outcomeCh <- resolveSessionOutcome{sessionID: sessionID, err: err}
	}()

	probeID := out.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, inPw, probeID, jsonrpcMethodNotFound, "method not found")

	cancel()

	outcome := awaitResolveSessionOutcome(t, outcomeCh)
	if outcome.err == nil {
		t.Fatal("resolveSession() error = nil, want a port-exit error for a context ended during the deferral")
	}
	if outcome.err.Kind != domain.ErrPortExit {
		t.Errorf("resolveSession() error kind = %q, want %q", outcome.err.Kind, domain.ErrPortExit)
	}
	if outcome.sessionID != "" {
		t.Errorf("resolveSession() session id = %q, want empty on a cancelled deferral", outcome.sessionID)
	}

	select {
	case line := <-out.ch:
		t.Fatalf("resolveSession() wrote %s to the connection, want no session/load or session/new after the context ended during the deferral", line)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestFallbackSessionRecordedIsGuardedOnLaterLoad confirms that the
// identifier a fallback session/new creates is recorded in the
// shared ledger, so a later attempt loading it, on another session
// sharing the same *sessionOrigins the way ClientProtocolAdapter's own
// value is shared across sessions, is guarded exactly as a
// first-attempt session would be.
func TestFallbackSessionRecordedIsGuardedOnLaterLoad(t *testing.T) {
	t.Parallel()

	const deferralMS = 150
	boundary := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	fakeNow := boundary.Add(-deferralMS * time.Millisecond)
	origins := &sessionOrigins{now: func() time.Time { return fakeNow }}

	firstState, firstOutPr, firstInPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	firstOut := newOutboundReader(firstOutPr)
	firstState.origins = origins

	firstOutcomeCh := runResolveSessionAsync(context.Background(), firstState, "prior-session", capsAdvertisingLoad())

	firstProbeID := firstOut.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, firstInPw, firstProbeID, jsonrpcMethodNotFound, "method not found")

	firstLoadID := firstOut.awaitMethod(t, methodSessionLoad)
	respondErrorLine(t, firstInPw, firstLoadID, jsonrpcMethodNotFound, "method not found")

	firstNewID := firstOut.awaitMethod(t, methodSessionNew)
	respondLine(t, firstInPw, firstNewID, newSessionResponse{SessionID: sessionId("fallback-session")})

	firstOutcome := awaitResolveSessionOutcome(t, firstOutcomeCh)
	if firstOutcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", firstOutcome.err)
	}
	if firstOutcome.sessionID != "fallback-session" {
		t.Fatalf("resolveSession() session id = %q, want %q", firstOutcome.sessionID, "fallback-session")
	}

	recordedAt, known := origins.createdAt("", "fallback-session")
	if !known {
		t.Fatalf("createdAt(%q) known = false, want true: createNewSession must record the fallback session's own identifier", "fallback-session")
	}
	if !recordedAt.Equal(fakeNow) {
		t.Errorf("createdAt(%q) = %v, want %v", "fallback-session", recordedAt, fakeNow)
	}

	secondState, secondOutPr, secondInPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	secondOut := newOutboundReader(secondOutPr)
	secondState.origins = origins

	secondOutcomeCh := runResolveSessionAsync(context.Background(), secondState, "fallback-session", capsAdvertisingLoad())

	secondProbeID := secondOut.awaitMethod(t, negativeControlMethod)
	respondErrorLine(t, secondInPw, secondProbeID, jsonrpcMethodNotFound, "method not found")

	select {
	case line := <-secondOut.ch:
		t.Fatalf("session/load for the fallback session reached the connection before the guard's deferral elapsed: %s", line)
	case <-time.After(deferralMS * time.Millisecond / 2):
	}

	secondLoadID := secondOut.awaitMethod(t, methodSessionLoad)
	sendLine(t, secondInPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fallback-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replayed"}}}}`)
	respondLine(t, secondInPw, secondLoadID, loadSessionResponse{})

	secondOutcome := awaitResolveSessionOutcome(t, secondOutcomeCh)
	if secondOutcome.err != nil {
		t.Fatalf("resolveSession() error = %v, want nil", secondOutcome.err)
	}
	if secondOutcome.sessionID != "fallback-session" {
		t.Errorf("resolveSession() session id = %q, want %q", secondOutcome.sessionID, "fallback-session")
	}
}
