package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	// kiroExecuteMethod answers on the default engine; the other engine
	// removes it.
	kiroExecuteMethod = "_kiro.dev/commands/execute"

	// kiroGetUsageMethod answers on the other engine. No handshake advertises
	// it, so it is tried only after the first is refused as unknown.
	kiroGetUsageMethod = "_kiro/account/getUsage"

	kiroUsageCommand = "usage"

	kiroCreditResource = "CREDIT"
)

// kiroV2AgentName is the name the default engine reports at handshake.
const kiroV2AgentName = "Kiro CLI Agent"

const (
	// kiroSessionUpdateMethod carries the default engine's retry_warning.
	kiroSessionUpdateMethod = "_kiro.dev/session/update"

	// kiroRateLimitMethod is the default engine's notification for a usage
	// limit, a throttle, and a capacity shortage alike.
	kiroRateLimitMethod = "_kiro.dev/error/rate_limit"

	kiroRetryWarningUpdate = "retry_warning"

	// The other engine's error types for an account whose credits or overage
	// allowance ran out.
	kiroUsageLimitError = "UsageLimitReachedError"
	kiroOverageError    = "OverageLimitReachedError"

	kiroThrottlingRetryType = "THROTTLING"
)

func init() {
	// The other engine reports no handshake name, so the launch selects the
	// declaration for it.
	declaration := Declaration{
		Live:        usage.Source{Category: usage.SourceProtocol},
		Pull:        kiroPullSource,
		PullWindows: kiroPullWindows,
		Launches:    kiroLaunches,

		PromptError:      kiroPromptError,
		TurnNotification: kiroTurnNotification,
	}
	Declarations.Register(kiroAgentName, declaration)
	Declarations.Register(kiroV2AgentName, declaration)
}

// kiroLaunches reports whether target starts the runtime.
func kiroLaunches(target agentcore.LaunchTarget) bool {
	return commandNamesRuntime(target, kiroExecutableName)
}

// kiroRefusal is a refusal of the account's credits, which belong to the account
// and not to a model.
func kiroRefusal(message string) *Refusal {
	return &Refusal{Message: message, WindowReset: true}
}

// kiroPromptError reads the other engine's classified error. Its error type
// outranks its retry class, and a throttle is transient, never a rejected
// credential.
func kiroPromptError(e jsonrpc.Error) PromptVerdict {
	members, ok := jsonObject(e.Data)
	if !ok {
		return PromptVerdict{}
	}
	errorType, _ := jsonString(members["errorType"])
	if errorType == kiroUsageLimitError || errorType == kiroOverageError {
		return PromptVerdict{Refusal: kiroRefusal("")}
	}
	if retryType, _ := jsonString(members["retryErrorType"]); retryType == kiroThrottlingRetryType {
		return PromptVerdict{Transient: true}
	}
	return PromptVerdict{}
}

// kiroTurnNotification reads the default engine's notifications. The engine
// retries a throttled or capacity-short request and announces each retry, so a
// rate-limit notification with no retry announced before it is a usage limit.
func kiroTurnNotification(sessionID, method string, params json.RawMessage) TurnSignal {
	members, ok := jsonObject(params)
	if !ok {
		return TurnSignal{}
	}
	if id, isString := jsonString(members["sessionId"]); isString && id != sessionID {
		return TurnSignal{}
	}
	switch method {
	case kiroSessionUpdateMethod:
		if kind, _ := jsonString(jsonMember(members["update"], "sessionUpdate")); kind == kiroRetryWarningUpdate {
			return TurnSignal{Retrying: true}
		}
	case kiroRateLimitMethod:
		message, _ := jsonString(members["message"])
		return TurnSignal{Refusal: kiroRefusal(message)}
	}
	return TurnSignal{}
}

var kiroPullSource = usage.Source{
	Strategy: usage.StrategyPull,
	Windows: usage.WindowDeclaration{
		Spans: map[string]usage.Span{kiroCreditResource: {Period: "Monthly"}},
		Units: map[string]string{kiroCreditResource: "credits"},
	},
}

type kiroExecuteParams struct {
	SessionID string           `json:"sessionId"`
	Command   kiroCommandParam `json:"command"`
}

type kiroCommandParam struct {
	Command string   `json:"command"`
	Args    struct{} `json:"args"`
}

type kiroUsageResult struct {
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

type kiroUsage struct {
	BillingCycleReset string               `json:"billingCycleReset"`
	UsageBreakdowns   []kiroUsageBreakdown `json:"usageBreakdowns"`
}

type kiroUsageBreakdown struct {
	ResourceType string   `json:"resourceType"`
	DisplayName  string   `json:"displayName"`
	Used         *float64 `json:"used"`
	Limit        *float64 `json:"limit"`
	HasLimit     bool     `json:"hasLimit"`
}

func kiroPullWindows(ctx context.Context, rt PullRuntime) ([]usage.WindowReading, error) {
	raw, err := kiroRequestUsage(ctx, rt)
	if err != nil {
		return nil, err
	}

	var result kiroUsageResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode kiro usage result: %w", err)
	}
	if result.Success != nil && !*result.Success {
		return nil, errors.New("kiro usage request reported failure")
	}

	// The answer's own object is the usage object on an engine that does not
	// wrap it.
	body := raw
	if len(result.Data) > 0 && string(result.Data) != "null" {
		body = result.Data
	}
	var info kiroUsage
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode kiro usage: %w", err)
	}

	resetsAt := kiroResetsAt(info.BillingCycleReset)
	var readings []usage.WindowReading
	for _, entry := range info.UsageBreakdowns {
		if !entry.HasLimit {
			continue
		}
		readings = append(readings, usage.WindowReading{
			Window:   entry.ResourceType,
			Span:     usage.Span{Scope: entry.DisplayName},
			Used:     entry.Used,
			Limit:    entry.Limit,
			ResetsAt: resetsAt,
		})
	}
	return readings, nil
}

func kiroRequestUsage(ctx context.Context, rt PullRuntime) (json.RawMessage, error) {
	raw, err := rt.Request(ctx, kiroExecuteMethod, kiroExecuteParams{
		SessionID: rt.SessionID(),
		Command:   kiroCommandParam{Command: kiroUsageCommand},
	})
	if errors.Is(err, ErrMethodNotFound) {
		raw, err = rt.Request(ctx, kiroGetUsageMethod, struct{}{})
		if errors.Is(err, ErrMethodNotFound) {
			return nil, usage.ErrPullUnsupported
		}
	}
	if err != nil {
		return nil, fmt.Errorf("kiro usage request: %w", err)
	}
	return raw, nil
}

// kiroResetsAt reads the billing cycle's reset, a calendar date the runtime
// states with no time or zone, as the start of that day in UTC. An absent or
// malformed date leaves the reset unknown.
func kiroResetsAt(date string) int64 {
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return 0
	}
	return day.Unix()
}
