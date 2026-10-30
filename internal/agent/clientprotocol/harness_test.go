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

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTestSession(t *testing.T, agentConfig domain.AgentConfig, maxLineBytes int) (*sessionState, *io.PipeReader, *io.PipeWriter) {
	t.Helper()
	return newTestSessionWithLogger(t, agentConfig, maxLineBytes, discardLogger())
}

func newTestSessionWithLogger(t *testing.T, agentConfig domain.AgentConfig, maxLineBytes int, logger *slog.Logger) (*sessionState, *io.PipeReader, *io.PipeWriter) {
	t.Helper()

	tools, agentErr := agentcore.ResolveToolDelivery(t.Context(), domain.StartSessionParams{}, agentcore.LaunchTarget{}, registry.MCPDialectACPSession, logger)
	if agentErr != nil {
		t.Fatalf("ResolveToolDelivery() = %v, want nil", agentErr)
	}
	t.Cleanup(func() { _ = tools.Close() })

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()

	state := &sessionState{
		tools:       tools,
		agentConfig: agentConfig,
		caps:        newCapabilityRecord(),
		usage:       agentcore.NewTurnEndUsage(),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      logger,
		origins:     &sessionOrigins{},
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(outPw, inPr, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(maxLineBytes))

	go runPump(state)

	t.Cleanup(func() {
		_ = inPw.Close()
		state.stopOnce.Do(func() { close(state.stopCh) })
		<-state.pumpDone
		state.conn.Close()
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
	ch  chan []byte
	err error
}

func newOutboundReader(r io.Reader) *outboundReader {
	rec := &outboundReader{ch: make(chan []byte, 64)}
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64<<10), 32<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			rec.ch <- line
		}
		rec.err = scanner.Err()
		close(rec.ch)
	}()
	return rec
}

func (r *outboundReader) next(t *testing.T) []byte {
	t.Helper()
	select {
	case line, ok := <-r.ch:
		if !ok {
			if r.err != nil {
				t.Fatalf("read outbound stream: %v", r.err)
			}
			t.Fatal("outbound stream ended with no more lines")
		}
		return line
	case <-waittest.Context(t).Done():
		t.Fatalf("the pump wrote no line within %v", waittest.HangGuard)
		return nil
	}
}

func (r *outboundReader) awaitMethod(t *testing.T, method string) (id json.RawMessage) {
	t.Helper()
	ctx := waittest.Context(t)
	for {
		select {
		case line, ok := <-r.ch:
			if !ok {
				if r.err != nil {
					t.Fatalf("read outbound stream before method %q: %v", method, r.err)
				}
				t.Fatalf("outbound stream ended before method %q was written", method)
			}
			var h wireHeader
			if err := json.Unmarshal(line, &h); err != nil {
				continue
			}
			if h.Method == method {
				return h.ID
			}
		case <-ctx.Done():
			t.Fatalf("method %q not written within %v", method, waittest.HangGuard)
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
