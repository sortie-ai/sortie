package codex

import (
	"encoding/json"
	"fmt"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

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
