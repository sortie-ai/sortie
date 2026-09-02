package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Kind classifies one parsed message.
type Kind int

const (
	// KindMalformed is a message the reader could not use. Err carries
	// the reason; every other field is zero.
	KindMalformed Kind = iota
	// KindResponse carries a present id and no method.
	KindResponse
	// KindNotification carries a method and an absent id.
	KindNotification
	// KindRequest carries a method and a present id, so the peer
	// expects a response.
	KindRequest
	// KindStreamEnd reports that the read side failed. It is delivered
	// once, as the last message, and only when the read failed; a
	// clean end of stream delivers nothing. Err carries the read
	// error.
	KindStreamEnd
)

// idKind distinguishes the wire form an [ID] preserves.
type idKind uint8

const (
	idAbsent idKind = iota
	idNumber
	idString
)

// ID is a JSON-RPC request identifier as it appeared on the wire: a
// JSON number, a JSON string, or absent. It preserves whichever form
// arrived and re-serializes that exact form.
type ID struct {
	kind idKind
	num  int64
	str  string
}

// NumberID returns an ID that renders as the JSON number n.
func NumberID(n int64) ID {
	return ID{kind: idNumber, num: n}
}

// Present reports whether id carries a value, as opposed to having
// arrived absent or as a JSON null.
func (id ID) Present() bool {
	return id.kind != idAbsent
}

// Number returns the numeric value of id and true when id arrived as
// a JSON number. It returns (0, false) otherwise, including when id
// arrived as a string.
func (id ID) Number() (int64, bool) {
	return id.num, id.kind == idNumber
}

// Equal reports whether id and other carry the same wire form and
// value.
func (id ID) Equal(other ID) bool {
	return id == other
}

// String returns a human-readable rendering of id, for logging.
func (id ID) String() string {
	switch id.kind {
	case idNumber:
		return strconv.FormatInt(id.num, 10)
	case idString:
		return id.str
	default:
		return "<absent>"
	}
}

// MarshalJSON renders id in the wire form it was constructed with or
// decoded from: a JSON number, a JSON string, or null when absent.
func (id ID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case idNumber:
		return json.Marshal(id.num)
	case idString:
		return json.Marshal(id.str)
	default:
		return []byte("null"), nil
	}
}

// UnmarshalJSON decodes a JSON-RPC id, accepting a JSON number, a
// JSON string, or an absent or null value, and preserves which form
// arrived so a later MarshalJSON call reproduces it verbatim.
func (id *ID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0, string(trimmed) == "null":
		*id = ID{}
		return nil
	case trimmed[0] == '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("unmarshal id: %w", err)
		}
		*id = ID{kind: idString, str: s}
		return nil
	default:
		var n int64
		if err := json.Unmarshal(trimmed, &n); err != nil {
			return fmt.Errorf("unmarshal id: %w", err)
		}
		*id = ID{kind: idNumber, num: n}
		return nil
	}
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message is one message read from the peer, or the connection's own
// report that the read side failed.
//
// Error carries the peer's JSON-RPC error object; Err carries this
// side's reason for not being able to use the message. The two are
// never both set.
type Message struct {
	Kind   Kind
	ID     ID
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  *Error
	Err    error
}

// Response is the reply to one call.
type Response struct {
	ID     ID
	Result json.RawMessage
	Error  *Error
}

// Handler receives every message the connection reads that is not the
// response to a call in flight.
//
// The connection invokes it synchronously on its reader goroutine, so
// a handler that blocks stops the connection reading: a response to a
// call already in flight is not correlated until the handler returns,
// and a handler that calls back into the connection and waits for the
// reply deadlocks. A handler that can block must bound the wait or
// hand the message to another goroutine.
type Handler func(Message)

// wireEnvelope is the JSON shape one newline-delimited line decodes
// into before classification.
type wireEnvelope struct {
	ID     ID              `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// parseMessage classifies one line of the newline-delimited stream
// into a [Message]. A line that is not valid JSON, or that carries
// neither a method nor a present id, classifies as KindMalformed with
// Err set.
func parseMessage(line []byte) Message {
	var wire wireEnvelope
	if err := json.Unmarshal(line, &wire); err != nil {
		return Message{Kind: KindMalformed, Err: fmt.Errorf("parse message: %w", err)}
	}

	if wire.Method != "" {
		if wire.ID.Present() {
			return Message{Kind: KindRequest, ID: wire.ID, Method: wire.Method, Params: wire.Params}
		}
		return Message{Kind: KindNotification, Method: wire.Method, Params: wire.Params}
	}
	if wire.ID.Present() {
		return Message{Kind: KindResponse, ID: wire.ID, Result: wire.Result, Error: wire.Error}
	}

	return Message{Kind: KindMalformed, Err: fmt.Errorf("parse message: no method or id in JSON-RPC message")}
}
