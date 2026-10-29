package usagesource

import (
	"context"
	"errors"
	"io/fs"

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
			Category: usage.SourceProtocol,
			Strategy: usage.StrategyLive,
			Spend: usage.SpendDeclaration{
				Shape:             usage.SpendProcessCumulative,
				CacheInInput:      true,
				ReasoningInOutput: true,
			},
		},
		PromptSpend: standardPromptSpend,
		Records: usage.Source{
			Category: usage.SourceJournal,
			Strategy: usage.StrategyRecords,
			Spend: usage.SpendDeclaration{
				Shape:             usage.SpendPerRequest,
				CacheInInput:      true,
				ReasoningInOutput: true,
				ServedModel:       true,
			},
			WrittenAtTurnEnd: true,
		},
		ReadRecords: copilotReadRecords,
		Launches: func(target agentcore.LaunchTarget) bool {
			return commandNamesRuntime(target, copilotExecutableName, copilotPackageName)
		},
	})
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
