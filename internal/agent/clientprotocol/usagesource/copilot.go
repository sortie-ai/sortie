package usagesource

import (
	"context"
	"errors"
	"io"
	"io/fs"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/copilot/quota"
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
		Pull:        quota.Source,
		PullWindows: copilotPullWindows,
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
		UsageUpdateBeforeRequest: true,
	})
}

// copilotPullWindows asks for the quota through the shared pull. The live
// channel carries no quota, so this is the declaration's only window source.
func copilotPullWindows(ctx context.Context, rt PullRuntime) ([]usage.WindowReading, error) {
	return quota.Pull(ctx, copilotQuotaRuntime{rt})
}

// copilotQuotaRuntime lets the quota pull reach the session's runtime.
type copilotQuotaRuntime struct {
	rt PullRuntime
}

func (r copilotQuotaRuntime) Env(name string) (string, bool) {
	return r.rt.LaunchEnv(name)
}

// Exchange cuts the configured command after the runtime word, which drops the
// ACP switch the headless server cannot take.
func (r copilotQuotaRuntime) Exchange(ctx context.Context, args []string, limit int64, exchange func(stdin io.Writer, stdout io.Reader) error) error {
	cmd := runtimeCommand(r.rt.CommandWords(), args, copilotExecutableName, copilotPackageName)
	cmd.Limit = limit
	return r.rt.RunHostExchange(ctx, cmd, exchange)
}

// copilotReadRecords returns the usage records of the session's journal in
// file order. The runtime's host resolves the journal's home, so a local and
// a remote launch read it the same way.
func copilotReadRecords(ctx context.Context, rt PullRuntime) ([]usage.SpendReading, error) {
	var readings []usage.SpendReading
	err := rt.ScanHomeFile(ctx, sessionstate.JournalPath(rt.SessionID()), sessionstate.MaxFileBytes, sessionstate.MaxLineBytes, func(line []byte) error {
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
