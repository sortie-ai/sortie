package usagesource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
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

const opencodeDenyAllRule = `{"action":"*","resource":"*","effect":"deny"}`

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
		Pull:           OpenCodeSource,
		PullWindows:    opencodePullWindows,
		PromptSpend:    sparsePromptSpend,
		ChildSession:   opencodeChildSession,
		Records:        OpenCodeRecords,
		ReadRecords:    opencodeReadRecords,
		LaunchAddition: LaunchAddition{Name: opencodeConfigVariable, Merge: opencodeDisableTitle},
		Launches: func(target agentcore.LaunchTarget) bool {
			return commandNamesRuntime(target, opencodeExecutableName, opencodePackageNames...)
		},
		PromptError: opencodePromptError,
	})
}

// opencodePromptError reads the error name the runtime copies from its session
// error into data. The error code is a generic internal-error code and is not
// read. Only a console-metered name takes a console window's reset: a quota
// refusal can come from a provider the console does not meter.
func opencodePromptError(e jsonrpc.Error) PromptVerdict {
	name, ok := jsonString(jsonMember(e.Data, "errorName"))
	if !ok {
		return PromptVerdict{}
	}
	limit, ok := OpenCodeRefusal(name)
	if !ok {
		return PromptVerdict{}
	}
	return PromptVerdict{Refusal: &Refusal{Name: name, Limit: limit, WindowReset: OpenCodeConsoleMetered(name)}}
}

// opencodeReadRecords returns the records of the session's export, in message
// order, and the refusal the export records for the current turn. The export
// runs on the runtime's host with the runtime's own command. A refusal that
// cannot be read leaves the spend records intact.
func opencodeReadRecords(ctx context.Context, rt PullRuntime) (Records, error) {
	id := rt.SessionID()
	if id == "" {
		return Records{}, errors.New("session id is empty")
	}
	out, err := rt.RunHostCommand(ctx, exportCommand(rt.CommandWords(), id))
	if err != nil {
		return Records{}, fmt.Errorf("run session export: %w", err)
	}
	readings, err := OpenCodeParse(out, id)
	if err != nil {
		return Records{}, err
	}
	return Records{Spend: readings, Refusal: opencodeRecordedRefusal(out, id)}, nil
}

func opencodeRecordedRefusal(export []byte, id string) RecordedRefusal {
	failure, ok, err := OpenCodeLastError(export, id)
	if err != nil || !ok {
		return RecordedRefusal{}
	}
	if _, refusal := OpenCodeRefusal(failure.Type); !refusal {
		return RecordedRefusal{}
	}
	return RecordedRefusal{Name: failure.Type, ResetsAt: OpenCodeRetryAt(failure.FailedAtMS, failure.RetryAfterMS)}
}

// exportCommand is the session export, run as the session's own runtime.
func exportCommand(words []string, id string) agentcore.HostCommand {
	cmd := runtimeCommand(words, OpenCodeArgs(id), opencodeExecutableName, opencodePackageNames...)
	cmd.Limit = usage.MaxRecordsBytes
	return cmd
}

// opencodePullWindows asks the runtime's own limits command, run as the
// session's own runtime.
func opencodePullWindows(ctx context.Context, rt PullRuntime) ([]usage.WindowReading, error) {
	return OpenCodePull(ctx, func(ctx context.Context, args []string) ([]byte, error) {
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

// opencodeDisableTitle merges the title switch into the operator's inline
// configuration, keeping every other member. The runtime reads the document's
// agents.title entry in place of the legacy agent.title entry, whole, so the
// legacy switch is only written when the document defines no agents.title. It
// leaves the value as set and reports false when the operator's own document
// keeps title generation on or has a shape the member cannot be added to.
func opencodeDisableTitle(current string, set bool) (string, bool) {
	if !set || strings.TrimSpace(current) == "" {
		return opencodeTitleDisabledConfig, true
	}
	document, ok := jsonObject([]byte(current))
	if !ok {
		return current, false
	}
	if raw, present := document["agents"]; present {
		agents, ok := jsonObject(raw)
		if !ok {
			return current, false
		}
		if _, titled := agents["title"]; titled {
			return mergeTitleSwitch(current, document, "agents", agents, "disabled")
		}
	}
	agent := map[string]json.RawMessage{}
	if raw, present := document["agent"]; present {
		if agent, ok = jsonObject(raw); !ok {
			return current, false
		}
	}
	return mergeTitleSwitch(current, document, "agent", agent, "disable")
}

func mergeTitleSwitch(current string, document map[string]json.RawMessage, group string, members map[string]json.RawMessage, field string) (string, bool) {
	title := map[string]json.RawMessage{}
	if raw, present := members["title"]; present {
		var ok bool
		if title, ok = jsonObject(raw); !ok {
			return current, false
		}
	}
	if raw, present := title[field]; present {
		return current, string(bytes.TrimSpace(raw)) == "true"
	}
	title[field] = json.RawMessage("true")
	var err error
	if members["title"], err = encodeJSON(title); err != nil {
		return current, false
	}
	if document[group], err = encodeJSON(members); err != nil {
		return current, false
	}
	merged, err := encodeJSON(document)
	if err != nil {
		return current, false
	}
	return string(merged), true
}

// opencodeDenyAll merges a deny-all rule into the operator's inline
// configuration as the last of its top-level rules, keeping every other member.
// The runtime lets the last matching rule decide and concatenates permissions
// after permission, so the rule goes at the end of permissions when the document
// has it. It leaves the value as set and reports false when a rule member has a
// shape the rule cannot be added to.
func opencodeDenyAll(current string, set bool) (string, bool) {
	if !set || strings.TrimSpace(current) == "" {
		return opencodeDenyAllConfig, true
	}
	document, ok := jsonObject([]byte(current))
	if !ok || hasAgentScopedPermission(document) {
		return current, false
	}
	member, rules := "permission", json.RawMessage(nil)
	if raw, present := document["permissions"]; present {
		member = "permissions"
		rules, ok = appendDenyAllRule(raw)
	} else {
		rules, ok = denyAllPermission(document["permission"])
	}
	if !ok {
		return current, false
	}
	document[member] = rules
	merged, err := encodeJSON(document)
	if err != nil {
		return current, false
	}
	return string(merged), true
}

// hasAgentScopedPermission reports whether the document may grant a tool for
// one agent. The runtime applies those rules after the top-level ones, so they
// override the deny-all; a group whose shape is unknown counts as granting.
func hasAgentScopedPermission(document map[string]json.RawMessage) bool {
	for _, group := range []string{"agent", "agents"} {
		raw, present := document[group]
		if !present {
			continue
		}
		var agents map[string]map[string]json.RawMessage
		if json.Unmarshal(raw, &agents) != nil {
			return true
		}
		for _, agent := range agents {
			if _, ok := agent["permission"]; ok {
				return true
			}
			if _, ok := agent["permissions"]; ok {
				return true
			}
		}
	}
	return false
}

func appendDenyAllRule(permissions json.RawMessage) (json.RawMessage, bool) {
	var rules []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(permissions), []byte("[")) || json.Unmarshal(permissions, &rules) != nil {
		return nil, false
	}
	encoded, err := encodeJSON(append(rules, json.RawMessage(opencodeDenyAllRule)))
	return encoded, err == nil
}

// denyAllPermission returns the permission member with its "*" rule replaced by
// a deny and moved last. It writes the members in the order the operator wrote
// them: a Go map would sort the keys, and the last matching rule wins. An absent
// member and a string, which the runtime reads as one "*" rule, yield the
// deny-all alone.
func denyAllPermission(permission json.RawMessage) (json.RawMessage, bool) {
	var members []objectMember
	if len(permission) > 0 && !bytes.HasPrefix(permission, []byte(`"`)) {
		var ok bool
		if members, ok = orderedMembers(permission); !ok {
			return nil, false
		}
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for _, member := range members {
		if member.key == "*" {
			continue
		}
		key, err := encodeJSON(member.key)
		if err != nil {
			return nil, false
		}
		out.Write(key)
		out.WriteByte(':')
		out.Write(member.value)
		out.WriteByte(',')
	}
	out.WriteString(`"*":"deny"}`)
	return out.Bytes(), true
}

type objectMember struct {
	key   string
	value json.RawMessage
}

// orderedMembers returns the members of a JSON object in the order written.
func orderedMembers(raw json.RawMessage) ([]objectMember, bool) {
	if _, ok := jsonObject(raw); !ok {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil, false
	}
	var members []objectMember
	for decoder.More() {
		token, err := decoder.Token()
		key, isString := token.(string)
		if err != nil || !isString {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		members = append(members, objectMember{key: key, value: value})
	}
	return members, true
}

// encodeJSON marshals v without escaping HTML characters, so a member the
// operator wrote keeps the characters it holds.
func encodeJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

type opencodeVerification struct{}

func (opencodeVerification) Claim(target agentcore.LaunchTarget) (VerificationSwitches, bool) {
	if !commandNamesRuntime(target, opencodeExecutableName, opencodePackageNames...) {
		return VerificationSwitches{}, false
	}
	return VerificationSwitches{Merges: []LaunchAddition{{Name: opencodeConfigVariable, Merge: opencodeDenyAll}}}, true
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
