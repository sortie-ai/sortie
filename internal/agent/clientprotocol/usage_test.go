package clientprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
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

func (r *fakeUsageReader) Claim(target agentcore.LaunchTarget, runtime string) ([]string, bool) {
	if target.RemoteCommand != "" {
		return nil, false
	}
	return []string{"FAKE_USAGE_OUTFILE=/dev/null"}, true
}

func (r *fakeUsageReader) Recognize(string) bool {
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

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}
	return events, outcome
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

	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, discardLogger())
	out := newOutboundReader(outPr)
	publishHandshake(state, reader)
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
		state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, discardLogger())
		out := newOutboundReader(outPr)
		publishHandshake(state, reader)
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
		state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, discardLogger())
		out := newOutboundReader(outPr)
		publishHandshake(state, reader)
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

			outcome := waittest.Receive(t, outcomeCh, "turn outcome")

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

	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, discardLogger())
	publishHandshake(state, reader)
	markSessionKnown(state)
	return state, newOutboundReader(outPr), inPw
}

// modelReachedMarker is distinctive so a barrier keying on it cannot be
// satisfied by the capability notice, which carries the same event type.
const modelReachedMarker = "model-reached"

// chunkBarrier returns an OnEvent callback and a channel closed once a message
// chunk's liveness notification is delivered. The pump records that the turn
// reached the model before emitting that event, so seeing it means the record
// is set, not that a reader won a race.
func chunkBarrier(events *[]domain.AgentEvent) (func(domain.AgentEvent), <-chan struct{}) {
	reached := make(chan struct{})
	var once sync.Once
	collect := collectEvents(events)
	return func(ev domain.AgentEvent) {
		collect(ev)
		if ev.Type == domain.EventNotification && ev.Message == "" {
			once.Do(func() { close(reached) })
		}
	}, reached
}

func sendModelReachedChunk(t *testing.T, w io.Writer) {
	t.Helper()
	sendLine(t, w, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+modelReachedMarker+`"}}}}`)
}

// awaitResponseTo scans until the pump has answered the request carrying rawID.
// The pump answers inside the call that handles the request, so seeing the
// answer means the whole of that handling ran.
func awaitResponseTo(t *testing.T, out *outboundReader, rawID string) {
	t.Helper()

	ctx := waittest.Context(t)
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
		case <-ctx.Done():
			t.Fatalf("request %s not answered within %v", rawID, waittest.HangGuard)
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
	waittest.Receive(t, reached, "the turn's assistant chunk to reach the pump")
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

		assertRecoveredOnAbnormalEnd(t, reader, events, waittest.Receive(t, outcomeCh, "turn outcome"), want, domain.ErrPortExit)
	})

	t.Run("the prompt is answered with an error", func(t *testing.T) {
		t.Parallel()

		reader := durableReader(want)
		state, out, inPw := readerSession(t, reader)

		var events []domain.AgentEvent
		promptID, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
		respondErrorLine(t, inPw, promptID, -32603, "internal error")

		assertRecoveredOnAbnormalEnd(t, reader, events, waittest.Receive(t, outcomeCh, "turn outcome"), want, domain.ErrResponseError)
	})

	t.Run("the prompt is answered with a result nothing can decode", func(t *testing.T) {
		t.Parallel()

		reader := durableReader(want)
		state, out, inPw := readerSession(t, reader)

		var events []domain.AgentEvent
		promptID, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
		sendLine(t, inPw, `{"jsonrpc":"2.0","id":`+string(promptID)+`,"result":"not an object"}`)

		assertRecoveredOnAbnormalEnd(t, reader, events, waittest.Receive(t, outcomeCh, "turn outcome"), want, domain.ErrTurnOutcomeUnknown)
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

	waittest.Receive(t, state.conn.WriteFailed(), "the prompt send's write to fail")
	sendModelReachedChunk(t, inPw)
	waittest.Receive(t, reached, "the turn's assistant chunk to reach the pump")

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")

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

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")

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

	waittest.Receive(t, reader.entered, "the abnormal end to consult the source")

	sendLine(t, inPw, `{"jsonrpc":"2.0","id":9001,"method":"`+methodElicitationCreate+`","params":{}}`)
	awaitResponseTo(t, out, "9001")
	awaitPumpObserved(t, state)
	close(reader.gate)

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")

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

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")

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
	waittest.Receive(t, done, "the pump to drain its inbox")
}

func publishHandshake(state *sessionState, reader usageReader) {
	state.inbox.Put(pumpItem{control: &pumpControl{handshake: &handshakeFacts{
		agentInfo:        implementation{Name: "other-cli", Version: "0.59.0"},
		agentInfoPresent: true,
		reader:           reader,
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

func TestSettleOffersNoRecognizerForARemoteLaunch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		remote         bool
		wantRecognizer bool
	}{
		{name: "local launch", remote: false, wantRecognizer: true},
		{name: "remote launch", remote: true, wantRecognizer: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A source that recognizes the name regardless of the launch, so
			// only the settlement itself can keep a remote launch from a
			// relaunch offer.
			source := &fakeUsageReader{recognizes: true}
			claims := usageClaims{unclaimed: []usageReader{source}}

			verdict := settleUsageSources(claims, &implementation{Name: "runtime"}, tt.remote)

			if got := verdict.recognizer != nil; got != tt.wantRecognizer {
				t.Errorf("settleUsageSources(remote=%v).recognizer set = %v, want %v", tt.remote, got, tt.wantRecognizer)
			}
		})
	}
}

func launchedBy(executable string) func(agentcore.LaunchTarget) bool {
	return func(target agentcore.LaunchTarget) bool {
		tokens := append([]string{target.Command}, target.Args...)
		return slices.Contains(append(tokens, strings.Fields(target.RemoteCommand)...), executable)
	}
}

func fixtureDeclarations(t *testing.T) *registry.Registry[usagesource.Declaration, struct{}] {
	t.Helper()

	declarations := registry.NewRegistry[usagesource.Declaration, struct{}]("usage declaration")
	windowsOnly := usagesource.Declaration{Live: usage.Source{Category: usage.SourceProtocol}}
	for name, launches := range map[string]func(agentcore.LaunchTarget) bool{
		"alpha": launchedBy("alpha-cli"),
		"beta":  launchedBy("beta-cli"),
		"gamma": nil,
	} {
		declaration := windowsOnly
		declaration.Launches = launches
		declarations.Register(name, declaration)
	}
	return declarations
}

func TestChooseDeclaration(t *testing.T) {
	t.Parallel()

	declarations := fixtureDeclarations(t)
	local := agentcore.LaunchTarget{Command: "alpha-cli", Args: []string{"--acp"}}
	tests := []struct {
		name           string
		info           *implementation
		target         agentcore.LaunchTarget
		wantKey        string
		wantConfigured string
		wantMismatch   bool
	}{
		{"handshake confirms the command", &implementation{Name: "alpha"}, local, "alpha", "alpha", false},
		{"handshake names another declared runtime", &implementation{Name: "beta"}, local, "beta", "alpha", true},
		{"handshake names a declaration with no launch matcher", &implementation{Name: "gamma"}, local, "gamma", "alpha", true},
		{"handshake names an undeclared runtime", &implementation{Name: "undeclared-runtime"}, local, usagesource.DefaultName, "alpha", true},
		{"no handshake name keeps the command's declaration", nil, local, "alpha", "alpha", false},
		{"empty handshake name keeps the command's declaration", &implementation{}, local, "alpha", "alpha", false},
		{"no name and a command naming nothing", nil, agentcore.LaunchTarget{Command: "agent"}, usagesource.DefaultName, "", false},
		{"undeclared name and a command naming nothing", &implementation{Name: "undeclared-runtime"}, agentcore.LaunchTarget{Command: "agent"}, usagesource.DefaultName, "", false},
		{"wrapper command", nil, agentcore.LaunchTarget{Command: "env", Args: []string{"MODEL=x", "alpha-cli", "--acp"}}, "alpha", "alpha", false},
		{"remote target whose command is ssh", nil, agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "beta-cli acp --model x"}, "beta", "beta", false},
		{"remote target naming nothing", nil, agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent --acp"}, usagesource.DefaultName, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := chooseDeclaration(declarations, tt.info, tt.target)

			if got.key != tt.wantKey || got.configured != tt.wantConfigured || got.mismatch != tt.wantMismatch {
				t.Errorf("chooseDeclaration(%+v, %+v) = {key %q, configured %q, mismatch %v}, want {%q, %q, %v}",
					tt.info, tt.target, got.key, got.configured, got.mismatch, tt.wantKey, tt.wantConfigured, tt.wantMismatch)
			}
			if got.declaration.Live.Spend.Shape == usage.SpendUndeclared || got.declaration.PromptSpend == nil {
				t.Errorf("chooseDeclaration(%+v, %+v) declaration maps no spend, want the default mapping where the selected one maps none", tt.info, tt.target)
			}
		})
	}
}

func TestNewSessionLedgerReadsRecordsOnLocalLaunchesOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    agentcore.LaunchTarget
		created   bool
		wantReads int
		wantOK    bool
	}{
		{"local continued session", agentcore.LaunchTarget{Command: "agent"}, false, 1, true},
		{"local created session", agentcore.LaunchTarget{Command: "agent"}, true, 0, true},
		{"remote continued session", agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent"}, false, 0, false},
		{"remote created session", agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent"}, true, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reads int
			decl := scriptedSpendDeclaration(nil, nil, nil)
			decl.ReadRecords = func(context.Context, usagesource.PullRuntime) ([]usage.SpendReading, error) {
				reads++
				return []usage.SpendReading{spendReading(10, 2, 0, "model-a", "u1")}, nil
			}
			state := &sessionState{logger: discardLogger(), target: tt.target}
			live := spendReading(100, 20, 0, "", "")

			ledger := newSessionLedger(t.Context(), state, decl, tt.created)

			if ledger == nil {
				t.Fatalf("newSessionLedger(%+v) = nil, want a ledger built from the declaration's live and records sources", tt.target)
			}
			if reads != tt.wantReads {
				t.Errorf("newSessionLedger(%+v) ran ReadRecords %d times, want %d", tt.target, reads, tt.wantReads)
			}
			settlement := ledger.Settle(usage.TurnEnd{Replied: true, ModelReached: true}, &live, usage.RecordsRead{OK: true})
			if !settlement.Figure || settlement.Run.InputTokens != 100 || settlement.Run.OutputTokens != 20 {
				t.Errorf("Settle() = {Figure %v, Run %+v}, want the live figure 100/20", settlement.Figure, settlement.Run)
			}

			history := usage.RecordsRead{OK: true, Readings: []usage.SpendReading{spendReading(10, 2, 0, "model-a", "u1")}}
			boundary := newSessionLedger(t.Context(), state, decl, tt.created).Settle(usage.TurnEnd{Replied: true, ModelReached: true}, nil, history)
			if boundary.Figure != tt.created {
				t.Errorf("newSessionLedger(%+v, created %v) counts a record that is on the host at session start = %v, want %v", tt.target, tt.created, boundary.Figure, tt.created)
			}

			read := readSessionRecords(t.Context(), state, decl)

			if read.OK != tt.wantOK || (len(read.Readings) != 0) != tt.wantOK {
				t.Errorf("readSessionRecords(%+v) = {OK %v, %d readings}, want OK %v", tt.target, read.OK, len(read.Readings), tt.wantOK)
			}
		})
	}
}

func TestChildSessionSpendSettlesTheTurn(t *testing.T) {
	t.Parallel()

	start := usagesource.ChildSignal{Call: "c1", Started: true}
	done := usagesource.ChildSignal{Call: "c1", Session: "child-1", Completed: true}
	both := domain.TokenUsage{InputTokens: 150, OutputTokens: 15, TotalTokens: 165}
	tests := []struct {
		name            string
		signals         []usagesource.ChildSignal
		target          agentcore.LaunchTarget
		wantUnaccounted bool
		want            domain.TokenUsage
	}{
		{"completed child counted", []usagesource.ChildSignal{start, done}, agentcore.LaunchTarget{Command: "agent"}, false, both},
		{"started child never completed", []usagesource.ChildSignal{start, {Session: "child-1"}}, agentcore.LaunchTarget{Command: "agent"}, true, both},
		{"remote turn that ran a child", []usagesource.ChildSignal{start, done}, sshLaunch("agent"), true, domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			live := spendReading(100, 10, 0, "", "")
			declaration := scriptedSpendDeclaration(&live, nil, nil)
			declaration.ChildSession = func(_ string, update json.RawMessage) usagesource.ChildSignal {
				var decoded struct{ Signal usagesource.ChildSignal }
				_ = json.Unmarshal(update, &decoded)
				return decoded.Signal
			}
			declaration.ReadRecords = func(_ context.Context, rt usagesource.PullRuntime) ([]usage.SpendReading, error) {
				if rt.SessionID() == "child-1" {
					return []usage.SpendReading{spendReading(50, 5, 0, "model-b", "c1")}, nil
				}
				return []usage.SpendReading{spendReading(100, 10, 0, "model-a", "p1")}, nil
			}
			ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
			if err != nil {
				t.Fatalf("usage.NewLedger() error = %v, want nil", err)
			}
			ledger.SessionCreated()
			state, out, inPw := newDeclaredSession(t, declaration, ledger)
			state.target = tt.target

			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
			promptID := out.awaitMethod(t, methodSessionPrompt)
			for _, signal := range tt.signals {
				body, _ := json.Marshal(signal)
				sendLine(t, inPw, fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"},"signal":%s}}}`, body))
			}
			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
			result := waittest.Receive(t, outcomeCh, "turn outcome").result

			if result.Usage != tt.want || result.SpendUnaccounted != tt.wantUnaccounted {
				t.Errorf("RunTurn() = {Usage %+v, SpendUnaccounted %v}, want {%+v, %v}", result.Usage, result.SpendUnaccounted, tt.want, tt.wantUnaccounted)
			}
		})
	}
}
