package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/copilot/quota"
	"github.com/sortie-ai/sortie/internal/agent/copilot/sessionstate"
	copilotlimits "github.com/sortie-ai/sortie/internal/agent/copilot/usagelimits"
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
		InitializeMeta:           copilotInitializeMeta(),
		TurnNotification:         copilotTurnNotification,
	})
}

// copilotSessionEventMethod is the notification that carries the session
// events the initialize request subscribed to.
const copilotSessionEventMethod = "github.com/copilot/sessionEvent"

// copilotInitializeMeta subscribes to the session error event, which the
// runtime sends only to a client that asks for it in the handshake.
func copilotInitializeMeta() json.RawMessage {
	meta, _ := json.Marshal(map[string]map[string][]string{ // a map of strings always encodes
		"github.com/copilot": {"events": {copilotlimits.SessionErrorEvent}},
	})
	return meta
}

// copilotTurnNotification reads a session error event of the session's own. The
// runtime reports a refused turn as agent text and an end_turn stop reason, so
// this event is the only structured sign of the refusal.
func copilotTurnNotification(sessionID, method string, params json.RawMessage) TurnSignal {
	if method != copilotSessionEventMethod {
		return TurnSignal{}
	}
	members, ok := jsonObject(params)
	if !ok {
		return TurnSignal{}
	}
	if id, isString := jsonString(members["sessionId"]); isString && id != sessionID {
		return TurnSignal{}
	}
	if eventType, _ := jsonString(members["type"]); eventType != copilotlimits.SessionErrorEvent {
		return TurnSignal{}
	}
	failure, err := copilotlimits.ParseSessionError(members["data"])
	if err != nil {
		return TurnSignal{}
	}
	refusal, ok := copilotlimits.RefusalOf(failure)
	if !ok {
		return TurnSignal{}
	}
	return TurnSignal{Refusal: &Refusal{Message: failure.Message, Limit: refusal.Limit, WindowReset: refusal.QuotaWindow}}
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
func copilotReadRecords(ctx context.Context, rt PullRuntime) (Records, error) {
	var readings []usage.SpendReading
	err := rt.ScanHomeFile(ctx, sessionstate.JournalPath(rt.SessionID()), sessionstate.MaxFileBytes, sessionstate.MaxLineBytes, func(line []byte) error {
		if reading, ok := sessionstate.UsageRecord(line); ok {
			readings = append(readings, reading)
		}
		return nil
	})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Records{}, nil
	case err != nil:
		return Records{}, err
	}
	return Records{Spend: readings}, nil
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
