// Package jsonrpc implements newline-delimited JSON-RPC framing over a
// byte stream. A [Conn] writes requests, notifications, and
// responses, correlates a response to the call awaiting it by request
// id, and routes every other message, including one that arrives
// when no call is in flight, to a caller-supplied [Handler]. Start
// from [NewConn].
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

// ErrClosed is returned, wrapped, by a write or a call issued after
// [Conn.Close].
var ErrClosed = errors.New("connection closed")

// Conn is one JSON-RPC session over a newline-delimited byte stream.
// A Conn is safe for concurrent use.
type Conn struct {
	w io.Writer
	r io.Reader
	h Handler

	writeMu sync.Mutex

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

// callResult is what a pending call receives: either the correlated
// [Response], or the reason the call will never receive one.
type callResult struct {
	resp Response
	err  error
}

// NewConn starts a connection that writes to w, reads newline-
// delimited JSON-RPC messages from r, and hands every message that is
// not a correlated response to h.
//
// NewConn panics when h is nil. It starts one reader goroutine before
// returning, and it does not close w or r; closing them is the
// caller's responsibility, and closing r is how the caller unblocks a
// read parked on the stream.
func NewConn(w io.Writer, r io.Reader, h Handler) *Conn {
	if h == nil {
		panic("jsonrpc: handler must be non-nil")
	}
	c := &Conn{
		w:       w,
		r:       r,
		h:       h,
		pending: make(map[int64]chan callResult),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// wireRequest is the JSON shape of a request or a notification. A
// notification omits id via the zero value and the omitempty tag.
type wireRequest struct {
	Method string `json:"method"`
	ID     int64  `json:"id,omitempty"`
	Params any    `json:"params,omitempty"`
}

// write serializes one already-encoded line against every other
// write on the connection, and reports ErrClosed after Close without
// touching w.
func (c *Conn) write(line []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return ErrClosed
	default:
	}

	_, err := c.w.Write(line)
	return err
}

func (c *Conn) marshalAndWriteRequest(method string, id int64, params any) error {
	data, err := json.Marshal(wireRequest{Method: method, ID: id, Params: params})
	if err != nil {
		return fmt.Errorf("marshal request %s: %w", method, err)
	}
	if err := c.write(append(data, '\n')); err != nil {
		return fmt.Errorf("write request %s: %w", method, err)
	}
	return nil
}

// Notify writes a notification, a message with no id, for method.
func (c *Conn) Notify(method string, params any) error {
	data, err := json.Marshal(wireRequest{Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("marshal notification %s: %w", method, err)
	}
	if err := c.write(append(data, '\n')); err != nil {
		return fmt.Errorf("write notification %s: %w", method, err)
	}
	return nil
}

// Respond writes a successful response to the request carrying id.
func (c *Conn) Respond(id int64, result any) error {
	resp := struct {
		ID     int64 `json:"id"`
		Result any   `json:"result"`
	}{ID: id, Result: result}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response id=%d: %w", id, err)
	}
	if err := c.write(append(data, '\n')); err != nil {
		return fmt.Errorf("write response id=%d: %w", id, err)
	}
	return nil
}

// RespondError writes an error response to the request carrying id.
func (c *Conn) RespondError(id int64, code int, message string) error {
	resp := struct {
		ID    int64 `json:"id"`
		Error Error `json:"error"`
	}{ID: id, Error: Error{Code: code, Message: message}}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal error response id=%d: %w", id, err)
	}
	if err := c.write(append(data, '\n')); err != nil {
		return fmt.Errorf("write error response id=%d: %w", id, err)
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
// a waiter, for a request whose response the caller deliberately
// ignores. The response later reaches the handler as an unmatched
// KindResponse message.
func (c *Conn) SendRequest(method string, params any) (int64, error) {
	id := c.allocateID()
	if err := c.marshalAndWriteRequest(method, id, params); err != nil {
		return 0, err
	}
	return id, nil
}

// Call writes a request for method and waits for its response.
//
// Call writes before it checks ctx, so a request whose context is
// already done is still sent. It returns (Response, nil) when the
// response arrives, including when the response carries a JSON-RPC
// error: an error response is an answer, not a transport failure.
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
	}
}

// Close stops the connection. It fails every call in flight with an
// error wrapping ErrClosed and makes every later write return an
// error wrapping ErrClosed. Close is idempotent and safe to call from
// any goroutine, and it does not close the underlying writer or
// reader.
//
// Close acquires the write mutex while it marks the connection
// closed, so no write is in flight when Close returns and none will
// start.
func (c *Conn) Close() {
	c.writeMu.Lock()
	c.closeOnce.Do(func() { close(c.closed) })
	c.writeMu.Unlock()

	for id, ch := range c.drainPending() {
		ch <- callResult{err: &closedCallError{id: id}}
	}
}

// Done returns a channel closed once the reader goroutine has
// exited. The handler is not invoked after Done closes.
func (c *Conn) Done() <-chan struct{} { return c.done }

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

// readLoop scans one line at a time, dispatches it per the routing
// rule, and reports the terminal condition once the scan ends.
func (c *Conn) readLoop() {
	scanner := bufio.NewScanner(c.r)
	scanner.Buffer(make([]byte, 0, MaxLineBytes), MaxLineBytes)

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
			if ch, ok := c.takePending(msg.ID); ok {
				ch <- callResult{resp: Response{ID: msg.ID, Result: msg.Result, Error: msg.Error}}
				continue
			}
		}
		c.h(msg)
	}

	if !closedEarly {
		c.termErr = scanner.Err()
		for id, ch := range c.drainPending() {
			ch <- callResult{err: c.termErrorFor(id)}
		}
		if c.termErr != nil {
			c.h(Message{Kind: KindStreamEnd, Err: c.termErr})
		}
	}
	close(c.done)
}
