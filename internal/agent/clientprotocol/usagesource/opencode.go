package usagesource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/opencode/sessionexport"
	"github.com/sortie-ai/sortie/internal/agent/opencode/usagelimits"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/registry"
)

// opencodeHandshakeName is the name OpenCode gives in its handshake.
const opencodeHandshakeName = "OpenCode"

const opencodeAgentName = "opencode"

const opencodeExecutableName = "opencode"

// opencodePackageNames are the npm packages that ship the runtime: the 2.x
// package and the 1.x package.
var opencodePackageNames = []string{"@opencode/cli", "opencode-ai"}

const opencodeConfigVariable = "OPENCODE_CONFIG_CONTENT"

// opencodeTitleDisabledConfig is the inline configuration that turns off the
// model request OpenCode makes to title each session it creates.
const opencodeTitleDisabledConfig = `{"agent":{"title":{"disable":true}}}`

// opencodeDenyAllConfig is the inline configuration that removes the tool
// definitions from the request, tool-server tools included, and ends the tool
// calls a verification prompt would otherwise provoke. The runtime appends a
// rule after the configuration that keeps the skill tool declared, so no
// configuration removes that one.
const opencodeDenyAllConfig = `{"permission":{"*":"deny"}}`

func init() {
	VerificationAdditions.Register(opencodeAgentName, func() VerificationAddition { return opencodeVerification{} })
	Declarations.Register(opencodeHandshakeName, Declaration{
		Live: usage.Source{
			Category: usage.SourceProtocol,
			Strategy: usage.StrategyLive,
			Spend: usage.SpendDeclaration{
				Shape:             usage.SpendPerTurn,
				CacheInInput:      false,
				ReasoningInOutput: false,
			},
		},
		Pull:         usagelimits.Source,
		PullWindows:  opencodePullWindows,
		PromptSpend:  opencodePromptSpend,
		ChildSession: opencodeChildSession,
		Records: usage.Source{
			Category: usage.SourceJournal,
			Strategy: usage.StrategyRecords,
			Spend: usage.SpendDeclaration{
				Shape:             usage.SpendPerRequest,
				CacheInInput:      false,
				ReasoningInOutput: false,
				ServedModel:       true,
			},
			WrittenAtTurnEnd: false,
		},
		ReadRecords:    opencodeReadRecords,
		LaunchAddition: LaunchAddition{Name: opencodeConfigVariable, Merge: opencodeDisableTitle},
		Launches: func(target agentcore.LaunchTarget) bool {
			return commandNamesRuntime(target, opencodeExecutableName, opencodePackageNames...)
		},
	})
}

type opencodePromptUsage struct {
	InputTokens       json.RawMessage `json:"inputTokens"`
	OutputTokens      json.RawMessage `json:"outputTokens"`
	ThoughtTokens     json.RawMessage `json:"thoughtTokens"`
	CachedReadTokens  json.RawMessage `json:"cachedReadTokens"`
	CachedWriteTokens json.RawMessage `json:"cachedWriteTokens"`
}

// opencodePromptSpend reads the per-turn counts of a prompt result. The runtime
// omits a counter that is zero, so an absent optional member reads zero while a
// present member that is not an integer reads unknown. The input and output
// counts are never omitted, so absent means unknown for them.
func opencodePromptSpend(result json.RawMessage) *usage.SpendReading {
	var decoded standardPromptResult
	if err := json.Unmarshal(result, &decoded); err != nil || len(decoded.Usage) == 0 || string(decoded.Usage) == "null" {
		return nil
	}
	var counts opencodePromptUsage
	if err := json.Unmarshal(decoded.Usage, &counts); err != nil {
		return nil
	}
	return &usage.SpendReading{
		Input:      jsonCount(counts.InputTokens),
		Output:     jsonCount(counts.OutputTokens),
		Reasoning:  sessionexport.CountOmittedAtZero(counts.ThoughtTokens),
		CacheRead:  sessionexport.CountOmittedAtZero(counts.CachedReadTokens),
		CacheWrite: sessionexport.CountOmittedAtZero(counts.CachedWriteTokens),
	}
}

// opencodeReadRecords returns the records of the session's export, in message
// order. The export runs on the runtime's host with the runtime's own command.
func opencodeReadRecords(ctx context.Context, rt PullRuntime) ([]usage.SpendReading, error) {
	id := rt.SessionID()
	if id == "" {
		return nil, errors.New("session id is empty")
	}
	out, err := rt.RunHostCommand(ctx, exportCommand(rt.CommandWords(), id))
	if err != nil {
		return nil, fmt.Errorf("run session export: %w", err)
	}
	records, err := sessionexport.Parse(out, id)
	if err != nil {
		return nil, err
	}
	readings := make([]usage.SpendReading, len(records))
	for i, record := range records {
		readings[i] = record.Spend
	}
	return readings, nil
}

// exportCommand is the session export, run as the session's own runtime.
func exportCommand(words []string, id string) agentcore.HostCommand {
	cmd := runtimeCommand(words, sessionexport.Args(id), opencodeExecutableName, opencodePackageNames...)
	cmd.Limit = usage.MaxRecordsBytes
	return cmd
}

// opencodePullWindows asks the runtime's own limits command, run as the
// session's own runtime.
func opencodePullWindows(ctx context.Context, rt PullRuntime) ([]usage.WindowReading, error) {
	return usagelimits.Pull(ctx, func(ctx context.Context, args []string) ([]byte, error) {
		return rt.RunHostCommand(ctx, runtimeCommand(rt.CommandWords(), args, opencodeExecutableName, opencodePackageNames...))
	})
}

// opencodeChildSession reads what one session update shows of a subagent
// session: the call that starts it, the call that ends it with its identifier,
// and the chunks the child forwards on the parent's stream.
func opencodeChildSession(sessionID string, update json.RawMessage) ChildSignal {
	var fields struct {
		SessionUpdate json.RawMessage `json:"sessionUpdate"`
		Status        json.RawMessage `json:"status"`
		ToolCallID    json.RawMessage `json:"toolCallId"`
		RawInput      json.RawMessage `json:"rawInput"`
		RawOutput     json.RawMessage `json:"rawOutput"`
		Meta          json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(update, &fields); err != nil {
		return ChildSignal{}
	}
	if kind, _ := jsonString(fields.SessionUpdate); kind == "tool_call_update" {
		status, _ := jsonString(fields.Status)
		call, _ := jsonString(fields.ToolCallID)
		if _, ok := jsonString(jsonMember(fields.RawInput, "agent")); ok && status == "in_progress" {
			return ChildSignal{Call: call, Started: true}
		}
		if status == "completed" || status == "failed" {
			metadata := jsonMember(fields.RawOutput, "metadata")
			if child, ok := jsonString(jsonMember(metadata, "sessionID")); ok && child != sessionID {
				return ChildSignal{Call: call, Session: child, Completed: status == "completed"}
			}
		}
	}
	forwarded := jsonMember(jsonMember(fields.Meta, "opencode/child-session"), "id")
	if child, ok := jsonString(forwarded); ok && child != sessionID {
		return ChildSignal{Session: child}
	}
	return ChildSignal{}
}

// jsonMember returns the raw member of a JSON object, nil when raw is not an
// object or holds no such member.
func jsonMember(raw json.RawMessage, key string) json.RawMessage {
	members, ok := jsonObject(raw)
	if !ok {
		return nil
	}
	return members[key]
}

// jsonString returns the string a JSON value holds; ok is false for an absent,
// null, or non-string value.
func jsonString(raw json.RawMessage) (string, bool) {
	var text string
	if !bytes.HasPrefix(raw, []byte(`"`)) || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

// jsonObject decodes a JSON object keeping each member's raw value; ok is false
// for anything that is not an object, null included.
func jsonObject(raw []byte) (map[string]json.RawMessage, bool) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil, false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, false
	}
	return members, true
}

// opencodeDisableTitle merges agent.title.disable true into the operator's
// inline configuration, keeping every other member. It leaves the value as set
// and reports false when the operator's own document keeps title generation on
// or has a shape the member cannot be added to.
func opencodeDisableTitle(current string, set bool) (string, bool) {
	if !set || strings.TrimSpace(current) == "" {
		return opencodeTitleDisabledConfig, true
	}
	document, ok := jsonObject([]byte(current))
	if !ok {
		return current, false
	}
	agent := map[string]json.RawMessage{}
	if raw, present := document["agent"]; present {
		if agent, ok = jsonObject(raw); !ok {
			return current, false
		}
	}
	title := map[string]json.RawMessage{}
	if raw, present := agent["title"]; present {
		if title, ok = jsonObject(raw); !ok {
			return current, false
		}
	}
	if raw, present := title["disable"]; present {
		return current, string(bytes.TrimSpace(raw)) == "true"
	}
	title["disable"] = json.RawMessage("true")
	var err error
	if agent["title"], err = json.Marshal(title); err != nil {
		return current, false
	}
	if document["agent"], err = json.Marshal(agent); err != nil {
		return current, false
	}
	merged, err := json.Marshal(document)
	if err != nil {
		return current, false
	}
	return string(merged), true
}

type opencodeVerification struct{}

func (opencodeVerification) Claim(target agentcore.LaunchTarget) (VerificationSwitches, bool) {
	if !namesRuntime(target, opencodeExecutableName, opencodePackageNames...) {
		return VerificationSwitches{}, false
	}
	return VerificationSwitches{Env: []string{"OPENCODE_CONFIG_CONTENT=" + opencodeDenyAllConfig}}, true
}

func (opencodeVerification) Context() registry.VerificationContext {
	return registry.DeclareVerificationContext(
		registry.VerificationKeepsSomeTools,
		registry.VerificationKeepsProjectInstructions,
		registry.VerificationKeepsUserInstructions,
		registry.VerificationKeepsSystemPrompt,
	)
}

func (opencodeVerification) Close() {}
