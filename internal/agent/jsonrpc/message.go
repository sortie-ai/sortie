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
	idNull
	idNumber
	idString
)

// ID is a JSON-RPC request identifier as it appeared on the wire: a
// JSON number, a JSON string, an explicit null, or absent. It
// preserves whichever form arrived and re-serializes that exact form.
//
// A null id is present; an absent one is not. The distinction is
// load-bearing in both directions. A request with no id member is a
// notification and is never answered, while a request whose id is
// null still expects an answer. And a response carrying a null id is
// the form the specification requires when a peer could not read the
// id it was answering, so classifying it as malformed would discard
// the error it exists to deliver.
type ID struct {
	kind idKind

	// raw is the id exactly as it arrived, for a number or a string,
	// and is what MarshalJSON writes back. A peer that numbered a
	// request 1e3 is answered with 1e3 rather than with 1000.
	raw string

	// num holds the value of a number that fits the plain integer
	// form this connection allocates for itself. numOK is false for
	// any other number, such as one carrying an exponent or a
	// fraction, which is still a valid id and is still preserved in
	// raw.
	num   int64
	numOK bool

	str string
}

// NumberID returns an ID that renders as the JSON number n.
func NumberID(n int64) ID {
	return ID{kind: idNumber, raw: strconv.FormatInt(n, 10), num: n, numOK: true}
}

// NullID returns an ID that renders as JSON null. It is the id a
// response carries when the peer could not read the id of the
// request it is answering.
func NullID() ID {
	return ID{kind: idNull}
}

// Present reports whether an id member arrived at all. An explicit
// null is present; only an omitted member is not.
func (id ID) Present() bool {
	return id.kind != idAbsent
}

// IsNull reports whether id arrived as an explicit JSON null.
func (id ID) IsNull() bool {
	return id.kind == idNull
}

// Number returns the numeric value of id and true when id arrived as
// a JSON number in the plain integer form. It returns (0, false)
// otherwise, including for a string, for null, and for a number
// written with an exponent or a fraction.
func (id ID) Number() (int64, bool) {
	return id.num, id.kind == idNumber && id.numOK
}

// Equal reports whether id and other carry the same wire form and
// the same bytes.
func (id ID) Equal(other ID) bool {
	return id == other
}

// String returns a human-readable rendering of id, for logging.
func (id ID) String() string {
	switch id.kind {
	case idNumber:
		return id.raw
	case idString:
		return id.str
	case idNull:
		return "null"
	default:
		return "<absent>"
	}
}

// MarshalJSON renders id in the wire form it was constructed with or
// decoded from. An absent id renders nothing and reports an error,
// because a message that needs an id has none to write.
func (id ID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case idNumber, idString:
		return []byte(id.raw), nil
	case idNull:
		return []byte("null"), nil
	default:
		return nil, fmt.Errorf("marshal id: no id to write")
	}
}

// UnmarshalJSON decodes a JSON-RPC id, accepting a JSON number, a
// JSON string, or null, and preserves both which form arrived and
// its exact bytes so a later MarshalJSON call reproduces it verbatim.
// A number this package cannot hold as a plain integer is still a
// valid id and is still preserved; only [ID.Number] reports it as
// unavailable.
func (id *ID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0:
		*id = ID{}
		return nil
	case string(trimmed) == "null":
		*id = ID{kind: idNull}
		return nil
	case trimmed[0] == '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("unmarshal id: %w", err)
		}
		*id = ID{kind: idString, raw: string(trimmed), str: s}
		return nil
	default:
		var n int64
		if err := json.Unmarshal(trimmed, &n); err != nil {
			*id = ID{kind: idNumber, raw: string(trimmed)}
			return nil
		}
		*id = ID{kind: idNumber, raw: string(trimmed), num: n, numOK: true}
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
