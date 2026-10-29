package usagesource

import (
	"encoding/json"
	"fmt"

	"github.com/sortie-ai/sortie/internal/agent/claude/ratelimit"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const claudeAgentACPName = "@agentclientprotocol/claude-agent-acp"

const claudeRateLimitMember = "_claude/rateLimit"

func init() {
	Declarations.Register(claudeAgentACPName, Declaration{
		Live:               claudeLiveSource,
		UsageUpdateWindows: claudeUsageUpdateWindows,
	})
}

// claudeLiveSource reads the Claude Code window table, because the bridge
// forwards the SDK's rate limit report unchanged.
var claudeLiveSource = usage.Source{Category: usage.SourceProtocol, Windows: ratelimit.Windows}

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
	var info ratelimit.Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("decode %s: %w", claudeRateLimitMember, err)
	}
	return info.Readings(), nil
}
