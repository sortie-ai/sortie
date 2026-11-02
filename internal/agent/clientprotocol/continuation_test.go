package clientprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

type startOutcome struct {
	sessionID string
	carried   bool
	raw       json.RawMessage
	probed    bool
	verdict   agentcore.ResumeVerdict
	err       *domain.AgentError
}

func runStartAsync(ctx context.Context, state *sessionState, resumeID string, caps agentCapabilities) <-chan startOutcome {
	ch := make(chan startOutcome, 1)
	go func() {
		var outcome startOutcome
		probe := continuationProbe(state, caps, "", nil)
		recording := agentcore.ResumeProbeFunc[json.RawMessage](func(ctx context.Context, id string) (continuationSignal, *domain.AgentError) {
			signal, agentErr := probe.ProbeResume(ctx, id)
			outcome.probed, outcome.verdict = true, signal.Verdict
			return signal, agentErr
		})

		start, agentErr := agentcore.ResolveSessionStart(ctx, resumeID, recording, state.logger)
		switch {
		case agentErr != nil:
			outcome.err = agentErr
		case start.CarriedHistory():
			outcome.sessionID, outcome.carried, outcome.raw = start.ResumeID(), true, start.Answer()
		default:
			outcome.sessionID, outcome.raw, outcome.err = createNewSession(ctx, state, "", nil)
		}
		ch <- outcome
	}()
	return ch
}

func awaitStarted(t *testing.T, ch <-chan startOutcome, wantID string, wantCarried bool) startOutcome {
	t.Helper()

	got := waittest.Receive(t, ch, "start outcome")
	if got.err != nil {
		t.Fatalf("start error = %v, want nil", got.err)
	}
	if got.sessionID != wantID || got.carried != wantCarried {
		t.Errorf("start = {id %q, carried %v}, want {id %q, carried %v}", got.sessionID, got.carried, wantID, wantCarried)
	}
	return got
}

func capsAdvertisingLoad() agentCapabilities {
	loadSession := true
	return agentCapabilities{LoadSession: &loadSession}
}

func capsAdvertisingResume() agentCapabilities {
	return agentCapabilities{SessionCapabilities: &sessionCapabilities{Resume: &sessionResumeCapabilities{}}}
}

func firstOutboundMethod(t *testing.T, out *outboundReader) (method string, id json.RawMessage) {
	t.Helper()
	line := out.next(t)
	var h wireHeader
	if err := json.Unmarshal(line, &h); err != nil {
		t.Fatalf("decode first outbound line %s: %v", line, err)
	}
	return h.Method, h.ID
}

func answerNegativeControl(t *testing.T, out *outboundReader, inPw io.Writer) {
	t.Helper()
	respondErrorLine(t, inPw, out.awaitMethod(t, negativeControlMethod), jsonrpc.MethodNotFoundCode, "method not found")
}

func sendReplayChunk(t *testing.T, inPw io.Writer, sessionID, text string) {
	t.Helper()
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"`+sessionID+`","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+text+`"}}}}`)
}

func assertSessionContinuationEntry(t *testing.T, state *sessionState, out *outboundReader, inPw io.Writer, wantGap bool) {
	t.Helper()

	markSessionKnown(state)
	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	// tokenCounts starts at gap on a local launch, so the notice fires whatever
	// the outcome under test.
	notice := firstNotification(t, events)
	gotGap := strings.Contains(notice.Message, capabilityLabelSessionContinuation)
	if gotGap != wantGap {
		t.Errorf("gap notice = %q, session continuation listed = %v, want %v", notice.Message, gotGap, wantGap)
	}
}

func assertUpdateReachesTurn(t *testing.T, state *sessionState, out *outboundReader, inPw io.Writer, sessionID, message string) {
	t.Helper()

	state.inbox.Put(pumpItem{control: &pumpControl{sessionID: sessionID}})
	var events []domain.AgentEvent
	turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	respondLine(t, inPw, out.awaitMethod(t, methodSessionPrompt), promptResponse{StopReason: stopReasonEndTurn})
	if turnOutcome := waittest.Receive(t, turnCh, "turn outcome"); turnOutcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", turnOutcome.err)
	}

	for _, ev := range events {
		if ev.Type == domain.EventNotification && ev.Message == message {
			return
		}
	}
	t.Errorf("events = %+v, want the session/update naming %q to reach the turn", events, sessionID)
}

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

type continuationScript func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage)

func rejects(method string, code int) continuationScript {
	return func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
		answerNegativeControl(t, out, inPw)
		respondErrorLine(t, inPw, out.awaitMethod(t, method), code, "rejected")
		respondLine(t, inPw, out.awaitMethod(t, methodSessionNew), result)
	}
}

func neverAnswers(method string) continuationScript {
	return func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
		if method == negativeControlMethod {
			out.awaitMethod(t, negativeControlMethod)
		} else {
			answerNegativeControl(t, out, inPw)
			out.awaitMethod(t, method)
		}
		respondLine(t, inPw, out.awaitMethod(t, methodSessionNew), result)
	}
}

func TestContinuationOutcome(t *testing.T) {
	t.Parallel()

	const priorID = "prior-session"
	unimplemented := jsonrpc.MethodNotFoundCode

	tests := []struct {
		name        string
		resumeID    string
		caps        agentCapabilities
		script      continuationScript
		wantCarried bool
		wantVerdict agentcore.ResumeVerdict
		wantLowered bool
	}{
		{
			name: "no resume id calls session/new alone",
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				respondLine(t, inPw, out.awaitMethod(t, methodSessionNew), result)
			},
		},
		{
			name:        "no route advertised replaces without any continuation call",
			resumeID:    priorID,
			wantVerdict: agentcore.ResumeReplaced,
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				method, id := firstOutboundMethod(t, out)
				if method != methodSessionNew {
					t.Fatalf("first outbound method = %q, want %q", method, methodSessionNew)
				}
				respondLine(t, inPw, id, result)
			},
		},
		{
			name:        "load confirmed by replay carries",
			resumeID:    priorID,
			caps:        capsAdvertisingLoad(),
			wantCarried: true,
			wantVerdict: agentcore.ResumeCarried,
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				answerNegativeControl(t, out, inPw)
				loadID := out.awaitMethod(t, methodSessionLoad)
				sendReplayChunk(t, inPw, priorID, "replayed")
				respondLine(t, inPw, loadID, result)
			},
		},
		{
			name:        "resume answered with a result carries without replay",
			resumeID:    priorID,
			caps:        capsAdvertisingResume(),
			wantCarried: true,
			wantVerdict: agentcore.ResumeCarried,
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				answerNegativeControl(t, out, inPw)
				respondLine(t, inPw, out.awaitMethod(t, methodSessionResume), result)
			},
		},
		{
			name:        "load answered with the negative control's own code replaces",
			resumeID:    priorID,
			caps:        capsAdvertisingLoad(),
			wantVerdict: agentcore.ResumeReplaced,
			wantLowered: true,
			script:      rejects(methodSessionLoad, unimplemented),
		},
		{
			name:        "load answered with another error code replaces",
			resumeID:    priorID,
			caps:        capsAdvertisingLoad(),
			wantVerdict: agentcore.ResumeReplaced,
			wantLowered: true,
			script:      rejects(methodSessionLoad, -32000),
		},
		{
			name:        "resume answered with an error replaces",
			resumeID:    priorID,
			caps:        capsAdvertisingResume(),
			wantVerdict: agentcore.ResumeReplaced,
			wantLowered: true,
			script:      rejects(methodSessionResume, unimplemented),
		},
		{
			name:        "load answered without any replay is unknown",
			resumeID:    priorID,
			caps:        capsAdvertisingLoad(),
			wantVerdict: agentcore.ResumeUnknown,
			wantLowered: true,
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				answerNegativeControl(t, out, inPw)
				respondLine(t, inPw, out.awaitMethod(t, methodSessionLoad), loadSessionResponse{})
				respondLine(t, inPw, out.awaitMethod(t, methodSessionNew), result)
			},
		},
		{
			name:        "a chunk seen before the load call does not confirm it",
			resumeID:    priorID,
			caps:        capsAdvertisingLoad(),
			wantVerdict: agentcore.ResumeUnknown,
			wantLowered: true,
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				probeID := out.awaitMethod(t, negativeControlMethod)
				sendReplayChunk(t, inPw, priorID, "arrived before the load call was even sent")
				respondErrorLine(t, inPw, probeID, jsonrpc.MethodNotFoundCode, "method not found")
				respondLine(t, inPw, out.awaitMethod(t, methodSessionLoad), loadSessionResponse{})
				respondLine(t, inPw, out.awaitMethod(t, methodSessionNew), result)
			},
		},
		{
			name:        "negative control never answers is unknown",
			resumeID:    priorID,
			caps:        capsAdvertisingLoad(),
			wantVerdict: agentcore.ResumeUnknown,
			wantLowered: true,
			script:      neverAnswers(negativeControlMethod),
		},
		{
			name:        "load never answers is unknown",
			resumeID:    priorID,
			caps:        capsAdvertisingLoad(),
			wantVerdict: agentcore.ResumeUnknown,
			wantLowered: true,
			script:      neverAnswers(methodSessionLoad),
		},
		{
			name:        "resume never answers is unknown",
			resumeID:    priorID,
			caps:        capsAdvertisingResume(),
			wantVerdict: agentcore.ResumeUnknown,
			wantLowered: true,
			script:      neverAnswers(methodSessionResume),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The fake clock keeps a slow runner from expiring the session/new
			// answer, which shares the short read timeout.
			synctest.Test(t, func(t *testing.T) {
				state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 50}, clientProtocolMaxLineBytes)
				out := newOutboundReader(outPr)
				result := json.RawMessage(`{"sessionId":"fallback-session","models":{"currentModelId":"scripted-model","availableModels":[]}}`)
				wantID := "fallback-session"
				if tt.wantCarried {
					wantID = priorID
				}

				outcomeCh := runStartAsync(context.Background(), state, tt.resumeID, tt.caps)
				tt.script(t, out, inPw, result)

				got := awaitStarted(t, outcomeCh, wantID, tt.wantCarried)
				if !bytes.Equal(got.raw, result) {
					t.Errorf("start raw result = %s, want %s: the opening call's own bytes, including members the decoded struct lacks", got.raw, result)
				}
				if tt.resumeID != "" && got.verdict != tt.wantVerdict {
					t.Errorf("continuation probe verdict = %v, want %v", got.verdict, tt.wantVerdict)
				}
				assertSessionContinuationEntry(t, state, out, inPw, tt.wantLowered)
			})
		})
	}
}

func TestStartNegativeControlStreamEndFails(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	outcomeCh := runStartAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

	out.awaitMethod(t, negativeControlMethod)
	if err := inPw.Close(); err != nil {
		t.Fatalf("close the simulated agent stream: %v", err)
	}

	got := waittest.Receive(t, outcomeCh, "start outcome")
	if got.err == nil {
		t.Fatal("start error = nil, want an error for a stream end during the negative control")
	}
	if got.err.Kind != domain.ErrPortExit {
		t.Errorf("start error kind = %q, want %q", got.err.Kind, domain.ErrPortExit)
	}
	if got.carried {
		t.Error("start carried = true, want false for a failed start")
	}
}

func TestUnconfirmedLoadClearsProvisionalSessionIDForFallbackUpdate(t *testing.T) {
	t.Parallel()

	const replayedMessage = "from the fallback session"

	tests := []struct {
		name   string
		script continuationScript
	}{
		{
			name: "load rejected",
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				answerNegativeControl(t, out, inPw)
				respondErrorLine(t, inPw, out.awaitMethod(t, methodSessionLoad), jsonrpc.MethodNotFoundCode, "method not found")
				newID := out.awaitMethod(t, methodSessionNew)
				sendReplayChunk(t, inPw, "fallback-session", replayedMessage)
				respondLine(t, inPw, newID, result)
			},
		},
		{
			name: "load answered without any replay",
			script: func(t *testing.T, out *outboundReader, inPw io.Writer, result json.RawMessage) {
				answerNegativeControl(t, out, inPw)
				respondLine(t, inPw, out.awaitMethod(t, methodSessionLoad), loadSessionResponse{})
				respondLine(t, inPw, out.awaitMethod(t, methodSessionNew), result)
				sendReplayChunk(t, inPw, "fallback-session", replayedMessage)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 50}, clientProtocolMaxLineBytes)
				out := newOutboundReader(outPr)

				outcomeCh := runStartAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())
				tt.script(t, out, inPw, json.RawMessage(`{"sessionId":"fallback-session"}`))

				got := awaitStarted(t, outcomeCh, "fallback-session", false)

				assertUpdateReachesTurn(t, state, out, inPw, got.sessionID, replayedMessage)
			})
		})
	}
}

func TestProbeLoadDefersUntilCreationMinuteElapses(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const deferralMS = 150
		boundary := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
		fakeNow := boundary.Add(-deferralMS * time.Millisecond)

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		state.origins.now = func() time.Time { return fakeNow }
		state.origins.record("", "prior-session")

		outcomeCh := runStartAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

		answerNegativeControl(t, out, inPw)

		waittest.Never(t, out.ch, deferralMS*time.Millisecond/2, "an outbound line before the guard's deferral elapsed")

		loadID := out.awaitMethod(t, methodSessionLoad)
		sendReplayChunk(t, inPw, "prior-session", "replayed from history")
		respondLine(t, inPw, loadID, loadSessionResponse{})

		awaitStarted(t, outcomeCh, "prior-session", true)

		assertSessionContinuationEntry(t, state, out, inPw, false)
	})
}

func TestProbeLoadNoDeferralWhenNotInCreationMinute(t *testing.T) {
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

			synctest.Test(t, func(t *testing.T) {
				state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
				out := newOutboundReader(outPr)
				tt.setUp(state.origins)

				outcomeCh := runStartAsync(context.Background(), state, "prior-session", capsAdvertisingLoad())

				answerNegativeControl(t, out, inPw)

				start := time.Now()
				method, loadID := firstOutboundMethod(t, out)
				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("session/load reached the connection after %v, want at once: the guard must not delay this case", elapsed)
				}
				if method != methodSessionLoad {
					t.Fatalf("first outbound method after the negative control = %q, want %q", method, methodSessionLoad)
				}

				sendReplayChunk(t, inPw, "prior-session", "replayed from history")
				respondLine(t, inPw, loadID, loadSessionResponse{})

				awaitStarted(t, outcomeCh, "prior-session", true)
			})
		})
	}
}

func TestProbeResumeIgnoresCreationLedger(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		deepInMinute := time.Date(2026, 3, 4, 5, 6, 30, 0, time.UTC)

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		state.origins.now = func() time.Time { return deepInMinute }
		state.origins.record("", "prior-session")

		outcomeCh := runStartAsync(context.Background(), state, "prior-session", capsAdvertisingResume())

		answerNegativeControl(t, out, inPw)

		start := time.Now()
		resumeID := out.awaitMethod(t, methodSessionResume)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("session/resume reached the connection after %v, want at once even though the ledger holds a current-minute entry for this identifier", elapsed)
		}
		respondLine(t, inPw, resumeID, resumeSessionResponse{})

		awaitStarted(t, outcomeCh, "prior-session", true)
	})
}

func TestStartFailsCancelledWhenContextEndsDuringLoadDeferral(t *testing.T) {
	t.Parallel()

	boundary := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	fakeNow := boundary.Add(-30 * time.Second)

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	state.origins.now = func() time.Time { return fakeNow }
	state.origins.record("", "prior-session")

	ctx, cancel := context.WithCancel(context.Background())
	outcomeCh := runStartAsync(ctx, state, "prior-session", capsAdvertisingLoad())

	answerNegativeControl(t, out, inPw)

	cancel()

	got := waittest.Receive(t, outcomeCh, "start outcome")
	if got.err == nil {
		t.Fatal("start error = nil, want a cancelled start for a context ended during the deferral")
	}
	if got.err.Kind != domain.ErrTurnCancelled {
		t.Errorf("start error kind = %q, want %q", got.err.Kind, domain.ErrTurnCancelled)
	}
	if got.sessionID != "" {
		t.Errorf("start session id = %q, want empty on a cancelled deferral", got.sessionID)
	}

	waittest.Never(t, out.ch, 50*time.Millisecond, "an outbound session/load or session/new after the context ended during the deferral")
}

func TestFallbackSessionRecordedIsGuardedOnLaterLoad(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const deferralMS = 150
		boundary := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
		fakeNow := boundary.Add(-deferralMS * time.Millisecond)
		origins := &sessionOrigins{now: func() time.Time { return fakeNow }}

		firstState, firstOutPr, firstInPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
		firstOut := newOutboundReader(firstOutPr)
		firstState.origins = origins

		firstOutcomeCh := runStartAsync(context.Background(), firstState, "prior-session", capsAdvertisingLoad())

		answerNegativeControl(t, firstOut, firstInPw)
		respondErrorLine(t, firstInPw, firstOut.awaitMethod(t, methodSessionLoad), jsonrpc.MethodNotFoundCode, "method not found")
		respondLine(t, firstInPw, firstOut.awaitMethod(t, methodSessionNew), newSessionResponse{SessionID: sessionId("fallback-session")})

		awaitStarted(t, firstOutcomeCh, "fallback-session", false)

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

		secondOutcomeCh := runStartAsync(context.Background(), secondState, "fallback-session", capsAdvertisingLoad())

		answerNegativeControl(t, secondOut, secondInPw)

		waittest.Never(t, secondOut.ch, deferralMS*time.Millisecond/2, "session/load for the fallback session before the guard's deferral elapsed")

		secondLoadID := secondOut.awaitMethod(t, methodSessionLoad)
		sendReplayChunk(t, secondInPw, "fallback-session", "replayed")
		respondLine(t, secondInPw, secondLoadID, loadSessionResponse{})

		awaitStarted(t, secondOutcomeCh, "fallback-session", true)
	})
}
