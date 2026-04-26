package copilot

import (
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// passthroughConfig holds Copilot CLI-specific settings extracted from
// the "copilot-cli" sub-object in WORKFLOW.md. All fields are optional
// with zero-value meaning "not configured."
type passthroughConfig struct {
	Model                 string
	MaxAutopilotContinues int
	Agent                 string
	AllowedTools          string
	DeniedTools           string
	AvailableTools        string
	ExcludedTools         string
	MCPConfig             string
	DisableBuiltinMCPs    bool
	NoCustomInstructions  bool
	Experimental          bool
}

// parsePassthroughConfig extracts Copilot CLI-specific settings from
// the raw config map. Missing or wrong-typed keys use zero-value
// defaults.
func parsePassthroughConfig(config map[string]any) passthroughConfig {
	return passthroughConfig{
		Model:                 typeutil.StringFrom(config, "model"),
		MaxAutopilotContinues: typeutil.IntFrom(config, "max_autopilot_continues", 0),
		Agent:                 typeutil.StringFrom(config, "agent"),
		AllowedTools:          typeutil.StringFrom(config, "allowed_tools"),
		DeniedTools:           typeutil.StringFrom(config, "denied_tools"),
		AvailableTools:        typeutil.StringFrom(config, "available_tools"),
		ExcludedTools:         typeutil.StringFrom(config, "excluded_tools"),
		MCPConfig:             typeutil.StringFrom(config, "mcp_config"),
		DisableBuiltinMCPs:    typeutil.BoolFrom(config, "disable_builtin_mcps", false),
		NoCustomInstructions:  typeutil.BoolFrom(config, "no_custom_instructions", false),
		Experimental:          typeutil.BoolFrom(config, "experimental", false),
	}
}

// buildArgs constructs the CLI argument slice for a Copilot CLI
// invocation. The arguments are passed directly to exec.Command,
// avoiding shell interpolation.
func buildArgs(state *sessionState, turn int, prompt string, pt passthroughConfig) []string { //nolint:unparam // turn mirrors the ForkPerTurnHooks.BuildArgs signature; copilot tracks sessions via state fields
	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"-s",
		"--autopilot",
		"--no-ask-user",
	}

	// Autopilot limit: safe default of 50 when not configured.
	maxContinues := pt.MaxAutopilotContinues
	if maxContinues <= 0 {
		maxContinues = 50
	}
	args = append(args, "--max-autopilot-continues", strconv.Itoa(maxContinues))

	// Session resume: fallback to --continue when session ID was
	// never captured, or use --resume with the known session ID.
	if state.fallbackToContinue {
		args = append(args, "--continue")
	} else if state.copilotSessionID != "" {
		args = append(args, "--resume", state.copilotSessionID)
	}

	if pt.Model != "" {
		args = append(args, "--model", pt.Model)
	}
	if pt.Agent != "" {
		args = append(args, "--agent", pt.Agent)
	}

	// Tool scoping: use --allow-all only when no explicit tool scoping
	// flags are configured. --allow-all overrides scoped flags.
	hasToolScoping := pt.AllowedTools != "" || pt.DeniedTools != "" ||
		pt.AvailableTools != "" || pt.ExcludedTools != ""
	if !hasToolScoping {
		args = append(args, "--allow-all")
	}
	if pt.AllowedTools != "" {
		args = append(args, "--allow-tool", pt.AllowedTools)
	}
	if pt.DeniedTools != "" {
		args = append(args, "--deny-tool", pt.DeniedTools)
	}
	if pt.AvailableTools != "" {
		args = append(args, "--available-tools", pt.AvailableTools)
	}
	if pt.ExcludedTools != "" {
		args = append(args, "--excluded-tools", pt.ExcludedTools)
	}
	if state.mcpConfigPath != "" {
		args = append(args, "--additional-mcp-config", "@"+state.mcpConfigPath)
	} else if v := formatMCPConfigValue(pt.MCPConfig); v != "" {
		args = append(args, "--additional-mcp-config", v)
	}
	if pt.DisableBuiltinMCPs {
		args = append(args, "--disable-builtin-mcps")
	}
	if pt.NoCustomInstructions {
		args = append(args, "--no-custom-instructions")
	}
	if pt.Experimental {
		args = append(args, "--experimental")
	}

	return args
}

// formatMCPConfigValue prepares an operator-provided MCP config value
// for the --additional-mcp-config flag. Inline JSON (value starting
// with "{") is passed as-is. File paths are prefixed with "@" so the
// Copilot CLI reads the file. Values already prefixed with "@" are
// passed through unchanged.
func formatMCPConfigValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "@") {
		return trimmed
	}
	return "@" + trimmed
}
