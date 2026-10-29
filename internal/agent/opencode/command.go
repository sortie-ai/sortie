package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

type passthroughConfig struct {
	Model                    string
	Agent                    string
	Effort                   string
	Variant                  string
	Thinking                 bool
	Pure                     bool
	DangerousSkipPermissions bool
	DisableAutocompact       bool
	AllowedTools             []string
	DeniedTools              []string
}

type permissionAction string

const (
	permissionAllow permissionAction = "allow"
	permissionAsk   permissionAction = "ask"
	permissionDeny  permissionAction = "deny"
)

type permissionPolicy map[string]permissionAction

var knownPermissionKeys = map[string]struct{}{
	"bash":               {},
	"codesearch":         {},
	"doom_loop":          {},
	"edit":               {},
	"external_directory": {},
	"glob":               {},
	"grep":               {},
	"list":               {},
	"lsp":                {},
	"question":           {},
	"read":               {},
	"skill":              {},
	"task":               {},
	"todowrite":          {},
	"webfetch":           {},
	"websearch":          {},

	"opencode_list_mcp_resources": {},
	"opencode_models":             {},
	"opencode_read_mcp_resource":  {},
	"opencode_session_move":       {},
	"opencode_session_rename":     {},
}

// parsePassthroughConfig extracts OpenCode-specific settings from the
// raw config map. A missing key uses its zero-value default. A key
// present with a non-string value for a string field reports a fault
// rather than defaulting; [checkCrossField] holds the allowed_tools and
// denied_tools overlap check.
func parsePassthroughConfig(config map[string]any) (passthroughConfig, *typeutil.TypeFault) {
	model, fault := registry.ModelSetting(config)
	if fault != nil {
		return passthroughConfig{}, fault
	}
	agent, fault := typeutil.StringField(config, "agent")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	effort, fault := registry.EffortSetting(config)
	if fault != nil {
		return passthroughConfig{}, fault
	}
	variant, fault := typeutil.StringField(config, "variant")
	if fault != nil {
		return passthroughConfig{}, fault
	}

	return passthroughConfig{
		Model:                    model,
		Agent:                    agent,
		Effort:                   effort,
		Variant:                  variant,
		Thinking:                 typeutil.BoolFrom(config, "thinking", false),
		Pure:                     typeutil.BoolFrom(config, "pure", false),
		DangerousSkipPermissions: typeutil.BoolFrom(config, "dangerously_skip_permissions", true),
		DisableAutocompact:       typeutil.BoolFrom(config, "disable_autocompact", true),
		AllowedTools:             slices.Clone(typeutil.ExtractStringSlice(config["allowed_tools"])),
		DeniedTools:              slices.Clone(typeutil.ExtractStringSlice(config["denied_tools"])),
	}, nil
}

// variantSlot returns the value that fills the runtime's one model-variant
// slot and the settings key it was written under. effort takes the slot
// when set; a passthrough setting both is refused before a session starts.
func (pt passthroughConfig) variantSlot() (value, key string) {
	if pt.Effort != "" {
		return pt.Effort, registry.EffortKey
	}
	return pt.Variant, "variant"
}

// variantConflictMessage returns the byte-identical message both
// [validateConfig] and [OpenCodeAdapter.StartSession] report when effort
// and variant are both set, or "" otherwise.
func variantConflictMessage(pt passthroughConfig) string {
	if pt.Effort == "" || pt.Variant == "" {
		return ""
	}
	return fmt.Sprintf("opencode.%s and opencode.variant both set the model variant; set one of them", registry.EffortKey)
}

// checkCrossField rejects a passthrough whose allowed_tools and
// denied_tools overlap, whose effort and variant are both set, that sets
// pure, or whose model-variant slot has no plain model to attach to.
func checkCrossField(pt passthroughConfig) error {
	if message := overlapMessage(pt.AllowedTools, pt.DeniedTools); message != "" {
		return fmt.Errorf("%s", message)
	}
	if message := variantConflictMessage(pt); message != "" {
		return fmt.Errorf("%s", message)
	}
	if pt.Pure {
		return errors.New("opencode.pure is not supported by OpenCode 2.x; remove it")
	}

	slot, slotKey := pt.variantSlot()
	switch {
	case slot != "" && pt.Model == "":
		return fmt.Errorf("opencode.%[1]s needs opencode.model on OpenCode 2.x; set opencode.model or remove opencode.%[1]s", slotKey)
	case slot != "" && strings.Contains(pt.Model, "#"):
		return fmt.Errorf("opencode.model already names a variant after #; remove that suffix or remove opencode.%s", slotKey)
	}
	return nil
}

// buildRunArgs builds one turn's argument vector. The runtime takes its
// working directory from PWD (agentcore.LaunchTarget.BindWorkspace) and
// its prompt from standard input, so neither rides on the command line.
func buildRunArgs(state *sessionState, pt passthroughConfig) []string {
	slot, _ := pt.variantSlot()
	args := []string{"run", "--format", "json", "--standalone"}
	if state.sessionID != "" {
		args = append(args, "--session", state.sessionID)
	}
	if pt.Model != "" {
		model := pt.Model
		if slot != "" {
			model += "#" + slot
		}
		args = append(args, "--model", model)
	}
	if pt.Agent != "" {
		args = append(args, "--agent", pt.Agent)
	}
	if pt.Thinking {
		args = append(args, "--thinking")
	}
	if pt.DangerousSkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// deleteArgs returns the session-deletion invocation's argument vector.
func deleteArgs(sessionID string) []string {
	return []string{"session", "delete", "--standalone", sessionID}
}

func buildRunEnv(base []string) []string {
	managedEnv := buildManagedEnv()

	env := make([]string, 0, len(base)+len(managedEnv))
	for _, entry := range base {
		if shouldDropManagedEnv(entry) {
			continue
		}
		env = append(env, entry)
	}

	keys := make([]string, 0, len(managedEnv))
	for key := range managedEnv {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		env = append(env, key+"="+managedEnv[key])
	}

	return env
}

// buildTurnEnv returns the environment every local turn's subprocess
// carries: the scrubbed base environment and managed settings
// [buildRunEnv] builds, with state.turnConfigContent appended as
// OPENCODE_CONFIG_CONTENT when non-empty. It is the one path a turn's
// environment is built from.
func buildTurnEnv(state *sessionState) []string {
	env := buildRunEnv(os.Environ())
	if state.turnConfigContent != "" {
		env = append(env, "OPENCODE_CONFIG_CONTENT="+state.turnConfigContent)
	}
	return env
}

// auxiliaryCommand builds a one-shot opencode subcommand through the
// session's launch target, carrying the same environment and managed
// variables every working turn carries, so a delete and an export query
// never diverge in what they run with.
func auxiliaryCommand(ctx context.Context, state *sessionState, args []string) (*exec.Cmd, error) {
	cmd, agentErr := state.target.AuxiliaryCommand(ctx, args, nil, buildRunEnv(os.Environ()), sortedEnvVars(buildManagedEnv())...)
	if agentErr != nil {
		return nil, agentErr
	}
	return cmd, nil
}

// sortedEnvVars converts managed into a name-sorted slice of
// [sshutil.EnvVar], so every SSH launch site carries the adapter's
// managed settings to [agentcore.LaunchTarget.SSHOptions] in the same
// deterministic order.
func sortedEnvVars(managed map[string]string) []sshutil.EnvVar {
	keys := make([]string, 0, len(managed))
	for key := range managed {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	vars := make([]sshutil.EnvVar, 0, len(keys))
	for _, key := range keys {
		vars = append(vars, sshutil.EnvVar{Name: key, Value: managed[key]})
	}
	return vars
}

// buildManagedEnv returns the environment variables the adapter itself
// sets on every launch. Tool policy, sharing, and compaction ride in the
// turn's inline configuration document, so the only managed variable is
// the update check.
func buildManagedEnv() map[string]string {
	return map[string]string{"OPENCODE_DISABLE_AUTOUPDATE": "true"}
}

func buildPermissionPolicy(pt passthroughConfig) (permissionPolicy, bool) {
	if len(pt.AllowedTools) == 0 && len(pt.DeniedTools) == 0 {
		return nil, false
	}

	policy := make(permissionPolicy, len(pt.AllowedTools)+len(pt.DeniedTools)+len(knownPermissionKeys))
	allowed := make(map[string]struct{}, len(pt.AllowedTools))
	for _, key := range pt.AllowedTools {
		allowed[key] = struct{}{}
		policy[key] = permissionAllow
		logUnknownPermissionKey(key)
	}

	if len(pt.AllowedTools) > 0 {
		for key := range knownPermissionKeys {
			if _, ok := allowed[key]; ok {
				continue
			}
			policy[key] = permissionDeny
		}
	}

	for _, key := range pt.DeniedTools {
		policy[key] = permissionDeny
		logUnknownPermissionKey(key)
	}

	return policy, true
}

func shouldDropManagedEnv(entry string) bool {
	key, _, found := strings.Cut(entry, "=")
	if !found {
		return false
	}

	switch key {
	case "OPENCODE_AUTO_SHARE",
		"OPENCODE_DISABLE_AUTOCOMPACT",
		"OPENCODE_DISABLE_AUTOUPDATE",
		"OPENCODE_DISABLE_LSP_DOWNLOAD",
		"OPENCODE_PERMISSION",
		"OPENCODE_CONFIG_CONTENT":
		return true
	default:
		return false
	}
}

// agentConfigDocument is the per-agent member of the turn's inline
// configuration document.
type agentConfigDocument struct {
	Title agentSwitch `json:"title"`
}

// agentSwitch is one agent's enablement setting.
type agentSwitch struct {
	Disable bool `json:"disable"`
}

// titleAgentDisabled returns the agent member that turns off the
// runtime's title agent. The title request it would make is billed but
// recorded in no message the per-message usage sum reads.
func titleAgentDisabled() *agentConfigDocument {
	return &agentConfigDocument{Title: agentSwitch{Disable: true}}
}

// inlineConfigDocument is the OPENCODE_CONFIG_CONTENT value: the
// runtime's own inline configuration document, carrying the tool
// policy, sharing, compaction, and tool servers.
type inlineConfigDocument struct {
	Permission permissionPolicy     `json:"permission,omitempty"`
	Share      string               `json:"share"`
	Compaction *inlineCompaction    `json:"compaction,omitempty"`
	Agent      *agentConfigDocument `json:"agent,omitempty"`
	MCP        json.RawMessage      `json:"mcp,omitempty"`
}

// inlineCompaction is [inlineConfigDocument]'s compaction member.
type inlineCompaction struct {
	Auto bool `json:"auto"`
}

// buildInlineConfig returns the OPENCODE_CONFIG_CONTENT document: the
// session's tool policy, sharing fixed to disabled, autocompaction disabled
// when pt asks for it, the title agent disabled, and mcp, the tool-server
// delivery, when non-empty. Unless pt skips permissions, the policy sets
// external_directory to ask when the tool lists leave it undecided, because
// OpenCode 2.x applies no default for it.
func buildInlineConfig(pt passthroughConfig, mcp []byte) (string, error) {
	policy, _ := buildPermissionPolicy(pt)
	if !pt.DangerousSkipPermissions {
		if policy == nil {
			policy = permissionPolicy{}
		}
		if _, decided := policy["external_directory"]; !decided {
			policy["external_directory"] = permissionAsk
		}
	}
	return marshalInlineConfig(pt, policy, mcp)
}

// buildVerificationInlineConfig denies every tool because the runtime offers no
// other switch that removes tool definitions without touching its provider or
// model resolution, and denied tools also end the tool calls that wrote a file
// into the workspace. The runtime appends its own skill rule after the
// configuration, so that one tool stays declared but unusable.
func buildVerificationInlineConfig(pt passthroughConfig, mcp []byte) (string, error) {
	return marshalInlineConfig(pt, permissionPolicy{"*": permissionDeny}, mcp)
}

// buildTurnConfig returns the reduced verification document when reduced, the
// working one otherwise, with mcp as its "mcp" member.
func buildTurnConfig(pt passthroughConfig, reduced bool, mcp []byte) (string, error) {
	if reduced {
		return buildVerificationInlineConfig(pt, mcp)
	}
	return buildInlineConfig(pt, mcp)
}

func marshalInlineConfig(pt passthroughConfig, policy permissionPolicy, mcp []byte) (string, error) {
	doc := inlineConfigDocument{Share: "disabled", Agent: titleAgentDisabled(), Permission: policy}
	if pt.DisableAutocompact {
		doc.Compaction = &inlineCompaction{Auto: false}
	}
	doc.MCP = mcp

	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal opencode inline configuration: %w", err)
	}
	return string(encoded), nil
}

func logUnknownPermissionKey(key string) {
	if _, ok := knownPermissionKeys[key]; ok {
		return
	}

	slog.Default().With(slog.String("component", "opencode-adapter")).Debug(
		"forwarding unknown opencode permission key",
		slog.String("permission_key", key),
	)
}
