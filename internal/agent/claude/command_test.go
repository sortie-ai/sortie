package claude

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

// newFirstTurnState returns a sessionState suitable for a first-turn invocation.
func newFirstTurnState(mcpConfigPath string) *sessionState {
	return &sessionState{
		claudeSessionID: "test-session-id",
		turnCount:       0,
		isContinuation:  false,
		mcpConfigPath:   mcpConfigPath,
	}
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
			args := buildArgs(state, "do work", pt)
			tt.checkArgs(t, args)
		})
	}
}
