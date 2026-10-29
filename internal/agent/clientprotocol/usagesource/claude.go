package usagesource

import (
	"encoding/json"
	"fmt"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const claudeAgentACPName = "@agentclientprotocol/claude-agent-acp"

const claudeRateLimitMember = "_claude/rateLimit"

func init() {
	Declarations.Register(claudeAgentACPName, Declaration{
		Live:               claudeLiveSource,
		UsageUpdateWindows: claudeUsageUpdateWindows,
	})
}

// claudeLiveSource holds the same table as the Claude Code stream source,
// because the bridge forwards the SDK's rate limit report unchanged.
var claudeLiveSource = usage.Source{
	Category: usage.SourceProtocol,
	Windows: usage.WindowDeclaration{
		Share: usage.ShareFraction,
		Statuses: map[string]domain.UsageWindowStatus{
			"allowed":         domain.UsageWindowAllowed,
			"allowed_warning": domain.UsageWindowWarning,
			"rejected":        domain.UsageWindowExhausted,
		},
		Spans: map[string]usage.Span{
			"five_hour":                  {Minutes: 300},
			"seven_day":                  {Minutes: 10080},
			"seven_day_opus":             {Minutes: 10080, Scope: "Opus"},
			"seven_day_sonnet":           {Minutes: 10080, Scope: "Sonnet"},
			"seven_day_overage_included": {Minutes: 10080},
			"overage":                    {Scope: "extra usage"},
		},
	},
}

type claudeRateLimit struct {
	Status        string   `json:"status"`
	ResetsAt      int64    `json:"resetsAt"`
	RateLimitType string   `json:"rateLimitType"`
	Utilization   *float64 `json:"utilization"`
}

func claudeUsageUpdateWindows(meta json.RawMessage) ([]usage.WindowReading, error) {
	if len(meta) == 0 {
		return nil, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(meta, &members); err != nil {
		return nil, fmt.Errorf("decode usage_update _meta: %w", err)
	}
	raw := members[claudeRateLimitMember]
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var info claudeRateLimit
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("decode %s: %w", claudeRateLimitMember, err)
	}
	return []usage.WindowReading{{
		Window:   info.RateLimitType,
		Share:    info.Utilization,
		Status:   info.Status,
		ResetsAt: info.ResetsAt,
	}}, nil
}
