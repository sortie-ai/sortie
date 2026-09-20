package clientprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/domain"
)

type fakeDrain struct {
	usage        agentcore.RecoveredUsage
	found        bool
	completeness usagesource.Completeness
}

type fakeUsageReader struct {
	mu sync.Mutex

	recognizes bool
	drains     []fakeDrain

	// gate, when non-nil, holds every Drain until closed; entered closes as the
	// first Drain begins. Never read under mu: a Drain parked on gate while
	// holding mu would deadlock the accessors below.
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once

	opened       string
	drainCalls   int
	lowerBounds  []int64
	completeness usagesource.Completeness
	closed       bool
}

func (r *fakeUsageReader) Claim(target agentcore.LaunchTarget) ([]string, bool) {
	if target.RemoteCommand != "" {
		return nil, false
	}
	return []string{"FAKE_USAGE_OUTFILE=/dev/null"}, true
}

func (r *fakeUsageReader) Recognize(name, version string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recognizes
}

func (r *fakeUsageReader) Open(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened = sessionID
}

func (r *fakeUsageReader) Drain(ctx context.Context, lowerBound int64) (agentcore.RecoveredUsage, string, bool) {
	if r.entered != nil {
		r.once.Do(func() { close(r.entered) })
	}
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return agentcore.RecoveredUsage{}, "", false
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.lowerBounds = append(r.lowerBounds, lowerBound)
	index := min(r.drainCalls, len(r.drains)-1)
	r.drainCalls++
	r.completeness = usagesource.CompletenessUnknown
	if index < 0 {
		return agentcore.RecoveredUsage{}, "", false
	}
	scripted := r.drains[index]
	if !scripted.found {
		return agentcore.RecoveredUsage{}, "", false
	}
	r.completeness = scripted.completeness
	return scripted.usage, "fake", true
}

func (r *fakeUsageReader) Completeness() usagesource.Completeness {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completeness
}

func (r *fakeUsageReader) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

func (r *fakeUsageReader) openedSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened
}

func (r *fakeUsageReader) observedLowerBounds() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.lowerBounds...)
}

func (r *fakeUsageReader) drainCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drainCalls
}

func quotaMeta(input, output int64) json.RawMessage {
	return json.RawMessage(`{"quota":{"token_count":{"input_tokens":` + strconv.FormatInt(input, 10) +
		`,"output_tokens":` + strconv.FormatInt(output, 10) + `}}}`)
}

// measuredTurn drives one whole turn: a tool call that completes, then a prompt
// result carrying the wire extension.
func measuredTurn(t *testing.T, state *sessionState, inPw interface {
	Write([]byte) (int, error)
}, out *outboundReader, meta json.RawMessage) ([]domain.AgentEvent, turnOutcome) {
	t.Helper()

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

	promptID := out.awaitMethod(t, methodSessionPrompt)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
		`{"sessionUpdate":"tool_call","toolCallId":"call_001","title":"Read main.go","kind":"read","status":"pending"}}}`)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
		`{"sessionUpdate":"tool_call_update","toolCallId":"call_001","status":"completed","title":"Read main.go"}}}`)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn, Meta: meta})

	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}
	return events, outcome
}

func TestSelectUsageReaderRefusesRemoteLaunch(t *testing.T) {
	t.Parallel()

	local, localEnv := selectUsageReader(agentcore.LaunchTarget{Command: "agent", WorkspacePath: "/w"})
	if local == nil {
		t.Error("selectUsageReader(local) reader = nil, want a source")
	}
	if len(localEnv) == 0 {
		t.Error("selectUsageReader(local) env = empty, want the source's own assignments")
	}

	remote, remoteEnv := selectUsageReader(agentcore.LaunchTarget{
		Command:       "ssh",
		RemoteCommand: "agent",
		SSHHost:       "worker",
		WorkspacePath: "/w",
	})
	if remote != nil {
		t.Error("selectUsageReader(remote) reader is non-nil, want none: nothing reads a far host's filesystem")
	}
	if remoteEnv != nil {
		t.Errorf("selectUsageReader(remote) env = %v, want nil", remoteEnv)
	}
}

func TestUsageDrainReportsTurnEndFigure(t *testing.T) {
	t.Parallel()

	reader := &fakeUsageReader{
		recognizes: true,
		drains: []fakeDrain{{
			found:        true,
			completeness: usagesource.CompletenessAccounted,
			usage: agentcore.RecoveredUsage{
				Run:   domain.TokenUsage{InputTokens: 1200, OutputTokens: 80, TotalTokens: 1280, CacheReadTokens: 900},
				Model: "model-of-record",
			},
		}},
	}

	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes,
		discardLogger(), withUsageReader(reader))
	out := newOutboundReader(outPr)
	publishHandshake(state, "gemini-cli", "0.59.0")
	markSessionKnown(state)

	events, outcome := measuredTurn(t, state, inPw, out, quotaMeta(1100, 70))

	if got := reader.openedSession(); got != "sess-test" {
		t.Errorf("reader.Open() session = %q, want %q", got, "sess-test")
	}
	if bounds := reader.observedLowerBounds(); len(bounds) != 1 || bounds[0] != 1170 {
		t.Errorf("reader.Drain() lower bounds = %v, want [1170]: the wire extension's own input plus output", bounds)
	}

	agenttest.AssertUsageContract(t, events)
	agenttest.AssertModelReported(t, events, "model-of-record")
	if !outcome.result.UsageMeasured {
		t.Error("result.UsageMeasured = false, want true once a drain produced a figure")
	}
	if outcome.result.SpendUnaccounted {
		t.Error("result.SpendUnaccounted = true, want false for a turn whose figure was measured")
	}
	want := domain.TokenUsage{InputTokens: 1200, OutputTokens: 80, TotalTokens: 1280, CacheReadTokens: 900}
	if outcome.result.Usage != want {
		t.Errorf("result.Usage = %+v, want %+v", outcome.result.Usage, want)
	}
}

func TestUsageMeasurementExistenceIsNotItsCompleteness(t *testing.T) {
	t.Parallel()

	t.Run("a figure short of the turn's own bound is a measurement", func(t *testing.T) {
		t.Parallel()

		short := domain.TokenUsage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105}
		reader := &fakeUsageReader{
			recognizes: true,
			drains: []fakeDrain{{
				found:        true,
				completeness: usagesource.CompletenessPartial,
				usage:        agentcore.RecoveredUsage{Run: short, Model: "model-of-record"},
			}},
		}
		state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes,
			discardLogger(), withUsageReader(reader))
		out := newOutboundReader(outPr)
		publishHandshake(state, "gemini-cli", "0.59.0")
		markSessionKnown(state)

		events, outcome := measuredTurn(t, state, inPw, out, quotaMeta(1100, 70))

		if countTokenUsageEvents(events) != 1 {
			t.Errorf("token_usage events = %d, want 1: the figure is short of the turn, not absent",
				countTokenUsageEvents(events))
		}
		if !outcome.result.UsageMeasured {
			t.Error("result.UsageMeasured = false, want true: the verdict says a measurement exists, not that it is whole")
		}
		if outcome.result.Usage != short {
			t.Errorf("result.Usage = %+v, want %+v", outcome.result.Usage, short)
		}
		if !outcome.result.SpendUnaccounted {
			t.Error("result.SpendUnaccounted = false, want true: the part of the turn the figure did not reach is still unknown")
		}
	})

	t.Run("no record at all is not a measurement", func(t *testing.T) {
		t.Parallel()

		reader := &fakeUsageReader{recognizes: true, drains: []fakeDrain{{found: false}}}
		state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes,
			discardLogger(), withUsageReader(reader))
		out := newOutboundReader(outPr)
		publishHandshake(state, "gemini-cli", "0.59.0")
		markSessionKnown(state)

		events, outcome := measuredTurn(t, state, inPw, out, quotaMeta(1100, 70))

		agenttest.AssertMeasurementAbsent(t, events, outcome.result)
		if !outcome.result.SpendUnaccounted {
			t.Error("result.SpendUnaccounted = false, want true: the wire extension reported spend nothing measured")
		}
		if state.caps.tokenCounts != capabilityGap {
			t.Errorf("tokenCounts = %q, want %q once the first drain proved nothing",
				state.caps.tokenCounts, capabilityGap)
		}
	})
}

func TestUsageReaderDroppedOnUnrecognizedBuild(t *testing.T) {
	t.Parallel()

	reader := &fakeUsageReader{
		recognizes: false,
		drains:     []fakeDrain{{found: true, usage: agentcore.RecoveredUsage{Run: domain.TokenUsage{InputTokens: 99}}}},
	}
	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes,
		discardLogger(), withUsageReader(reader))
	out := newOutboundReader(outPr)
	publishHandshake(state, "other-cli", "0.58.0")
	markSessionKnown(state)

	events, outcome := measuredTurn(t, state, inPw, out, quotaMeta(1250, 40))

	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
	if state.caps.tokenCounts != capabilityGap {
		t.Errorf("tokenCounts = %q, want %q for a build the source was not measured against",
			state.caps.tokenCounts, capabilityGap)
	}
	if reader.observedLowerBounds() != nil {
		t.Error("the dropped source was still drained, want no drain at all")
	}
	if !hasNoticeContaining(events, capabilityLabelTokenCounts) {
		t.Errorf("no gap notice named %q; the session must say what it cannot report", capabilityLabelTokenCounts)
	}
}

func TestCancelledTurnRecordsSpendOccurred(t *testing.T) {
	t.Parallel()

	// Both cases stream an update before the cancel, so the difference is which
	// kind arrived, not whether anything did; a case that saw nothing would
	// pass for the wrong reason.
	tests := []struct {
		name                 string
		update               string
		wantSpendUnaccounted bool
	}{
		{
			name:                 "a cancelled turn that streamed assistant text is spend of an unknown amount",
			update:               `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"}}`,
			wantSpendUnaccounted: true,
		},
		{
			name:                 "a cancelled turn that only saw a plan is not spend",
			update:               `{"sessionUpdate":"plan","entries":[{"content":"Look","priority":"high","status":"pending"}]}`,
			wantSpendUnaccounted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)
			markSessionKnown(state)

			ctx, cancel := context.WithCancel(context.Background())
			var events []domain.AgentEvent
			outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

			promptID := out.awaitMethod(t, methodSessionPrompt)
			sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
				tt.update+`}}`)
			awaitPumpObserved(t, state)
			cancel()
			out.awaitMethod(t, methodSessionCancel)
			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonCancelled})

			outcome := awaitOutcome(t, outcomeCh)

			if outcome.result.SpendUnaccounted != tt.wantSpendUnaccounted {
				t.Errorf("result.SpendUnaccounted = %v, want %v",
					outcome.result.SpendUnaccounted, tt.wantSpendUnaccounted)
			}
			if outcome.result.UsageMeasured {
				t.Error("result.UsageMeasured = true, want false: unaccounted spend is not a measurement")
			}
			if countTokenUsageEvents(events) != 0 {
				t.Error("a cancelled turn emitted a token_usage event, want none")
			}
		})
	}
}

func TestNoReaderReportsSpendOccurredFromTheWireExtension(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	events, outcome := measuredTurn(t, state, inPw, out, quotaMeta(900, 30))

	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
	if !outcome.result.SpendUnaccounted {
		t.Error("result.SpendUnaccounted = false, want true when the runtime reported spend and nothing measured it")
	}
}

// durableReader returns a source holding run graded CompletenessUnknown: what a
// kill leaves behind proves the finished requests, never that the turn's last
// one is in it.
func durableReader(run domain.TokenUsage) *fakeUsageReader {
	return &fakeUsageReader{
		recognizes: true,
		drains: []fakeDrain{{
			found:        true,
			completeness: usagesource.CompletenessUnknown,
			usage:        agentcore.RecoveredUsage{Run: run, Model: "model-of-record"},
		}},
	}
}

func readerSession(t *testing.T, reader usageReader) (*sessionState, *outboundReader, *io.PipeWriter) {
	t.Helper()

	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes,
		discardLogger(), withUsageReader(reader))
	publishHandshake(state, "gemini-cli", "0.59.0")
	markSessionKnown(state)
	return state, newOutboundReader(outPr), inPw
}

// modelReachedMarker is distinctive so a barrier keying on it cannot be
// satisfied by the capability notice, which carries the same event type.
const modelReachedMarker = "model-reached"

// chunkBarrier returns an OnEvent callback and a channel closed once the chunk
// carrying modelReachedMarker is delivered. The pump records that the turn
// reached the model before emitting that event, so seeing it means the record
// is set, not that a reader won a race.
func chunkBarrier(events *[]domain.AgentEvent) (func(domain.AgentEvent), <-chan struct{}) {
	reached := make(chan struct{})
	var once sync.Once
	collect := collectEvents(events)
	return func(ev domain.AgentEvent) {
		collect(ev)
		if ev.Type == domain.EventNotification && strings.Contains(ev.Message, modelReachedMarker) {
			once.Do(func() { close(reached) })
		}
	}, reached
}

func sendModelReachedChunk(t *testing.T, w io.Writer) {
	t.Helper()
	sendLine(t, w, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+modelReachedMarker+`"}}}}`)
}

func awaitReached(t *testing.T, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the turn's assistant chunk to reach the pump")
	}
}

// awaitResponseTo scans until the pump has answered the request carrying rawID.
// The pump answers inside the call that handles the request, so seeing the
// answer means the whole of that handling ran.
func awaitResponseTo(t *testing.T, out *outboundReader, rawID string) {
	t.Helper()

	deadline := time.After(awaitTimeout)
	for {
		select {
		case line, ok := <-out.ch:
			if !ok {
				t.Fatalf("outbound stream ended before request %s was answered", rawID)
			}
			var h wireHeader
			if err := json.Unmarshal(line, &h); err != nil {
				continue
			}
			if h.Method == "" && string(h.ID) == rawID {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the answer to request %s", rawID)
			return
		}
	}
}

// modelReachedTurn starts a turn and drives it as far as one assistant chunk,
// the transport's evidence the turn reached the model, leaving the caller to
// end the turn.
func modelReachedTurn(t *testing.T, state *sessionState, inPw io.Writer, out *outboundReader, events *[]domain.AgentEvent) (json.RawMessage, <-chan turnOutcome) {
	t.Helper()

	onEvent, reached := chunkBarrier(events)
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: onEvent})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	sendModelReachedChunk(t, inPw)
	awaitReached(t, reached)
	return promptID, outcomeCh
}

// assertRecoveredOnAbnormalEnd checks one abnormal end: the source was
// consulted with no bound, its held figure reached the result, the turn still
// reports the unmeasured part, and the disposition is unchanged.
func assertRecoveredOnAbnormalEnd(
	t *testing.T,
	reader *fakeUsageReader,
	events []domain.AgentEvent,
	outcome turnOutcome,
	want domain.TokenUsage,
	wantKind domain.AgentErrorKind,
) {
	t.Helper()

	if calls := reader.drainCallCount(); calls != 1 {
		t.Errorf("reader.Drain() calls = %d, want 1: an abnormal end must still consult the source", calls)
	}
	if bounds := reader.observedLowerBounds(); len(bounds) != 1 || bounds[0] != 0 {
		t.Errorf("reader.Drain() lower bounds = %v, want [0]: no wire result arrived to carry one", bounds)
	}
	if !outcome.result.UsageMeasured {
		t.Error("result.UsageMeasured = false, want true: the source held a figure for this session")
	}
	if outcome.result.Usage != want {
		t.Errorf("result.Usage = %+v, want %+v", outcome.result.Usage, want)
	}
	if !outcome.result.SpendUnaccounted {
		t.Error("result.SpendUnaccounted = false, want true: what the interrupted request cost is still unknown")
	}
	if got := countTokenUsageEvents(events); got != 1 {
		t.Errorf("token_usage events = %d, want 1", got)
	}
	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) {
		t.Fatalf("runTurn() error = %v (%T), want *domain.AgentError: recovery must not change the disposition",
			outcome.err, outcome.err)
	}
	if agentErr.Kind != wantKind {
		t.Errorf("AgentError.Kind = %q, want %q: recovery must not change the disposition", agentErr.Kind, wantKind)
	}
}

func TestAbnormalTurnEndRecoversRecordedSpend(t *testing.T) {
	t.Parallel()

	want := domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}

	t.Run("the stream ends with the turn in flight", func(t *testing.T) {
		t.Parallel()

		reader := durableReader(want)
		state, out, inPw := readerSession(t, reader)

		var events []domain.AgentEvent
		_, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
		inPw.Close() //nolint:errcheck,gosec // ending the simulated stream

		assertRecoveredOnAbnormalEnd(t, reader, events, awaitOutcome(t, outcomeCh), want, domain.ErrPortExit)
	})

	t.Run("the prompt is answered with an error", func(t *testing.T) {
		t.Parallel()

		reader := durableReader(want)
		state, out, inPw := readerSession(t, reader)

		var events []domain.AgentEvent
		promptID, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
		respondErrorLine(t, inPw, promptID, -32603, "internal error")

		assertRecoveredOnAbnormalEnd(t, reader, events, awaitOutcome(t, outcomeCh), want, domain.ErrResponseError)
	})

	t.Run("the prompt is answered with a result nothing can decode", func(t *testing.T) {
		t.Parallel()

		reader := durableReader(want)
		state, out, inPw := readerSession(t, reader)

		var events []domain.AgentEvent
		promptID, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
		sendLine(t, inPw, `{"jsonrpc":"2.0","id":`+string(promptID)+`,"result":"not an object"}`)

		assertRecoveredOnAbnormalEnd(t, reader, events, awaitOutcome(t, outcomeCh), want, domain.ErrTurnOutcomeUnknown)
	})
}

func TestWriteFailureTurnEndRecoversRecordedSpend(t *testing.T) {
	t.Parallel()

	want := domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}
	reader := durableReader(want)
	state, inPw := newFailWriteSession(t, domain.AgentConfig{ReadTimeoutMS: 1000}, errors.New("boom"),
		withUsageReader(reader))
	markSessionKnown(state)

	var events []domain.AgentEvent
	onEvent, reached := chunkBarrier(&events)
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: onEvent})

	select {
	case <-state.conn.WriteFailed():
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the prompt send's write to fail")
	}
	sendModelReachedChunk(t, inPw)
	awaitReached(t, reached)

	outcome := awaitOutcome(t, outcomeCh)

	assertRecoveredOnAbnormalEnd(t, reader, events, outcome, want, domain.ErrPortExit)
	var agentErr *domain.AgentError
	if errors.As(outcome.err, &agentErr) && agentErr.Message != promptSendFailedMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, promptSendFailedMessage)
	}
}

func TestAbnormalTurnEndWithNoRecordIsNotAMeasuredZero(t *testing.T) {
	t.Parallel()

	reader := &fakeUsageReader{recognizes: true, drains: []fakeDrain{{found: false}}}
	state, out, inPw := readerSession(t, reader)

	var events []domain.AgentEvent
	_, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
	inPw.Close() //nolint:errcheck,gosec // ending the simulated stream

	outcome := awaitOutcome(t, outcomeCh)

	if calls := reader.drainCallCount(); calls != 1 {
		t.Errorf("reader.Drain() calls = %d, want 1: an abnormal end must still consult the source", calls)
	}
	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
	if !outcome.result.SpendUnaccounted {
		t.Error("result.SpendUnaccounted = false, want true: the turn reached the model and nothing measured it")
	}
	if state.caps.tokenCounts != capabilityGap {
		t.Errorf("tokenCounts = %q, want %q once the first drain proved nothing", state.caps.tokenCounts, capabilityGap)
	}
}

func TestRecoveringTurnKeepsTheDispositionItAlreadyObserved(t *testing.T) {
	t.Parallel()

	want := domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}
	reader := durableReader(want)
	reader.gate = make(chan struct{})
	reader.entered = make(chan struct{})
	state, out, inPw := readerSession(t, reader)

	var events []domain.AgentEvent
	promptID, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
	respondErrorLine(t, inPw, promptID, -32603, "internal error")

	select {
	case <-reader.entered:
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the abnormal end to consult the source")
	}

	sendLine(t, inPw, `{"jsonrpc":"2.0","id":9001,"method":"`+methodElicitationCreate+`","params":{}}`)
	awaitResponseTo(t, out, "9001")
	awaitPumpObserved(t, state)
	close(reader.gate)

	outcome := awaitOutcome(t, outcomeCh)

	assertRecoveredOnAbnormalEnd(t, reader, events, outcome, want, domain.ErrResponseError)
}

func TestAbnormalTurnEndWithoutModelEvidenceConsultsNothing(t *testing.T) {
	t.Parallel()

	reader := durableReader(domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110})
	state, out, inPw := readerSession(t, reader)

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	out.awaitMethod(t, methodSessionPrompt)
	awaitPumpObserved(t, state)
	inPw.Close() //nolint:errcheck,gosec // ending the simulated stream

	outcome := awaitOutcome(t, outcomeCh)

	if calls := reader.drainCallCount(); calls != 0 {
		t.Errorf("reader.Drain() calls = %d, want 0: nothing in this turn showed it reached a model", calls)
	}
	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
	if outcome.result.SpendUnaccounted {
		t.Error("result.SpendUnaccounted = true, want false: a turn with no model evidence is not unaccounted spend")
	}
}

func TestTurnResultGradesItsOwnMeasurement(t *testing.T) {
	t.Parallel()

	run := domain.TokenUsage{InputTokens: 1100, OutputTokens: 70, TotalTokens: 1170}

	tests := []struct {
		name                 string
		completeness         usagesource.Completeness
		wantSpendUnaccounted bool
	}{
		{
			name:                 "a figure proven to reach the turn's bound accounts for it",
			completeness:         usagesource.CompletenessAccounted,
			wantSpendUnaccounted: false,
		},
		{
			name:                 "a figure short of the turn's bound leaves the rest unaccounted",
			completeness:         usagesource.CompletenessPartial,
			wantSpendUnaccounted: true,
		},
		{
			name:                 "a figure with no bound to prove it is not proven to account for the turn",
			completeness:         usagesource.CompletenessUnknown,
			wantSpendUnaccounted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakeUsageReader{
				recognizes: true,
				drains: []fakeDrain{{
					found:        true,
					completeness: tt.completeness,
					usage:        agentcore.RecoveredUsage{Run: run, Model: "model-of-record"},
				}},
			}
			state, out, inPw := readerSession(t, reader)

			events, outcome := measuredTurn(t, state, inPw, out, quotaMeta(1100, 70))

			if outcome.result.SpendUnaccounted != tt.wantSpendUnaccounted {
				t.Errorf("result.SpendUnaccounted = %v, want %v", outcome.result.SpendUnaccounted, tt.wantSpendUnaccounted)
			}
			if !outcome.result.UsageMeasured {
				t.Error("result.UsageMeasured = false, want true: every grade here is a measurement")
			}
			if outcome.result.Usage != run {
				t.Errorf("result.Usage = %+v, want %+v", outcome.result.Usage, run)
			}
			if got := countTokenUsageEvents(events); got != 1 {
				t.Errorf("token_usage events = %d, want 1", got)
			}
		})
	}
}

// awaitPumpObserved blocks until the pump has taken every item queued before
// this call, by putting a control message the pump answers; the inbox's single
// order puts that answer behind the update.
func awaitPumpObserved(t *testing.T, state *sessionState) {
	t.Helper()

	done := make(chan struct{})
	state.inbox.Put(pumpItem{control: &pumpControl{answerOpen: done}})
	select {
	case <-done:
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the pump to drain its inbox")
	}
}

func publishHandshake(state *sessionState, name, version string) {
	state.inbox.Put(pumpItem{control: &pumpControl{handshake: &handshakeFacts{
		agentInfo:        implementation{Name: name, Version: version},
		agentInfoPresent: true,
	}}})
}

func countTokenUsageEvents(events []domain.AgentEvent) int {
	count := 0
	for _, event := range events {
		if event.Type == domain.EventTokenUsage {
			count++
		}
	}
	return count
}

func hasNoticeContaining(events []domain.AgentEvent, fragment string) bool {
	for _, event := range events {
		if event.Type == domain.EventNotification && strings.Contains(event.Message, fragment) {
			return true
		}
	}
	return false
}

func TestSpendUnaccountedIsGradedPerTurn(t *testing.T) {
	t.Parallel()

	first := domain.TokenUsage{InputTokens: 100, TotalTokens: 100}
	second := domain.TokenUsage{InputTokens: 1000, TotalTokens: 1000}

	tests := []struct {
		name   string
		grades [2]usagesource.Completeness
		want   [2]bool
	}{
		{
			name:   "a turn falls short after one that accounted for itself",
			grades: [2]usagesource.Completeness{usagesource.CompletenessAccounted, usagesource.CompletenessPartial},
			want:   [2]bool{false, true},
		},
		{
			name:   "a turn falls short after one that already had",
			grades: [2]usagesource.Completeness{usagesource.CompletenessPartial, usagesource.CompletenessPartial},
			want:   [2]bool{true, true},
		},
		{
			name:   "a turn accounts for itself after one that fell short",
			grades: [2]usagesource.Completeness{usagesource.CompletenessPartial, usagesource.CompletenessAccounted},
			want:   [2]bool{true, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakeUsageReader{
				recognizes: true,
				drains: []fakeDrain{
					{
						found:        true,
						completeness: tt.grades[0],
						usage:        agentcore.RecoveredUsage{Run: first, Model: "model-of-record"},
					},
					{
						found:        true,
						completeness: tt.grades[1],
						usage:        agentcore.RecoveredUsage{Run: second, Model: "model-of-record"},
					},
				},
			}
			state, out, inPw := readerSession(t, reader)

			_, firstOutcome := measuredTurn(t, state, inPw, out, quotaMeta(1000, 0))
			if firstOutcome.result.SpendUnaccounted != tt.want[0] {
				t.Errorf("first result.SpendUnaccounted = %v, want %v",
					firstOutcome.result.SpendUnaccounted, tt.want[0])
			}
			if firstOutcome.result.Usage != first {
				t.Errorf("first result.Usage = %+v, want %+v", firstOutcome.result.Usage, first)
			}

			_, secondOutcome := measuredTurn(t, state, inPw, out, quotaMeta(200, 0))
			if secondOutcome.result.SpendUnaccounted != tt.want[1] {
				t.Errorf("second result.SpendUnaccounted = %v, want %v: the turn reports its own grade",
					secondOutcome.result.SpendUnaccounted, tt.want[1])
			}
			if secondOutcome.result.Usage != second {
				t.Errorf("second result.Usage = %+v, want %+v", secondOutcome.result.Usage, second)
			}
			if !secondOutcome.result.UsageMeasured {
				t.Error("second result.UsageMeasured = false, want true: both turns drained a figure")
			}
			if bounds := reader.observedLowerBounds(); len(bounds) != 2 || bounds[0] != 1000 || bounds[1] != 200 {
				t.Errorf("reader.Drain() lower bounds = %v, want [1000 200]: each turn is drained against its own", bounds)
			}
		})
	}
}
