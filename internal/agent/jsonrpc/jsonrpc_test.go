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

func newTestConn(t *testing.T, opts ...jsonrpc.Option) *testConn {
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
	tc.conn = jsonrpc.NewConn(reqW, peerR, func(msg jsonrpc.Message) { tc.handled <- msg }, opts...)
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

// TestConn_RoutesEveryNonMatchingMessageWhileCallInFlight checks that a
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
			if want == jsonrpc.KindRequest && !msg.ID.Equal(jsonrpc.NumberID(7)) {
				t.Errorf("handler message %d ID = %s, want %s", i, msg.ID, jsonrpc.NumberID(7))
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
		if !outcome.resp.ID.Equal(jsonrpc.NumberID(id)) {
			t.Errorf("Call() response ID = %s, want %s", outcome.resp.ID, jsonrpc.NumberID(id))
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

// TestConn_NotificationBeforeAnyCallReachesHandler checks that a
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

// TestConn_OverLongLineFailsRead covers the MaxLineBytes case: a
// line longer than MaxLineBytes with no newline fails the
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

// TestConn_RespondError_WritesExactBytes checks the exact bytes
// RespondError writes to the connection.
func TestConn_RespondError_WritesExactBytes(t *testing.T) {
	t.Parallel()

	var w captureWriter
	conn := jsonrpc.NewConn(&w, strings.NewReader(""), func(jsonrpc.Message) {})

	err := conn.RespondError(jsonrpc.NumberID(7), -32001, "sortie refuses requests that only a person could answer")

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

// TestConn_ConcurrentCallsGetUniqueIDsAndCompleteLines checks that
// concurrent Call and Notify operations each write one
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

// TestConn_DoneClosesAfterCloseAndReaderClose checks that the reader
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

// TestConn_VersionMember checks that WithVersionMember makes the
// connection write "jsonrpc":"2.0" on every outgoing message kind,
// and that without it no such member is written, for each kind
// independently.
func TestConn_VersionMember(t *testing.T) {
	t.Parallel()

	sendRequest := func(conn *jsonrpc.Conn) error {
		_, err := conn.SendRequest("session/prompt", nil)
		return err
	}
	notify := func(conn *jsonrpc.Conn) error {
		return conn.Notify("session/update", nil)
	}
	respond := func(conn *jsonrpc.Conn) error {
		return conn.Respond(jsonrpc.NumberID(1), map[string]any{"ok": true})
	}
	respondError := func(conn *jsonrpc.Conn) error {
		return conn.RespondError(jsonrpc.NumberID(1), -32001, "denied")
	}

	tests := []struct {
		name        string
		withVersion bool
		write       func(conn *jsonrpc.Conn) error
	}{
		{"request, WithVersionMember set", true, sendRequest},
		{"request, WithVersionMember unset", false, sendRequest},
		{"notification, WithVersionMember set", true, notify},
		{"notification, WithVersionMember unset", false, notify},
		{"response, WithVersionMember set", true, respond},
		{"response, WithVersionMember unset", false, respond},
		{"error response, WithVersionMember set", true, respondError},
		{"error response, WithVersionMember unset", false, respondError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var w captureWriter
			var opts []jsonrpc.Option
			if tt.withVersion {
				opts = append(opts, jsonrpc.WithVersionMember())
			}
			conn := jsonrpc.NewConn(&w, strings.NewReader(""), func(jsonrpc.Message) {}, opts...)

			if err := tt.write(conn); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}

			got := w.String()
			hasMember := strings.Contains(got, `"jsonrpc":"2.0"`)
			if hasMember != tt.withVersion {
				t.Errorf("%s wrote %q, want jsonrpc member present = %v", tt.name, got, tt.withVersion)
			}
		})
	}
}

// TestConn_MaxLineBytesOptionIsPerConnection checks that
// WithMaxLineBytes bounds only the connection it configures, and that
// a connection built with no such option keeps today's MaxLineBytes
// default: the same line that a custom, smaller bound rejects is
// accepted by a connection left at the default.
func TestConn_MaxLineBytesOptionIsPerConnection(t *testing.T) {
	t.Parallel()

	const smallBound = 64
	pad := strings.Repeat("x", 200)
	line := fmt.Sprintf(`{"method":"test/large","params":{"pad":%q}}`, pad)
	if len(line) <= smallBound {
		t.Fatalf("test line is %d bytes, want more than %d to exercise the small bound", len(line), smallBound)
	}

	t.Run("default bound accepts a line over a smaller custom bound", func(t *testing.T) {
		t.Parallel()

		tc := newTestConn(t)
		writeLine(t, tc.peerW, line)

		select {
		case msg := <-tc.handled:
			if msg.Kind != jsonrpc.KindNotification {
				t.Errorf("handler message Kind = %v, want %v", msg.Kind, jsonrpc.KindNotification)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a line within the default MaxLineBytes")
		}
	})

	t.Run("custom bound rejects the same line", func(t *testing.T) {
		t.Parallel()

		tc := newTestConn(t, jsonrpc.WithMaxLineBytes(smallBound))

		// The scanner gives up after the first smallBound-sized read,
		// well short of a full line, so the peer's write blocks
		// forever on the unconsumed remainder; write on its own
		// goroutine rather than through writeLine, which would block
		// this test goroutine too.
		go func() {
			_, _ = io.WriteString(tc.peerW, line+"\n")
		}()

		select {
		case msg := <-tc.handled:
			if msg.Kind != jsonrpc.KindStreamEnd {
				t.Errorf("handler message Kind = %v, want %v", msg.Kind, jsonrpc.KindStreamEnd)
			}
			if !errors.Is(msg.Err, bufio.ErrTooLong) {
				t.Errorf("handler message Err = %v, want error wrapping bufio.ErrTooLong", msg.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for KindStreamEnd on a line over the custom bound")
		}
	})
}

// TestConn_UnmatchedStringIDResponseReachesHandler checks that a
// response whose id is a JSON string reaches the handler as an
// unmatched KindResponse: the connection's pending-call table is
// keyed on the numeric ids it allocates itself, so a string id never
// matches an entry and the reader routes it to the handler instead of
// silently dropping it.
func TestConn_UnmatchedStringIDResponseReachesHandler(t *testing.T) {
	t.Parallel()

	tc := newTestConn(t)

	writeLine(t, tc.peerW, `{"id":"unmatched-string","result":{"ok":true}}`)

	select {
	case msg := <-tc.handled:
		if msg.Kind != jsonrpc.KindResponse {
			t.Errorf("handler message Kind = %v, want %v", msg.Kind, jsonrpc.KindResponse)
		}
		if _, isNumber := msg.ID.Number(); isNumber {
			t.Error("handler message ID.Number() reported a number, want a non-numeric id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the handler to receive the unmatched string-id response")
	}
}

// TestConn_RespondRejectsIDsThatNameNoRequest checks the guards on
// the two reply paths. An absent id names no request at all, so
// neither path can carry a reply for it. A null id is answerable on
// both, because a response carries the id its request carried, and
// because null is the form an error about an unreadable id must take.
func TestConn_RespondRejectsIDsThatNameNoRequest(t *testing.T) {
	t.Parallel()

	t.Run("Respond refuses an absent id", func(t *testing.T) {
		t.Parallel()

		tc := newTestConn(t)
		if err := tc.conn.Respond(jsonrpc.ID{}, map[string]any{"ok": true}); err == nil {
			t.Error("Respond(ID{}, ...) error = nil, want non-nil")
		}
	})

	t.Run("Respond answers a null id and writes it", func(t *testing.T) {
		t.Parallel()

		var w bytes.Buffer
		conn := jsonrpc.NewConn(&w, strings.NewReader(""), func(jsonrpc.Message) {})
		t.Cleanup(conn.Close)

		if err := conn.Respond(jsonrpc.NullID(), map[string]any{"ok": true}); err != nil {
			t.Fatalf("Respond(NullID(), ...) error = %v", err)
		}
		const want = `{"id":null,"result":{"ok":true}}` + "\n"
		if got := w.String(); got != want {
			t.Errorf("Respond(NullID(), ...) wrote %q, want %q", got, want)
		}
	})

	t.Run("RespondError refuses an absent id", func(t *testing.T) {
		t.Parallel()

		tc := newTestConn(t)
		if err := tc.conn.RespondError(jsonrpc.ID{}, -32700, "parse error"); err == nil {
			t.Error("RespondError(ID{}, ...) error = nil, want non-nil")
		}
	})

	t.Run("RespondError accepts a null id and writes it", func(t *testing.T) {
		t.Parallel()

		var w bytes.Buffer
		conn := jsonrpc.NewConn(&w, strings.NewReader(""), func(jsonrpc.Message) {})
		t.Cleanup(conn.Close)

		if err := conn.RespondError(jsonrpc.NullID(), -32700, "parse error"); err != nil {
			t.Fatalf("RespondError(NullID(), ...) error = %v", err)
		}
		const want = `{"id":null,"error":{"code":-32700,"message":"parse error"}}` + "\n"
		if got := w.String(); got != want {
			t.Errorf("RespondError(NullID(), ...) wrote %q, want %q", got, want)
		}
	})
}

// TestConn_ReadsALineLargerThanTheInitialBuffer checks that the
// reader grows toward its bound rather than being limited by the
// buffer it starts with. The connection starts small so that a large
// bound costs no memory until a line needs it, which is only safe if
// growth actually happens.
func TestConn_ReadsALineLargerThanTheInitialBuffer(t *testing.T) {
	t.Parallel()

	const bound = 1 << 20
	line := fmt.Sprintf(`{"method":"big","params":{"blob":%q}}`, strings.Repeat("x", 256<<10))
	if len(line) <= 64<<10 {
		t.Fatalf("line is %d bytes, which does not exceed the initial buffer", len(line))
	}

	tc := newTestConn(t, jsonrpc.WithMaxLineBytes(bound))
	// Written on its own goroutine because the pipe blocks until the
	// reader drains it, and without t, since a test helper that calls
	// Fatalf may only run on the test's own goroutine. The write error
	// is carried back rather than discarded, so a failed write reports
	// itself instead of arriving as a timeout.
	wrote := make(chan error, 1)
	go func() {
		_, err := io.WriteString(tc.peerW, line+"\n")
		wrote <- err
	}()

	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("write line: %v", err)
		}
		select {
		case msg := <-tc.handled:
			assertBigNotification(t, msg)
		case <-time.After(5 * time.Second):
			t.Fatal("no message reached the handler for a line above the initial buffer")
		}
	case msg := <-tc.handled:
		assertBigNotification(t, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("no message reached the handler for a line above the initial buffer")
	}
}

// assertBigNotification checks the message the oversized line carries.
func assertBigNotification(t *testing.T, msg jsonrpc.Message) {
	t.Helper()

	if msg.Kind != jsonrpc.KindNotification {
		t.Fatalf("Kind = %v, want %v (Err=%v)", msg.Kind, jsonrpc.KindNotification, msg.Err)
	}
	if msg.Method != "big" {
		t.Errorf("Method = %q, want %q", msg.Method, "big")
	}
}

// TestNewConn_SkipsNilOptions checks that a nil entry in the option
// slice is ignored rather than panicking inside the constructor. A
// caller assembling options conditionally can produce one, and the
// panic would surface here instead of at the call site that built it.
func TestNewConn_SkipsNilOptions(t *testing.T) {
	t.Parallel()

	var w bytes.Buffer
	conn := jsonrpc.NewConn(&w, strings.NewReader(""), func(jsonrpc.Message) {}, nil, jsonrpc.WithVersionMember(), nil)
	t.Cleanup(conn.Close)

	if err := conn.Notify("ping", nil); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if got := w.String(); !strings.Contains(got, `"jsonrpc":"2.0"`) {
		t.Errorf("Notify() wrote %q, want the version member the non-nil option asked for", got)
	}
}

// entrySignalingWriter wraps an io.Writer and closes done the instant
// Write is entered, before delegating to the wrapped writer. Unlike a
// writer that signals once Write returns, this lets a test observe
// that a write has been parked inside a call that never returns on
// its own.
type entrySignalingWriter struct {
	w    io.Writer
	once sync.Once
	done chan struct{}
}

func newEntrySignalingWriter(w io.Writer) *entrySignalingWriter {
	return &entrySignalingWriter{w: w, done: make(chan struct{})}
}

func (s *entrySignalingWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.done) })
	return s.w.Write(p)
}

// TestConn_CloseReturnsWithWriteParked checks that Close returns while
// a write is parked on a writer that never accepts the bytes, rather
// than waiting for the write to finish.
func TestConn_CloseReturnsWithWriteParked(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	sig := newEntrySignalingWriter(pw)
	conn := jsonrpc.NewConn(sig, strings.NewReader(""), func(jsonrpc.Message) {})

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- conn.Notify("parked/notify", nil)
	}()

	// Registered before the assertions below, so a t.Fatal on the
	// regression path still releases the parked write.
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
		select {
		case <-writeErr:
		case <-time.After(2 * time.Second):
		}
	})

	select {
	case <-sig.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the write to enter the wrapped writer")
	}

	closeReturned := make(chan struct{})
	go func() {
		conn.Close()
		close(closeReturned)
	}()

	select {
	case <-closeReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not return while a write was parked on a writer that does not accept bytes")
	}
}

// TestConn_WriteAfterCloseReportsErrClosedWithoutTouchingWriter checks
// that a write issued after Close reports an error wrapping ErrClosed
// and never reaches the underlying writer.
func TestConn_WriteAfterCloseReportsErrClosedWithoutTouchingWriter(t *testing.T) {
	t.Parallel()

	var w captureWriter
	conn := jsonrpc.NewConn(&w, strings.NewReader(""), func(jsonrpc.Message) {})
	conn.Close()

	if err := conn.Notify("after/close", nil); !errors.Is(err, jsonrpc.ErrClosed) {
		t.Errorf("Notify() after Close() error = %v, want error wrapping jsonrpc.ErrClosed", err)
	}
	if got := w.String(); got != "" {
		t.Errorf("Notify() after Close() wrote %q to the writer, want none", got)
	}
}
