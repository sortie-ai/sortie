package jsonrpc_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
)

// captureWriter records every Write call under a mutex so a test can
// assert on the exact bytes a write method produced, whether or not
// the test itself runs anything concurrently.
type captureWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *captureWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// callOutcome is what a backgrounded Call reports once it returns.
type callOutcome struct {
	resp jsonrpc.Response
	err  error
}

// testConn wires a [jsonrpc.Conn] between two [io.Pipe] pairs so a
// test can write fixture lines as the peer and observe the requests
// the connection writes back, without touching any unexported symbol.
type testConn struct {
	conn    *jsonrpc.Conn
	reqR    *io.PipeReader
	peerW   *io.PipeWriter
	handled chan jsonrpc.Message
}

func newTestConn(t *testing.T) *testConn {
	t.Helper()

	reqR, reqW := io.Pipe()
	peerR, peerW := io.Pipe()
	t.Cleanup(func() {
		_ = reqR.Close()
		_ = reqW.Close()
		_ = peerR.Close()
		_ = peerW.Close()
	})

	tc := &testConn{reqR: reqR, peerW: peerW, handled: make(chan jsonrpc.Message, 16)}
	tc.conn = jsonrpc.NewConn(reqW, peerR, func(msg jsonrpc.Message) { tc.handled <- msg })
	return tc
}

// startCall launches Call on its own goroutine, since Call blocks
// until it observes a response, a done context, or the connection
// closing, and returns the id the connection assigned to the request
// it wrote, read straight off the wire rather than assumed.
func (tc *testConn) startCall(t *testing.T, ctx context.Context) (id int64, done <-chan callOutcome) {
	t.Helper()

	ch := make(chan callOutcome, 1)
	go func() {
		resp, err := tc.conn.Call(ctx, "call/method", nil)
		ch <- callOutcome{resp: resp, err: err}
	}()

	line := readLine(t, tc.reqR)
	return extractID(t, line), ch
}

func readLine(t *testing.T, r io.Reader) []byte {
	t.Helper()

	line, err := bufio.NewReader(r).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	return bytes.TrimRight(line, "\n")
}

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()

	if _, err := io.WriteString(w, line+"\n"); err != nil {
		t.Fatalf("write line %q: %v", line, err)
	}
}

func extractID(t *testing.T, line []byte) int64 {
	t.Helper()

	var wire struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(line, &wire); err != nil {
		t.Fatalf("unmarshal request line %s: %v", line, err)
	}
	if wire.ID == 0 {
		t.Fatalf("request line %s carries no id", line)
	}
	return wire.ID
}

// TestConn_RoutesEveryNonMatchingMessageWhileCallInFlight is AC2: a
// notification, an unmatched response, a malformed line, and a
// server-initiated request that arrive while a Call is in flight each
// reach the handler in order and with the right classification, and
// the Call still receives its own response.
func TestConn_RoutesEveryNonMatchingMessageWhileCallInFlight(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)
	id, done := tc.startCall(t, context.Background())
	unmatchedID := id + 1000

	writeLine(t, tc.peerW, `{"method":"progress/update","params":{"pct":50}}`)
	writeLine(t, tc.peerW, fmt.Sprintf(`{"id":%d,"result":{"ignored":true}}`, unmatchedID))
	writeLine(t, tc.peerW, `not json`)
	writeLine(t, tc.peerW, `{"id":7,"method":"item/tool/call","params":{"tool":"x"}}`)
	writeLine(t, tc.peerW, fmt.Sprintf(`{"id":%d,"result":{"ok":true}}`, id))

	wantKinds := []jsonrpc.Kind{
		jsonrpc.KindNotification,
		jsonrpc.KindResponse,
		jsonrpc.KindMalformed,
		jsonrpc.KindRequest,
	}
	for i, want := range wantKinds {
		select {
		case msg := <-tc.handled:
			if msg.Kind != want {
				t.Errorf("handler message %d Kind = %v, want %v", i, msg.Kind, want)
			}
			if want == jsonrpc.KindMalformed && msg.Err == nil {
				t.Errorf("handler message %d Err = nil, want non-nil", i)
			}
			if want == jsonrpc.KindRequest && msg.ID != 7 {
				t.Errorf("handler message %d ID = %d, want 7", i, msg.ID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for handler message %d (Kind %v)", i, want)
		}
	}

	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("Call() error = %v, want nil", outcome.err)
		}
		if outcome.resp.ID != id {
			t.Errorf("Call() response ID = %d, want %d", outcome.resp.ID, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Call to return its own response")
	}

	select {
	case msg := <-tc.handled:
		t.Errorf("handler received an unexpected extra message: %+v", msg)
	default:
	}
}

// TestConn_NotificationBeforeAnyCallReachesHandler is AC3: a
// notification delivered before any Call has been issued still
// reaches the handler. No Call is ever issued in this test, so a
// router keying on "a call is in flight" would fail it.
func TestConn_NotificationBeforeAnyCallReachesHandler(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)

	writeLine(t, tc.peerW, `{"method":"thread/started","params":{"threadId":"abc-123"}}`)

	select {
	case msg := <-tc.handled:
		if msg.Kind != jsonrpc.KindNotification {
			t.Errorf("handler message Kind = %v, want %v", msg.Kind, jsonrpc.KindNotification)
		}
		if msg.Method != "thread/started" {
			t.Errorf("handler message Method = %q, want %q", msg.Method, "thread/started")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive a notification delivered before any call")
	}
}

func TestConn_CallContextDone(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, done := tc.startCall(t, ctx)

	cancel()

	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Errorf("Call() error = %v, want error satisfying errors.Is(err, context.Canceled)", outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Call to return after context cancellation")
	}
}

func TestConn_CallCleanEOF(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)
	id, done := tc.startCall(t, context.Background())

	if err := tc.peerW.Close(); err != nil {
		t.Fatalf("peerW.Close() error = %v", err)
	}

	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, io.EOF) {
			t.Errorf("Call() error = %v, want error wrapping io.EOF", outcome.err)
		}
		want := fmt.Sprintf("unexpected EOF waiting for response id=%d", id)
		if outcome.err.Error() != want {
			t.Errorf("Call() error = %q, want %q", outcome.err.Error(), want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Call to fail on a clean end of stream")
	}
}

func TestConn_CallReadFailure(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)
	id, done := tc.startCall(t, context.Background())
	readErr := errors.New("boom")

	if err := tc.peerW.CloseWithError(readErr); err != nil {
		t.Fatalf("peerW.CloseWithError() error = %v", err)
	}

	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, readErr) {
			t.Errorf("Call() error = %v, want error wrapping %v", outcome.err, readErr)
		}
		want := fmt.Sprintf("scanner error waiting for response id=%d: %s", id, readErr)
		if outcome.err.Error() != want {
			t.Errorf("Call() error = %q, want %q", outcome.err.Error(), want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Call to fail on a read error")
	}
}

func TestConn_CallErrClosed(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)
	id, done := tc.startCall(t, context.Background())

	tc.conn.Close()

	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, jsonrpc.ErrClosed) {
			t.Errorf("Call() error = %v, want error wrapping jsonrpc.ErrClosed", outcome.err)
		}
		want := fmt.Sprintf("connection closed waiting for response id=%d", id)
		if outcome.err.Error() != want {
			t.Errorf("Call() error = %q, want %q", outcome.err.Error(), want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Call to fail after Close")
	}
}

// TestConn_OverLongLineFailsRead covers the MaxLineBytes case AC11
// names: a line longer than MaxLineBytes with no newline fails the
// read, delivers a final KindStreamEnd message, and fails the call in
// flight, all wrapping the scanner's own error.
func TestConn_OverLongLineFailsRead(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)
	_, done := tc.startCall(t, context.Background())

	go func() {
		overLong := bytes.Repeat([]byte("a"), jsonrpc.MaxLineBytes+1)
		_, _ = tc.peerW.Write(overLong)
	}()

	select {
	case msg := <-tc.handled:
		if msg.Kind != jsonrpc.KindStreamEnd {
			t.Errorf("handler message Kind = %v, want %v", msg.Kind, jsonrpc.KindStreamEnd)
		}
		if !errors.Is(msg.Err, bufio.ErrTooLong) {
			t.Errorf("handler message Err = %v, want error wrapping bufio.ErrTooLong", msg.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for KindStreamEnd on an over-long line")
	}

	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, bufio.ErrTooLong) {
			t.Errorf("Call() error = %v, want error wrapping bufio.ErrTooLong", outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Call to fail on an over-long line")
	}

	select {
	case <-tc.conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close after an over-long line")
	}
}

// TestConn_RespondError_WritesExactBytes is AC10's first half.
func TestConn_RespondError_WritesExactBytes(t *testing.T) {
	t.Parallel()

	var w captureWriter
	conn := jsonrpc.NewConn(&w, strings.NewReader(""), func(jsonrpc.Message) {})

	err := conn.RespondError(7, -32001, "sortie refuses requests that only a person could answer")

	if err != nil {
		t.Fatalf("RespondError(7, -32001, ...) error = %v", err)
	}
	const want = `{"id":7,"error":{"code":-32001,"message":"sortie refuses requests that only a person could answer"}}` + "\n"
	got := w.String()
	if got != want {
		t.Errorf("RespondError(7, -32001, ...) wrote %q, want %q", got, want)
	}
	if strings.Contains(got, `"jsonrpc"`) {
		t.Errorf("RespondError(7, -32001, ...) wrote %q, want no jsonrpc member", got)
	}
}

// TestConn_ConcurrentCallsGetUniqueIDsAndCompleteLines is AC10's
// second half: concurrent Call and Notify operations each write one
// complete, separately parseable line, and no two calls receive the
// same request id.
func TestConn_ConcurrentCallsGetUniqueIDsAndCompleteLines(t *testing.T) {
	t.Parallel()

	reqR, reqW := io.Pipe()
	peerR, peerW := io.Pipe()
	t.Cleanup(func() {
		_ = reqR.Close()
		_ = reqW.Close()
		_ = peerR.Close()
		_ = peerW.Close()
	})
	conn := jsonrpc.NewConn(reqW, peerR, func(jsonrpc.Message) {})

	var (
		peerMu    sync.Mutex
		seenIDs   = make(map[int64]bool)
		duplicate bool
		malformed bool
	)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		scanner := bufio.NewScanner(reqR)
		scanner.Buffer(make([]byte, 0, jsonrpc.MaxLineBytes), jsonrpc.MaxLineBytes)
		for scanner.Scan() {
			var wire struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &wire); err != nil {
				peerMu.Lock()
				malformed = true
				peerMu.Unlock()
				continue
			}
			if wire.ID == 0 {
				continue
			}

			peerMu.Lock()
			if seenIDs[wire.ID] {
				duplicate = true
			}
			seenIDs[wire.ID] = true
			peerMu.Unlock()

			if _, err := peerW.Write([]byte(fmt.Sprintf(`{"id":%d,"result":{"ok":true}}`, wire.ID) + "\n")); err != nil {
				return
			}
		}
	}()

	const numCalls = 20
	const numNotifies = 20
	var wg sync.WaitGroup
	errs := make(chan error, numCalls+numNotifies)
	for i := range numCalls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := conn.Call(context.Background(), "call/method", map[string]any{"i": i})
			errs <- err
		}(i)
	}
	for i := range numNotifies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- conn.Notify("note/method", map[string]any{"i": i})
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Call/Notify: %v", err)
		}
	}
	if err := reqR.Close(); err != nil {
		t.Fatalf("reqR.Close() error = %v", err)
	}
	select {
	case <-peerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fake peer to observe the closed pipe")
	}

	peerMu.Lock()
	defer peerMu.Unlock()
	if malformed {
		t.Error("the fake peer observed a line that did not parse as JSON")
	}
	if duplicate {
		t.Error("two calls received the same request id")
	}
	if len(seenIDs) != numCalls {
		t.Errorf("the fake peer observed %d unique call ids, want %d", len(seenIDs), numCalls)
	}
}

// TestConn_DoneClosesAfterCloseAndReaderClose is AC9: the reader
// goroutine exits, signaled by Done closing, once Close has run and
// the underlying reader has been closed to unblock the parked read.
func TestConn_DoneClosesAfterCloseAndReaderClose(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	conn := jsonrpc.NewConn(io.Discard, pr, func(jsonrpc.Message) {})

	conn.Close()
	if err := pr.Close(); err != nil {
		t.Fatalf("pr.Close() error = %v", err)
	}

	select {
	case <-conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close after Close and reader close")
	}
}
