package copilot

import (
	"strings"
	"testing"
)

// assertHasArgPair fails if flag and value do not appear as consecutive
// elements in args.
func assertHasArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("buildArgs() missing %q %q in [%s]", flag, value, strings.Join(args, " "))
}

// assertHasFlag fails if flag does not appear anywhere in args.
func assertHasFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			return
		}
	}
	t.Errorf("buildArgs() missing flag %q in [%s]", flag, strings.Join(args, " "))
}

// assertNoFlag fails if flag appears anywhere in args.
func assertNoFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			t.Errorf("buildArgs() unexpectedly contains flag %q in [%s]", flag, strings.Join(args, " "))
			return
		}
	}
}

func TestParsePassthroughConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
		want   passthroughConfig
	}{
		{
			name:   "empty config produces zero-value struct",
			config: map[string]any{},
			want:   passthroughConfig{},
		},
		{
			name:   "nil config produces zero-value struct",
			config: nil,
			want:   passthroughConfig{},
		},
		{
			name: "all fields set",
			config: map[string]any{
				"model":                   "gpt-5",
				"max_autopilot_continues": float64(25), // JSON numbers decode as float64
				"agent":                   "custom-agent",
				"allowed_tools":           "bash edit_file",
				"denied_tools":            "ask_user",
				"available_tools":         "bash",
				"excluded_tools":          "web_fetch",
				"mcp_config":              "/path/to/mcp.json",
				"disable_builtin_mcps":    true,
				"no_custom_instructions":  true,
				"experimental":            true,
			},
			want: passthroughConfig{
				Model:                 "gpt-5",
				MaxAutopilotContinues: 25,
				Agent:                 "custom-agent",
				AllowedTools:          "bash edit_file",
				DeniedTools:           "ask_user",
				AvailableTools:        "bash",
				ExcludedTools:         "web_fetch",
				MCPConfig:             "/path/to/mcp.json",
				DisableBuiltinMCPs:    true,
				NoCustomInstructions:  true,
				Experimental:          true,
			},
		},
		{
			name:   "partial config only model",
			config: map[string]any{"model": "claude-opus-4.6"},
			want:   passthroughConfig{Model: "claude-opus-4.6"},
		},
		{
			name:   "integer max_autopilot_continues accepted",
			config: map[string]any{"max_autopilot_continues": int(10)},
			want:   passthroughConfig{MaxAutopilotContinues: 10},
		},
		{
			name:   "fractional float for int field returns default zero",
			config: map[string]any{"max_autopilot_continues": float64(12.5)},
			want:   passthroughConfig{},
		},
		{
			name:   "wrong type for string field produces empty string",
			config: map[string]any{"model": 42},
			want:   passthroughConfig{},
		},
		{
			name:   "wrong type for bool field produces false",
			config: map[string]any{"experimental": "yes"},
			want:   passthroughConfig{},
		},
		{
			name:   "bool fields default to false when absent",
			config: map[string]any{"model": "m"},
			want:   passthroughConfig{Model: "m"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parsePassthroughConfig(tt.config)
			if got != tt.want {
				t.Errorf("parsePassthroughConfig() =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}

func TestBuildArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		state  *sessionState
		prompt string
		pt     passthroughConfig
		check  func(t *testing.T, args []string)
	}{
		{
			name: "first turn no session ID has no resume or continue",
			state: &sessionState{
				copilotSessionID:   "",
				fallbackToContinue: false,
			},
			prompt: "fix the bug",
			pt:     passthroughConfig{},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "-p", "fix the bug")
				assertHasArgPair(t, args, "--output-format", "json")
				assertHasFlag(t, args, "-s")
				assertHasFlag(t, args, "--allow-all")
				assertHasFlag(t, args, "--autopilot")
				assertHasFlag(t, args, "--no-ask-user")
				assertHasArgPair(t, args, "--max-autopilot-continues", "50")
				assertNoFlag(t, args, "--resume")
				assertNoFlag(t, args, "--continue")
			},
		},
		{
			name: "turn with copilotSessionID uses resume flag",
			state: &sessionState{
				copilotSessionID:   "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f",
				fallbackToContinue: false,
			},
			prompt: "continue task",
			pt:     passthroughConfig{},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--resume", "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f")
				assertNoFlag(t, args, "--continue")
			},
		},
		{
			name: "fallbackToContinue uses continue not resume",
			state: &sessionState{
				copilotSessionID:   "",
				fallbackToContinue: true,
			},
			prompt: "retry",
			pt:     passthroughConfig{},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasFlag(t, args, "--continue")
				assertNoFlag(t, args, "--resume")
			},
		},
		{
			name: "fallbackToContinue takes priority over empty session ID",
			state: &sessionState{
				copilotSessionID:   "",
				fallbackToContinue: true,
			},
			prompt: "p",
			pt:     passthroughConfig{},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasFlag(t, args, "--continue")
				assertNoFlag(t, args, "--resume")
			},
		},
		{
			name:   "custom max autopilot continues overrides default",
			state:  &sessionState{},
			prompt: "p",
			pt:     passthroughConfig{MaxAutopilotContinues: 10},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--max-autopilot-continues", "10")
			},
		},
		{
			name:   "zero max autopilot continues falls back to default 50",
			state:  &sessionState{},
			prompt: "p",
			pt:     passthroughConfig{MaxAutopilotContinues: 0},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--max-autopilot-continues", "50")
			},
		},
		{
			name:   "negative max autopilot continues falls back to default 50",
			state:  &sessionState{},
			prompt: "p",
			pt:     passthroughConfig{MaxAutopilotContinues: -1},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--max-autopilot-continues", "50")
			},
		},
		{
			name:   "all passthrough flags included",
			state:  &sessionState{},
			prompt: "work",
			pt: passthroughConfig{
				Model:                 "gpt-5",
				MaxAutopilotContinues: 20,
				Agent:                 "my-agent",
				AllowedTools:          "bash edit_file",
				DeniedTools:           "ask_user",
				AvailableTools:        "bash",
				ExcludedTools:         "web_fetch",
				MCPConfig:             "/path/to/mcp.json",
				DisableBuiltinMCPs:    true,
				NoCustomInstructions:  true,
				Experimental:          true,
			},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--model", "gpt-5")
				assertHasArgPair(t, args, "--max-autopilot-continues", "20")
				assertHasArgPair(t, args, "--agent", "my-agent")
				assertHasArgPair(t, args, "--allow-tool", "bash edit_file")
				assertHasArgPair(t, args, "--deny-tool", "ask_user")
				assertHasArgPair(t, args, "--available-tools", "bash")
				assertHasArgPair(t, args, "--excluded-tools", "web_fetch")
				assertHasArgPair(t, args, "--additional-mcp-config", "@/path/to/mcp.json")
				assertHasFlag(t, args, "--disable-builtin-mcps")
				assertHasFlag(t, args, "--no-custom-instructions")
				assertHasFlag(t, args, "--experimental")
			},
		},
		{
			name:   "empty passthrough config produces no optional flags",
			state:  &sessionState{},
			prompt: "p",
			pt:     passthroughConfig{},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertNoFlag(t, args, "--model")
				assertNoFlag(t, args, "--agent")
				assertNoFlag(t, args, "--allow-tool")
				assertNoFlag(t, args, "--deny-tool")
				assertNoFlag(t, args, "--available-tools")
				assertNoFlag(t, args, "--excluded-tools")
				assertNoFlag(t, args, "--additional-mcp-config")
				assertNoFlag(t, args, "--disable-builtin-mcps")
				assertNoFlag(t, args, "--no-custom-instructions")
				assertNoFlag(t, args, "--experimental")
			},
		},
		{
			// Worker-generated path takes priority over operator-configured path.
			name:   "worker mcp config takes priority over operator",
			state:  &sessionState{mcpConfigPath: "/ws/.sortie/mcp.json"},
			prompt: "p",
			pt:     passthroughConfig{MCPConfig: "/op/mcp.json"},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--additional-mcp-config", "@/ws/.sortie/mcp.json")
			},
		},
		{
			name:   "worker mcp config used when operator config absent",
			state:  &sessionState{mcpConfigPath: "/ws/.sortie/mcp.json"},
			prompt: "p",
			pt:     passthroughConfig{},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--additional-mcp-config", "@/ws/.sortie/mcp.json")
			},
		},
		{
			name:   "whitespace-only operator mcp config omitted",
			state:  &sessionState{},
			prompt: "p",
			pt:     passthroughConfig{MCPConfig: "   "},
			check: func(t *testing.T, args []string) {
				t.Helper()
				assertNoFlag(t, args, "--additional-mcp-config")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildArgs(tt.state, tt.prompt, tt.pt)
			tt.check(t, got)
		})
	}
}

func TestFormatMCPConfigValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "inline JSON passed as-is",
			input: `{"mcpServers":{}}`,
			want:  `{"mcpServers":{}}`,
		},
		{
			name:  "inline JSON with leading whitespace is trimmed",
			input: `  {"mcpServers":{}}`,
			want:  `{"mcpServers":{}}`,
		},
		{
			name:  "bare absolute path gets @ prefix",
			input: "/path/to/mcp.json",
			want:  "@/path/to/mcp.json",
		},
		{
			name:  "bare relative path gets @ prefix",
			input: "mcp.json",
			want:  "@mcp.json",
		},
		{
			name:  "at-prefixed path passed as-is",
			input: "@/path/to/mcp.json",
			want:  "@/path/to/mcp.json",
		},
		{
			name:  "windows path gets @ prefix",
			input: `C:\path\to\mcp.json`,
			want:  `@C:\path\to\mcp.json`,
		},
		{
			name:  "empty string stays empty",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatMCPConfigValue(tt.input)
			if got != tt.want {
				t.Errorf("formatMCPConfigValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildArgs_AlwaysPresent(t *testing.T) {
	t.Parallel()

	// Required flags must appear in every invocation regardless of configuration.
	required := []string{"-p", "--output-format", "-s", "--allow-all", "--autopilot", "--no-ask-user", "--max-autopilot-continues"}
	got := buildArgs(&sessionState{}, "test prompt", passthroughConfig{})

	for _, flag := range required {
		assertHasFlag(t, got, flag)
	}

	// The prompt value must follow -p.
	assertHasArgPair(t, got, "-p", "test prompt")
	assertHasArgPair(t, got, "--output-format", "json")
}
