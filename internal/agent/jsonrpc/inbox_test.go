package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestInbox_PutNeverWaitsAndOrderHolds asserts that a large volume of
// puts with no taker return promptly, and concurrent producers each
// keep their own put order once one consumer drains the inbox.
func TestInbox_PutNeverWaitsAndOrderHolds(t *testing.T) {
	t.Parallel()

	t.Run("100000 puts with no taker return within 5s", func(t *testing.T) {
		t.Parallel()

		inbox := NewInbox[int]()
		done := make(chan struct{})
		go func() {
			for i := range 100000 {
				inbox.Put(i)
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("100000 Put calls with no taker did not return within 5s")
		}
	})

	t.Run("four producers put interleaved sequences and one consumer preserves each producer's order", func(t *testing.T) {
		t.Parallel()

		const producers = 4
		const perProducer = 2000

		type produced struct {
			producer int
			seq      int
		}

		inbox := NewInbox[produced]()
		var wg sync.WaitGroup
		for p := range producers {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				for seq := range perProducer {
					inbox.Put(produced{producer: p, seq: seq})
				}
			}(p)
		}

		taken := make([]produced, 0, producers*perProducer)
		drainDone := make(chan struct{})
		go func() {
			defer close(drainDone)
			for len(taken) < producers*perProducer {
				<-inbox.Ready()
				item, ok := inbox.Take()
				if !ok {
					continue
				}
				taken = append(taken, item)
			}
		}()

		wg.Wait()
		select {
		case <-drainDone:
		case <-time.After(5 * time.Second):
			t.Fatal("consumer did not take every produced item within 5s")
		}

		lastSeq := make([]int, producers)
		for i := range lastSeq {
			lastSeq[i] = -1
		}
		for _, item := range taken {
			if item.seq != lastSeq[item.producer]+1 {
				t.Fatalf("producer %d: took sequence %d, want %d (producer order violated)", item.producer, item.seq, lastSeq[item.producer]+1)
			}
			lastSeq[item.producer] = item.seq
		}
	})
}

// TestInbox_ReadyReflectsQueuedOrClosed asserts that across randomized
// put, take, and close interleavings, Ready holds a value exactly when
// an item is queued or the inbox is closed, and a receive from Ready
// followed by one Take yields either an item or ok == false with the
// inbox closed and empty.
func TestInbox_ReadyReflectsQueuedOrClosed(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(1))
	inbox := NewInbox[int]()
	var model []int
	closed := false
	next := 0

	assertReady := func() {
		t.Helper()
		want := len(model) > 0 || closed
		got := len(inbox.ready) == 1
		if got != want {
			t.Fatalf("Ready() signaled = %v, want %v (queued=%d closed=%v)", got, want, len(model), closed)
		}
	}
	assertReady()

	for range 5000 {
		switch op := rng.Intn(3); {
		case op == 0 && !closed:
			inbox.Put(next)
			model = append(model, next)
			next++
			assertReady()

		case op == 1:
			inbox.Close()
			closed = true
			assertReady()

		default:
			if len(model) == 0 && !closed {
				// Nothing queued and the inbox is still open: Ready
				// would block forever, exactly the state assertReady
				// already confirms is unsignaled. There is nothing to
				// take.
				assertReady()
				continue
			}
			<-inbox.Ready()
			v, ok := inbox.Take()
			if len(model) == 0 {
				if ok {
					t.Fatalf("Take() = (%d, true), want ok = false with nothing queued", v)
				}
				if !closed {
					t.Fatal("Take() ok = false while the inbox is still open, want Ready() unreachable in that state")
				}
			} else {
				if !ok {
					t.Fatalf("Take() ok = false, want true with %d item(s) modeled queued", len(model))
				}
				if v != model[0] {
					t.Fatalf("Take() = %d, want %d (FIFO order)", v, model[0])
				}
				model = model[1:]
			}
			assertReady()
		}
	}
}

// TestInbox_CloseTakesQueuedThenRefusesLater asserts that repeated Close
// calls do not panic, items put before Close are taken in order, and a
// value put after Close is never taken.
func TestInbox_CloseTakesQueuedThenRefusesLater(t *testing.T) {
	t.Parallel()

	inbox := NewInbox[int]()
	inbox.Put(1)
	inbox.Put(2)
	inbox.Close()
	inbox.Close()
	inbox.Put(3)

	var got []int
	for {
		<-inbox.Ready()
		v, ok := inbox.Take()
		if !ok {
			break
		}
		got = append(got, v)
	}
	if want := []int{1, 2}; !slices.Equal(got, want) {
		t.Errorf("Take() sequence = %v, want %v", got, want)
	}
}

// TestInbox_TakeReleasesStorage asserts that after Take, the inbox
// references no taken item, and retained storage stays low both in
// steady state and after a burst fully drains.
func TestInbox_TakeReleasesStorage(t *testing.T) {
	t.Parallel()

	t.Run("one million put/take cycles with at most one queued keep retained storage low", func(t *testing.T) {
		t.Parallel()

		inbox := NewInbox[int]()
		for i := range 1_000_000 {
			inbox.Put(i)
			<-inbox.Ready()
			v, ok := inbox.Take()
			if !ok || v != i {
				t.Fatalf("Take() = (%d, %v), want (%d, true)", v, ok, i)
			}
			if cap(inbox.items) > 64 {
				t.Fatalf("cap(items) = %d after cycle %d, want at most 64", cap(inbox.items), i)
			}
		}
	})

	t.Run("a 4096 burst followed by 4096 takes drops retained storage", func(t *testing.T) {
		t.Parallel()

		inbox := NewInbox[int]()
		for i := range 4096 {
			inbox.Put(i)
		}
		for range 4096 {
			<-inbox.Ready()
			if _, ok := inbox.Take(); !ok {
				t.Fatal("Take() ok = false, want true while items remain")
			}
		}
		if cap(inbox.items) > 64 {
			t.Errorf("cap(items) = %d after draining the burst, want at most 64", cap(inbox.items))
		}
	})
}

// TestDeliver_PanicsOnNilInboxOrWrap checks that Deliver panics inside
// the constructor when either argument is nil, rather than surfacing as
// a nil-pointer dereference the first time the reader delivers a
// message through the sink it built.
func TestDeliver_PanicsOnNilInboxOrWrap(t *testing.T) {
	t.Parallel()

	t.Run("nil inbox", func(t *testing.T) {
		t.Parallel()

		defer func() {
			if recover() == nil {
				t.Fatal("Deliver(inbox=nil, ...) did not panic, want panic")
			}
		}()
		Deliver[Message](nil, func(m Message) Message { return m })
	})

	t.Run("nil wrap", func(t *testing.T) {
		t.Parallel()

		defer func() {
			if recover() == nil {
				t.Fatal("Deliver(..., wrap=nil) did not panic, want panic")
			}
		}()
		Deliver(NewInbox[Message](), nil)
	})
}

// takeWithin waits up to timeout for inbox to hold an item or be
// closed, then takes one. It fails t when nothing arrives in time, so
// a delivery that never happens fails the test instead of blocking it
// until the package timeout.
func takeWithin[T any](t *testing.T, inbox *Inbox[T], timeout time.Duration) (T, bool) {
	t.Helper()
	select {
	case <-inbox.Ready():
	case <-time.After(timeout):
		t.Fatalf("inbox held nothing to take within %v", timeout)
	}
	return inbox.Take()
}

// extractRequestID reads id from a JSON-RPC request line without
// decoding the rest.
func extractRequestID(t *testing.T, line []byte) int64 {
	t.Helper()
	var wire struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(line, &wire); err != nil {
		t.Fatalf("unmarshal request line %s: %v", line, err)
	}
	return wire.ID
}

// TestInbox_ReaderNeverWaits asserts that a Conn running on io.Pipe with
// an inbox nobody takes from lets each pipe write return once the
// reader has read it, so a burst delivered while a Call is outstanding
// does not stop that Call's own response from arriving. The negative
// control shows the transport test can detect a wrap that blocks.
func TestInbox_ReaderNeverWaits(t *testing.T) {
	t.Parallel()

	t.Run("a burst while a call is outstanding does not delay the call", func(t *testing.T) {
		t.Parallel()

		reqR, reqW := io.Pipe()
		peerR, peerW := io.Pipe()
		t.Cleanup(func() {
			_ = reqR.Close()
			_ = reqW.Close()
			_ = peerR.Close()
			_ = peerW.Close()
		})

		inbox := NewInbox[Message]()
		conn := NewConn(reqW, peerR, Deliver(inbox, func(m Message) Message { return m }))
		t.Cleanup(conn.Close)

		type callOutcome struct {
			resp Response
			err  error
		}
		callDone := make(chan callOutcome, 1)
		go func() {
			resp, err := conn.Call(context.Background(), "call/method", nil)
			callDone <- callOutcome{resp: resp, err: err}
		}()

		line, err := bufio.NewReader(reqR).ReadBytes('\n')
		if err != nil {
			t.Fatalf("read request line: %v", err)
		}
		id := extractRequestID(t, line)

		const burst = 4096
		go func() {
			for i := range burst {
				fmt.Fprintf(peerW, "{\"method\":\"filler\",\"params\":{\"i\":%d}}\n", i) //nolint:errcheck,gosec // best-effort fixture write; a failed write surfaces through the assertion that reads its effect
			}
			fmt.Fprintf(peerW, "{\"id\":%d,\"result\":{}}\n", id) //nolint:errcheck,gosec // best-effort fixture write; a failed write surfaces through the assertion that reads its effect
		}()

		select {
		case got := <-callDone:
			if got.err != nil {
				t.Fatalf("Call() error = %v, want nil", got.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Call() did not return within 2s despite the reader never waiting on the inbox")
		}

		for i := range burst {
			<-inbox.Ready()
			msg, ok := inbox.Take()
			if !ok {
				t.Fatalf("Take() ok = false at filler %d, want true", i)
			}
			if msg.Method != "filler" {
				t.Fatalf("delivered message %d Method = %q, want %q", i, msg.Method, "filler")
			}
		}
	})

	t.Run("negative control: a wrap blocked from its 17th call leaves the call unreturned", func(t *testing.T) {
		t.Parallel()

		reqR, reqW := io.Pipe()
		peerR, peerW := io.Pipe()
		t.Cleanup(func() {
			_ = reqR.Close()
			_ = reqW.Close()
			_ = peerR.Close()
			_ = peerW.Close()
		})

		var calls atomic.Int64
		release := make(chan struct{})
		wrap := func(m Message) Message {
			if calls.Add(1) >= 17 {
				<-release
			}
			return m
		}
		inbox := NewInbox[Message]()
		conn := NewConn(reqW, peerR, Deliver(inbox, wrap))
		t.Cleanup(conn.Close)

		callDone := make(chan struct{}, 1)
		go func() {
			_, _ = conn.Call(context.Background(), "call/method", nil)
			close(callDone)
		}()

		line, err := bufio.NewReader(reqR).ReadBytes('\n')
		if err != nil {
			t.Fatalf("read request line: %v", err)
		}
		id := extractRequestID(t, line)

		go func() {
			for i := range 32 {
				fmt.Fprintf(peerW, "{\"method\":\"filler\",\"params\":{\"i\":%d}}\n", i) //nolint:errcheck,gosec // best-effort fixture write; a failed write surfaces through the assertion that reads its effect
			}
			fmt.Fprintf(peerW, "{\"id\":%d,\"result\":{}}\n", id) //nolint:errcheck,gosec // best-effort fixture write; a failed write surfaces through the assertion that reads its effect
		}()

		select {
		case <-callDone:
			close(release)
			t.Fatal("Call() returned within 300ms despite a wrap blocked on its 17th call, want it still unreturned")
		case <-time.After(300 * time.Millisecond):
		}
		close(release)

		select {
		case <-callDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Call() did not return after releasing the blocked wrap")
		}
	})
}

// TestInbox_StreamEndDeliveredLast asserts that on a read error,
// KindStreamEnd is taken after every earlier message, and Done closes
// only once it was put.
func TestInbox_StreamEndDeliveredLast(t *testing.T) {
	t.Parallel()

	reqR, reqW := io.Pipe()
	peerR, peerW := io.Pipe()
	t.Cleanup(func() {
		_ = reqR.Close()
		_ = reqW.Close()
		_ = peerR.Close()
	})

	inbox := NewInbox[Message]()
	conn := NewConn(reqW, peerR, Deliver(inbox, func(m Message) Message { return m }))
	t.Cleanup(conn.Close)

	go func() {
		_, _ = io.WriteString(peerW, "{\"method\":\"one\"}\n{\"method\":\"two\"}\n")
		_ = peerW.CloseWithError(errors.New("boom"))
	}()

	msg1, ok := takeWithin(t, inbox, 2*time.Second)
	if !ok || msg1.Method != "one" {
		t.Fatalf("first message = %+v, ok=%v, want method %q", msg1, ok, "one")
	}
	msg2, ok := takeWithin(t, inbox, 2*time.Second)
	if !ok || msg2.Method != "two" {
		t.Fatalf("second message = %+v, ok=%v, want method %q", msg2, ok, "two")
	}
	msg3, ok := takeWithin(t, inbox, 2*time.Second)
	if !ok || msg3.Kind != KindStreamEnd {
		t.Fatalf("third message = %+v, ok=%v, want Kind %v", msg3, ok, KindStreamEnd)
	}

	select {
	case <-conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close after KindStreamEnd was put")
	}
}
