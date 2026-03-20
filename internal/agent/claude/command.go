package claude

import (
	"crypto/rand"
	"fmt"
	"strconv"
)

// passthroughConfig holds Claude Code-specific settings extracted from
// the "claude-code" sub-object in WORKFLOW.md. All fields are optional
// with zero-value meaning "not configured."
type passthroughConfig struct {
	PermissionMode     string
	Model              string
	FallbackModel      string
	MaxTurns           int
	MaxBudgetUSD       float64
	Effort             string
	AllowedTools       string
	DisallowedTools    string
	SystemPrompt       string
	MCPConfig          string
	SessionPersistence bool
}

// parsePassthroughConfig extracts Claude Code-specific settings from
// the raw config map. Missing or wrong-typed keys use zero-value
// defaults; SessionPersistence defaults to true.
func parsePassthroughConfig(config map[string]any) passthroughConfig {
	return passthroughConfig{
		PermissionMode:     stringFrom(config, "permission_mode"),
		Model:              stringFrom(config, "model"),
		FallbackModel:      stringFrom(config, "fallback_model"),
		MaxTurns:           intFrom(config, "max_turns", 0),
		MaxBudgetUSD:       floatFrom(config, "max_budget_usd", 0),
		Effort:             stringFrom(config, "effort"),
		AllowedTools:       stringFrom(config, "allowed_tools"),
		DisallowedTools:    stringFrom(config, "disallowed_tools"),
		SystemPrompt:       stringFrom(config, "system_prompt"),
		MCPConfig:          stringFrom(config, "mcp_config"),
		SessionPersistence: boolFrom(config, "session_persistence", true),
	}
}

// buildArgs constructs the CLI argument slice for a Claude Code
// invocation. The arguments are passed directly to exec.Command,
// avoiding shell interpolation.
func buildArgs(state *sessionState, prompt string, pt passthroughConfig) []string {
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
	}

	// Permission handling.
	if pt.PermissionMode != "" {
		args = append(args, "--permission-mode", pt.PermissionMode)
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}

	// Session management: first turn of a new session uses --session-id,
	// all other cases (continuation turns or continuation sessions) use
	// --resume.
	if state.turnCount == 0 && !state.isContinuation {
		args = append(args, "--session-id", state.claudeSessionID)
	} else {
		args = append(args, "--resume", state.claudeSessionID)
	}

	// Optional pass-through flags.
	if pt.Model != "" {
		args = append(args, "--model", pt.Model)
	}
	if pt.FallbackModel != "" {
		args = append(args, "--fallback-model", pt.FallbackModel)
	}
	if pt.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(pt.MaxTurns))
	}
	if pt.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(pt.MaxBudgetUSD, 'f', -1, 64))
	}
	if pt.Effort != "" {
		args = append(args, "--effort", pt.Effort)
	}
	if pt.AllowedTools != "" {
		args = append(args, "--allowedTools", pt.AllowedTools)
	}
	if pt.DisallowedTools != "" {
		args = append(args, "--disallowedTools", pt.DisallowedTools)
	}
	if pt.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", pt.SystemPrompt)
	}
	if pt.MCPConfig != "" {
		args = append(args, "--mcp-config", pt.MCPConfig)
	}
	if !pt.SessionPersistence {
		args = append(args, "--no-session-persistence")
	}

	return args
}

// newUUID generates a random v4 UUID string using crypto/rand.
// Panics if the system random source is unavailable.
func newUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("claude: crypto/rand unavailable: %v", err))
	}
	// Set version (4) and variant (RFC 4122).
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// stringFrom extracts a string value from config by key. Returns
// empty string if the key is absent or not a string.
func stringFrom(config map[string]any, key string) string {
	v, ok := config[key].(string)
	if !ok {
		return ""
	}
	return v
}

// intFrom extracts an integer value from config by key. Handles both
// int and float64 (JSON numbers decode as float64). Returns
// defaultVal if absent or wrong type.
func intFrom(config map[string]any, key string, defaultVal int) int {
	raw, ok := config[key]
	if !ok {
		return defaultVal
	}
	switch v := raw.(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return defaultVal
	}
}

// floatFrom extracts a float64 value from config by key. Returns
// defaultVal if absent or wrong type.
func floatFrom(config map[string]any, key string, defaultVal float64) float64 {
	raw, ok := config[key]
	if !ok {
		return defaultVal
	}
	switch v := raw.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	default:
		return defaultVal
	}
}

// boolFrom extracts a boolean value from config by key. Returns
// defaultVal if absent or wrong type.
func boolFrom(config map[string]any, key string, defaultVal bool) bool {
	v, ok := config[key].(bool)
	if !ok {
		return defaultVal
	}
	return v
}
