package clientprotocol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

const awaitTimeout = 5 * time.Second

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTestSession(t *testing.T, agentConfig domain.AgentConfig, maxLineBytes int) (*sessionState, *io.PipeReader, *io.PipeWriter) {
	t.Helper()
	return newTestSessionWithLogger(t, agentConfig, maxLineBytes, discardLogger())
}

// withUsageReader must be applied before the pump starts.
func withUsageReader(reader usageReader) func(*sessionState) {
	return func(state *sessionState) {
		state.reader = reader
		state.caps = newCapabilityRecord(false, true)
	}
}

// newTestSessionWithLogger behaves like newTestSession, but wires
// state's logger to logger instead of one that discards everything,
// for a test that needs to observe what the pump logs.
func newTestSessionWithLogger(t *testing.T, agentConfig domain.AgentConfig, maxLineBytes int, logger *slog.Logger, opts ...func(*sessionState)) (*sessionState, *io.PipeReader, *io.PipeWriter) {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()

	state := &sessionState{
		agentConfig: agentConfig,
		caps:        newCapabilityRecord(false, false),
		usage:       agentcore.NewTurnEndUsage(),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      logger,
		origins:     &sessionOrigins{},
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(outPw, inPr, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(maxLineBytes))

	for _, opt := range opts {
		opt(state)
	}

	go runPump(state)

	t.Cleanup(func() {
		_ = inPw.Close()
		state.stopOnce.Do(func() { close(state.stopCh) })
		<-state.pumpDone
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
	})

	return state, outPr, inPw
}

func fakeSession(state *sessionState) domain.Session {
	return domain.Session{ID: "sess-test", Internal: state}
}

func markSessionKnown(state *sessionState) {
	state.inbox.Put(pumpItem{control: &pumpControl{sessionID: "sess-test"}})
}

func collectEvents(events *[]domain.AgentEvent) func(domain.AgentEvent) {
	var mu sync.Mutex
	return func(e domain.AgentEvent) {
		mu.Lock()
		*events = append(*events, e)
		mu.Unlock()
	}
}

type turnOutcome struct {
	result domain.TurnResult
	err    error
}

func runTurnAsync(state *sessionState, params domain.RunTurnParams) <-chan turnOutcome {
	return runTurnAsyncCtx(context.Background(), state, params)
}

func runTurnAsyncCtx(ctx context.Context, state *sessionState, params domain.RunTurnParams) <-chan turnOutcome {
	ch := make(chan turnOutcome, 1)
	go func() {
		result, err := runTurn(ctx, fakeSession(state), params)
		ch <- turnOutcome{result: result, err: err}
	}()
	return ch
}

func awaitOutcome(t *testing.T, ch <-chan turnOutcome) turnOutcome {
	t.Helper()
	select {
	case outcome := <-ch:
		return outcome
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the turn to finish")
		return turnOutcome{}
	}
}

type wireHeader struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

type wireResponse struct {
	ID     json.RawMessage `json:"id"`
	Error  *jsonrpc.Error  `json:"error"`
	Result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	} `json:"result"`
}

type outboundReader struct {
	ch chan []byte
}

// newOutboundReader starts scanning r in the background.
func newOutboundReader(r io.Reader) *outboundReader {
	rec := &outboundReader{ch: make(chan []byte, 64)}
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64<<10), 32<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			rec.ch <- line
		}
		close(rec.ch)
	}()
	return rec
}

func (r *outboundReader) next(t *testing.T) []byte {
	t.Helper()
	select {
	case line, ok := <-r.ch:
		if !ok {
			t.Fatal("outbound stream ended with no more lines")
		}
		return line
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the pump to write a line")
		return nil
	}
}

func (r *outboundReader) awaitMethod(t *testing.T, method string) (id json.RawMessage) {
	t.Helper()
	deadline := time.After(awaitTimeout)
	for {
		select {
		case line, ok := <-r.ch:
			if !ok {
				t.Fatalf("outbound stream ended before method %q was written", method)
			}
			var h wireHeader
			if err := json.Unmarshal(line, &h); err != nil {
				continue
			}
			if h.Method == method {
				return h.ID
			}
		case <-deadline:
			t.Fatalf("timed out waiting for method %q", method)
			return nil
		}
	}
}

func sendLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(w, line); err != nil {
		t.Fatalf("write fixture line %s: %v", line, err)
	}
}

// respondLine splices id in verbatim so the response carries whatever wire form
// the caller captured from the request line.
func respondLine(t *testing.T, w io.Writer, id json.RawMessage, result any) {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal response result: %v", err)
	}
	sendLine(t, w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, string(id), string(body)))
}

// respondErrorLine splices id in verbatim so the response carries whatever wire
// form the caller captured from the request line.
func respondErrorLine(t *testing.T, w io.Writer, id json.RawMessage, code int, message string) {
	t.Helper()
	body, err := json.Marshal(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
	if err != nil {
		t.Fatalf("marshal error response body: %v", err)
	}
	sendLine(t, w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":%s}`, string(id), string(body)))
}

func decodeResponse(t *testing.T, line []byte) wireResponse {
	t.Helper()
	var resp wireResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode response line %s: %v", line, err)
	}
	return resp
}

func assertRawID(t *testing.T, line []byte, wantRaw string) {
	t.Helper()
	var h wireHeader
	if err := json.Unmarshal(line, &h); err != nil {
		t.Fatalf("decode line %s: %v", line, err)
	}
	if string(h.ID) != wantRaw {
		t.Errorf("response id = %s, want %s", h.ID, wantRaw)
	}
}
