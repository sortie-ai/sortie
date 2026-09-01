package claude

import (
	"crypto/rand"
	"fmt"
	"strconv"

	"github.com/sortie-ai/sortie/internal/typeutil"
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

// parsePassthroughConfig extracts Claude Code-specific settings from the
// raw config map. A missing key uses its zero-value default;
// SessionPersistence defaults to true. A key present with a non-string
// value for a string field reports a fault rather than defaulting.
func parsePassthroughConfig(config map[string]any) (passthroughConfig, *typeutil.TypeFault) {
	permissionMode, fault := typeutil.StringField(config, "permission_mode")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	model, fault := typeutil.StringField(config, "model")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	fallbackModel, fault := typeutil.StringField(config, "fallback_model")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	effort, fault := typeutil.StringField(config, "effort")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	allowedTools, fault := typeutil.StringField(config, "allowed_tools")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	disallowedTools, fault := typeutil.StringField(config, "disallowed_tools")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	systemPrompt, fault := typeutil.StringField(config, "system_prompt")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	mcpConfig, fault := typeutil.StringField(config, "mcp_config")
	if fault != nil {
		return passthroughConfig{}, fault
	}

	return passthroughConfig{
		PermissionMode:     permissionMode,
		Model:              model,
		FallbackModel:      fallbackModel,
		MaxTurns:           typeutil.IntFrom(config, "max_turns", 0),
		MaxBudgetUSD:       typeutil.FloatFrom(config, "max_budget_usd", 0),
		Effort:             effort,
		AllowedTools:       allowedTools,
		DisallowedTools:    disallowedTools,
		SystemPrompt:       systemPrompt,
		MCPConfig:          mcpConfig,
		SessionPersistence: typeutil.BoolFrom(config, "session_persistence", true),
	}, nil
}

// buildArgs constructs the CLI argument slice for a Claude Code
// invocation. The arguments are passed directly to exec.Command,
// avoiding shell interpolation.
func buildArgs(state *sessionState, turn int, prompt string, pt passthroughConfig) []string {
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
	}

	if pt.PermissionMode != "" {
		args = append(args, "--permission-mode", pt.PermissionMode)
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}

	// Session management: first turn of a new session uses --session-id,
	// all other cases (continuation turns or continuation sessions) use
	// --resume.
	if turn == 1 && !state.isContinuation {
		args = append(args, "--session-id", state.claudeSessionID)
	} else {
		args = append(args, "--resume", state.claudeSessionID)
	}

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
	if state.mcpConfigPath != "" {
		args = append(args, "--mcp-config", state.mcpConfigPath)
	} else if pt.MCPConfig != "" {
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
