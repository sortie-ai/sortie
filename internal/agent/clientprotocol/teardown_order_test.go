package clientprotocol

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

type teardownOrderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *teardownOrderRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *teardownOrderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// orderedStdinWriter wraps the session's stdinCloser and records the permission
// reply write completing (deliberately delayed, so it is still in flight when
// teardown begins), the session/close request being written, and stdin closing.
type orderedStdinWriter struct {
	w                 *io.PipeWriter
	rec               *teardownOrderRecorder
	replyWriteStarted chan struct{}
	once              sync.Once
}

func (o *orderedStdinWriter) Write(p []byte) (int, error) {
	line := string(p)
	switch {
	case strings.Contains(line, `"outcome"`):
		o.once.Do(func() { close(o.replyWriteStarted) })
		// A reply the runtime is slow to accept: without answer_open's flush,
		// teardown's later steps could run while this write is still in flight.
		time.Sleep(100 * time.Millisecond)
		o.rec.record("permission_reply")
	case strings.Contains(line, `"method":"session/close"`):
		o.rec.record("session_close_request")
	}
	return o.w.Write(p)
}

func (o *orderedStdinWriter) Close() error {
	o.rec.record("stdin_closed")
	return o.w.Close()
}

func TestTeardown_AnswerOpenWrittenBeforeCloseSessionAndStdinClose(t *testing.T) {
	t.Parallel()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, outPr) }()

	rec := &teardownOrderRecorder{}
	writer := &orderedStdinWriter{w: outPw, rec: rec, replyWriteStarted: make(chan struct{})}

	state := &sessionState{
		caps:           newCapabilityRecord(false),
		stopCh:         make(chan struct{}),
		pumpDone:       make(chan struct{}),
		logger:         discardLogger(),
		origins:        &sessionOrigins{},
		stdinCloser:    writer,
		closeSessionID: "sess-order",
		agentConfig:    domain.AgentConfig{ReadTimeoutMS: 60000},
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(writer, inPr, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	go runPump(state)
	markSessionKnown(state)
	// Closing inPw first ends the connection's reader (conn.Done()), which is
	// the only thing that lets runPump's select start observing state.stopCh;
	// stopping the pump before that would deadlock waiting on pumpDone.
	t.Cleanup(func() {
		_ = inPw.Close()
		state.stopOnce.Do(func() { close(state.stopCh) })
		<-state.pumpDone
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
	})

	sendLine(t, inPw, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"reject-id"}],"toolCall":{"toolCallId":"tc-1","title":"do a thing"}}}`)

	select {
	case <-writer.replyWriteStarted:
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for the pump to begin writing the permission reply")
	}

	const grace = 1 * time.Second
	graceCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	awaitAnswerOpen(graceCtx, grace)(state)

	if got := rec.snapshot(); len(got) != 1 || got[0] != "permission_reply" {
		t.Fatalf("recorded events right after answer_open returned = %v, want exactly [permission_reply] (answer_open must wait for the in-flight write, not just enqueue the request to answer it)", got)
	}

	runTeardown(state, []teardownStep{
		{name: "close_session", run: closeSession(context.Background(), graceCtx, grace)},
		{name: "close_stdin", run: closeStdin},
	})

	deadline := time.Now().Add(awaitTimeout)
	for len(rec.snapshot()) < 3 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	got := rec.snapshot()
	want := []string{"permission_reply", "session_close_request", "stdin_closed"}
	if len(got) != len(want) {
		t.Fatalf("recorded events = %v, want %v", got, want)
	}
	for i, ev := range want {
		if got[i] != ev {
			t.Errorf("event %d = %q, want %q (full order: %v)", i, got[i], ev, got)
		}
	}
}

func TestLogCloseSessionOutcome(t *testing.T) {
	t.Parallel()

	t.Run("bound elapsed logs Warn with bound and outcome", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		state := &sessionState{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}

		logCloseSessionOutcome(state, context.Background(), 500*time.Millisecond, closeCallOutcome{err: context.DeadlineExceeded})

		output := buf.String()
		if !strings.Contains(output, "level=WARN") {
			t.Errorf("output missing level=WARN: %s", output)
		}
		if !strings.Contains(output, "session/close did not complete before the wait ended") {
			t.Errorf("output missing the WARN message: %s", output)
		}
		if !strings.Contains(output, "bound=500ms") {
			t.Errorf("output missing bound=500ms: %s", output)
		}
		if !strings.Contains(output, `outcome="bound elapsed"`) {
			t.Errorf(`output missing outcome="bound elapsed": %s`, output)
		}
	})

	t.Run("caller deadline logs Warn with outcome caller deadline", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		state := &sessionState{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}

		callerCtx, cancel := context.WithCancel(context.Background())
		cancel()

		logCloseSessionOutcome(state, callerCtx, 500*time.Millisecond, closeCallOutcome{err: context.Canceled})

		output := buf.String()
		if !strings.Contains(output, `outcome="caller deadline"`) {
			t.Errorf(`output missing outcome="caller deadline": %s`, output)
		}
	})

	t.Run("any other failure logs Debug with no fields, and no Warn", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		state := &sessionState{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}

		logCloseSessionOutcome(state, context.Background(), 500*time.Millisecond, closeCallOutcome{err: jsonrpc.ErrClosed})

		output := buf.String()
		if strings.Contains(output, "level=WARN") {
			t.Errorf("output carries an unexpected WARN record: %s", output)
		}
		if !strings.Contains(output, "level=DEBUG") {
			t.Errorf("output missing a DEBUG record: %s", output)
		}
		if strings.Contains(output, "bound=") {
			t.Errorf("Debug record carries fields, want none: %s", output)
		}
	})
}

func TestAwaitAnswerOpen_PumpNeverAnswersReturnsWithinBound(t *testing.T) {
	t.Parallel()

	state := &sessionState{inbox: jsonrpc.NewInbox[pumpItem]()}

	const grace = 400 * time.Millisecond
	graceCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		awaitAnswerOpen(graceCtx, grace)(state)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(awaitTimeout):
		t.Fatal("awaitAnswerOpen() did not return within the bound against a pump that never answers")
	}
	if elapsed := time.Since(start); elapsed >= closeCallBound(grace)+1*time.Second {
		t.Errorf("awaitAnswerOpen() took %v, want close to its own %v bound", elapsed, closeCallBound(grace))
	}

	var ranNextStep bool
	runTeardown(state, []teardownStep{
		{name: "next", run: func(*sessionState) { ranNextStep = true }},
	})
	if !ranNextStep {
		t.Error("a later teardown step did not run after awaitAnswerOpen returned, want teardown to continue")
	}
}

// The pipes StartOutputRelease closes on give-up are never wired to this
// connection, so nothing but the abandonment-armed stop arm in runPump's select
// can end the pump. Removing that arm leaves the pump waiting on a reader
// nothing here unparks, and this test times out.
func TestStopSessionReturnsBoundedWithReaderGenuinelyParked(t *testing.T) {
	t.Parallel()

	const grace = 30 * time.Millisecond
	reaped := make(chan struct{})
	state, _, _ := newTestSessionWithRelease(t, grace, reaped)

	close(reaped)
	select {
	case <-state.release.Abandoned():
	case <-time.After(awaitTimeout):
		t.Fatal("the release never abandoned within the test's wait bound")
	}

	select {
	case <-state.conn.Done():
		t.Fatal("the connection's reader ended on its own, want it to stay parked for the rest of this test")
	default:
	}

	done := make(chan error, 1)
	go func() {
		done <- stopSession(context.Background(), fakeSession(state))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stopSession() error = %v, want nil", err)
		}
	case <-time.After(awaitTimeout):
		t.Fatal("stopSession() did not return within the bound though the release had already abandoned, want the abandonment-armed stop arm to end the pump")
	}
}
