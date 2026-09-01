package jsonrpc

import (
	"encoding/json"
	"fmt"
)

// Kind classifies one parsed message.
type Kind int

const (
	// KindMalformed is a message the reader could not use. Err carries
	// the reason; every other field is zero.
	KindMalformed Kind = iota
	// KindResponse carries a non-zero id and no method.
	KindResponse
	// KindNotification carries a method and an absent or zero id.
	KindNotification
	// KindRequest carries a method and a non-zero id, so the peer
	// expects a response.
	KindRequest
	// KindStreamEnd reports that the read side failed. It is delivered
	// once, as the last message, and only when the read failed; a
	// clean end of stream delivers nothing. Err carries the read
	// error.
	KindStreamEnd
)

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
	ID     int64
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  *Error
	Err    error
}

// Response is the reply to one call.
type Response struct {
	ID     int64
	Result json.RawMessage
	Error  *Error
}

// Handler receives every message the connection reads that is not the
// response to a call in flight.
type Handler func(Message)

// wireEnvelope is the JSON shape one newline-delimited line decodes
// into before classification. A zero id is indistinguishable from an
// absent one, which is what lets a zero id beside a method classify
// as a notification rather than a request.
type wireEnvelope struct {
	ID     int64           `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// parseMessage classifies one line of the newline-delimited stream
// into a [Message]. A line that is not valid JSON, or that carries
// neither a method nor a non-zero id, classifies as KindMalformed
// with Err set.
func parseMessage(line []byte) Message {
	var wire wireEnvelope
	if err := json.Unmarshal(line, &wire); err != nil {
		return Message{Kind: KindMalformed, Err: fmt.Errorf("parse message: %w", err)}
	}

	if wire.Method != "" {
		if wire.ID != 0 {
			return Message{Kind: KindRequest, ID: wire.ID, Method: wire.Method, Params: wire.Params}
		}
		return Message{Kind: KindNotification, Method: wire.Method, Params: wire.Params}
	}
	if wire.ID != 0 {
		return Message{Kind: KindResponse, ID: wire.ID, Result: wire.Result, Error: wire.Error}
	}

	return Message{Kind: KindMalformed, Err: fmt.Errorf("parse message: no method or id in JSON-RPC message")}
}
