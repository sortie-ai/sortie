//go:build unix

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// nopWriteCloser is an io.WriteCloser that discards all writes.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// countingDoneContext records each evaluation of Done so a test can detect
// whether a closed cancellation channel keeps a select loop running.
type countingDoneContext struct {
	context.Context
	calls atomic.Int64
}

func (c *countingDoneContext) Done() <-chan struct{} {
	c.calls.Add(1)
	return c.Context.Done()
}

// interruptTrackingWriteCloser observes best-effort turn/interrupt requests.
type interruptTrackingWriteCloser struct {
	interrupts chan struct{}
	count      atomic.Int64
}

func (w *interruptTrackingWriteCloser) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"method":"turn/interrupt"`)) {
		w.count.Add(1)
		select {
		case w.interrupts <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (*interruptTrackingWriteCloser) Close() error { return nil }

// loadFixture reads testdata/<name> and returns its bytes.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loadFixture(%q): %v", name, err)
	}
	return data
}

// signalingWriter wraps w and closes done the first time Write is
// called. A goroutine waiting on done can then release a scripted
// response without racing jsonrpc.Conn.Call's own pending-map
// registration: Call registers its waiter, in program order, strictly
// before the write this observes.
type signalingWriter struct {
	w    io.Writer
	once sync.Once
	done chan struct{}
}

func newSignalingWriter(w io.Writer) *signalingWriter {
	if w == nil {
		w = io.Discard
	}
	return &signalingWriter{w: w, done: make(chan struct{})}
}

func (s *signalingWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.once.Do(func() { close(s.done) })
	return n, err
}

// peekRequestID extracts the id field from one line codex writes,
// without decoding the rest. It returns 0 for a fire-and-forget
// notification, which carries no id.
func peekRequestID(line []byte) int64 {
	var wire struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(line, &wire)
	return wire.ID
}

// peekResponseID reports whether line is a JSON-RPC response line (a
// non-zero id with no method), and its id, mirroring the
// classification jsonrpc.Conn's own reader applies.
func peekResponseID(line []byte) (int64, bool) {
	var wire struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(line, &wire); err != nil {
		return 0, false
	}
	if wire.Method != "" || wire.ID == 0 {
		return 0, false
	}
	return wire.ID, true
}

// scanOutboundLines reads r until EOF or a read error, calling onLine
// with each newline-delimited line it finds, delimiter stripped. It
// exists so a fixture peer can observe what codex writes without
// reaching for bufio: the discriminator that turns a line into a
// jsonrpc.Message is unexported outside the jsonrpc package, and no
// helper here may reach it, or duplicate its classification, even for
// this narrower purpose of peeking at an id to gate a reply.
func scanOutboundLines(r io.Reader, onLine func(line []byte)) {
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				onLine(buf[:i])
				buf = buf[i+1:]
			}
		}
		if err != nil {
			return
		}
	}
}

// fixtureSegment is one alternating block of a flat JSONL fixture: a
// run of notification or malformed lines with no id (id 0), or a
// single response line keyed by the id jsonrpc.Conn will allocate for
// the call it answers.
type fixtureSegment struct {
	id    int64
	lines []string
}

// splitFixtureSegments splits fixtureData's non-empty lines into
// fixtureSegments in order. A response line (a non-empty id with no
// method) starts a new gated segment; every other line joins the
// nearest notification segment.
func splitFixtureSegments(fixtureData []byte) []fixtureSegment {
	var segments []fixtureSegment
	var junk []string
	flush := func() {
		if len(junk) > 0 {
			segments = append(segments, fixtureSegment{lines: junk})
			junk = nil
		}
	}
	for line := range strings.SplitSeq(string(fixtureData), "\n") {
		if line == "" {
			continue
		}
		if id, ok := peekResponseID([]byte(line)); ok {
			flush()
			segments = append(segments, fixtureSegment{id: id, lines: []string{line}})
			continue
		}
		junk = append(junk, line)
	}
	flush()
	return segments
}

// startFixturePeer drives the peer side of a sessionState's
// jsonrpc.Conn from segments, over outPr/inPw. A notification segment
// (id 0) is written immediately; a response segment is written only
// once the peer has observed codex write the matching id. That
// gating is required for correctness: Call registers its pending
// waiter before it writes the request, so a response readable earlier
// would be misrouted as an unmatched response instead of answering
// the call.
//
// An empty segments list closes inPw immediately, reproducing a
// clean end of stream with nothing scripted at all — this is safe
// because no call is ever left waiting on a response that might also
// race a connection-termination signal. A non-empty segments list
// never closes inPw on its own once exhausted; it keeps draining
// outPr so a later fire-and-forget write (e.g. a Respond to a
// server-initiated request) is never left to block forever on the
// unbuffered pipe, and it leaves closing the connection, or ending
// state.msgCh, to the test — closing eagerly right after a scripted
// response would race jsonrpc.Conn.Call's own select between that
// response and the connection's termination signal, which Go resolves
// pseudo-randomly when both are ready. A test that needs the
// connection to appear to end after some exchange achieves that by
// controlling state.msgCh (or state.conn.Close, for a call with no
// scripted response to race) directly instead.
func startFixturePeer(t *testing.T, outPr *io.PipeReader, inPw *io.PipeWriter, segments []fixtureSegment) {
	t.Helper()

	if len(segments) == 0 {
		_ = inPw.Close()
		// Drain outPr forever so the call this empty fixture is meant
		// to fail still gets to write its request before failing.
		go func() {
			_, _ = io.Copy(io.Discard, outPr)
		}()
		return
	}

	go func() {
		write := func(s string) bool {
			_, err := fmt.Fprintln(inPw, s)
			return err == nil
		}

		si := 0
		flushNotifications := func() bool {
			for si < len(segments) && segments[si].id == 0 {
				for _, l := range segments[si].lines {
					if !write(l) {
						return false
					}
				}
				si++
			}
			return true
		}
		if !flushNotifications() {
			return
		}

		scanOutboundLines(outPr, func(line []byte) {
			if si >= len(segments) {
				return
			}
			if peekRequestID(line) != segments[si].id {
				return
			}
			if !write(segments[si].lines[0]) {
				return
			}
			si++
			flushNotifications()
		})
	}()
}

// makeTestState builds a sessionState wired to a real jsonrpc.Conn
// whose peer replays fixtureData, safe for use in RunTurn unit tests
// that do not launch a real subprocess. The session starts in the
// turn phase: every fixture here represents a session already past
// the handshake, the point at which StartSession itself calls
// beginTurnPhase.
func makeTestState(t *testing.T, fixtureData []byte) *sessionState {
	t.Helper()
	return makeTestStateWithStdin(t, fixtureData, nil)
}

// makeTestStateWithStdin behaves like makeTestState but records every
// line the connection writes into recorder, so a test can assert on
// the exact bytes RunTurn wrote back to the app-server. recorder sits
// ahead of the pipe the fixture peer gates on, in one io.MultiWriter,
// so a write is recorded before jsonrpc.Conn.write's call to the pipe
// can even return — recording via the peer's own read of that pipe
// would race the caller reading recorder's contents immediately after
// RunTurn returns.
func makeTestStateWithStdin(t *testing.T, fixtureData []byte, recorder *capturingWriteCloser) *sessionState {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
		_ = inPw.Close()
	})

	var w io.Writer = outPw
	if recorder != nil {
		w = io.MultiWriter(recorder, outPw)
	}

	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		msgCh:      make(chan jsonrpc.Message, 16),
		readerDone: make(chan struct{}),
		stopCh:     make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(w, inPr, sessionHandler(state))
	go watchTermination(state)
	state.turnPhase.Store(true)

	startFixturePeer(t, outPr, inPw, splitFixtureSegments(fixtureData))

	return state
}

// makeTestStateWithMalformedBeforeResponse builds a sessionState like
// makeTestState, except malformed is written only once the peer has
// observed codex's turn/start write, immediately before response —
// reproducing a line that fails to parse arriving on the wire between
// a request and its response.
func makeTestStateWithMalformedBeforeResponse(t *testing.T, malformed, response string) *sessionState {
	t.Helper()

	sig := newSignalingWriter(nil)
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = inPr.Close()
		_ = inPw.Close()
	})

	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		msgCh:      make(chan jsonrpc.Message, 16),
		readerDone: make(chan struct{}),
		stopCh:     make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(sig, inPr, sessionHandler(state))
	go watchTermination(state)
	state.turnPhase.Store(true)

	go func() {
		<-sig.done
		_, _ = fmt.Fprintln(inPw, malformed)
		_, _ = fmt.Fprintln(inPw, response)
	}()

	return state
}

// gatedTurnStartState builds a sessionState whose jsonrpc.Conn answers
// exactly one turn/start call with response once its peer observes
// codex write it, with a no-op handler. Every other message the test
// needs is delivered by pushing directly onto state.msgCh, including
// closing it: since the handler is a no-op, the connection never
// contends for state.msgCh, so a test can end a turn's message stream
// deterministically instead of racing jsonrpc.Conn.Call's own select
// between a scripted response and a connection-termination signal.
func gatedTurnStartState(t *testing.T, response string) *sessionState {
	t.Helper()

	sig := newSignalingWriter(nil)
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = inPr.Close()
		_ = inPw.Close()
	})

	state := &sessionState{
		threadID: "thread-001",
		target:   agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:   make(chan struct{}),
		msgCh:    make(chan jsonrpc.Message, 16),
		stopCh:   make(chan struct{}),
		acc:      agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(sig, inPr, func(jsonrpc.Message) {})
	state.turnPhase.Store(true)

	go func() {
		<-sig.done
		_, _ = fmt.Fprintln(inPw, response)
	}()

	return state
}

// fakeSession wraps state in a domain.Session suitable for RunTurn.
func fakeSession(state *sessionState) domain.Session {
	return domain.Session{
		ID:       state.threadID,
		AgentPID: "12345",
		Internal: state,
	}
}

// collectEvents is an OnEvent callback that appends to a slice.
func collectEvents(events *[]domain.AgentEvent) func(domain.AgentEvent) {
	var mu sync.Mutex
	return func(e domain.AgentEvent) {
		mu.Lock()
		*events = append(*events, e)
		mu.Unlock()
	}
}

// firstEventOfType returns the first event with the given type, or the
// zero value if none was found.
func firstEventOfType(events []domain.AgentEvent, t domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == t {
			return e, true
		}
	}
	return domain.AgentEvent{}, false
}

func TestRunTurn_InvalidInternalType(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	session := domain.Session{Internal: "not-a-session-state"}
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestRunTurn_SuccessfulTurn(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want %v", result.ExitReason, domain.EventTurnCompleted)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("Usage.OutputTokens = %d, want 50", result.Usage.OutputTokens)
	}
	if result.SessionID != "thread-001" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "thread-001")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage event, none found")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal: agentcore.TerminalSuccess,
		Work:     agentcore.WorkUnobservable,
	}, result, err)
}

// filterEventsOfType returns every event of the given type, in order.
func filterEventsOfType(events []domain.AgentEvent, typ domain.AgentEventType) []domain.AgentEvent {
	var out []domain.AgentEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestRunTurn_TokenUsageUpdated drives RunTurn against a session
// captured from codex 0.121.0 carrying two thread/tokenUsage/updated
// notifications for the current turn followed by a turn/completed with
// no usage member (the app-server protocol carries none there). It
// asserts two token_usage events are emitted, the run-cumulative
// snapshot after the second matches the notification's total minus the
// zero baseline of a fresh thread, the turn_completed event carries the
// same snapshot, and TurnResult.Usage equals it.
func TestRunTurn_TokenUsageUpdated(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "token_usage_updated.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := filterEventsOfType(events, domain.EventTokenUsage)
	if len(tokenUsageEvents) != 2 {
		t.Fatalf("token_usage event count = %d, want 2", len(tokenUsageEvents))
	}

	wantSnapshot := domain.TokenUsage{InputTokens: 27549, OutputTokens: 82, CacheReadTokens: 27392, TotalTokens: 27631}
	if got := tokenUsageEvents[1].Usage; got != wantSnapshot {
		t.Errorf("second token_usage event Usage = %+v, want %+v", got, wantSnapshot)
	}

	completed, ok := firstEventOfType(events, domain.EventTurnCompleted)
	if !ok {
		t.Fatal("expected turn_completed event, none found")
	}
	if completed.Usage != wantSnapshot {
		t.Errorf("turn_completed.Usage = %+v, want %+v", completed.Usage, wantSnapshot)
	}
	if result.Usage != wantSnapshot {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantSnapshot)
	}

	agenttest.AssertUsageContract(t, events)
}

// TestRunTurn_TokenUsageUpdated_ResumedThreadBaseline drives a thread
// whose first matching notification already reports a non-zero
// total.totalTokens because the thread resumed from a prior run. It
// asserts a preceding notification carrying another turn's id
// contributes nothing and only raises the baseline, and that the first
// reported snapshot for the current turn is the difference between that
// notification's total and last breakdowns.
func TestRunTurn_TokenUsageUpdated_ResumedThreadBaseline(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-002\"}}\n" +
		// A notification from an earlier turn of the resumed thread:
		// contributes nothing to this turn and only raises the baseline.
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-001\",\"tokenUsage\":{\"last\":{\"totalTokens\":5000,\"inputTokens\":4000,\"cachedInputTokens\":1000,\"outputTokens\":1000,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":5000,\"inputTokens\":4000,\"cachedInputTokens\":1000,\"outputTokens\":1000,\"reasoningOutputTokens\":0}}}}\n" +
		// The current turn's first matching notification: total is
		// already thread-cumulative from the resumed session.
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-002\",\"tokenUsage\":{\"last\":{\"totalTokens\":13846,\"inputTokens\":13818,\"cachedInputTokens\":13692,\"outputTokens\":28,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":27631,\"inputTokens\":27549,\"cachedInputTokens\":27392,\"outputTokens\":82,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"completed\"}}}\n"

	state := makeTestState(t, []byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "continue",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := filterEventsOfType(events, domain.EventTokenUsage)
	if len(tokenUsageEvents) != 1 {
		t.Fatalf("token_usage event count = %d, want 1 (the other-turn notification contributes nothing)", len(tokenUsageEvents))
	}

	// The baseline is total minus last (27549-13818, 82-28, 27392-13692),
	// so the reported snapshot equals last exactly: total_tokens 13846.
	wantSnapshot := domain.TokenUsage{InputTokens: 13818, OutputTokens: 28, CacheReadTokens: 13692, TotalTokens: 13846}
	if got := tokenUsageEvents[0].Usage; got != wantSnapshot {
		t.Errorf("token_usage event Usage = %+v, want %+v (baseline is total minus last)", got, wantSnapshot)
	}
	if result.Usage != wantSnapshot {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantSnapshot)
	}
}

// TestRunTurn_TokenUsageUpdated_EmptyTurnIDAdoptsFirstNotification
// drives a turn whose turn/start response body fails to unmarshal,
// leaving the adapter's current turn id empty, followed by two
// thread/tokenUsage/updated notifications sharing one non-empty turnId.
// It asserts the adapter adopts the first notification's turnId rather
// than treating every notification as belonging to another turn, so
// both notifications contribute and the run-cumulative snapshot after
// the second reports the full total rather than zero.
func TestRunTurn_TokenUsageUpdated_EmptyTurnIDAdoptsFirstNotification(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":\"malformed turn/start body\"}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-777\",\"tokenUsage\":{\"last\":{\"totalTokens\":13785,\"inputTokens\":13731,\"cachedInputTokens\":13700,\"outputTokens\":54,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":13785,\"inputTokens\":13731,\"cachedInputTokens\":13700,\"outputTokens\":54,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-777\",\"tokenUsage\":{\"last\":{\"totalTokens\":13846,\"inputTokens\":13818,\"cachedInputTokens\":13692,\"outputTokens\":28,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":27631,\"inputTokens\":27549,\"cachedInputTokens\":27392,\"outputTokens\":82,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-777\",\"status\":\"completed\"}}}\n"

	state := makeTestState(t, []byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := filterEventsOfType(events, domain.EventTokenUsage)
	if len(tokenUsageEvents) != 2 {
		t.Fatalf("token_usage event count = %d, want 2 (both notifications must contribute once the empty turn id adopts the first)", len(tokenUsageEvents))
	}

	wantSnapshot := domain.TokenUsage{InputTokens: 27549, OutputTokens: 82, TotalTokens: 27631}
	got := tokenUsageEvents[1].Usage
	if got.InputTokens != wantSnapshot.InputTokens || got.OutputTokens != wantSnapshot.OutputTokens || got.TotalTokens != wantSnapshot.TotalTokens {
		t.Errorf("second token_usage event Usage = %+v, want input/output/total %+v (not zero)", got, wantSnapshot)
	}
	if result.Usage.TotalTokens != wantSnapshot.TotalTokens {
		t.Errorf("TurnResult.Usage.TotalTokens = %d, want %d", result.Usage.TotalTokens, wantSnapshot.TotalTokens)
	}
}

func TestRunTurn_FirstTurnEmitsSessionStarted(t *testing.T) {
	t.Parallel()

	// turnCount=0 → incremented to 1 inside RunTurn → EventSessionStarted.
	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "hello",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventSessionStarted)
	if !ok {
		t.Fatal("expected EventSessionStarted on first turn, not found")
	}
	if e.SessionID != "thread-001" {
		t.Errorf("EventSessionStarted.SessionID = %q, want %q", e.SessionID, "thread-001")
	}
}

func TestRunTurn_SubsequentTurnEmitsNotification(t *testing.T) {
	t.Parallel()

	// Pre-set turnCount=1 so the adapter sees this as the second turn.
	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	state.turnCount = 1
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "continue",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if _, ok := firstEventOfType(events, domain.EventSessionStarted); ok {
		t.Error("did not expect EventSessionStarted on subsequent turn")
	}
}

func TestRunTurn_FailedTurnContextWindowExceeded(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_failed.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})

	if err == nil {
		t.Fatal("RunTurn() expected error, got nil")
	}
	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %v, want %v", result.ExitReason, domain.EventTurnFailed)
	}
	if _, ok := firstEventOfType(events, domain.EventTurnFailed); !ok {
		t.Error("expected EventTurnFailed event, none found")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage event on failed turn, none found")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   "Context window exceeded",
		Work:              agentcore.WorkUnobservable,
	}, result, err)
}

// TestRunTurn_StdoutClosedBeforeTurnCompleted pins the channel-closed
// abort arm: the disposition and kind are unchanged from before the
// shared decision, and the arm now emits exactly one turn_failed event
// where it emitted none before.
func TestRunTurn_StdoutClosedBeforeTurnCompleted(t *testing.T) {
	t.Parallel()

	// Only the turn/start response — no turn/completed — so state.msgCh
	// is closed directly, right after turn/started, before turn/completed
	// would arrive.
	state := gatedTurnStartState(t, `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`)
	adapter, _ := NewCodexAdapter(map[string]any{})

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan outcome, 1)
	var events []domain.AgentEvent
	go func() {
		result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: collectEvents(&events),
		})
		outcomeCh <- outcome{result, err}
	}()

	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/started"}
	close(state.msgCh)

	got := <-outcomeCh
	result, err := got.result, got.err
	requireAgentError(t, err, domain.ErrPortExit)

	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
	if turnFailedEvents[0].Message != "subprocess stdout closed unexpectedly" {
		t.Errorf("turn_failed Message = %q, want %q", turnFailedEvents[0].Message, "subprocess stdout closed unexpectedly")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   "subprocess stdout closed unexpectedly",
		Work:              agentcore.WorkUnobservable,
	}, result, err)
}

func TestRunTurn_StdoutEOFBeforeTurnStartResponse(t *testing.T) {
	t.Parallel()

	// Empty fixture: msgCh closes before any turn/start response arrives.
	// Tests the !ok path in the session-scoped response-wait loop.
	state := makeTestState(t, nil)
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

// TestRunTurn_TurnStartErrorResponse pins the turn/start error abort
// arm: the disposition and kind are unchanged from before the shared
// decision, and the arm now emits exactly one turn_failed event where
// it emitted none before.
func TestRunTurn_TurnStartErrorResponse(t *testing.T) {
	t.Parallel()

	// turn/start response carries an error — RunTurn should return ErrTurnFailed.
	fixture := "{\"id\":1,\"error\":{\"code\":-32000,\"message\":\"thread not found\"}}\n"
	state := makeTestState(t, []byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})
	requireAgentError(t, err, domain.ErrTurnFailed)

	const wantMessage = "turn/start error: thread not found"
	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
	if turnFailedEvents[0].Message != wantMessage {
		t.Errorf("turn_failed Message = %q, want %q", turnFailedEvents[0].Message, wantMessage)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   wantMessage,
		Work:              agentcore.WorkUnobservable,
	}, result, err)
}

func TestRunTurn_CancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	// Call's context arm returns context.Canceled.
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

// TestRunTurn_CancelledMainLoopWaitsForCompletion sends cancellation only
// after the turn/start response and a notification have entered the main
// event loop. With no further messages pending, the closed Done channel must
// be disabled after one interrupt; otherwise the loop keeps evaluating it.
func TestRunTurn_CancelledMainLoopWaitsForCompletion(t *testing.T) {
	t.Parallel()

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &countingDoneContext{Context: parentCtx}

	stdin := &interruptTrackingWriteCloser{interrupts: make(chan struct{}, 1)}
	state := newInterruptedStatusState(t, stdin)

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan outcome, 1)
	finished := make(chan struct{})

	adapter, _ := NewCodexAdapter(map[string]any{})
	go func() {
		defer close(finished)
		result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: func(domain.AgentEvent) {},
		})
		outcomeCh <- outcome{result: result, err: err}
	}()

	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	})

	// The unbuffered send returns only once RunTurn has received it,
	// which proves its main event loop is running (rather than still
	// inside the turn/start call) before cancellation.
	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/started", Params: json.RawMessage(`{"turnId":"turn-001"}`)}

	cancel()
	select {
	case <-stdin.interrupts:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not send turn/interrupt after cancellation")
	}

	callsAfterInterrupt := ctx.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := ctx.calls.Load(); got != callsAfterInterrupt {
		t.Errorf("ctx.Done() call count changed with no pending message: got %d, want %d", got, callsAfterInterrupt)
	}
	if got := stdin.count.Load(); got != 1 {
		t.Errorf("turn/interrupt request count = %d, want 1", got)
	}

	// A terminal event must still be received and mapped after cancellation.
	turnCompletedMsg := jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"turn-001","status":"interrupted"}}`)}
	select {
	case state.msgCh <- turnCompletedMsg:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not wait for turn/completed after cancellation")
	}

	select {
	case got := <-outcomeCh:
		if got.result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCancelled)
		}
		requireAgentError(t, got.err, domain.ErrTurnCancelled)
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return after turn/completed")
	}
}

// TestRunTurn_CancelledMainLoopReportsCancelledDespiteCompletedStatus drives
// the same cancel-then-interrupt sequence as
// TestRunTurn_CancelledMainLoopWaitsForCompletion, but the app-server's
// turn/completed notification, delivered after cancellation, reports status
// completed rather than interrupted. The disposition must still be a
// cancellation, and no turn_completed event may reach the stream.
func TestRunTurn_CancelledMainLoopReportsCancelledDespiteCompletedStatus(t *testing.T) {
	t.Parallel()

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &countingDoneContext{Context: parentCtx}

	stdin := &interruptTrackingWriteCloser{interrupts: make(chan struct{}, 1)}
	state := newInterruptedStatusState(t, stdin)

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan outcome, 1)
	finished := make(chan struct{})

	var events []domain.AgentEvent
	adapter, _ := NewCodexAdapter(map[string]any{})
	go func() {
		defer close(finished)
		result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: collectEvents(&events),
		})
		outcomeCh <- outcome{result: result, err: err}
	}()

	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	})

	// The unbuffered send returns only once RunTurn has received it,
	// which proves its main event loop is running (rather than still
	// inside the turn/start call) before cancellation.
	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/started", Params: json.RawMessage(`{"turnId":"turn-001"}`)}

	cancel()
	select {
	case <-stdin.interrupts:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not send turn/interrupt after cancellation")
	}
	if got := stdin.count.Load(); got != 1 {
		t.Errorf("turn/interrupt request count = %d, want 1", got)
	}

	turnCompletedMsg := jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"turn-001","status":"completed"}}`)}
	select {
	case state.msgCh <- turnCompletedMsg:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not wait for turn/completed after cancellation")
	}

	select {
	case got := <-outcomeCh:
		if got.result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCancelled)
		}
		requireAgentError(t, got.err, domain.ErrTurnCancelled)
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return after turn/completed")
	}

	if got := filterEventsOfType(events, domain.EventTurnCancelled); len(got) != 1 {
		t.Errorf("turn_cancelled event count = %d, want 1", len(got))
	}
	if got := filterEventsOfType(events, domain.EventTurnCompleted); len(got) != 0 {
		t.Errorf("turn_completed event count = %d, want 0", len(got))
	}
}

func TestRunTurn_ItemStartedAndCompletedEmitsToolResult(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_items.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "run command",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want EventTurnCompleted", result.ExitReason)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult from item tracking, not found")
	}
	if e.ToolName != "ls -la" {
		t.Errorf("ToolResult.ToolName = %q, want %q", e.ToolName, "ls -la")
	}
}

func TestRunTurn_AgentMessageTextEmitsNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_agent_message.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "explain",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// item/completed with agentMessage type and non-empty text emits EventNotification.
	notifs := 0
	for _, e := range events {
		if e.Type == domain.EventNotification {
			notifs++
		}
	}
	if notifs == 0 {
		t.Error("expected at least one EventNotification for agentMessage text, found none")
	}
}

func TestRunTurn_MiscNotifications(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_misc_notifications.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// some/unknown/method → EventOtherMessage
	if _, ok := firstEventOfType(events, domain.EventOtherMessage); !ok {
		t.Error("expected EventOtherMessage for unknown notification method, not found")
	}
	// turn/plan/updated → EventNotification
	found := false
	for _, e := range events {
		if e.Type == domain.EventNotification && e.Message == "plan updated" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'plan updated' EventNotification, not found")
	}
}

// TestRunTurn_MCPServerStartupFailureWarnsWithoutFailingTurn asserts that
// a failed mcpServer/startupStatus/updated notification is logged at
// warn, naming the server and the reported reason, and does not fail
// the turn or the session.
func TestRunTurn_MCPServerStartupFailureWarnsWithoutFailingTurn(t *testing.T) {
	var logs bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	state := makeTestState(t, loadFixture(t, "runturn_mcp_startup_failed.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v, want nil (a failed MCP server startup must not fail the turn)", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want EventTurnCompleted", result.ExitReason)
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, "level=WARN") {
		t.Errorf("log output = %q, want a WARN-level entry", logOutput)
	}
	if !strings.Contains(logOutput, "sortie-tools") {
		t.Errorf("log output = %q, want it to name the server %q", logOutput, "sortie-tools")
	}
	if !strings.Contains(logOutput, "executable not found") {
		t.Errorf("log output = %q, want it to name the reported reason %q", logOutput, "executable not found")
	}
}

// --- Human-only-request recognition and refusal ---

// runTurnFixtureWithServerRequest builds a turn/start response, a
// turn/started notification, and one server request (a method carrying
// both "id" and "method") for the given method and request id, followed
// by the given trailing lines (e.g. a turn/completed notification, or
// none when the request is expected to end the attempt before any more
// input is read).
func runTurnFixtureWithServerRequest(requestID int64, method string, trailing ...string) []byte {
	var fixture strings.Builder
	fmt.Fprintf(&fixture,
		"{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n"+
			"{\"method\":\"turn/started\",\"params\":{}}\n"+
			"{\"id\":%d,\"method\":%q,\"params\":{}}\n",
		requestID, method,
	)
	for _, line := range trailing {
		fixture.WriteString(line)
		fixture.WriteString("\n")
	}
	return []byte(fixture.String())
}

const turnCompletedLine = `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"completed"}}}`

// TestRunTurn_PermissionRequestDeniedContinues drives the two current-form
// permission requests through the turn loop and asserts the exact bytes
// written back to the app-server for that request id, and that the turn
// continues to turn/completed rather than ending.
func TestRunTurn_PermissionRequestDeniedContinues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		requestID int64
	}{
		{name: "item/commandExecution/requestApproval", method: "item/commandExecution/requestApproval", requestID: 20},
		{name: "item/fileChange/requestApproval", method: "item/fileChange/requestApproval", requestID: 21},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := &capturingWriteCloser{}
			state := makeTestStateWithStdin(t, runTurnFixtureWithServerRequest(tt.requestID, tt.method, turnCompletedLine), stdin)
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			if result.ExitReason != domain.EventTurnCompleted {
				t.Errorf("ExitReason = %q, want %q (turn must continue past the refusal)", result.ExitReason, domain.EventTurnCompleted)
			}

			write, ok := stdin.find(fmt.Sprintf(`{"id":%d,`, tt.requestID))
			if !ok {
				t.Fatalf("no response written for request id %d", tt.requestID)
			}
			want := fmt.Sprintf(`{"id":%d,"result":{"decision":"decline"}}`, tt.requestID) + "\n"
			if write != want {
				t.Errorf("response for %s = %q, want %q", tt.method, write, want)
			}
		})
	}
}

// TestRunTurn_LegacyPermissionRequestDeniedContinues drives the two legacy
// ReviewDecision-form permission requests through the turn loop and asserts
// the exact denial payload the schema declares, and that the turn
// continues rather than ending.
func TestRunTurn_LegacyPermissionRequestDeniedContinues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		requestID int64
	}{
		{name: "applyPatchApproval", method: "applyPatchApproval", requestID: 22},
		{name: "execCommandApproval", method: "execCommandApproval", requestID: 23},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := &capturingWriteCloser{}
			state := makeTestStateWithStdin(t, runTurnFixtureWithServerRequest(tt.requestID, tt.method, turnCompletedLine), stdin)
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			if result.ExitReason != domain.EventTurnCompleted {
				t.Errorf("ExitReason = %q, want %q (turn must continue past the refusal)", result.ExitReason, domain.EventTurnCompleted)
			}

			write, ok := stdin.find(fmt.Sprintf(`{"id":%d,`, tt.requestID))
			if !ok {
				t.Fatalf("no response written for request id %d", tt.requestID)
			}
			want := fmt.Sprintf(`{"id":%d,"result":{"decision":{"denied":{"rejection":%q}}}}`, tt.requestID, codexRefusalMessage) + "\n"
			if write != want {
				t.Errorf("response for %s = %q, want %q", tt.method, write, want)
			}
		})
	}
}

// TestRunTurn_PermissionRequestEmitsNotification asserts that a continued
// permission refusal emits RefusalPosture.Notice as an EventNotification,
// so the operator learns why the turn kept going even though nothing was
// granted.
func TestRunTurn_PermissionRequestEmitsNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, runTurnFixtureWithServerRequest(24, "item/commandExecution/requestApproval", turnCompletedLine))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	wantNotice := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending).NoticeWithDetail("")

	var found bool
	for _, e := range filterEventsOfType(events, domain.EventNotification) {
		if e.Message == wantNotice {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no EventNotification with Message = %q found", wantNotice)
	}
}

// TestRunTurn_HumanInputRequestEndsAttempt drives each ClassHumanInput
// method through the turn loop and asserts the exact bytes written back
// for that request id, and that the attempt ends with the
// human-input-required outcome rather than continuing or reading further
// input.
func TestRunTurn_HumanInputRequestEndsAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		requestID int64
		detail    string
		wantWrite string
	}{
		{
			name:      "mcpServer/elicitation/request",
			method:    "mcpServer/elicitation/request",
			requestID: 30,
			detail:    detailAnswerToQuestion,
			wantWrite: `{"id":30,"result":{"action":"decline"}}` + "\n",
		},
		{
			name:      "item/permissions/requestApproval",
			method:    "item/permissions/requestApproval",
			requestID: 31,
			detail:    detailWiderAccess,
			wantWrite: `{"id":31,"error":{"code":-32001,"message":"sortie refuses requests that only a person could answer"}}` + "\n",
		},
		{
			name:      "item/tool/requestUserInput",
			method:    "item/tool/requestUserInput",
			requestID: 32,
			detail:    detailAnswerToQuestion,
			wantWrite: `{"id":32,"error":{"code":-32001,"message":"sortie refuses requests that only a person could answer"}}` + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := &capturingWriteCloser{}
			state := makeTestStateWithStdin(t, runTurnFixtureWithServerRequest(tt.requestID, tt.method), stdin)
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			dispositiontest.AssertDispositionContract(t, agentcore.HumanInputEvidence(tt.detail), result, err)

			write, ok := stdin.find(fmt.Sprintf(`{"id":%d,`, tt.requestID))
			if !ok {
				t.Fatalf("no response written for request id %d", tt.requestID)
			}
			if write != tt.wantWrite {
				t.Errorf("response for %s = %q, want %q", tt.method, write, tt.wantWrite)
			}
		})
	}
}

// TestRunTurn_CancelledReturnsWithinBoundWhenTurnCompletedNeverArrives
// pins the post-cancellation bound: after the best-effort turn/interrupt,
// the loop must return once read_timeout_ms elapses rather than reading
// until stdout closes. The runtime's own turn/completed never arrives in
// this test.
func TestRunTurn_CancelledReturnsWithinBoundWhenTurnCompletedNeverArrives(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &interruptTrackingWriteCloser{interrupts: make(chan struct{}, 1)}
	state := newInterruptedStatusState(t, stdin)
	state.agentConfig = domain.AgentConfig{ReadTimeoutMS: 50}

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan outcome, 1)
	finished := make(chan struct{})

	adapter, _ := NewCodexAdapter(map[string]any{})
	go func() {
		defer close(finished)
		result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: func(domain.AgentEvent) {},
		})
		outcomeCh <- outcome{result: result, err: err}
	}()

	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	})

	// The unbuffered send returns only once RunTurn has received it,
	// which proves its main event loop is running (rather than still
	// inside the turn/start call) before cancellation.
	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/started", Params: json.RawMessage(`{"turnId":"turn-001"}`)}

	start := time.Now()
	cancel()

	select {
	case <-stdin.interrupts:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not send turn/interrupt after cancellation")
	}

	// turn/completed is never sent. RunTurn must return once the
	// configured read_timeout_ms bound elapses, not merely eventually.
	select {
	case got := <-outcomeCh:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("RunTurn returned after %v, want well within the 50ms read_timeout_ms bound", elapsed)
		}
		if got.result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCancelled)
		}
		requireAgentError(t, got.err, domain.ErrTurnCancelled)
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn did not return within the bound; it kept reading after cancellation with no turn/completed")
	}
}

// --- StopSession ---

func TestStopSession_InvalidInternalType(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: "wrong"})
	if err == nil {
		t.Fatal("StopSession() expected error for wrong internal type, got nil")
	}
}

func TestRunTurn_MultiTurnNoRace(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-001\"}}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-001\",\"tokenUsage\":{\"last\":{\"totalTokens\":150,\"inputTokens\":100,\"cachedInputTokens\":10,\"outputTokens\":50,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":150,\"inputTokens\":100,\"cachedInputTokens\":10,\"outputTokens\":50,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"completed\"}}}\n" +
		"{\"id\":2,\"result\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-002\"}}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-002\",\"tokenUsage\":{\"last\":{\"totalTokens\":300,\"inputTokens\":200,\"cachedInputTokens\":20,\"outputTokens\":100,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":300,\"inputTokens\":200,\"cachedInputTokens\":20,\"outputTokens\":100,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"completed\"}}}\n"

	state := makeTestState(t, []byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})
	session := fakeSession(state)

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(1) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(1) ExitReason = %v, want %v", result1.ExitReason, domain.EventTurnCompleted)
	}

	result2, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "second turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(2) error = %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(2) ExitReason = %v, want %v", result2.ExitReason, domain.EventTurnCompleted)
	}
}

func TestRunTurn_StdoutEOFBetweenTurns(t *testing.T) {
	t.Parallel()

	// One complete turn, pushed directly onto state.msgCh once the
	// gated turn/start call resolves. After the first turn returns,
	// the connection is closed directly: the second turn's call can
	// only resolve via that closed connection, since no response was
	// ever scripted for it to race.
	state := gatedTurnStartState(t, `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`)
	adapter, _ := NewCodexAdapter(map[string]any{})
	session := fakeSession(state)

	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/started", Params: json.RawMessage(`{"turnId":"turn-001"}`)}
	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "thread/tokenUsage/updated", Params: json.RawMessage(`{"threadId":"thread-001","turnId":"turn-001","tokenUsage":{"last":{"totalTokens":15,"inputTokens":10,"cachedInputTokens":0,"outputTokens":5,"reasoningOutputTokens":0},"total":{"totalTokens":15,"inputTokens":10,"cachedInputTokens":0,"outputTokens":5,"reasoningOutputTokens":0}}}`)}
	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"turn-001","status":"completed"}}`)}

	if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first turn",
		OnEvent: func(domain.AgentEvent) {},
	}); err != nil {
		t.Fatalf("RunTurn(1) unexpected error: %v", err)
	}

	state.conn.Close()

	// The second call must return ErrPortExit immediately.
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "second turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestStopSession_NilState(t *testing.T) {
	t.Parallel()

	// State with nil proc and nil waitCh — StopSession should return nil.
	state := &sessionState{
		stdin:  nopWriteCloser{},
		waitCh: nil,
	}
	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: state})
	if err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}
}

func TestStopSession_WithActiveReaderGoroutine(t *testing.T) {
	t.Parallel()

	// Provide more messages than the channel buffer (16) so the reader
	// goroutine is blocked on a channel send when StopSession closes stopCh.
	line := []byte("{\"method\":\"turn/started\",\"params\":{}}\n")
	state := makeTestState(t, bytes.Repeat(line, 20))
	// Simulate the subprocess having already exited so waitCh does not block.
	close(state.waitCh)

	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: state})
	if err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	// StopSession waits on readerDone internally; it must be closed on return.
	select {
	case <-state.readerDone:
		// OK
	default:
		t.Error("readerDone should be closed after StopSession")
	}
}

// --- Handshake/turn-phase isolation and wait-loop termination ---

// TestHandshakeIsolation_PreTurnMessagesDoNotReachFirstTurn drives one
// jsonrpc.Conn through initializeHandshake, authenticateIfNeeded, and
// startThread, then reproduces what the handshake-phase handler
// queues before the turn phase begins — a notification and a line
// that fails to parse — and calls beginTurnPhase before running one
// turn on the same connection. It asserts the turn completes normally
// and the pre-turn messages produce no event and do not fail the
// turn.
func TestHandshakeIsolation_PreTurnMessagesDoNotReachFirstTurn(t *testing.T) {
	t.Parallel()

	state := handshakeState(t,
		`{"id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"codex-app-server"}}}`,
		`{"id":2,"result":{"account":{"id":"user-1"}}}`,
		`{"id":3,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
		`{"id":4,"result":{"turn":{"id":"turn-001","status":"starting"}}}`,
		`{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"completed"}}}`,
	)

	if err := initializeHandshake(context.Background(), state); err != nil {
		t.Fatalf("initializeHandshake() error = %v", err)
	}
	if err := authenticateIfNeeded(context.Background(), state); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v", err)
	}
	threadID, err := startThread(context.Background(), state, passthroughConfig{})
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	state.threadID = threadID

	// Reproduce what the handshake-phase handler queues before the
	// turn phase begins: a notification and a line that fails to
	// parse, the same shapes the app-server might send between the
	// handshake and the first turn.
	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "some/unsolicited"}
	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindMalformed, Err: errors.New("malformed")}

	beginTurnPhase(state)

	adapter, _ := NewCodexAdapter(map[string]any{})
	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	for _, e := range events {
		if e.Type == domain.EventOtherMessage && e.Message == "some/unsolicited" {
			t.Errorf("event stream leaked the pre-turn notification %q", e.Message)
		}
	}
}

// TestRunTurn_StdoutParseFailureBeforeResponse pins that a line which
// fails to parse arriving between the turn/start write and its
// response ends the turn as domain.ErrPortExit, the same disposition
// TestRunTurn_StdoutParseFailure pins for a malformed line arriving
// after the response.
func TestRunTurn_StdoutParseFailureBeforeResponse(t *testing.T) {
	t.Parallel()

	state := makeTestStateWithMalformedBeforeResponse(t,
		"not valid json",
		`{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`,
	)
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if !strings.HasPrefix(agentErr.Message, "stdout read error: ") {
		t.Errorf("AgentError.Message = %q, want prefix %q", agentErr.Message, "stdout read error: ")
	}

	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
}

// TestAuthenticateIfNeeded_LoginWaitEOF drives the login wait past a
// clean end of stream and asserts it returns the pinned text rather
// than timing out, within 2s while readTimeout keeps its 30s default.
func TestAuthenticateIfNeeded_LoginWaitEOF(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "test-key")

	state := authWaitState(t, map[int64]string{
		1: `{"id":1,"result":{"account":null}}`,
		2: `{"id":2,"result":{}}`,
	})

	if got := readTimeout(state); got != 30*time.Second {
		t.Fatalf("readTimeout() = %v, want the 30s default", got)
	}

	done := make(chan error, 1)
	go func() { done <- authenticateIfNeeded(context.Background(), state) }()

	// authenticateIfNeeded only reads state.msgCh once
	// account/login/start has already returned, and Call never reads
	// state.msgCh at all, so closing it here cannot race Call's own
	// resolution on the connection.
	close(state.msgCh)

	select {
	case err := <-done:
		const want = "unexpected EOF waiting for login"
		if err == nil || err.Error() != want {
			t.Errorf("authenticateIfNeeded() error = %v, want %q", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authenticateIfNeeded() did not return within 2s of the stream ending")
	}
}

// TestAuthenticateIfNeeded_LoginWaitReadError drives the login wait
// past a read failure (rather than a clean end of stream) and asserts
// it returns the pinned text naming the underlying error, within 2s.
func TestAuthenticateIfNeeded_LoginWaitReadError(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "test-key")

	wantErr := errors.New("boom")
	state := authWaitState(t, map[int64]string{
		1: `{"id":1,"result":{"account":null}}`,
		2: `{"id":2,"result":{}}`,
	})

	done := make(chan error, 1)
	go func() { done <- authenticateIfNeeded(context.Background(), state) }()

	state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindStreamEnd, Err: wantErr}

	select {
	case err := <-done:
		want := "scanner error waiting for login: " + wantErr.Error()
		if err == nil || err.Error() != want {
			t.Errorf("authenticateIfNeeded() error = %v, want %q", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authenticateIfNeeded() did not return within 2s of the read failure")
	}
}

// TestStartThread_NotificationWaitEOF drives the thread/started wait
// past a clean end of stream and asserts it returns the thread ID
// with a nil error, within 2s, rather than waiting for the 30s
// readTimeout default.
func TestStartThread_NotificationWaitEOF(t *testing.T) {
	t.Parallel()

	state := authWaitState(t, map[int64]string{
		1: `{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
	})

	type outcome struct {
		threadID string
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		threadID, err := startThread(context.Background(), state, passthroughConfig{})
		done <- outcome{threadID, err}
	}()

	close(state.msgCh)

	select {
	case got := <-done:
		if got.err != nil {
			t.Errorf("startThread() error = %v, want nil", got.err)
		}
		if got.threadID != "thread-abc" {
			t.Errorf("startThread() threadID = %q, want %q", got.threadID, "thread-abc")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startThread() did not return within 2s of the stream ending")
	}
}
