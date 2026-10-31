package codex

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// tokenUsageBreakdown is one token-count breakdown inside a
// thread/tokenUsage/updated notification. Every count is a pointer so a member the runtime omits or sends as null
// stays unknown instead of reading as zero.
type tokenUsageBreakdown struct {
	InputTokens           *int64 `json:"inputTokens"`
	CachedInputTokens     *int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens *int64 `json:"cacheWriteInputTokens"`
	OutputTokens          *int64 `json:"outputTokens"`
}

// reading maps the breakdown to a reading served by model.
func (b tokenUsageBreakdown) reading(model string) usage.SpendReading {
	return usage.SpendReading{
		Input:      b.InputTokens,
		Output:     b.OutputTokens,
		CacheRead:  b.CachedInputTokens,
		CacheWrite: b.CacheWriteInputTokens,
		Model:      model,
	}
}

// threadTokenUsage holds the last and thread-cumulative total token
// breakdowns from a thread/tokenUsage/updated notification.
type threadTokenUsage struct {
	Last  tokenUsageBreakdown `json:"last"`
	Total tokenUsageBreakdown `json:"total"`
}

// reading maps the thread-cumulative total to a reading, carrying the
// request's own counts in Own.
func (u threadTokenUsage) reading(model string) usage.SpendReading {
	reading := u.Total.reading(model)
	own := u.Last.reading(model)
	reading.Own = &own
	return reading
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
	Message        string         `json:"message"`
	CodexErrorInfo codexErrorInfo `json:"codexErrorInfo,omitempty"`
}

// codexErrorInfo is the variant name of a turn error's codexErrorInfo
// member. The wire carries a unit variant as a camelCase string and a
// variant with data as a one-member object keyed by the variant name.
type codexErrorInfo string

// UnmarshalJSON accepts both wire forms and never fails: an
// unrecognized shape leaves the variant empty, so the turn's error
// message survives whatever the member holds.
func (c *codexErrorInfo) UnmarshalJSON(data []byte) error {
	*c = ""
	var name string
	if json.Unmarshal(data, &name) == nil {
		*c = codexErrorInfo(name)
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) == nil {
		for key := range object {
			if *c == "" || key < string(*c) {
				*c = codexErrorInfo(key)
			}
		}
	}
	return nil
}

// itemParams is the params payload of item/started and item/completed
// notifications.
type itemParams struct {
	Item struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		Command   string          `json:"command,omitempty"`
		Status    string          `json:"status,omitempty"`
		Changes   json.RawMessage `json:"changes,omitempty"`
		Server    string          `json:"server,omitempty"`
		Tool      string          `json:"tool,omitempty"`
		Namespace string          `json:"namespace,omitempty"`
	} `json:"item"`
}

// toolCall maps a tool item to a tool call and reports whether the item is a
// tool item at all. Built-in items name no tool; MCP and dynamic tool items
// are named by the tool they called. A malformed changes list gives an edit
// with no argument.
func (p itemParams) toolCall() (agentcore.ToolCall, bool) {
	item := p.Item
	switch item.Type {
	case "commandExecution":
		return agentcore.ToolCall{Kind: domain.ToolKindShell, Argument: item.Command}, true
	case "fileChange":
		return agentcore.ToolCall{Kind: domain.ToolKindEdit, Argument: changedPaths(item.Changes)}, true
	case "mcpToolCall":
		return agentcore.ToolCall{Name: agentcore.QualifiedToolName(item.Server, item.Tool)}, true
	case "dynamicToolCall":
		return agentcore.ToolCall{Name: agentcore.QualifiedToolName(item.Namespace, item.Tool)}, true
	}
	return agentcore.ToolCall{}, false
}

// changedPaths joins the path of each fileChange change with ", " in order.
func changedPaths(changes json.RawMessage) string {
	var list []struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(changes, &list) != nil {
		return ""
	}
	paths := make([]string, 0, len(list))
	for _, change := range list {
		paths = append(paths, change.Path)
	}
	return strings.Join(paths, ", ")
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
// response used by the adapter. Model is a sibling of Thread at the
// top level of the result, not a member of it, and is empty when the
// response omits it.
type threadResult struct {
	Thread struct {
		ID   string  `json:"id"`
		Path *string `json:"path"`
	} `json:"thread"`
	Model string `json:"model"`
}

// path returns the thread's reported on-disk path, empty when the
// response omitted it or reported null.
func (r threadResult) path() string {
	if r.Thread.Path == nil {
		return ""
	}
	return *r.Thread.Path
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
	Success bool   `json:"success"`
	Error   string `json:"error"`
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

// modelReroutedParams is the params payload of a model/rerouted
// notification, sent when the runtime moves a live turn to a
// different model.
type modelReroutedParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	FromModel string `json:"fromModel"`
	ToModel   string `json:"toModel"`
}

// parseModelRerouted unmarshals the params payload of a
// model/rerouted notification.
func parseModelRerouted(params json.RawMessage) (modelReroutedParams, error) {
	var p modelReroutedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return modelReroutedParams{}, fmt.Errorf("parse model/rerouted params: %w", err)
	}
	return p, nil
}

// rateLimitWindow is one usage window of an account/rateLimits/updated
// notification. ResetsAt is a pointer so a window without a reset
// instant is distinguishable from one resetting at Unix zero.
type rateLimitWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
	ResetsAt           *int64 `json:"resetsAt"`
}

// rateLimitsUpdatedParams is the params payload of an
// account/rateLimits/updated notification. A window the runtime does not
// report is null on the wire and so nil here.
type rateLimitsUpdatedParams struct {
	RateLimits struct {
		LimitID   string           `json:"limitId"`
		LimitName *string          `json:"limitName"`
		Primary   *rateLimitWindow `json:"primary"`
		Secondary *rateLimitWindow `json:"secondary"`
	} `json:"rateLimits"`
}

// limitModel returns the model the notification's windows meter, or the
// empty string when the bucket meters every model. The default bucket and a
// limitName that is absent or only repeats the limitId name no model.
func (p rateLimitsUpdatedParams) limitModel() string {
	limits := p.RateLimits
	switch {
	case limits.LimitID == "codex", limits.LimitName == nil, *limits.LimitName == "", *limits.LimitName == limits.LimitID:
		return ""
	}
	return *limits.LimitName
}

func (p rateLimitsUpdatedParams) windowReadings() []usage.WindowReading {
	var readings []usage.WindowReading
	model := p.limitModel()
	for _, w := range []struct {
		suffix string
		window *rateLimitWindow
	}{{"/primary", p.RateLimits.Primary}, {"/secondary", p.RateLimits.Secondary}} {
		if w.window == nil {
			continue
		}
		share := float64(w.window.UsedPercent)
		reading := usage.WindowReading{Window: p.RateLimits.LimitID + w.suffix, Share: &share}
		reading.Span.Model = model
		if w.window.WindowDurationMins != nil {
			reading.Span.Minutes = *w.window.WindowDurationMins
		}
		if p.RateLimits.LimitName != nil {
			reading.Span.Scope = *p.RateLimits.LimitName
		}
		if w.window.ResetsAt != nil {
			reading.ResetsAt = *w.window.ResetsAt
		}
		readings = append(readings, reading)
	}
	return readings
}

// parseRateLimitsUpdated unmarshals the params payload of an
// account/rateLimits/updated notification.
func parseRateLimitsUpdated(params json.RawMessage) (rateLimitsUpdatedParams, error) {
	var p rateLimitsUpdatedParams
	if err := json.Unmarshal(params, &p); err != nil {
		return rateLimitsUpdatedParams{}, fmt.Errorf("parse account/rateLimits/updated params: %w", err)
	}
	return p, nil
}

// mapCodexErrorInfo maps a codexErrorInfo variant name to a domain error
// kind, comparing names case-insensitively. Retryable vs non-retryable
// classification is encoded in the AgentErrorKind value.
func mapCodexErrorInfo(info string) domain.AgentErrorKind {
	switch strings.ToLower(info) {
	case "unauthorized":
		return domain.ErrCredentialUnverified
	case "badrequest":
		return domain.ErrResponseError
	case "usagelimitexceeded":
		return domain.ErrUsageLimit
	case "contextwindowexceeded", "sandboxerror":
		return domain.ErrTurnFailed
	case "httpconnectionfailed", "responsestreamconnectionfailed",
		"responsestreamdisconnected", "responsetoomanyfailedattempts",
		"internalservererror", "other":
		return domain.ErrTurnFailed
	default:
		return domain.ErrTurnFailed
	}
}
