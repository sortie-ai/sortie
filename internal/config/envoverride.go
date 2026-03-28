package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

// dotenvPathOverride holds the .env file path set by the CLI --env-file flag.
// Read by applyEnvOverrides on each call (supporting dynamic reload).
// Access is synchronized via dotenvPathMu.
var (
	dotenvPathOverride string
	dotenvPathMu       sync.RWMutex
)

// SetDotEnvPath sets the .env file path for config overrides.
// Safe for concurrent use. Call this from cmd/sortie/main.go before any
// [NewServiceConfig] calls. If not set, applyEnvOverrides falls back to
// os.Getenv("SORTIE_ENV_FILE").
func SetDotEnvPath(path string) {
	dotenvPathMu.Lock()
	dotenvPathOverride = path
	dotenvPathMu.Unlock()
}

// getDotEnvPath returns the current .env file path override. Safe for
// concurrent use.
func getDotEnvPath() string {
	dotenvPathMu.RLock()
	defer dotenvPathMu.RUnlock()
	return dotenvPathOverride
}

// envOverride maps a SORTIE_* environment variable to a config field path.
type envOverride struct {
	EnvVar  string                    // e.g. "SORTIE_TRACKER_KIND"
	Section string                    // empty string = top-level field
	Field   string                    // dotted for nested (e.g. "comments.on_dispatch")
	Coerce  func(string) (any, error) // string→typed coercion; error fails startup
}

// envOverrides is the curated registry of environment variable overrides.
// Each entry maps exactly one SORTIE_* variable to one config field.
var envOverrides = []envOverride{
	// Tracker
	{"SORTIE_TRACKER_KIND", "tracker", "kind", coerceString},
	{"SORTIE_TRACKER_ENDPOINT", "tracker", "endpoint", coerceString},
	{"SORTIE_TRACKER_API_KEY", "tracker", "api_key", coerceString},
	{"SORTIE_TRACKER_PROJECT", "tracker", "project", coerceString},
	{"SORTIE_TRACKER_ACTIVE_STATES", "tracker", "active_states", coerceCSVList},
	{"SORTIE_TRACKER_TERMINAL_STATES", "tracker", "terminal_states", coerceCSVList},
	{"SORTIE_TRACKER_QUERY_FILTER", "tracker", "query_filter", coerceString},
	{"SORTIE_TRACKER_HANDOFF_STATE", "tracker", "handoff_state", coerceString},
	{"SORTIE_TRACKER_IN_PROGRESS_STATE", "tracker", "in_progress_state", coerceString},

	// Tracker comments
	{"SORTIE_TRACKER_COMMENTS_ON_DISPATCH", "tracker", "comments.on_dispatch", coerceEnvBool},
	{"SORTIE_TRACKER_COMMENTS_ON_COMPLETION", "tracker", "comments.on_completion", coerceEnvBool},
	{"SORTIE_TRACKER_COMMENTS_ON_FAILURE", "tracker", "comments.on_failure", coerceEnvBool},

	// Polling
	{"SORTIE_POLLING_INTERVAL_MS", "polling", "interval_ms", coerceEnvInt},

	// Workspace
	{"SORTIE_WORKSPACE_ROOT", "workspace", "root", coerceString},

	// Agent
	{"SORTIE_AGENT_KIND", "agent", "kind", coerceString},
	{"SORTIE_AGENT_COMMAND", "agent", "command", coerceString},
	{"SORTIE_AGENT_TURN_TIMEOUT_MS", "agent", "turn_timeout_ms", coerceEnvInt},
	{"SORTIE_AGENT_READ_TIMEOUT_MS", "agent", "read_timeout_ms", coerceEnvInt},
	{"SORTIE_AGENT_STALL_TIMEOUT_MS", "agent", "stall_timeout_ms", coerceEnvInt},
	{"SORTIE_AGENT_MAX_CONCURRENT_AGENTS", "agent", "max_concurrent_agents", coerceEnvInt},
	{"SORTIE_AGENT_MAX_TURNS", "agent", "max_turns", coerceEnvInt},
	{"SORTIE_AGENT_MAX_RETRY_BACKOFF_MS", "agent", "max_retry_backoff_ms", coerceEnvInt},
	{"SORTIE_AGENT_MAX_SESSIONS", "agent", "max_sessions", coerceEnvInt},

	// Top-level
	{"SORTIE_DB_PATH", "", "db_path", coerceString},
}

// secretEnvVars lists SORTIE_* variables whose values must never be
// logged. Used by applyEnvOverrides for debug-level diagnostics.
var secretEnvVars = map[string]bool{
	"SORTIE_TRACKER_API_KEY": true,
}

// applyEnvOverrides merges SORTIE_* environment variables and .env file
// values into the raw config map. Returns a set of field paths that were
// set from environment sources (used by section builders to skip $VAR
// expansion). Returns an error on .env parse failures or type coercion
// failures.
func applyEnvOverrides(raw map[string]any) (map[string]bool, error) {
	// Resolve dotenv path: CLI flag → env var → empty (no loading).
	dotenvPath := getDotEnvPath()
	if dotenvPath == "" {
		dotenvPath = os.Getenv("SORTIE_ENV_FILE")
	}

	var dotenv map[string]string
	if dotenvPath != "" {
		var err error
		dotenv, err = parseDotEnv(dotenvPath)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if dotenv == nil {
			// Path was set but file does not exist.
			slog.Warn("env file not found, skipping", //nolint:gosec // G706: path is operator-provided via CLI flag or env var
				slog.String("path", dotenvPath))
		}
	}

	envKeys := make(map[string]bool)

	for _, ov := range envOverrides {
		val := os.Getenv(ov.EnvVar)
		if val == "" && dotenv != nil {
			val = dotenv[ov.EnvVar]
		}
		if val == "" {
			continue
		}

		coerced, err := ov.Coerce(val)
		if err != nil {
			fieldPath := ov.Field
			if ov.Section != "" {
				fieldPath = ov.Section + "." + ov.Field
			}
			return nil, &ConfigError{
				Field:   fieldPath,
				Message: fmt.Sprintf("%s (from %s)", err.Error(), ov.EnvVar),
			}
		}

		// Debug-level logging of applied overrides; mask secrets.
		logVal := val
		if secretEnvVars[ov.EnvVar] {
			logVal = "***"
		}
		slog.Debug("env override applied", //nolint:gosec // G706: values are operator-provided env vars, not user input
			slog.String("var", ov.EnvVar),
			slog.String("value", logVal))

		if ov.Section == "" {
			// Top-level field.
			raw[ov.Field] = coerced
			envKeys[ov.Field] = true
			continue
		}

		if strings.Contains(ov.Field, ".") {
			// Nested field (e.g. "comments.on_dispatch" under "tracker").
			parts := strings.SplitN(ov.Field, ".", 2)
			secMap := ensureSubMap(raw, ov.Section)
			subMap := ensureSubMap(secMap, parts[0])
			subMap[parts[1]] = coerced
			envKeys[ov.Section+"."+ov.Field] = true
			continue
		}

		// Section-level field.
		secMap := ensureSubMap(raw, ov.Section)
		secMap[ov.Field] = coerced
		envKeys[ov.Section+"."+ov.Field] = true
	}

	return envKeys, nil
}

// ensureSubMap ensures m[key] is a map[string]any and returns it. If
// the existing value is nil or absent, a fresh empty map is created and
// assigned. If the existing value is a non-map type, it is replaced and
// a warning is logged for the operator (silent data loss).
func ensureSubMap(m map[string]any, key string) map[string]any {
	existing, ok := m[key]
	if ok {
		if v, isMap := existing.(map[string]any); isMap {
			return v
		}
		// Present but wrong type — warn about silent data loss.
		if existing != nil {
			slog.Warn("env override replaced non-map YAML section",
				slog.String("section", key),
				slog.String("yaml_type", fmt.Sprintf("%T", existing)))
		}
	}
	v := make(map[string]any)
	m[key] = v
	return v
}

// --- coercion functions for env override values ---

func coerceString(val string) (any, error) {
	return val, nil
}

func coerceEnvInt(val string) (any, error) {
	trimmed := strings.TrimSpace(val)
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid integer value: %s", val)
	}
	return n, nil
}

func coerceCSVList(val string) (any, error) {
	if val == "" {
		return []any{}, nil
	}
	parts := strings.Split(val, ",")
	result := make([]any, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result, nil
}

func coerceEnvBool(val string) (any, error) {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return nil, fmt.Errorf("invalid boolean value: %s (expected true/false/1/0)", val)
	}
}
