package copilot

import "strconv"

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
		Model:                 stringFrom(config, "model"),
		MaxAutopilotContinues: intFrom(config, "max_autopilot_continues", 0),
		Agent:                 stringFrom(config, "agent"),
		AllowedTools:          stringFrom(config, "allowed_tools"),
		DeniedTools:           stringFrom(config, "denied_tools"),
		AvailableTools:        stringFrom(config, "available_tools"),
		ExcludedTools:         stringFrom(config, "excluded_tools"),
		MCPConfig:             stringFrom(config, "mcp_config"),
		DisableBuiltinMCPs:    boolFrom(config, "disable_builtin_mcps", false),
		NoCustomInstructions:  boolFrom(config, "no_custom_instructions", false),
		Experimental:          boolFrom(config, "experimental", false),
	}
}

// buildArgs constructs the CLI argument slice for a Copilot CLI
// invocation. The arguments are passed directly to exec.Command,
// avoiding shell interpolation.
func buildArgs(state *sessionState, prompt string, pt passthroughConfig) []string {
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
		args = append(args, "--additional-mcp-config", state.mcpConfigPath)
	} else if pt.MCPConfig != "" {
		args = append(args, "--additional-mcp-config", pt.MCPConfig)
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

func stringFrom(config map[string]any, key string) string {
	v, ok := config[key].(string)
	if !ok {
		return ""
	}
	return v
}

// intFrom extracts an integer value from config by key. Handles both
// int and float64 (JSON numbers decode as float64). Fractional
// float64 values are rejected to prevent silent truncation. Returns
// defaultVal if absent, wrong type, or fractional.
func intFrom(config map[string]any, key string, defaultVal int) int {
	raw, ok := config[key]
	if !ok {
		return defaultVal
	}
	switch v := raw.(type) {
	case int:
		return v
	case float64:
		if v != float64(int(v)) {
			return defaultVal
		}
		return int(v)
	default:
		return defaultVal
	}
}

func boolFrom(config map[string]any, key string, defaultVal bool) bool {
	v, ok := config[key].(bool)
	if !ok {
		return defaultVal
	}
	return v
}
