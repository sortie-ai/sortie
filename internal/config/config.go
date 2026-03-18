package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ServiceConfig is the typed runtime view of WORKFLOW.md front matter.
// Construct via [NewServiceConfig] from the raw map produced by the
// workflow loader. All environment indirection and path expansion is
// applied at construction time; callers receive fully resolved values.
type ServiceConfig struct {
	Tracker   TrackerConfig
	Polling   PollingConfig
	Workspace WorkspaceConfig
	Hooks     HooksConfig
	Agent     AgentConfig

	// Extensions holds top-level front matter keys not covered by the
	// core schema (e.g. "server", "worker"). Consumers access these
	// via map lookup. Never nil after construction.
	Extensions map[string]any
}

// TrackerConfig holds issue tracker connection and query settings.
type TrackerConfig struct {
	Kind           string
	Endpoint       string
	APIKey         string
	Project        string
	ActiveStates   []string
	TerminalStates []string
}

// PollingConfig holds the poll loop timing.
type PollingConfig struct {
	IntervalMS int
}

// WorkspaceConfig holds the workspace root path after expansion.
type WorkspaceConfig struct {
	Root string
}

// HooksConfig holds workspace lifecycle hook scripts and their timeout.
type HooksConfig struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	TimeoutMS    int
}

// AgentConfig holds coding-agent adapter selection, timeouts, and
// concurrency limits.
type AgentConfig struct {
	Kind                 string
	Command              string
	TurnTimeoutMS        int
	ReadTimeoutMS        int
	StallTimeoutMS       int
	MaxConcurrentAgents  int
	MaxTurns             int
	MaxRetryBackoffMS    int
	MaxConcurrentByState map[string]int
}

// knownTopLevelKeys enumerates the front matter keys consumed by the
// core schema. Anything else is collected into Extensions.
var knownTopLevelKeys = map[string]bool{
	"tracker":   true,
	"polling":   true,
	"workspace": true,
	"hooks":     true,
	"agent":     true,
}

// NewServiceConfig converts a raw front matter map into a validated
// [ServiceConfig]. It applies built-in defaults, resolves `$VAR`
// environment indirection on selected fields, expands `~` in path
// fields, coerces string-encoded integers, and normalizes per-state
// concurrency map keys to lowercase. Returns a [*ConfigError] when a
// field value cannot be coerced to the expected type.
func NewServiceConfig(raw map[string]any) (ServiceConfig, error) {
	if raw == nil {
		raw = map[string]any{}
	}

	tracker := buildTrackerConfig(extractSubMap(raw, "tracker"))

	polling, err := buildPollingConfig(extractSubMap(raw, "polling"))
	if err != nil {
		return ServiceConfig{}, err
	}

	workspace, err := buildWorkspaceConfig(extractSubMap(raw, "workspace"))
	if err != nil {
		return ServiceConfig{}, err
	}

	hooks := buildHooksConfig(extractSubMap(raw, "hooks"))

	agent, err := buildAgentConfig(extractSubMap(raw, "agent"))
	if err != nil {
		return ServiceConfig{}, err
	}

	extensions := make(map[string]any)
	for k, v := range raw {
		if !knownTopLevelKeys[k] {
			extensions[k] = v
		}
	}

	return ServiceConfig{
		Tracker:    tracker,
		Polling:    polling,
		Workspace:  workspace,
		Hooks:      hooks,
		Agent:      agent,
		Extensions: extensions,
	}, nil
}

// --- section builders ---

func buildTrackerConfig(m map[string]any) TrackerConfig {
	return TrackerConfig{
		Kind:     extractString(m, "kind"),
		Endpoint: resolveEnvRef(extractString(m, "endpoint")),
		APIKey:   resolveEnv(extractString(m, "api_key")),
		Project:  resolveEnvRef(extractString(m, "project")),
		// States are stored with original casing; the orchestrator
		// normalizes both sides to lowercase when comparing.
		ActiveStates:   extractStringSlice(mapVal(m, "active_states")),
		TerminalStates: extractStringSlice(mapVal(m, "terminal_states")),
	}
}

func buildPollingConfig(m map[string]any) (PollingConfig, error) {
	intervalMS, err := coerceIntField(m, "interval_ms", "polling.interval_ms")
	if err != nil {
		return PollingConfig{}, err
	}
	if intervalMS == 0 {
		intervalMS = 30000
	}
	return PollingConfig{IntervalMS: intervalMS}, nil
}

func buildWorkspaceConfig(m map[string]any) (WorkspaceConfig, error) {
	rootRaw := extractString(m, "root")
	root, err := expandPath(rootRaw)
	if err != nil {
		return WorkspaceConfig{}, &ConfigError{
			Field:   "workspace.root",
			Message: err.Error(),
		}
	}
	if root == "" {
		root = filepath.Join(os.TempDir(), "sortie_workspaces")
	}
	return WorkspaceConfig{Root: root}, nil
}

func buildHooksConfig(m map[string]any) HooksConfig {
	timeoutMS, err := coerceIntFieldSafe(m, "timeout_ms")
	if err != nil || timeoutMS <= 0 {
		timeoutMS = 60000
	}
	return HooksConfig{
		AfterCreate:  extractString(m, "after_create"),
		BeforeRun:    extractString(m, "before_run"),
		AfterRun:     extractString(m, "after_run"),
		BeforeRemove: extractString(m, "before_remove"),
		TimeoutMS:    timeoutMS,
	}
}

func buildAgentConfig(m map[string]any) (AgentConfig, error) {
	kind := extractString(m, "kind")
	if kind == "" {
		kind = "claude-code"
	}

	command := extractString(m, "command")

	turnTimeoutMS, err := coerceIntField(m, "turn_timeout_ms", "agent.turn_timeout_ms")
	if err != nil {
		return AgentConfig{}, err
	}
	if turnTimeoutMS == 0 {
		turnTimeoutMS = 3600000
	}

	readTimeoutMS, err := coerceIntField(m, "read_timeout_ms", "agent.read_timeout_ms")
	if err != nil {
		return AgentConfig{}, err
	}
	if readTimeoutMS == 0 {
		readTimeoutMS = 5000
	}

	// stall_timeout_ms: zero is a valid sentinel that disables stall
	// detection. Only default when the key is absent from the map.
	stallTimeoutMS := 300000
	if v, exists := m["stall_timeout_ms"]; exists && v != nil {
		parsed, err := coerceInt(v)
		if err != nil {
			return AgentConfig{}, &ConfigError{
				Field:   "agent.stall_timeout_ms",
				Message: fmt.Sprintf("invalid integer value: %v", v),
			}
		}
		stallTimeoutMS = parsed
	}

	maxConcurrent, err := coerceIntField(m, "max_concurrent_agents", "agent.max_concurrent_agents")
	if err != nil {
		return AgentConfig{}, err
	}
	if maxConcurrent == 0 {
		maxConcurrent = 10
	}

	maxTurns, err := coerceIntField(m, "max_turns", "agent.max_turns")
	if err != nil {
		return AgentConfig{}, err
	}
	if maxTurns == 0 {
		maxTurns = 20
	}

	maxRetryBackoff, err := coerceIntField(m, "max_retry_backoff_ms", "agent.max_retry_backoff_ms")
	if err != nil {
		return AgentConfig{}, err
	}
	if maxRetryBackoff == 0 {
		maxRetryBackoff = 300000
	}

	byState := normalizeByStateMap(mapVal(m, "max_concurrent_agents_by_state"))

	return AgentConfig{
		Kind:                 kind,
		Command:              command,
		TurnTimeoutMS:        turnTimeoutMS,
		ReadTimeoutMS:        readTimeoutMS,
		StallTimeoutMS:       stallTimeoutMS,
		MaxConcurrentAgents:  maxConcurrent,
		MaxTurns:             maxTurns,
		MaxRetryBackoffMS:    maxRetryBackoff,
		MaxConcurrentByState: byState,
	}, nil
}

// --- resolution helpers ---

func resolveEnv(val string) string {
	return os.ExpandEnv(val)
}

// resolveEnvRef performs targeted environment variable resolution: it
// expands the value only when the entire string is an env var reference
// ($VAR or ${VAR}). Mixed content such as URIs with embedded
// $-references is returned unchanged to avoid destructive rewriting
// (architecture Section 6.1: do not rewrite URIs).
func resolveEnvRef(val string) string {
	trimmed := strings.TrimSpace(val)
	if strings.HasPrefix(trimmed, "$") {
		return os.ExpandEnv(trimmed)
	}
	return val
}

func expandPath(val string) (string, error) {
	if val == "" {
		return "", nil
	}
	if val == "~" || strings.HasPrefix(val, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		val = filepath.Join(home, val[1:])
	}
	return os.ExpandEnv(val), nil
}

// --- coercion helpers ---

func coerceInt(val any) (int, error) {
	switch v := val.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case int32:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	default:
		return 0, fmt.Errorf("unsupported type %T", val)
	}
}

// coerceIntField coerces a map value to int, returning a ConfigError on
// failure. Returns 0 when the key is absent or nil.
func coerceIntField(m map[string]any, key, field string) (int, error) {
	v, exists := m[key]
	if !exists || v == nil {
		return 0, nil
	}
	n, err := coerceInt(v)
	if err != nil {
		return 0, &ConfigError{
			Field:   field,
			Message: fmt.Sprintf("invalid integer value: %v", v),
		}
	}
	return n, nil
}

// coerceIntFieldSafe is like coerceIntField but never returns a
// ConfigError — the caller handles failure by falling back to a default.
func coerceIntFieldSafe(m map[string]any, key string) (int, error) {
	v, exists := m[key]
	if !exists || v == nil {
		return 0, nil
	}
	return coerceInt(v)
}

// --- extraction helpers ---

func extractSubMap(raw map[string]any, key string) map[string]any {
	if raw == nil {
		return nil
	}
	v, ok := raw[key]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

func extractString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func extractStringSlice(val any) []string {
	if val == nil {
		return nil
	}
	// yaml.v3 decodes lists as []any, not []string.
	slice, ok := val.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if s, ok := item.(string); ok {
			result = append(result, s)
		} else {
			result = append(result, fmt.Sprintf("%v", item))
		}
	}
	return result
}

func mapVal(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func normalizeByStateMap(val any) map[string]int {
	result := make(map[string]int)
	if val == nil {
		return result
	}
	rawMap, ok := val.(map[string]any)
	if !ok {
		return result
	}
	for key, v := range rawMap {
		n, err := coerceInt(v)
		if err != nil || n <= 0 {
			continue
		}
		result[strings.ToLower(key)] = n
	}
	return result
}
