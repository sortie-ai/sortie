package main

import (
	"fmt"
	"log/slog"
	"net"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/logging"
)

func resolveWorkflowPath(args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("too many arguments")
	}
	raw := "./WORKFLOW.md"
	if len(args) == 1 {
		raw = args[0]
	}
	return filepath.Abs(raw)
}

// trackerConfigMap converts typed [config.TrackerConfig] fields into
// the raw map expected by [registry.TrackerConstructor]. Adapter
// packages extract their required fields from this map.
func trackerConfigMap(tc config.TrackerConfig) map[string]any {
	return map[string]any{
		"kind":              tc.Kind,
		"endpoint":          tc.Endpoint,
		"api_key":           tc.APIKey,
		"project":           tc.Project,
		"active_states":     tc.ActiveStates,
		"terminal_states":   tc.TerminalStates,
		"query_filter":      tc.QueryFilter,
		"handoff_state":     tc.HandoffState,
		"in_progress_state": tc.InProgressState,
		"comments": map[string]any{
			"on_dispatch":   tc.Comments.OnDispatch,
			"on_completion": tc.Comments.OnCompletion,
			"on_failure":    tc.Comments.OnFailure,
		},
	}
}

// agentConfigMap converts typed [config.AgentConfig] fields into the
// raw map expected by [registry.AgentConstructor]. Adapter packages
// extract their required fields from this map.
//
// Orchestrator-only fields (max_turns, max_concurrent_agents,
// max_retry_backoff_ms, max_concurrent_agents_by_state) are
// intentionally excluded. They are consumed by the orchestrator via
// the typed [config.AgentConfig] before this map reaches the adapter
// constructor, and including them would shadow adapter-specific
// extension keys of the same name during [mergeExtensions].
func agentConfigMap(ac config.AgentConfig) map[string]any {
	return map[string]any{
		"kind":             ac.Kind,
		"command":          ac.Command,
		"turn_timeout_ms":  ac.TurnTimeoutMS,
		"read_timeout_ms":  ac.ReadTimeoutMS,
		"stall_timeout_ms": ac.StallTimeoutMS,
	}
}

// mergeExtensions copies adapter-specific config from the Extensions
// map into dst. Adapters may define their own configuration fields
// in a sub-object named after their kind value. Existing keys in dst
// are not overwritten.
func mergeExtensions(dst map[string]any, extensions map[string]any, kind string) {
	sub, ok := extensions[kind].(map[string]any)
	if !ok {
		return
	}
	for k, v := range sub {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
}

// mergeTrackerCredentials copies api_key, project, and endpoint from
// the tracker config into dst when the corresponding key is absent.
// Called only when the CI provider kind matches the tracker kind so
// that shared-platform credentials flow to the CI adapter without
// operator duplication.
func mergeTrackerCredentials(dst map[string]any, tc config.TrackerConfig) {
	if _, ok := dst["api_key"]; !ok && tc.APIKey != "" {
		dst["api_key"] = tc.APIKey
	}
	if _, ok := dst["project"]; !ok && tc.Project != "" {
		dst["project"] = tc.Project
	}
	if _, ok := dst["endpoint"]; !ok && tc.Endpoint != "" {
		dst["endpoint"] = tc.Endpoint
	}
}

// resolveDBPath returns the effective database file path. An absolute
// cfgPath is used as-is. A relative cfgPath is joined with workflowDir
// so database location is deterministic regardless of the process CWD.
// An empty cfgPath falls back to .sortie.db inside workflowDir.
func resolveDBPath(cfgPath, workflowDir string) string {
	if cfgPath == "" {
		return filepath.Join(workflowDir, ".sortie.db")
	}
	if filepath.IsAbs(cfgPath) {
		return cfgPath
	}
	return filepath.Join(workflowDir, cfgPath)
}

// resolveServerPort determines the effective HTTP server port from the
// CLI flag and workflow extensions. Returns the port, whether the
// server should be started, and an error if an explicitly configured
// port is invalid. When no port is configured, the default port is
// returned with the server enabled. Port 0 disables the server.
func resolveServerPort(portFlag int, portFlagSet bool, extensions map[string]any) (int, bool, error) {
	if portFlagSet {
		if portFlag < 0 || portFlag > 65535 {
			return 0, false, fmt.Errorf("invalid --port value %d: must be between 0 and 65535", portFlag)
		}
		return portFlag, portFlag != 0, nil
	}

	serverExt, ok := extensions["server"].(map[string]any)
	if !ok {
		return defaultServerPort, true, nil
	}

	portVal, exists := serverExt["port"]
	if !exists {
		return defaultServerPort, true, nil
	}

	var port int
	switch v := portVal.(type) {
	case int:
		port = v
	case float64:
		if v != float64(int(v)) {
			return 0, false, fmt.Errorf("invalid server.port value %v: must be an integer", v)
		}
		port = int(v)
	default:
		return 0, false, fmt.Errorf("invalid server.port value: unsupported type %T, must be an integer", portVal)
	}

	if port < 0 || port > 65535 {
		return 0, false, fmt.Errorf("invalid server.port value %d: must be between 0 and 65535", port)
	}
	return port, port != 0, nil
}

// resolveServerHost determines the effective HTTP server bind address
// from the CLI flag and workflow extensions. Returns an error if the
// resolved value is not a parseable IP address. When no host is
// configured, the loopback address is returned.
func resolveServerHost(hostFlag string, hostFlagSet bool, extensions map[string]any) (string, error) {
	if hostFlagSet {
		if net.ParseIP(hostFlag) == nil {
			return "", fmt.Errorf("invalid --host value %q: not a valid IP address", hostFlag)
		}
		return hostFlag, nil
	}

	serverExt, ok := extensions["server"].(map[string]any)
	if !ok {
		return defaultServerHost, nil
	}

	hostVal, exists := serverExt["host"]
	if !exists {
		return defaultServerHost, nil
	}

	hostStr, ok := hostVal.(string)
	if !ok {
		return "", fmt.Errorf("invalid server.host value: unsupported type %T, must be a string", hostVal)
	}

	if net.ParseIP(hostStr) == nil {
		return "", fmt.Errorf("invalid server.host value %q: not a valid IP address", hostStr)
	}
	return hostStr, nil
}

// hasServerPortExtension reports whether the extensions map contains a
// server object with a port key. The check is presence-based: even
// server.port matching the default counts as explicit configuration.
func hasServerPortExtension(extensions map[string]any) bool {
	serverExt, ok := extensions["server"].(map[string]any)
	if !ok {
		return false
	}
	_, exists := serverExt["port"]
	return exists
}

// resolveLogLevel determines the effective log level from the CLI flag
// and workflow extensions. Precedence: CLI flag > logging.level
// extension > default (info).
func resolveLogLevel(flagValue string, flagSet bool, extensions map[string]any) (slog.Level, error) {
	if flagSet {
		return logging.ParseLevel(flagValue)
	}

	loggingExt, ok := extensions["logging"].(map[string]any)
	if !ok {
		return slog.LevelInfo, nil
	}

	rawLevel, ok := loggingExt["level"]
	if !ok || rawLevel == nil {
		return slog.LevelInfo, nil
	}

	levelStr, ok := rawLevel.(string)
	if !ok {
		return 0, fmt.Errorf("invalid logging.level: expected string, got %T", rawLevel)
	}

	return logging.ParseLevel(levelStr)
}

// resolveLogFormat determines the effective log output format from the CLI
// flag and workflow extensions. Precedence: CLI flag > logging.format
// extension > default (text). All map and type accesses use the comma-ok
// idiom to avoid panics on unexpected extension shapes.
func resolveLogFormat(flagValue string, flagSet bool, extensions map[string]any) (logging.Format, error) {
	if flagSet {
		return logging.ParseFormat(flagValue)
	}

	loggingRaw, ok := extensions["logging"]
	if !ok {
		return logging.FormatText, nil
	}
	loggingExt, ok := loggingRaw.(map[string]any)
	if !ok {
		return logging.FormatText, nil
	}

	formatRaw, ok := loggingExt["format"]
	if !ok {
		return logging.FormatText, nil
	}

	formatStr, ok := formatRaw.(string)
	if !ok {
		return "", fmt.Errorf("invalid logging.format: expected string, got %T", formatRaw)
	}

	return logging.ParseFormat(formatStr)
}
