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

// parsePassthroughConfig extracts Copilot CLI-specific settings from the
// raw config map. A missing key uses its zero-value default. A key
// present with a non-string value for a string field reports a fault
// rather than defaulting.
func parsePassthroughConfig(config map[string]any) (passthroughConfig, *typeutil.TypeFault) {
	model, fault := typeutil.StringField(config, "model")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	agent, fault := typeutil.StringField(config, "agent")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	allowedTools, fault := typeutil.StringField(config, "allowed_tools")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	deniedTools, fault := typeutil.StringField(config, "denied_tools")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	availableTools, fault := typeutil.StringField(config, "available_tools")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	excludedTools, fault := typeutil.StringField(config, "excluded_tools")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	mcpConfig, fault := typeutil.StringField(config, "mcp_config")
	if fault != nil {
		return passthroughConfig{}, fault
	}

	return passthroughConfig{
		Model:                 model,
		MaxAutopilotContinues: typeutil.IntFrom(config, "max_autopilot_continues", 0),
		Agent:                 agent,
		AllowedTools:          allowedTools,
		DeniedTools:           deniedTools,
		AvailableTools:        availableTools,
		ExcludedTools:         excludedTools,
		MCPConfig:             mcpConfig,
		DisableBuiltinMCPs:    typeutil.BoolFrom(config, "disable_builtin_mcps", false),
		NoCustomInstructions:  typeutil.BoolFrom(config, "no_custom_instructions", false),
		Experimental:          typeutil.BoolFrom(config, "experimental", false),
	}, nil
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

	args = append(args, "--max-autopilot-continues", strconv.Itoa(effectiveMaxAutopilotContinues(pt)))

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

	// Tool scoping: the blanket grant covers a call no scoping rule
	// mentions, so a deny rule still outranks it and a visibility filter
	// is a different mechanism that keeps its own effect beside it.
	// An approval allow-list is the one exception: it is a subset of the
	// grant, so the grant would subsume and defeat it if both were sent.
	if blanketGrantApplies(pt.AllowedTools) {
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

// effectiveMaxAutopilotContinues resolves the autopilot continuation
// ceiling: pt.MaxAutopilotContinues when positive, or a safe default of 50
// otherwise. It is the only place the default is resolved.
func effectiveMaxAutopilotContinues(pt passthroughConfig) int {
	if pt.MaxAutopilotContinues > 0 {
		return pt.MaxAutopilotContinues
	}
	return 50
}

// blanketGrantApplies reports whether the launch should carry --allow-all.
// The grant is a superset of an approval allow-list, so it is withheld
// when allowedTools holds a non-whitespace value; a deny rule and the two
// visibility filters are different mechanisms and never withhold it.
func blanketGrantApplies(allowedTools string) bool {
	return strings.TrimSpace(allowedTools) == ""
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
