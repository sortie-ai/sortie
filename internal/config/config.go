// Package config converts raw workflow front matter into typed runtime
// configuration. Start with [NewServiceConfig] to obtain a
// [ServiceConfig] from a generic map, and inspect [ConfigError] for
// structured diagnostics on invalid values.
package config

import (
	"fmt"
	"math"
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

	// CIFeedback holds CI feedback provider selection and tuning.
	// Zero-value (Kind == "") means CI feedback is disabled.
	CIFeedback CIFeedbackConfig

	// DBPath is the environment- and tilde-expanded path for the SQLite
	// database. It may be relative; callers resolve it against the
	// WORKFLOW.md directory. Empty string means the caller should apply
	// its own default (typically .sortie.db adjacent to WORKFLOW.md).
	//
	// DBPath is read once at startup to open the database. Dynamic
	// reloads update this field in memory but have no effect on the
	// already-open database connection.
	DBPath string

	// Extensions holds top-level front matter keys not covered by the
	// core schema (e.g. "server", "worker"). Consumers access these
	// via map lookup. Never nil after construction.
	Extensions map[string]any
}

// TrackerConfig holds issue tracker connection and query settings.
type TrackerConfig struct {
	Kind            string
	Endpoint        string
	APIKey          string
	Project         string
	ActiveStates    []string
	TerminalStates  []string
	QueryFilter     string
	HandoffState    string
	InProgressState string
	Comments        TrackerCommentsConfig
}

// TrackerCommentsConfig holds the boolean flags controlling whether
// the orchestrator posts tracker comments at session lifecycle points.
type TrackerCommentsConfig struct {
	OnDispatch   bool
	OnCompletion bool
	OnFailure    bool
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
	MaxSessions          int
}

// knownTopLevelKeys enumerates the front matter keys consumed by the
// core schema. Anything else is collected into Extensions.
var knownTopLevelKeys = map[string]bool{
	"tracker":     true,
	"polling":     true,
	"workspace":   true,
	"hooks":       true,
	"agent":       true,
	"db_path":     true,
	"ci_feedback": true,
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

	envKeys, err := applyEnvOverrides(raw)
	if err != nil {
		return ServiceConfig{}, err
	}

	rawTracker := extractSubMap(raw, "tracker")
	tracker := buildTrackerConfig(rawTracker, envKeys)

	// Validate handoff_state: enforce string type, reject explicit empty
	// values, and detect env var indirection that resolved to empty.
	if rawVal, ok := rawTracker["handoff_state"]; ok && rawVal != nil {
		s, isStr := rawVal.(string)
		if !isStr {
			return ServiceConfig{}, &ConfigError{
				Field:   "tracker.handoff_state",
				Message: fmt.Sprintf("expected string, got %T", rawVal),
			}
		}
		if s == "" {
			return ServiceConfig{}, &ConfigError{
				Field:   "tracker.handoff_state",
				Message: "must not be empty",
			}
		}
		if tracker.HandoffState == "" {
			return ServiceConfig{}, &ConfigError{
				Field:   "tracker.handoff_state",
				Message: "resolved to empty (check environment variable)",
			}
		}
	}
	if err := validateHandoffState(tracker.HandoffState, tracker.ActiveStates, tracker.TerminalStates); err != nil {
		return ServiceConfig{}, err
	}

	// Validate in_progress_state: enforce string type, reject explicit empty
	// values, and detect env var indirection that resolved to empty.
	if rawVal, ok := rawTracker["in_progress_state"]; ok && rawVal != nil {
		s, isStr := rawVal.(string)
		if !isStr {
			return ServiceConfig{}, &ConfigError{
				Field:   "tracker.in_progress_state",
				Message: fmt.Sprintf("expected string, got %T", rawVal),
			}
		}
		if s == "" {
			return ServiceConfig{}, &ConfigError{
				Field:   "tracker.in_progress_state",
				Message: "must not be empty",
			}
		}
		if tracker.InProgressState == "" {
			return ServiceConfig{}, &ConfigError{
				Field:   "tracker.in_progress_state",
				Message: "resolved to empty (check environment variable)",
			}
		}
	}
	if err := validateInProgressState(tracker.InProgressState, tracker.ActiveStates, tracker.TerminalStates, tracker.HandoffState); err != nil {
		return ServiceConfig{}, err
	}

	// Validate tracker.comments fields: reject non-boolean types.
	if rawComments, ok := rawTracker["comments"]; ok && rawComments != nil {
		commentsMap, isMap := rawComments.(map[string]any)
		if !isMap {
			return ServiceConfig{}, &ConfigError{
				Field:   "tracker.comments",
				Message: fmt.Sprintf("expected map, got %T", rawComments),
			}
		}
		for _, key := range []string{"on_dispatch", "on_completion", "on_failure"} {
			if v, exists := commentsMap[key]; exists && v != nil {
				if _, isBool := v.(bool); !isBool {
					return ServiceConfig{}, &ConfigError{
						Field:   "tracker.comments." + key,
						Message: fmt.Sprintf("expected bool, got %T", v),
					}
				}
			}
		}
	}

	polling, err := buildPollingConfig(extractSubMap(raw, "polling"))
	if err != nil {
		return ServiceConfig{}, err
	}

	workspace, err := buildWorkspaceConfig(extractSubMap(raw, "workspace"), envKeys)
	if err != nil {
		return ServiceConfig{}, err
	}

	hooks := buildHooksConfig(extractSubMap(raw, "hooks"))

	agent, err := buildAgentConfig(extractSubMap(raw, "agent"))
	if err != nil {
		return ServiceConfig{}, err
	}

	dbPath, err := buildDBPath(raw, envKeys)
	if err != nil {
		return ServiceConfig{}, err
	}

	ciFeedback, err := buildCIFeedbackConfig(extractSubMap(raw, "ci_feedback"))
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
		CIFeedback: ciFeedback,
		DBPath:     dbPath,
		Extensions: extensions,
	}, nil
}

func buildTrackerConfig(m map[string]any, envKeys map[string]bool) TrackerConfig {
	commentsMap := extractSubMap(m, "comments")

	endpoint := extractString(m, "endpoint")
	if !envKeys["tracker.endpoint"] {
		endpoint = resolveEnvRef(endpoint)
	}

	apiKey := extractString(m, "api_key")
	if !envKeys["tracker.api_key"] {
		apiKey = resolveEnv(apiKey)
	}

	project := extractString(m, "project")
	if !envKeys["tracker.project"] {
		project = resolveEnvRef(project)
	}

	queryFilter := extractString(m, "query_filter")
	if !envKeys["tracker.query_filter"] {
		queryFilter = resolveEnvRef(queryFilter)
	}

	handoffState := extractString(m, "handoff_state")
	if !envKeys["tracker.handoff_state"] {
		handoffState = resolveEnvRef(handoffState)
	}

	inProgressState := extractString(m, "in_progress_state")
	if !envKeys["tracker.in_progress_state"] {
		inProgressState = resolveEnvRef(inProgressState)
	}

	return TrackerConfig{
		Kind:            extractString(m, "kind"),
		Endpoint:        endpoint,
		APIKey:          apiKey,
		Project:         project,
		ActiveStates:    extractStringSlice(mapVal(m, "active_states")),
		TerminalStates:  extractStringSlice(mapVal(m, "terminal_states")),
		QueryFilter:     queryFilter,
		HandoffState:    handoffState,
		InProgressState: inProgressState,
		Comments: TrackerCommentsConfig{
			OnDispatch:   coerceBool(commentsMap, "on_dispatch"),
			OnCompletion: coerceBool(commentsMap, "on_completion"),
			OnFailure:    coerceBool(commentsMap, "on_failure"),
		},
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

func buildWorkspaceConfig(m map[string]any, envKeys map[string]bool) (WorkspaceConfig, error) {
	rootRaw := extractString(m, "root")
	root, err := expandPath(rootRaw, !envKeys["workspace.root"])
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

func buildDBPath(raw map[string]any, envKeys map[string]bool) (string, error) {
	v, exists := raw["db_path"]
	if !exists || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &ConfigError{
			Field:   "db_path",
			Message: fmt.Sprintf("expected string, got %T", v),
		}
	}
	// Explicit empty string (db_path: "") is equivalent to omitting
	// the field — the caller applies its default path.
	if s == "" {
		return "", nil
	}
	expanded, err := expandPath(s, !envKeys["db_path"])
	if err != nil {
		return "", &ConfigError{
			Field:   "db_path",
			Message: err.Error(),
		}
	}
	if expanded == "" {
		return "", &ConfigError{
			Field:   "db_path",
			Message: "resolved to empty (check environment variable)",
		}
	}
	return expanded, nil
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

	maxSessions, err := coerceIntField(m, "max_sessions", "agent.max_sessions")
	if err != nil {
		return AgentConfig{}, err
	}
	if maxSessions < 0 {
		return AgentConfig{}, &ConfigError{
			Field:   "agent.max_sessions",
			Message: "must be non-negative",
		}
	}

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
		MaxSessions:          maxSessions,
	}, nil
}

func buildCIFeedbackConfig(m map[string]any) (CIFeedbackConfig, error) {
	kind := extractString(m, "kind")
	if kind == "" {
		return CIFeedbackConfig{}, nil
	}

	maxRetries, err := coerceIntField(m, "max_retries", "ci_feedback.max_retries")
	if err != nil {
		return CIFeedbackConfig{}, err
	}
	if _, exists := m["max_retries"]; !exists {
		maxRetries = 2
	}

	maxLogLines, err := coerceIntField(m, "max_log_lines", "ci_feedback.max_log_lines")
	if err != nil {
		return CIFeedbackConfig{}, err
	}

	escalation := extractString(m, "escalation")
	if escalation == "" {
		escalation = "label"
	}
	if escalation != "label" && escalation != "comment" {
		return CIFeedbackConfig{}, &ConfigError{
			Field:   "ci_feedback.escalation",
			Message: fmt.Sprintf("must be \"label\" or \"comment\", got %q", escalation),
		}
	}

	escalationLabel := extractString(m, "escalation_label")
	if escalationLabel == "" {
		escalationLabel = "needs-human"
	}

	return CIFeedbackConfig{
		Kind:            kind,
		MaxRetries:      maxRetries,
		MaxLogLines:     maxLogLines,
		Escalation:      escalation,
		EscalationLabel: escalationLabel,
	}, nil
}

// validateHandoffState checks that handoffState does not collide with
// active or terminal states. Returns a *ConfigError on violation.
func validateHandoffState(handoffState string, activeStates, terminalStates []string) error {
	if handoffState == "" {
		return nil
	}
	lower := strings.ToLower(handoffState)
	for _, s := range activeStates {
		if strings.ToLower(s) == lower {
			return &ConfigError{
				Field:   "tracker.handoff_state",
				Message: fmt.Sprintf("%q collides with active state %q", handoffState, s),
			}
		}
	}
	for _, s := range terminalStates {
		if strings.ToLower(s) == lower {
			return &ConfigError{
				Field:   "tracker.handoff_state",
				Message: fmt.Sprintf("%q collides with terminal state %q", handoffState, s),
			}
		}
	}
	return nil
}

// validateInProgressState checks that inProgressState does not collide
// with terminal states or handoff_state, and is present in active states.
// Returns a *ConfigError on violation.
func validateInProgressState(inProgressState string, activeStates, terminalStates []string, handoffState string) error {
	if inProgressState == "" {
		return nil
	}
	lower := strings.ToLower(inProgressState)
	for _, s := range terminalStates {
		if strings.ToLower(s) == lower {
			return &ConfigError{
				Field:   "tracker.in_progress_state",
				Message: fmt.Sprintf("%q collides with terminal state %q", inProgressState, s),
			}
		}
	}
	isActive := false
	for _, s := range activeStates {
		if strings.ToLower(s) == lower {
			isActive = true
			break
		}
	}
	if !isActive {
		return &ConfigError{
			Field:   "tracker.in_progress_state",
			Message: fmt.Sprintf("%q is not in active_states; reconciliation would immediately cancel the worker", inProgressState),
		}
	}
	if handoffState != "" && strings.ToLower(handoffState) == lower {
		return &ConfigError{
			Field:   "tracker.in_progress_state",
			Message: fmt.Sprintf("%q collides with handoff_state %q", inProgressState, handoffState),
		}
	}
	return nil
}

func resolveEnv(s string) string {
	return os.ExpandEnv(s)
}

// resolveEnvRef performs targeted environment variable resolution: it
// expands the value only when the entire string is an env var reference
// ($VAR or ${VAR}). Mixed content such as URIs with embedded
// $-references is returned unchanged to avoid destructive rewriting.
func resolveEnvRef(s string) string {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "$") {
		return os.ExpandEnv(trimmed)
	}
	return s
}

func expandPath(p string, expandEnv bool) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		p = filepath.Join(home, p[1:])
	}
	if expandEnv {
		return os.ExpandEnv(p), nil
	}
	return p, nil
}

// coerceBool returns the boolean value for key in m. Returns false when
// the key is absent, the value is nil, or the value is not a bool type.
func coerceBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

func coerceInt(x any) (int, error) {
	switch v := x.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case int32:
		return int(v), nil
	case float64:
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("fractional value %v", v)
		}
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	default:
		return 0, fmt.Errorf("unsupported type %T", x)
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

func extractStringSlice(raw any) []string {
	if raw == nil {
		return nil
	}
	// yaml.v3 decodes lists as []any, not []string.
	slice, ok := raw.([]any)
	if !ok {
		return nil
	}
	strs := make([]string, 0, len(slice))
	for _, elem := range slice {
		if s, ok := elem.(string); ok {
			strs = append(strs, s)
		} else {
			strs = append(strs, fmt.Sprintf("%v", elem))
		}
	}
	return strs
}

func mapVal(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

// CIFeedbackConfig holds CI feedback provider selection and tuning.
type CIFeedbackConfig struct {
	// Kind identifies the CI status provider adapter (e.g. "github").
	// Empty string means CI feedback is disabled.
	Kind string

	// MaxRetries is the maximum number of CI-fix continuation dispatches
	// per issue before escalation. Default 2. Zero means no retries
	// (escalate immediately on first CI failure).
	MaxRetries int

	// MaxLogLines controls log excerpt fetching for failing CI checks.
	// Positive value: fetch up to N lines from the first failing check.
	// Zero: disable log fetching. The parsing layer resolves absent
	// YAML keys to the adapter default before storing; after parsing,
	// zero unambiguously means disabled.
	MaxLogLines int

	// Escalation controls what happens when max_retries is exceeded.
	// Valid values: "label" (default) adds a label to the issue,
	// "comment" posts a comment on the issue.
	Escalation string

	// EscalationLabel is the label applied when escalation is "label".
	// Default "needs-human".
	EscalationLabel string
}

func normalizeByStateMap(raw any) map[string]int {
	byState := make(map[string]int)
	if raw == nil {
		return byState
	}
	rawMap, ok := raw.(map[string]any)
	if !ok {
		return byState
	}
	for key, v := range rawMap {
		n, err := coerceInt(v)
		if err != nil || n <= 0 {
			continue
		}
		byState[strings.ToLower(key)] = n
	}
	return byState
}
