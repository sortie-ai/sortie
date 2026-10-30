package opencode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

type rawRunEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part,omitempty"`
	Error     *rawRunError    `json:"error,omitempty"`
}

type rawRunError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
	Status  any    `json:"status,omitempty"` // a JSON number when present

	// RetryAfterMs is the delay in milliseconds after the failure at which
	// the runtime expects a retry to succeed.
	RetryAfterMs *int64 `json:"retryAfterMs"`
}

const (
	// freeTierAuthType is the error type a provider authorization
	// refusal, free-tier refusals included, carries.
	freeTierAuthType = "provider.auth"
	// freeTierMessageMarker is the substring that distinguishes a
	// free-tier refusal from any other provider's authorization refusal
	// under the same error type.
	freeTierMessageMarker = "free tier can only be used from within OpenCode"
)

// freeTierRequiredTools lists, in the order [freeTierRefusalClause]
// reports them, the tools the hosted free tier requires in a session's
// tool set.
var freeTierRequiredTools = [...]string{"bash", "read"}

type rawPartTime struct {
	Start int64 `json:"start,omitempty"`
	End   int64 `json:"end,omitempty"`
}

type rawStepStartPart struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	SessionID string `json:"sessionID"`
	Snapshot  string `json:"snapshot,omitempty"`
	Type      string `json:"type"`
}

type rawTextPart struct {
	ID        string      `json:"id"`
	MessageID string      `json:"messageID"`
	SessionID string      `json:"sessionID"`
	Type      string      `json:"type"`
	Text      string      `json:"text"`
	Time      rawPartTime `json:"time,omitempty"`
}

type rawReasoningPart struct {
	ID        string      `json:"id"`
	MessageID string      `json:"messageID"`
	SessionID string      `json:"sessionID"`
	Type      string      `json:"type"`
	Text      string      `json:"text"`
	Time      rawPartTime `json:"time,omitempty"`
}

type rawToolPart struct {
	ID        string       `json:"id"`
	MessageID string       `json:"messageID"`
	SessionID string       `json:"sessionID"`
	Type      string       `json:"type"`
	Tool      string       `json:"tool"`
	CallID    string       `json:"callID"`
	State     rawToolState `json:"state"`
}

// toolMappings gives the kind and main-argument member of each OpenCode 2.x
// tool the adapter reports an argument for.
var toolMappings = map[string]agentcore.ToolMapping{
	"shell": {Kind: domain.ToolKindShell, Key: "command"},
	"read":  {Kind: domain.ToolKindRead, Key: "path"},
	"write": {Kind: domain.ToolKindEdit, Key: "path"},
	"edit":  {Kind: domain.ToolKindEdit, Key: "path"},
	"skill": {Kind: domain.ToolKindSkill, Key: "id"},
}

type rawToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input,omitempty"`
	Output   any             `json:"output,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Error    string          `json:"error,omitempty"`
	Title    string          `json:"title,omitempty"`
	Time     rawPartTime     `json:"time,omitempty"`
}

type rawStepFinishPart struct {
	ID        string          `json:"id"`
	MessageID string          `json:"messageID"`
	SessionID string          `json:"sessionID"`
	Type      string          `json:"type"`
	Reason    string          `json:"reason"`
	Tokens    json.RawMessage `json:"tokens,omitempty"`
}

// childSession is one subagent call a turn's stream showed.
type childSession struct {
	// call is the tool part's id.
	call string

	// session is the child session's id; empty when the stream named none.
	session string

	// completed is true when the part's state.status is "completed".
	completed bool
}

// subagentCall reports whether part is a subagent call and, when it is, the
// child session it started. The members read are the ones the protocol route
// reads from the same tool output: input.agent names the call, and the tool's
// output object, carried as the state's metadata, holds the child's id.
func subagentCall(part rawToolPart, ownSessionID string) (childSession, bool) {
	var input struct {
		Agent string `json:"agent"`
	}
	if json.Unmarshal(part.State.Input, &input) != nil || input.Agent == "" {
		return childSession{}, false
	}
	var output struct {
		Metadata struct {
			SessionID string `json:"sessionID"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(part.State.Metadata, &output)
	child := childSession{call: part.ID, completed: part.State.Status == "completed"}
	if id := output.Metadata.SessionID; id != ownSessionID {
		child.session = id
	}
	return child, true
}

func parseRunEvent(line []byte) (rawRunEvent, error) {
	var event rawRunEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return rawRunEvent{}, fmt.Errorf("parse run event: %w", err)
	}
	return event, nil
}

func parseStepStartPart(raw json.RawMessage) (rawStepStartPart, error) {
	var part rawStepStartPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawStepStartPart{}, err
	}
	return part, nil
}

func parseTextPart(raw json.RawMessage) (rawTextPart, error) {
	var part rawTextPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawTextPart{}, err
	}
	return part, nil
}

func parseReasoningPart(raw json.RawMessage) (rawReasoningPart, error) {
	var part rawReasoningPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawReasoningPart{}, err
	}
	return part, nil
}

func parseToolPart(raw json.RawMessage) (rawToolPart, error) {
	var part rawToolPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawToolPart{}, err
	}
	return part, nil
}

func parseStepFinishPart(raw json.RawMessage) (rawStepFinishPart, error) {
	var part rawStepFinishPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawStepFinishPart{}, err
	}
	return part, nil
}

// freeTierRefusalClause returns a clause naming the free-tier-required
// tools state's policy denies, or "" when runErr does not match the
// free-tier refusal envelope, or the policy denies neither tool.
func freeTierRefusalClause(runErr *rawRunError, pt passthroughConfig) string {
	if runErr == nil || !isFreeTierRefusal(runErr) {
		return ""
	}

	policy, ok := buildPermissionPolicy(pt)
	if !ok {
		return ""
	}

	var denied []string
	for _, tool := range freeTierRequiredTools {
		if policy[tool] == permissionDeny {
			denied = append(denied, tool)
		}
	}

	switch len(denied) {
	case 0:
		return ""
	case 1:
		return "; the opencode.allowed_tools and opencode.denied_tools settings deny " + denied[0] +
			", a tool the runtime's free tier requires"
	default:
		return "; the opencode.allowed_tools and opencode.denied_tools settings deny " +
			strings.Join(denied, " and ") + ", tools the runtime's free tier requires"
	}
}

// isFreeTierRefusal reports whether runErr matches the free-tier
// refusal envelope.
func isFreeTierRefusal(runErr *rawRunError) bool {
	if runErr.Type != freeTierAuthType {
		return false
	}
	status, ok := runErr.Status.(float64)
	if !ok || status != 403 {
		return false
	}
	return strings.Contains(runErr.Message, freeTierMessageMarker)
}
