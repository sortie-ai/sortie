package opencode

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
}

func parsePassthroughConfig(config map[string]any) (passthroughConfig, error) {
	pt := passthroughConfig{
		Model:                    typeutil.StringFrom(config, "model"),
		Agent:                    typeutil.StringFrom(config, "agent"),
		Variant:                  typeutil.StringFrom(config, "variant"),
		Thinking:                 typeutil.BoolFrom(config, "thinking", false),
		Pure:                     typeutil.BoolFrom(config, "pure", false),
		DangerousSkipPermissions: typeutil.BoolFrom(config, "dangerously_skip_permissions", true),
		DisableAutocompact:       typeutil.BoolFrom(config, "disable_autocompact", true),
		AllowedTools:             slices.Clone(typeutil.ExtractStringSlice(config["allowed_tools"])),
		DeniedTools:              slices.Clone(typeutil.ExtractStringSlice(config["denied_tools"])),
	}

	if message := overlapMessage(pt.AllowedTools, pt.DeniedTools); message != "" {
		return passthroughConfig{}, fmt.Errorf("%s", message)
	}

	return pt, nil
}

func buildRunArgs(state *sessionState, prompt string, pt passthroughConfig) []string {
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

func buildRunEnv(base []string, pt passthroughConfig) ([]string, error) {
	managedEnv, err := buildManagedEnv(pt)
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

func buildSSHRemoteCommand(remoteCommand string, extraEnv map[string]string) string {
	if len(extraEnv) == 0 {
		return remoteCommand
	}

	keys := make([]string, 0, len(extraEnv))
	for key := range extraEnv {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	parts := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		parts = append(parts, key+"="+sshutil.ShellQuote(extraEnv[key]))
	}
	parts = append(parts, remoteCommand)

	return strings.Join(parts, " ")
}

func buildManagedEnv(pt passthroughConfig) (map[string]string, error) {
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

// mcpConfigDocumentEntry is one server entry of [mcpConfigDocument].
type mcpConfigDocumentEntry struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Enabled     bool              `json:"enabled"`
}

// renderMCPConfigDocument translates servers into the runtime's own
// MCP configuration document as compact JSON. A nil Server.Enabled
// renders true, matching the runtime's own default.
// buildMCPConfigContent returns the translated configuration document
// a session started with these parameters delivers, or an empty string
// when it delivers none: a remote launch, no generated configuration,
// or a configuration declaring no server. It is the adapter's own
// composition, so a conformance test measures what a session does
// rather than repeating the steps and measuring itself.
func buildMCPConfigContent(mcpConfigPath string, remote bool) (string, error) {
	if mcpConfigPath == "" || remote {
		return "", nil
	}

	servers, err := mcpconfig.Parse(mcpConfigPath)
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		return "", nil
	}
	return renderMCPConfigDocument(servers)
}

// appendMCPConfigEnv appends the delivery variable to env when content
// is non-empty, and returns env unchanged otherwise.
func appendMCPConfigEnv(env []string, content string) []string {
	if content == "" {
		return env
	}
	return append(env, "OPENCODE_CONFIG_CONTENT="+content)
}

func renderMCPConfigDocument(servers []mcpconfig.Server) (string, error) {
	doc := mcpConfigDocument{MCP: make(map[string]mcpConfigDocumentEntry, len(servers))}

	for _, server := range servers {
		enabled := true
		if server.Enabled != nil {
			enabled = *server.Enabled
		}

		switch server.Transport {
		case mcpconfig.TransportStdio:
			doc.MCP[server.Name] = mcpConfigDocumentEntry{
				Type:        "local",
				Command:     append([]string{server.Command}, server.Args...),
				Environment: server.Env,
				Enabled:     enabled,
			}
		case mcpconfig.TransportHTTP:
			doc.MCP[server.Name] = mcpConfigDocumentEntry{
				Type:    "remote",
				URL:     server.URL,
				Headers: server.Headers,
				Enabled: enabled,
			}
		default:
			return "", fmt.Errorf("mcp server %q: entry carries neither command nor url", server.Name)
		}
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal opencode mcp document: %w", err)
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
