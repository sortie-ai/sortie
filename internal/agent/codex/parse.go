package codex

import (
	"encoding/json"
	"fmt"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/typeutil"
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

// tokenUsageBreakdown is one token-count breakdown inside a
// thread/tokenUsage/updated notification.
type tokenUsageBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

// threadTokenUsage holds the last and thread-cumulative total token
// breakdowns from a thread/tokenUsage/updated notification.
type threadTokenUsage struct {
	Last  tokenUsageBreakdown `json:"last"`
	Total tokenUsageBreakdown `json:"total"`
}

// tokenUsageUpdatedParams is the params payload of a
// thread/tokenUsage/updated notification. TokenUsage is a pointer so a
// notification whose tokenUsage object is absent from the wire payload
// is distinguishable from one reporting an all-zero breakdown.
type tokenUsageUpdatedParams struct {
	ThreadID   string            `json:"threadId"`
	TurnID     string            `json:"turnId"`
	TokenUsage *threadTokenUsage `json:"tokenUsage"`
}

// turnCompletedParams is the params payload of a turn/completed
// notification. The app-server protocol carries no usage member here;
// token usage arrives separately on thread/tokenUsage/updated.
type turnCompletedParams struct {
	Turn struct {
		ID     string     `json:"id"`
		Status string     `json:"status"`
		Error  *turnError `json:"error,omitempty"`
	} `json:"turn"`
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

// mcpServerStartupStatus is the params payload of an
// mcpServer/startupStatus/updated notification, reporting whether a
// declared MCP server started successfully.
type mcpServerStartupStatus struct {
	ThreadID      string `json:"threadId"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	FailureReason string `json:"failureReason"`
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

// normalizeBreakdown converts a raw [tokenUsageBreakdown] into a
// [domain.TokenUsage]. InputTokens is taken from b.InputTokens, already
// inclusive of b.CachedInputTokens; OutputTokens is taken from
// b.OutputTokens, already inclusive of b.ReasoningOutputTokens;
// CacheReadTokens is set from b.CachedInputTokens; TotalTokens is
// computed as InputTokens plus OutputTokens rather than taken from
// b.TotalTokens.
func normalizeBreakdown(b tokenUsageBreakdown) domain.TokenUsage {
	return domain.TokenUsage{
		InputTokens:     b.InputTokens,
		OutputTokens:    b.OutputTokens,
		TotalTokens:     b.InputTokens + b.OutputTokens,
		CacheReadTokens: b.CachedInputTokens,
	}
}

// parseTokenUsageUpdated unmarshals the params payload of a
// thread/tokenUsage/updated notification.
func parseTokenUsageUpdated(params json.RawMessage) (tokenUsageUpdatedParams, error) {
	var p tokenUsageUpdatedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return tokenUsageUpdatedParams{}, fmt.Errorf("parse thread/tokenUsage/updated params: %w", err)
	}
	return p, nil
}

// subtractUsage returns a minus b componentwise, floored at zero, with
// TotalTokens recomputed as InputTokens plus OutputTokens.
func subtractUsage(a, b domain.TokenUsage) domain.TokenUsage {
	result := domain.TokenUsage{
		InputTokens:     max(a.InputTokens-b.InputTokens, 0),
		OutputTokens:    max(a.OutputTokens-b.OutputTokens, 0),
		CacheReadTokens: max(a.CacheReadTokens-b.CacheReadTokens, 0),
	}
	result.TotalTokens = result.InputTokens + result.OutputTokens
	return result
}

// maxUsage returns the componentwise maximum of a and b, with
// TotalTokens recomputed as InputTokens plus OutputTokens.
func maxUsage(a, b domain.TokenUsage) domain.TokenUsage {
	result := domain.TokenUsage{
		InputTokens:     max(a.InputTokens, b.InputTokens),
		OutputTokens:    max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens: max(a.CacheReadTokens, b.CacheReadTokens),
	}
	result.TotalTokens = result.InputTokens + result.OutputTokens
	return result
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
// event. Truncated to 200 runes.
func summarizeItem(itemType, itemID string) string {
	s := fmt.Sprintf("[%s] %s", itemType, itemID)
	return typeutil.TruncateRunes(s, 200)
}
