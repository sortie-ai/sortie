package clientprotocol

import (
	"context"
	"encoding/json"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
)

// usageReader supplies token measurements read outside the wire, for a
// runtime whose protocol carries no spend counter. A reader must claim the
// resolved launch target, recognize the build the handshake reports, and
// produce a record on its first drain; failing any of those drops the reader
// and leaves the session unmeasured with the token ceiling inactive.
//
// Drain reports whether a measurement exists at all; Completeness grades how
// far the figure it just returned was proven to reach.
type usageReader interface {
	Claim(target agentcore.LaunchTarget) ([]string, bool)
	Recognize(name, version string) bool
	Open(sessionID string)
	Drain(ctx context.Context, lowerBound int64) (agentcore.RecoveredUsage, string, bool)
	Completeness() usagesource.Completeness
	Close()
}

// selectUsageReader returns the first registered source that claims target,
// together with the environment assignments its launch needs. A nil reader
// means an unmeasured session.
func selectUsageReader(target agentcore.LaunchTarget) (usageReader, []string) {
	for _, reader := range usagesource.Readers() {
		if env, claimed := reader.Claim(target); claimed {
			return reader, env
		}
	}
	return nil, nil
}

// promptQuota is the wire extension a prompt result may carry. It is a
// presence signal and a lower bound, never the accounting figure: it omits
// cache-read and reasoning tokens, and names the serving model wrongly often
// enough that its per-model breakdown is not read here at all.
type promptQuota struct {
	Quota *struct {
		TokenCount struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"token_count"`
	} `json:"quota"`
}

// spendLowerBound returns the input-plus-output figure a prompt result reports
// for its own turn, and whether the extension was present at all. A present
// block reporting zero is a turn that reached no model, which is why presence
// is reported separately from the bound.
func spendLowerBound(meta json.RawMessage) (int64, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	var decoded promptQuota
	if err := json.Unmarshal(meta, &decoded); err != nil || decoded.Quota == nil {
		return 0, false
	}
	count := decoded.Quota.TokenCount
	return max(count.InputTokens, 0) + max(count.OutputTokens, 0), true
}
