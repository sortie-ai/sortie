package codex

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/domain"
)

// rpcRequest is a JSON-RPC 2.0 request sent to the app-server.
type rpcRequest struct {
	Method string `json:"method"`
	ID     int64  `json:"id,omitempty"`
	Params any    `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response from the app-server.
type rpcResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// rpcNotification is a server-initiated notification (no id field).
type rpcNotification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// parsedMessage is the result of parsing a single JSONL line from the
// app-server. Exactly one of IsResponse or IsNotification is true when
// Err is nil.
type parsedMessage struct {
	IsResponse     bool
	IsNotification bool
	Response       rpcResponse
	Notification   rpcNotification
	Err            error
}

// turnUsage holds raw token usage fields from the app-server
// turn/completed notification.
type turnUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
}

// turnCompletedParams is the params payload of a turn/completed
// notification.
type turnCompletedParams struct {
	Turn struct {
		ID     string     `json:"id"`
		Status string     `json:"status"`
		Error  *turnError `json:"error,omitempty"`
	} `json:"turn"`
	Usage *turnUsage `json:"usage,omitempty"`
}

// turnError is the error object inside a failed turn/completed
// notification.
type turnError struct {
	Message        string `json:"message"`
	CodexErrorInfo string `json:"codexErrorInfo,omitempty"`
}

// itemParams is the params payload of item/started and item/completed
// notifications.
type itemParams struct {
	Item struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Text    string `json:"text,omitempty"`
		Command string `json:"command,omitempty"`
		Status  string `json:"status,omitempty"`
	} `json:"item"`
}

// toolCallParams is the params payload of an item/tool/call request.
type toolCallParams struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// threadResult is the subset of thread/start or thread/resume
// response used by the adapter.
type threadResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

// turnStartResult is the subset of turn/start response used by the
// adapter.
type turnStartResult struct {
	Turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"turn"`
}

// accountResult is the subset of account/read response used by the
// adapter.
type accountResult struct {
	Account json.RawMessage `json:"account"`
}

// accountLoginNotification is the params payload of an
// account/login/completed notification.
type accountLoginNotification struct {
	Success bool `json:"success"`
}

// wireMessage is used for initial discrimination of JSON-RPC messages.
// A message with a non-zero ID and no Method is a response; a message
// with a Method is a request or notification.
type wireMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// parseMessage parses a single JSONL line from the app-server stdout.
// It discriminates between responses (non-zero id, no method) and
// notifications (method present, zero or absent id).
func parseMessage(line []byte) parsedMessage {
	var wire wireMessage
	if err := json.Unmarshal(line, &wire); err != nil {
		return parsedMessage{Err: fmt.Errorf("parse message: %w", err)}
	}

	// Responses have a non-zero id and no method field. Notifications
	// and requests have a method field. When both are present (a
	// request from the server such as item/tool/call), treat it as a
	// notification so the event loop dispatches on Method.
	if wire.Method != "" {
		return parsedMessage{
			IsNotification: true,
			Notification: rpcNotification{
				Method: wire.Method,
				Params: wire.Params,
			},
			// Preserve the request ID for item/tool/call responses.
			Response: rpcResponse{ID: wire.ID},
		}
	}
	if wire.ID != 0 {
		return parsedMessage{
			IsResponse: true,
			Response: rpcResponse{
				ID:     wire.ID,
				Result: wire.Result,
				Error:  wire.Error,
			},
		}
	}

	return parsedMessage{Err: fmt.Errorf("parse message: no method or id in JSON-RPC message")}
}

// normalizeUsage converts raw app-server token usage into a
// [domain.TokenUsage]. TotalTokens is computed as input + output.
// CacheReadTokens is set from CachedInputTokens.
func normalizeUsage(u *turnUsage) domain.TokenUsage {
	if u == nil {
		return domain.TokenUsage{}
	}
	return domain.TokenUsage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		TotalTokens:     u.InputTokens + u.OutputTokens,
		CacheReadTokens: u.CachedInputTokens,
	}
}

// mapTurnStatus maps a turn/completed status string to a domain event
// type.
func mapTurnStatus(status string) domain.AgentEventType {
	switch status {
	case "completed":
		return domain.EventTurnCompleted
	case "interrupted":
		return domain.EventTurnCancelled
	case "failed":
		return domain.EventTurnFailed
	default:
		return domain.EventTurnFailed
	}
}

// mapCodexErrorInfo maps a codexErrorInfo string to a domain error
// kind. Retryable vs non-retryable classification is encoded in the
// AgentErrorKind value.
func mapCodexErrorInfo(info string) domain.AgentErrorKind {
	switch info {
	case "Unauthorized":
		return domain.ErrResponseError
	case "BadRequest":
		return domain.ErrResponseError
	case "ContextWindowExceeded", "UsageLimitExceeded", "SandboxError":
		return domain.ErrTurnFailed
	case "HttpConnectionFailed", "ResponseStreamConnectionFailed",
		"ResponseStreamDisconnected", "ResponseTooManyFailedAttempts",
		"InternalServerError", "Other":
		return domain.ErrTurnFailed
	default:
		return domain.ErrTurnFailed
	}
}

// summarizeItem returns a short human-readable string for an item
// event. Truncated to 200 characters.
func summarizeItem(itemType, itemID string) string {
	s := fmt.Sprintf("[%s] %s", itemType, itemID)
	return truncate(s, 200)
}

// toolResultFor constructs the JSON-RPC result payload for a dynamic
// tool call response.
func toolResultFor(success bool, output string) map[string]any {
	return map[string]any{
		"success": success,
		"output":  output,
		"contentItems": []map[string]any{
			{"type": "inputText", "text": output},
		},
	}
}

// truncate shortens s to maxLen runes if it exceeds that length.
func truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen])
}
