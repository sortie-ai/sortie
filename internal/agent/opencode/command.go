package opencode

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

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

	allowed := make(map[string]struct{}, len(pt.AllowedTools))
	for _, key := range pt.AllowedTools {
		allowed[key] = struct{}{}
	}

	var conflicts []string
	for _, key := range pt.DeniedTools {
		if _, ok := allowed[key]; ok {
			conflicts = append(conflicts, key)
		}
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		return passthroughConfig{}, fmt.Errorf("allowed_tools and denied_tools overlap: %s", strings.Join(conflicts, ", "))
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
		"OPENCODE_PERMISSION":
		return true
	default:
		return false
	}
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
