package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

type passthroughConfig struct {
	Model                    string
	Agent                    string
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
	model, fault := typeutil.StringField(config, "model")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	agent, fault := typeutil.StringField(config, "agent")
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
		Variant:                  variant,
		Thinking:                 typeutil.BoolFrom(config, "thinking", false),
		Pure:                     typeutil.BoolFrom(config, "pure", false),
		DangerousSkipPermissions: typeutil.BoolFrom(config, "dangerously_skip_permissions", true),
		DisableAutocompact:       typeutil.BoolFrom(config, "disable_autocompact", true),
		AllowedTools:             slices.Clone(typeutil.ExtractStringSlice(config["allowed_tools"])),
		DeniedTools:              slices.Clone(typeutil.ExtractStringSlice(config["denied_tools"])),
	}, nil
}

// checkCrossField rejects a passthrough whose allowed_tools and
// denied_tools overlap.
func checkCrossField(pt passthroughConfig) error {
	if message := overlapMessage(pt.AllowedTools, pt.DeniedTools); message != "" {
		return fmt.Errorf("%s", message)
	}
	return nil
}

// buildRunArgs builds one turn's argument vector for state.major's
// contract: 1.x takes the workspace on --dir and the prompt as the
// final, "--"-separated positional argument; 2.x takes neither, since
// its working directory comes from PWD (agentcore.LaunchTarget.BindWorkspace)
// and its prompt from standard input.
func buildRunArgs(state *sessionState, prompt string, pt passthroughConfig) []string {
	if state.major == major2 {
		args := []string{"run", "--format", "json", "--standalone"}
		if state.sessionID != "" {
			args = append(args, "--session", state.sessionID)
		}
		if pt.Model != "" {
			model := pt.Model
			if pt.Variant != "" {
				model += "#" + pt.Variant
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

	args := []string{"run", "--format", "json", "--dir", state.target.WorkspacePath}

	if state.sessionID != "" {
		args = append(args, "--session", state.sessionID)
	}
	if pt.Model != "" {
		args = append(args, "--model", pt.Model)
	}
	if pt.Agent != "" {
		args = append(args, "--agent", pt.Agent)
	}
	if pt.Variant != "" {
		args = append(args, "--variant", pt.Variant)
	}
	if pt.Thinking {
		args = append(args, "--thinking")
	}
	if pt.Pure {
		args = append(args, "--pure")
	}
	if pt.DangerousSkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}

	args = append(args, "--", prompt)
	return args
}

// buildTurnStdin returns the reader a turn's subprocess reads its
// prompt from. On major1, preamble alone (nil on a local launch, the
// SSH preamble otherwise): the prompt already rode on buildRunArgs's
// positional argument. On major2, prompt follows preamble, since 2.x
// takes no positional prompt.
func buildTurnStdin(major runtimeMajor, prompt string, preamble io.Reader) io.Reader {
	if major != major2 {
		return preamble
	}
	promptReader := strings.NewReader(prompt)
	if preamble == nil {
		return promptReader
	}
	return io.MultiReader(preamble, promptReader)
}

// exportArgs returns the usage-recovery invocation's argument vector
// for major.
func exportArgs(major runtimeMajor, sessionID string) []string {
	if major == major2 {
		return []string{"session", "export", "--standalone", "--sanitize", sessionID}
	}
	return []string{"export", "--sanitize", sessionID}
}

// deleteArgs returns the session-deletion invocation's argument vector
// for major.
func deleteArgs(major runtimeMajor, sessionID string) []string {
	if major == major2 {
		return []string{"session", "delete", "--standalone", sessionID}
	}
	return []string{"session", "delete", sessionID}
}

func buildRunEnv(base []string, pt passthroughConfig, major runtimeMajor) ([]string, error) {
	managedEnv, err := buildManagedEnv(pt, major)
	if err != nil {
		return nil, err
	}

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

	return env, nil
}

// buildTurnEnv returns the environment every local turn's subprocess
// carries: the scrubbed base environment and managed settings
// [buildRunEnv] builds for state.major, with state.turnConfigContent
// appended as OPENCODE_CONFIG_CONTENT when non-empty. It is the one
// path a turn's environment is built from, on both majors.
func buildTurnEnv(state *sessionState) ([]string, error) {
	env, err := buildRunEnv(os.Environ(), state.passthrough, state.major)
	if err != nil {
		return nil, err
	}
	if state.turnConfigContent != "" {
		env = append(env, "OPENCODE_CONFIG_CONTENT="+state.turnConfigContent)
	}
	return env, nil
}

// auxiliaryCommand builds a one-shot opencode subcommand through the
// session's launch target, carrying the same environment and managed
// variables every working turn carries, so a delete, an export query,
// and a models query never diverge in what they run with.
func auxiliaryCommand(ctx context.Context, state *sessionState, args []string) (*exec.Cmd, error) {
	env, err := buildRunEnv(os.Environ(), state.passthrough, state.major)
	if err != nil {
		return nil, err
	}
	managedEnv, err := buildManagedEnv(state.passthrough, state.major)
	if err != nil {
		return nil, err
	}
	cmd, agentErr := state.target.AuxiliaryCommand(ctx, args, nil, env, sortedEnvVars(managedEnv)...)
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
// sets on every launch for major. On major2, tool policy, sharing, and
// compaction ride in the turn's inline configuration document instead,
// so the only managed variable is the update check.
func buildManagedEnv(pt passthroughConfig, major runtimeMajor) (map[string]string, error) {
	if major == major2 {
		return map[string]string{"OPENCODE_DISABLE_AUTOUPDATE": "true"}, nil
	}

	managed := map[string]string{
		"OPENCODE_AUTO_SHARE":           "false",
		"OPENCODE_DISABLE_AUTOCOMPACT":  strconv.FormatBool(pt.DisableAutocompact),
		"OPENCODE_DISABLE_AUTOUPDATE":   "true",
		"OPENCODE_DISABLE_LSP_DOWNLOAD": "true",
	}

	policy, ok := buildPermissionPolicy(pt)
	if !ok {
		return managed, nil
	}

	encoded, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("marshal opencode permission policy: %w", err)
	}
	managed["OPENCODE_PERMISSION"] = string(encoded)

	return managed, nil
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

// mcpConfigDocument is the runtime's own inline MCP configuration
// document shape, delivered through its inline-configuration
// environment variable.
type mcpConfigDocument struct {
	MCP map[string]mcpConfigDocumentEntry `json:"mcp"`
}

// mcpConfigDocumentEntry is one server entry of [mcpConfigDocument] and
// of [inlineConfigDocument].
type mcpConfigDocumentEntry struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Enabled     bool              `json:"enabled"`
}

// inlineConfigDocument is the 2.x OPENCODE_CONFIG_CONTENT value: the
// runtime's own inline configuration document, carrying the tool
// policy, sharing, compaction, and tool servers a 1.x session instead
// spreads across several environment variables and its own MCP
// document.
type inlineConfigDocument struct {
	Permission permissionPolicy                  `json:"permission,omitempty"`
	Share      string                            `json:"share"`
	Compaction *inlineCompaction                 `json:"compaction,omitempty"`
	MCP        map[string]mcpConfigDocumentEntry `json:"mcp,omitempty"`
}

// inlineCompaction is [inlineConfigDocument]'s compaction member.
type inlineCompaction struct {
	Auto bool `json:"auto"`
}

// translateMCPServers parses mcpConfigPath into the runtime's own MCP
// server entries, or returns nil for a remote launch, an empty path,
// or a configuration declaring no server. A nil Server.Enabled renders
// true, matching the runtime's own default.
func translateMCPServers(mcpConfigPath string, remote bool) (map[string]mcpConfigDocumentEntry, error) {
	if mcpConfigPath == "" || remote {
		return nil, nil
	}

	servers, err := mcpconfig.Parse(mcpConfigPath)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, nil
	}
	return buildMCPConfigEntries(servers)
}

// buildTurnConfigContent returns the turn-carried configuration value
// [sessionState.turnConfigContent] holds for major: the 1.x shape,
// {"mcp":servers} rendered by json.Marshal, non-empty only when servers
// is non-empty; or the 2.x inline document [buildInlineConfig] builds.
func buildTurnConfigContent(major runtimeMajor, pt passthroughConfig, servers map[string]mcpConfigDocumentEntry) (string, error) {
	if major == major2 {
		return buildInlineConfig(pt, servers)
	}
	if len(servers) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(mcpConfigDocument{MCP: servers})
	if err != nil {
		return "", fmt.Errorf("marshal opencode mcp document: %w", err)
	}
	return string(encoded), nil
}

// buildInlineConfig returns the 2.x OPENCODE_CONFIG_CONTENT document:
// the session's tool policy, sharing fixed to disabled, autocompaction
// disabled when pt asks for it, and servers when non-empty.
func buildInlineConfig(pt passthroughConfig, servers map[string]mcpConfigDocumentEntry) (string, error) {
	doc := inlineConfigDocument{Share: "disabled"}
	if policy, ok := buildPermissionPolicy(pt); ok {
		doc.Permission = policy
	}
	if pt.DisableAutocompact {
		doc.Compaction = &inlineCompaction{Auto: false}
	}
	if len(servers) > 0 {
		doc.MCP = servers
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal opencode inline configuration: %w", err)
	}
	return string(encoded), nil
}

// buildMCPConfigEntries translates servers into the runtime's own MCP
// server-entry shape, shared by [translateMCPServers] and
// [buildTurnConfigContent] so the two never drift on how a server
// renders.
func buildMCPConfigEntries(servers []mcpconfig.Server) (map[string]mcpConfigDocumentEntry, error) {
	entries := make(map[string]mcpConfigDocumentEntry, len(servers))

	for _, server := range servers {
		enabled := true
		if server.Enabled != nil {
			enabled = *server.Enabled
		}

		switch server.Transport {
		case mcpconfig.TransportStdio:
			entries[server.Name] = mcpConfigDocumentEntry{
				Type:        "local",
				Command:     append([]string{server.Command}, server.Args...),
				Environment: server.Env,
				Enabled:     enabled,
			}
		case mcpconfig.TransportHTTP:
			entries[server.Name] = mcpConfigDocumentEntry{
				Type:    "remote",
				URL:     server.URL,
				Headers: server.Headers,
				Enabled: enabled,
			}
		default:
			return nil, fmt.Errorf("mcp server %q: entry carries neither command nor url", server.Name)
		}
	}

	return entries, nil
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
