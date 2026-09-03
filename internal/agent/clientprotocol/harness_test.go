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

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// awaitTimeout bounds every wait a test performs on a channel the pump
// or the connection's reader goroutine feeds. It is generous enough to
// absorb scheduler noise without masking a genuine hang: a test that
// needs longer than this to observe an expected line has a defect this
// bound is meant to surface, not paper over.
const awaitTimeout = 5 * time.Second

// discardLogger returns a logger that writes nowhere, for a session
// under test that only cares about wire behavior, not diagnostics.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// newTestSession builds a *sessionState wired to an in-memory
// jsonrpc.Conn with no subprocess involved: outPr lets a test observe,
// as newline-delimited lines, what the pump writes toward the agent;
// inPw lets a test deliver a line as if the agent had sent it, or
// close (or CloseWithError) to end the simulated stream. The pump
// goroutine is already running when this returns.
//
// t.Cleanup always drives the pump to exit, whether or not the test
// itself already ended the stream, so no test leaks the goroutine.
func newTestSession(t *testing.T, agentConfig domain.AgentConfig, maxLineBytes int) (*sessionState, *io.PipeReader, *io.PipeWriter) {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()

	state := &sessionState{
		agentConfig: agentConfig,
		caps:        newCapabilityRecord(false),
		itemCh:      make(chan pumpItem, pumpChannelCapacity),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      discardLogger(),
	}
	state.conn = jsonrpc.NewConn(outPw, inPr, pumpHandler(state.itemCh, state.stopCh),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(maxLineBytes))

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

// fakeSession wraps state in a domain.Session suitable for runTurn.
func fakeSession(state *sessionState) domain.Session {
	return domain.Session{ID: "sess-test", Internal: state}
}

// markSessionKnown publishes the sessionID control message a real
// StartSession would have sent, so pump-level checks that key on
// p.sessionIDKnown behave as they would in production. Every test in
// this package that needs a known session ID uses the same one.
func markSessionKnown(state *sessionState) {
	state.itemCh <- pumpItem{control: &pumpControl{sessionID: "sess-test"}}
}

// collectEvents is an OnEvent callback that appends to a slice under a
// mutex, safe for the pump's own goroutine to call concurrently with
// the test goroutine reading the slice after the turn ends.
func collectEvents(events *[]domain.AgentEvent) func(domain.AgentEvent) {
	var mu sync.Mutex
	return func(e domain.AgentEvent) {
		mu.Lock()
		*events = append(*events, e)
		mu.Unlock()
	}
}

// turnOutcome bundles a runTurn call's return values so a test can
// hand them across a goroutine boundary on one channel.
type turnOutcome struct {
	result domain.TurnResult
	err    error
}

// runTurnAsync starts runTurn on its own goroutine and returns the
// channel its outcome arrives on, so the calling test can drive the
// simulated peer while the turn is in flight.
func runTurnAsync(state *sessionState, params domain.RunTurnParams) <-chan turnOutcome {
	return runTurnAsyncCtx(context.Background(), state, params)
}

// runTurnAsyncCtx behaves like runTurnAsync, but lets a test supply
// its own context, for exercising orchestrator cancellation.
func runTurnAsyncCtx(ctx context.Context, state *sessionState, params domain.RunTurnParams) <-chan turnOutcome {
	ch := make(chan turnOutcome, 1)
	go func() {
		result, err := runTurn(ctx, fakeSession(state), params)
		ch <- turnOutcome{result: result, err: err}
	}()
	return ch
}

// awaitOutcome waits for outcome on ch, failing t if it does not
// arrive within awaitTimeout.
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

// wireHeader decodes the two fields of a JSON-RPC line that classify
// it without committing to a params or result shape.
type wireHeader struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// wireResponse decodes a response line whose result is a permission
// reply, the only response shape these tests write assertions
// against.
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

// outboundReader scans lines the pump writes to outPr (what the
// production adapter would write toward the agent's standard input)
// and republishes each on a channel, so a test can wait for a specific
// line without racing the scanner's own buffering.
type outboundReader struct {
	ch chan []byte
}

// newOutboundReader starts scanning r in the background. Lines larger
// than the buffer below fail the scan silently from the reader's
// perspective; every fixture in this package's tests stays well under
// it except the dedicated line-bound case, which builds its own
// connection with a small bound instead of using this reader.
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

// next returns the next line the pump wrote, failing t if none
// arrives within awaitTimeout or the stream ends first.
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

// awaitMethod scans forward until it finds a line naming method,
// returning its raw id bytes, failing t if the stream ends or
// awaitTimeout elapses first. Lines for a different method are
// consumed and discarded, matching how a real peer would ignore
// traffic it does not care about while waiting for one specific call.
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

// sendLine writes line, followed by a newline, to w, failing t on
// error.
func sendLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(w, line); err != nil {
		t.Fatalf("write fixture line %s: %v", line, err)
	}
}

// respondLine writes a JSON-RPC success response answering id with
// result, splicing id in verbatim so it carries whatever wire form the
// caller captured from a request line.
func respondLine(t *testing.T, w io.Writer, id json.RawMessage, result any) {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal response result: %v", err)
	}
	sendLine(t, w, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, string(id), string(body)))
}

// respondErrorLine writes a JSON-RPC error response answering id with
// code and message, splicing id in verbatim so it carries whatever
// wire form the caller captured from a request line.
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

// decodeResponse decodes line as a wireResponse, failing t on a
// decode error.
func decodeResponse(t *testing.T, line []byte) wireResponse {
	t.Helper()
	var resp wireResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode response line %s: %v", line, err)
	}
	return resp
}

// assertRawID fails t unless line's top-level "id" member is exactly
// wantRaw, byte for byte: an id echoed in another form is one the
// agent cannot correlate.
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
