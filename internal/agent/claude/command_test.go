package claude

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/registry"
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

// assertNoFlag fails if flag appears anywhere in args.
func assertNoFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		t.Errorf("buildArgs() unexpectedly contains flag %q in [%s]", flag, strings.Join(args, " "))
		return
	}
}

// newFirstTurnState returns a sessionState suitable for a first-turn invocation.
func newFirstTurnState(mcpConfigPath string) *sessionState {
	return &sessionState{
		claudeSessionID: "test-session-id",
		isContinuation:  false,
		mcpConfigPath:   mcpConfigPath,
	}
}

// TestParsePassthroughConfig_TypeFault covers the funnel's fault path for
// a wrong-typed string field: it returns the zero passthroughConfig and a
// fault whose rendering names the key and the type found. Every string
// key the funnel reads gets its own case, so a key whose fault arm was
// never wired shows up as a gap here rather than at runtime.
func TestParsePassthroughConfig_TypeFault(t *testing.T) {
	t.Parallel()

	keys := []string{
		"permission_mode",
		"model",
		"fallback_model",
		"effort",
		"allowed_tools",
		"disallowed_tools",
		"system_prompt",
		"mcp_config",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			pt, fault := parsePassthroughConfig(map[string]any{key: 123})

			if fault == nil {
				t.Fatalf("parsePassthroughConfig(%s=123) fault = nil, want non-nil", key)
			}
			if fault.Key != key {
				t.Errorf("parsePassthroughConfig(%s=123) fault.Key = %q, want %q", key, fault.Key, key)
			}
			wantErr := key + ": expected string, got integer"
			if fault.Error() != wantErr {
				t.Errorf("parsePassthroughConfig(%s=123) fault.Error() = %q, want %q", key, fault.Error(), wantErr)
			}
			if pt != (passthroughConfig{}) {
				t.Errorf("parsePassthroughConfig(%s=123) passthroughConfig = %+v, want zero value", key, pt)
			}
		})
	}
}

// TestMCPInjectionConformance proves claude-code's real buildArgs
// output carries the generated MCP config path, matching its declared
// disposition.
func TestMCPInjectionConformance(t *testing.T) {
	t.Parallel()

	declared, ok := registry.Agents.Meta("claude-code")
	if !ok {
		t.Fatal(`registry.Agents.Meta("claude-code") reported not registered`)
	}

	const mcpConfigPath = "/ws/.sortie/mcp.json"
	state := newFirstTurnState(mcpConfigPath)
	args := buildArgs(state, 1, "do work", passthroughConfig{SessionPersistence: true})

	agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{Args: args})
}

func TestBuildArgs_MCPConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		mcpConfigPath string
		ptMCPConfig   string
		checkArgs     func(t *testing.T, args []string)
	}{
		{
			// Worker-generated config takes priority over operator config.
			name:          "worker path takes priority over operator",
			mcpConfigPath: "/ws/.sortie/mcp.json",
			ptMCPConfig:   "/op/mcp.json",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--mcp-config", "/ws/.sortie/mcp.json")
			},
		},
		{
			// Operator config is used when no worker config is present.
			name:          "operator config used when worker path absent",
			mcpConfigPath: "",
			ptMCPConfig:   "/op/mcp.json",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--mcp-config", "/op/mcp.json")
			},
		},
		{
			// Neither set: no --mcp-config flag emitted.
			name:          "neither set produces no mcp config flag",
			mcpConfigPath: "",
			ptMCPConfig:   "",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertNoFlag(t, args, "--mcp-config")
			},
		},
		{
			// Only worker path set, no operator config.
			name:          "worker path used when operator config absent",
			mcpConfigPath: "/ws/.sortie/mcp.json",
			ptMCPConfig:   "",
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				assertHasArgPair(t, args, "--mcp-config", "/ws/.sortie/mcp.json")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newFirstTurnState(tt.mcpConfigPath)
			pt := passthroughConfig{MCPConfig: tt.ptMCPConfig, SessionPersistence: true}
			args := buildArgs(state, 1, "do work", pt)
			tt.checkArgs(t, args)
		})
	}
}
