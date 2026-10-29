// Package jsonrpc implements newline-delimited JSON-RPC framing over a
// byte stream. A [Conn] writes requests, notifications, and
// responses, correlates a response to the call awaiting it by request
// id, and delivers every other message, including one that arrives
// when no call is in flight, into a caller-supplied [Sink]. Every
// outgoing line is queued and written by the connection's own writer
// goroutine, so no send waits on the peer reading it. Start from
// [NewConn]. [WriteContentLengthFrame] and [ReadContentLengthFrame]
// frame a message with a Content-Length header instead, for a peer that
// speaks that framing, and need no [Conn].
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxLineBytes bounds one message. It is the cap this project chose,
// not a limit any peer documents.
const MaxLineBytes = 1 << 20

// initialLineBytes is the buffer the reader starts with before the
// scanner grows it toward the connection's bound.
const initialLineBytes = 64 << 10

// MethodNotFoundCode is the JSON-RPC 2.0 reserved error code for a
// method the receiver does not implement or recognize.
const MethodNotFoundCode = -32601

// MethodNotFoundMessage is the message this package pairs with
// [MethodNotFoundCode].
const MethodNotFoundMessage = "method not found"

// ErrClosed is returned, wrapped, by a write or a call issued after
// [Conn.Close].
var ErrClosed = errors.New("connection closed")

// Conn is one JSON-RPC session over a newline-delimited byte stream.
// A Conn is safe for concurrent use.
type Conn struct {
	w    io.Writer
	r    io.Reader
	sink Sink

	versionMember bool
	maxLineBytes  int

	// outbox is the ordered, unbounded queue of encoded lines and flush
	// markers the writer goroutine drains. Every write method appends
	// to it and returns without waiting for the line to reach w.
	outbox *Inbox[outboxItem]

	// writeFailedCh closes once a write to w has failed. The failure is
	// terminal for the connection: every call already pending fails,
	// and every later Notify, Respond, RespondError, SendRequest, and
	// Call fails at the enqueue check instead of attempting a write.
	writeFailedCh chan struct{}
	writeErr      error // set before writeFailedCh closes; read only after

	// callMu guards nextID and pending together, so a request id is
	// never allocated without its waiter being registered under the
	// same lock.
	callMu  sync.Mutex
	nextID  int64
	pending map[int64]chan callResult

	closeOnce sync.Once
	closed    chan struct{}

	done    chan struct{}
	termErr error
}

// outboxItem is one entry in a [Conn]'s write queue: either an
// already-encoded line to write, or a flush marker whose done channel
// closes once every line queued ahead of it has been written.
type outboxItem struct {
	line []byte
	done chan struct{}
}

// callResult is what a pending call receives: either the correlated
// [Response], or the reason the call will never receive one.
type callResult struct {
	resp Response
	err  error
}

// connConfig holds the state an [Option] sets before [NewConn] starts
// the reader goroutine.
type connConfig struct {
	versionMember bool
	maxLineBytes  int
}

// Option configures a [Conn]. Apply one or more to [NewConn].
type Option func(*connConfig)

// WithVersionMember makes the connection write "jsonrpc":"2.0" on
// every outgoing request, notification, response, and error response.
// Without this option, no such member is written.
func WithVersionMember() Option {
	return func(cfg *connConfig) {
		cfg.versionMember = true
	}
}

// WithMaxLineBytes sets the maximum length of one line the connection
// will read. Zero or negative values are ignored, leaving the default
// of [MaxLineBytes] in effect.
func WithMaxLineBytes(n int) Option {
	return func(cfg *connConfig) {
		if n > 0 {
			cfg.maxLineBytes = n
		}
	}
}

// NewConn starts a connection that writes to w, reads newline-
// delimited JSON-RPC messages from r, and delivers every message that
// is not a correlated response into sink.
//
// NewConn panics when sink is nil. It starts a reader goroutine and a
// writer goroutine before returning, and it does not close w or r:
// closing them is the caller's responsibility. Closing r is how the
// caller ends a read parked on the stream. Closing w is what finally
// ends a write the writer goroutine is in the middle of; until then,
// that write keeps the writer goroutine busy, but every other queued
// line still waits its turn rather than being attempted out of order.
// With no options passed, behavior is byte-identical to a connection
// with none of this package's options applied.
func NewConn(w io.Writer, r io.Reader, sink Sink, opts ...Option) *Conn {
	if sink == nil {
		panic("jsonrpc: sink must be non-nil")
	}
	cfg := connConfig{maxLineBytes: MaxLineBytes}
	for _, opt := range opts {
		// A caller assembling options conditionally can hand over a nil
		// entry, and dereferencing it here would panic inside a
		// constructor rather than at the call site that built it.
		if opt == nil {
			continue
		}
		opt(&cfg)
	}
	c := &Conn{
		w:             w,
		r:             r,
		sink:          sink,
		versionMember: cfg.versionMember,
		maxLineBytes:  cfg.maxLineBytes,
		outbox:        NewInbox[outboxItem](),
		writeFailedCh: make(chan struct{}),
		pending:       make(map[int64]chan callResult),
		closed:        make(chan struct{}),
		done:          make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c
}

// wireRequest is the JSON shape of a request or a notification. A
// notification omits id via the zero value and the omitempty tag.
type wireRequest struct {
	JSONRPC string `json:"jsonrpc,omitempty"`
	Method  string `json:"method"`
	ID      int64  `json:"id,omitempty"`
	Params  any    `json:"params,omitempty"`
}

// enqueue appends line to the outbox for the writer goroutine to send.
// It reports ErrClosed after [Conn.Close], or the write-failure error
// after a write has failed, whether that is seen before the append or
// through the outbox refusing it.
func (c *Conn) enqueue(line []byte) error {
	if err := c.sendErr(); err != nil {
		return err
	}
	if !c.outbox.Put(outboxItem{line: line}) {
		return c.sendErr()
	}
	return nil
}

// sendErr reports why the connection accepts no further lines:
// ErrClosed after Close, the write-failure error after a failed write,
// or nil while it still accepts them. Close and failWrite each close
// their channel before the outbox, so an append the outbox refuses
// always finds a non-nil error here.
func (c *Conn) sendErr() error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	select {
	case <-c.writeFailedCh:
		return fmt.Errorf("write failed: %w", c.writeErr)
	default:
		return nil
	}
}

func (c *Conn) marshalAndWriteRequest(method string, id int64, params any) error {
	wire := wireRequest{Method: method, ID: id, Params: params}
	if c.versionMember {
		wire.JSONRPC = "2.0"
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("marshal request %s: %w", method, err)
	}
	if err := c.enqueue(append(data, '\n')); err != nil {
		return fmt.Errorf("write request %s: %w", method, err)
	}
	return nil
}

// Notify writes a notification, a message with no id, for method.
func (c *Conn) Notify(method string, params any) error {
	wire := wireRequest{Method: method, Params: params}
	if c.versionMember {
		wire.JSONRPC = "2.0"
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("marshal notification %s: %w", method, err)
	}
	if err := c.enqueue(append(data, '\n')); err != nil {
		return fmt.Errorf("write notification %s: %w", method, err)
	}
	return nil
}

// Respond writes a successful response to the request carrying id.
//
// It refuses only an absent id, which names no request. A null id is
// answered like any other, because a response must carry the id its
// request carried, and refusing it would strand a peer that numbered
// its request null on an answer that never arrives.
func (c *Conn) Respond(id ID, result any) error {
	if !id.Present() {
		return fmt.Errorf("respond: %s is not the id of a request to answer", id)
	}
	resp := struct {
		JSONRPC string `json:"jsonrpc,omitempty"`
		ID      ID     `json:"id"`
		Result  any    `json:"result"`
	}{ID: id, Result: result}
	if c.versionMember {
		resp.JSONRPC = "2.0"
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response id=%s: %w", id, err)
	}
	if err := c.enqueue(append(data, '\n')); err != nil {
		return fmt.Errorf("write response id=%s: %w", id, err)
	}
	return nil
}

// RespondError writes an error response to the request carrying id.
//
// It refuses an absent id. A null id is allowed, both because a
// response carries the id its request carried and because null is the
// form the specification requires for an error reporting that the
// request's own id could not be read.
func (c *Conn) RespondError(id ID, code int, message string) error {
	if !id.Present() {
		return fmt.Errorf("respond error: %s is not the id of a request to answer", id)
	}
	resp := struct {
		JSONRPC string `json:"jsonrpc,omitempty"`
		ID      ID     `json:"id"`
		Error   Error  `json:"error"`
	}{ID: id, Error: Error{Code: code, Message: message}}
	if c.versionMember {
		resp.JSONRPC = "2.0"
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal error response id=%s: %w", id, err)
	}
	if err := c.enqueue(append(data, '\n')); err != nil {
		return fmt.Errorf("write error response id=%s: %w", id, err)
	}
	return nil
}

// allocateID returns the next request id, monotonic from 1 and never
// reused within a connection. Call and SendRequest draw from the same
// counter.
func (c *Conn) allocateID() int64 {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	c.nextID++
	return c.nextID
}

// addPending registers a waiter for id, buffered so the reader never
// blocks handing a response to a waiter that has already given up.
func (c *Conn) addPending(id int64) chan callResult {
	ch := make(chan callResult, 1)
	c.callMu.Lock()
	c.pending[id] = ch
	c.callMu.Unlock()
	return ch
}

// takePending removes and returns the waiter for id, tolerating an id
// with no registered waiter.
func (c *Conn) takePending(id int64) (chan callResult, bool) {
	c.callMu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.callMu.Unlock()
	return ch, ok
}

// drainPending removes every pending waiter at once and returns them,
// so the caller can resolve each without holding callMu.
func (c *Conn) drainPending() map[int64]chan callResult {
	c.callMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan callResult)
	c.callMu.Unlock()
	return pending
}

// removePending drops the waiter for id, tolerating an id the reader
// has already taken.
func (c *Conn) removePending(id int64) {
	c.callMu.Lock()
	delete(c.pending, id)
	c.callMu.Unlock()
}

// SendRequest writes a request and returns its id without registering
// a waiter. One caller uses it for a request whose response it
// deliberately ignores; the response later reaches the sink as an
// unmatched KindResponse message. A second caller uses it so the
// response arrives through the sink in wire order, behind the
// notifications that preceded it, rather than being delivered ahead
// of them the way [Conn.Call] would deliver it; that caller compares
// the returned id against a later [Message.ID] with [ID.Equal].
func (c *Conn) SendRequest(method string, params any) (ID, error) {
	id := c.allocateID()
	if err := c.marshalAndWriteRequest(method, id, params); err != nil {
		return ID{}, err
	}
	return NumberID(id), nil
}

// Call writes a request for method and enqueues it without waiting for
// it to reach the peer, then waits for its response, for ctx to end,
// for [Conn.Close], for the reader to terminate, or for a write on
// this connection to fail, whichever happens first. It returns
// (Response, nil) when the response arrives, including when the
// response carries a JSON-RPC error: an error response is an answer,
// not a transport failure.
func (c *Conn) Call(ctx context.Context, method string, params any) (Response, error) {
	id := c.allocateID()
	ch := c.addPending(id)

	if err := c.marshalAndWriteRequest(method, id, params); err != nil {
		c.removePending(id)
		return Response{}, err
	}

	select {
	case result := <-ch:
		return result.resp, result.err
	case <-ctx.Done():
		c.removePending(id)
		return Response{}, ctx.Err()
	case <-c.closed:
		c.removePending(id)
		return Response{}, &closedCallError{id: id}
	case <-c.done:
		c.removePending(id)
		return Response{}, c.termErrorFor(id)
	case <-c.writeFailedCh:
		c.removePending(id)
		return Response{}, &writeFailedCallError{id: id, err: c.writeErr}
	}
}

// Flush waits until every line enqueued on this connection before this
// call returns has been written to the underlying writer, or until ctx
// ends, the connection closes, or a write fails, whichever happens
// first. A caller building on this synchronization point can be sure
// that once Flush returns nil, every reply it enqueued earlier has
// already reached the peer.
func (c *Conn) Flush(ctx context.Context) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	select {
	case <-c.writeFailedCh:
		return fmt.Errorf("flush: write failed: %w", c.writeErr)
	default:
	}

	marker := make(chan struct{})
	c.outbox.Put(outboxItem{done: marker})

	select {
	case <-marker:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.writeFailedCh:
		return fmt.Errorf("flush: write failed: %w", c.writeErr)
	case <-c.closed:
		return ErrClosed
	}
}

// Close stops the connection. It fails every call in flight with an
// error wrapping ErrClosed and makes every later write return an
// error wrapping ErrClosed without attempting it. Close is idempotent
// and safe to call from any goroutine, and it does not close the
// underlying writer or reader.
//
// Close never waits for the peer. Lines already enqueued before Close
// runs are still written afterward, in order, by the writer goroutine;
// Close neither waits for them nor interrupts them, and it does not
// stop the writer goroutine from draining them.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.outbox.Close()
	})

	for id, ch := range c.drainPending() {
		ch <- callResult{err: &closedCallError{id: id}}
	}
}

// Done returns a channel closed once the reader goroutine has
// exited. Nothing is delivered into the sink after Done closes.
func (c *Conn) Done() <-chan struct{} { return c.done }

// WriteFailed returns a channel that closes once a write on this
// connection has failed. The failure is terminal for the connection:
// it fails every call already pending, and every later Notify,
// Respond, RespondError, SendRequest, and Call fails at the enqueue
// check instead of attempting a write. A caller with something in
// flight that does not itself wait for a response, such as a
// fire-and-forget [Conn.Notify] or [Conn.SendRequest], can watch this
// channel to learn that the send it issued may never reach the peer.
func (c *Conn) WriteFailed() <-chan struct{} { return c.writeFailedCh }

// Err reports the read side's terminal condition. It is nil after a
// clean end of stream and after Close, and the read error otherwise.
// It must not be read before Done closes.
func (c *Conn) Err() error { return c.termErr }

// termErrorFor builds the error a call in flight receives when the
// reader exits: an unexpected-EOF error when the stream ended
// cleanly, or a wrapped scanner error otherwise.
func (c *Conn) termErrorFor(id int64) error {
	if c.termErr == nil {
		return &unexpectedEOFCallError{id: id}
	}
	return fmt.Errorf("scanner error waiting for response id=%d: %w", id, c.termErr)
}

// closedCallError is returned by a call in flight when Close runs
// before its response arrives.
type closedCallError struct{ id int64 }

func (e *closedCallError) Error() string {
	return fmt.Sprintf("connection closed waiting for response id=%d", e.id)
}

func (e *closedCallError) Unwrap() error { return ErrClosed }

// unexpectedEOFCallError is returned by a call in flight when the
// stream ends cleanly before its response arrives.
type unexpectedEOFCallError struct{ id int64 }

func (e *unexpectedEOFCallError) Error() string {
	return fmt.Sprintf("unexpected EOF waiting for response id=%d", e.id)
}

func (e *unexpectedEOFCallError) Unwrap() error { return io.EOF }

// writeFailedCallError is returned by a call in flight when a write on
// this connection fails before its response arrives.
type writeFailedCallError struct {
	id  int64
	err error
}

func (e *writeFailedCallError) Error() string {
	return fmt.Sprintf("write failed waiting for response id=%d: %v", e.id, e.err)
}

func (e *writeFailedCallError) Unwrap() error { return e.err }

// writeLoop is the connection's sole writer. It takes each item the
// outbox holds in order, writes its line to w or, for a flush marker,
// closes its done channel, and exits once the outbox has been closed
// and fully drained or once a write fails. No deadline bounds a write:
// a peer that has stopped reading, not merely one that reads slowly,
// is the only thing that ever parks this goroutine, and closing w is
// what a caller uses to end that park.
func (c *Conn) writeLoop() {
	for {
		<-c.outbox.Ready()
		item, ok := c.outbox.Take()
		if !ok {
			return
		}
		if item.done != nil {
			close(item.done)
			continue
		}
		if _, err := c.w.Write(item.line); err != nil {
			c.failWrite(err)
			return
		}
	}
}

// failWrite records err as the connection's terminal write failure,
// exactly once, and fails every pending call with it. It closes the
// outbox after writeFailedCh, so a line appended once the writer
// goroutine has exited is refused with the write-failure error rather
// than accepted into a queue nothing drains.
func (c *Conn) failWrite(err error) {
	c.writeErr = err
	close(c.writeFailedCh)
	c.outbox.Close()
	for id, ch := range c.drainPending() {
		ch <- callResult{err: &writeFailedCallError{id: id, err: err}}
	}
}

// readLoop scans one line at a time, dispatches it per the routing
// rule, and reports the terminal condition once the scan ends.
func (c *Conn) readLoop() {
	scanner := bufio.NewScanner(c.r)
	// The scanner grows its buffer on demand up to the bound, so
	// starting small keeps a large bound from costing every session
	// that memory at construction. The starting size is capped by the
	// bound itself, because a starting buffer larger than the maximum
	// would raise the effective limit past the one this connection was
	// given.
	scanner.Buffer(make([]byte, 0, min(initialLineBytes, c.maxLineBytes)), c.maxLineBytes)

	closedEarly := false
scanLoop:
	for scanner.Scan() {
		select {
		case <-c.closed:
			closedEarly = true
			break scanLoop
		default:
		}

		// The scanner reuses its byte slice between scans, so the
		// line must be copied before it is parsed or retained.
		line := make([]byte, len(scanner.Bytes()))
		copy(line, scanner.Bytes())
		msg := parseMessage(line)
		if msg.Kind == KindResponse {
			if n, isNumber := msg.ID.Number(); isNumber {
				if ch, ok := c.takePending(n); ok {
					ch <- callResult{resp: Response{ID: msg.ID, Result: msg.Result, Error: msg.Error}}
					continue
				}
			}
		}
		c.sink.deliver(msg)
	}

	if !closedEarly {
		c.termErr = scanner.Err()
		for id, ch := range c.drainPending() {
			ch <- callResult{err: c.termErrorFor(id)}
		}
		if c.termErr != nil {
			c.sink.deliver(Message{Kind: KindStreamEnd, Err: c.termErr})
		}
	}
	close(c.done)
}
