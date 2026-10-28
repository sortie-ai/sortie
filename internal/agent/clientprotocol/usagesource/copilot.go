package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/copilot/sessionstate"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/registry"
)

// copilotHandshakeName is the name Copilot CLI gives in its handshake.
const copilotHandshakeName = "Copilot"

const copilotAgentName = "copilot-cli"

const copilotExecutableName = "copilot"

const copilotPackageName = "@github/copilot"

func init() {
	VerificationAdditions.Register(copilotAgentName, func() VerificationAddition { return copilotVerification{} })
	Declarations.Register(copilotHandshakeName, Declaration{
		Live: usage.Source{
			Strategy: usage.StrategyLive,
			Spend: usage.SpendDeclaration{
				Shape:             usage.SpendProcessCumulative,
				CacheInInput:      true,
				ReasoningInOutput: true,
			},
		},
		PromptSpend: copilotPromptSpend,
		Records: usage.Source{
			Strategy: usage.StrategyRecords,
			Spend: usage.SpendDeclaration{
				Shape:             usage.SpendPerRequest,
				CacheInInput:      true,
				ReasoningInOutput: true,
				ServedModel:       true,
			},
		},
		ReadRecords: copilotReadRecords,
	})
}

type copilotPromptResult struct {
	Usage json.RawMessage `json:"usage"`
}

type copilotPromptUsage struct {
	InputTokens       json.RawMessage `json:"inputTokens"`
	OutputTokens      json.RawMessage `json:"outputTokens"`
	CachedReadTokens  json.RawMessage `json:"cachedReadTokens"`
	CachedWriteTokens json.RawMessage `json:"cachedWriteTokens"`
}

// copilotPromptSpend reads the process-cumulative counts of a prompt result. It
// leaves out totalTokens and thoughtTokens: the ledger derives the total, and
// reasoning is already inside the output count.
func copilotPromptSpend(result json.RawMessage) *usage.SpendReading {
	var decoded copilotPromptResult
	if err := json.Unmarshal(result, &decoded); err != nil || len(decoded.Usage) == 0 || string(decoded.Usage) == "null" {
		return nil
	}
	var counts copilotPromptUsage
	if err := json.Unmarshal(decoded.Usage, &counts); err != nil {
		return nil
	}
	return &usage.SpendReading{
		Input:      jsonCount(counts.InputTokens),
		Output:     jsonCount(counts.OutputTokens),
		CacheRead:  jsonCount(counts.CachedReadTokens),
		CacheWrite: jsonCount(counts.CachedWriteTokens),
	}
}

// jsonCount returns the integer a JSON member holds, nil for an absent, null,
// fractional, or non-numeric member.
func jsonCount(raw json.RawMessage) *int64 {
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// copilotReadRecords returns the usage records of the session's journal in
// file order. The journal lives under COPILOT_HOME when the launch sets it
// and under the user's home directory otherwise.
func copilotReadRecords(ctx context.Context, rt PullRuntime) ([]usage.SpendReading, error) {
	id := rt.SessionID()
	if !sessionstate.ValidSessionID(id) {
		return nil, errors.New("session id is not a safe journal path segment")
	}
	homeVar, rel := "HOME", sessionstate.DefaultHome+"/"+sessionstate.EventsRel(id)
	if home, ok := rt.LaunchEnv(sessionstate.HomeVar); ok && home != "" {
		homeVar, rel = sessionstate.HomeVar, sessionstate.EventsRel(id)
	}
	var readings []usage.SpendReading
	err := rt.ScanHomeFile(ctx, homeVar, rel, sessionstate.MaxFileBytes, sessionstate.MaxLineBytes, func(line []byte) error {
		if reading, ok := sessionstate.UsageRecord(line); ok {
			readings = append(readings, reading)
		}
		return nil
	})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return readings, nil
}

type copilotVerification struct{}

func (copilotVerification) Claim(target agentcore.LaunchTarget) (VerificationSwitches, bool) {
	if !namesRuntime(target, copilotExecutableName, copilotPackageName) {
		return VerificationSwitches{}, false
	}
	return VerificationSwitches{Args: []string{"--available-tools", "view", "--no-custom-instructions"}}, true
}

// Context keeps both instruction files because the runtime loads them over the
// protocol whatever the switch says.
func (copilotVerification) Context() registry.VerificationContext {
	return registry.DeclareVerificationContext(
		registry.VerificationKeepsSomeTools,
		registry.VerificationKeepsSystemPrompt,
		registry.VerificationKeepsProjectInstructions,
		registry.VerificationKeepsUserInstructions,
	)
}

func (copilotVerification) Close() {}
