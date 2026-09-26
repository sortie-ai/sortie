package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestAuxiliaryCommand_SuccessYieldsNilError(t *testing.T) {
	t.Parallel()

	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       "/usr/bin/opencode",
			WorkspacePath: t.TempDir(),
		},
	}

	cmd, err := auxiliaryCommand(context.Background(), state, []string{"session", "list"})
	if err != nil {
		t.Fatalf("auxiliaryCommand() error = %v, want nil", err)
	}
	if cmd == nil {
		t.Fatal("auxiliaryCommand() cmd = nil, want non-nil")
	}
}

// envLookup returns the value for key in an env []string slice.
func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if after, ok := strings.CutPrefix(e, prefix); ok {
			return after, true
		}
	}
	return "", false
}

// assertEnvPresent fails unless key is present in env with the given value.
func assertEnvPresent(t *testing.T, env []string, key, wantVal string) {
	t.Helper()
	got, ok := envLookup(env, key)
	if !ok {
		t.Errorf("env %q absent, want %q=%q", key, key, wantVal)
		return
	}
	if got != wantVal {
		t.Errorf("env %q = %q, want %q", key, got, wantVal)
	}
}

// assertEnvAbsent fails if key is present in env.
func assertEnvAbsent(t *testing.T, env []string, key string) {
	t.Helper()
	if _, ok := envLookup(env, key); ok {
		t.Errorf("env %q is present, want absent", key)
	}
}

// assertHasArgPair fails if flag and value do not appear as consecutive
// elements in args.
func assertHasArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("buildRunArgs() missing %q %q in [%s]", flag, value, strings.Join(args, " "))
}

// assertHasFlag fails if flag does not appear in args.
func assertHasFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		return
	}
	t.Errorf("buildRunArgs() missing flag %q in [%s]", flag, strings.Join(args, " "))
}

// assertNoFlag fails if flag appears anywhere in args.
func assertNoFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		t.Errorf("buildRunArgs() unexpected flag %q in [%s]", flag, strings.Join(args, " "))
		return
	}
}

// newTestSessionState returns a sessionState suitable for buildRunArgs tests.
func newTestSessionState(workspacePath, sessionID string) *sessionState {
	return &sessionState{
		target: agentcore.LaunchTarget{
			WorkspacePath: workspacePath,
		},
		sessionID: sessionID,
	}
}

func TestNewOpenCodeAdapter_ParsePassthroughConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    map[string]any
		wantErr   bool
		checkFunc func(t *testing.T, pt passthroughConfig)
	}{
		{
			name:   "defaults",
			config: map[string]any{},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if !pt.DangerousSkipPermissions {
					t.Error("DangerousSkipPermissions = false, want true (default)")
				}
				if !pt.DisableAutocompact {
					t.Error("DisableAutocompact = false, want true (default)")
				}
			},
		},
		{
			name: "allowed_tools_parse",
			config: map[string]any{
				"allowed_tools": []any{"read", "edit"},
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if len(pt.AllowedTools) != 2 {
					t.Fatalf("AllowedTools len = %d, want 2", len(pt.AllowedTools))
				}
				if pt.AllowedTools[0] != "read" {
					t.Errorf("AllowedTools[0] = %q, want %q", pt.AllowedTools[0], "read")
				}
				if pt.AllowedTools[1] != "edit" {
					t.Errorf("AllowedTools[1] = %q, want %q", pt.AllowedTools[1], "edit")
				}
			},
		},
		{
			name: "denied_tools_parse",
			config: map[string]any{
				"denied_tools": []any{"bash"},
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if len(pt.DeniedTools) != 1 {
					t.Fatalf("DeniedTools len = %d, want 1", len(pt.DeniedTools))
				}
				if pt.DeniedTools[0] != "bash" {
					t.Errorf("DeniedTools[0] = %q, want %q", pt.DeniedTools[0], "bash")
				}
			},
		},
		{
			name: "unknown_key_preserved",
			config: map[string]any{
				"allowed_tools": []any{"customtool"},
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if len(pt.AllowedTools) != 1 || pt.AllowedTools[0] != "customtool" {
					t.Errorf("AllowedTools = %v, want [customtool]", pt.AllowedTools)
				}
			},
		},
		{
			name: "overlap_error",
			config: map[string]any{
				"allowed_tools": []any{"bash"},
				"denied_tools":  []any{"bash"},
			},
			wantErr: true,
		},
		{
			name: "model_and_flags",
			config: map[string]any{
				"model":                        "anthropic/claude-3-5-sonnet",
				"dangerously_skip_permissions": false,
				"disable_autocompact":          false,
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if pt.Model != "anthropic/claude-3-5-sonnet" {
					t.Errorf("Model = %q, want %q", pt.Model, "anthropic/claude-3-5-sonnet")
				}
				if pt.DangerousSkipPermissions {
					t.Error("DangerousSkipPermissions = true, want false")
				}
				if pt.DisableAutocompact {
					t.Error("DisableAutocompact = true, want false")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := NewOpenCodeAdapter(tt.config)

			if tt.wantErr {
				if err == nil {
					t.Fatal("NewOpenCodeAdapter() error = nil, want error")
				}
				if !strings.Contains(err.Error(), "bash") {
					t.Errorf("error = %q, want it to mention %q", err.Error(), "bash")
				}
				return
			}

			if err != nil {
				t.Fatalf("NewOpenCodeAdapter() error = %v", err)
			}

			oc, ok := a.(*OpenCodeAdapter)
			if !ok {
				t.Fatalf("adapter type = %T, want *OpenCodeAdapter", a)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, oc.passthrough)
			}
		})
	}
}

// TestParsePassthroughConfig_TypeFault covers the funnel's fault path for
// a wrong-typed string field: it returns the zero passthroughConfig and a
// fault whose rendering names the key and the type found. Every string
// key the funnel reads gets its own case, so a key whose fault arm was
// never wired shows up as a gap here rather than at runtime.
func TestParsePassthroughConfig_TypeFault(t *testing.T) {
	t.Parallel()

	keys := []string{"model", "agent", "variant"}

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
			if pt.Model != "" || pt.Agent != "" || pt.Variant != "" || pt.AllowedTools != nil || pt.DeniedTools != nil {
				t.Errorf("parsePassthroughConfig(%s=123) passthroughConfig = %+v, want zero value", key, pt)
			}
		})
	}
}

// TestMCPInjectionConformance proves opencode's real launch surface
// matches its declared disposition, on both a local and a remote
// launch. Both channels the adapter builds are captured, not the
// argument slice alone: opencode is the adapter that carries most of
// its configuration through the environment, so an environment-only
// leak would otherwise go unseen.
func TestMCPInjectionConformance(t *testing.T) {
	t.Parallel()

	declared, ok := registry.Agents.Meta("opencode")
	if !ok {
		t.Fatal(`registry.Agents.Meta("opencode") reported not registered`)
	}

	dir := t.TempDir()
	mcpConfigPath := filepath.Join(dir, ".sortie", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpConfigPath), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	const generatedConfig = `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server","--workflow","/repo/WORKFLOW.md"],"env":{"SORTIE_ISSUE_ID":"abc-123"}}}}`
	if err := os.WriteFile(mcpConfigPath, []byte(generatedConfig), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Run("local launch delivers the translated document", func(t *testing.T) {
		t.Parallel()

		servers, err := translateMCPServers(mcpConfigPath, false)
		if err != nil {
			t.Fatalf("translateMCPServers() error = %v", err)
		}
		document, err := json.Marshal(mcpConfigDocument{MCP: servers})
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}

		state := newTestSessionState("/workspace", "")
		state.major = major1
		args := buildRunArgs(state, "do work", passthroughConfig{})
		env, err := buildRunEnv([]string{"PATH=/usr/bin"}, passthroughConfig{}, major1)
		if err != nil {
			t.Fatalf("buildRunEnv() error = %v", err)
		}
		env = append(env, "OPENCODE_CONFIG_CONTENT="+string(document))

		agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{Args: args, Env: env})
	})

	t.Run("remote launch delivers nothing", func(t *testing.T) {
		t.Parallel()

		state := newTestSessionState("/workspace", "")
		state.major = major1
		args := buildRunArgs(state, "do work", passthroughConfig{})
		env, err := buildRunEnv([]string{"PATH=/usr/bin"}, passthroughConfig{}, major1)
		if err != nil {
			t.Fatalf("buildRunEnv() error = %v", err)
		}

		// StartSession's remote guard skips rendering entirely, so a
		// remote session's turn environment carries nothing to append
		// here.
		agenttest.AssertMCPInjection(t, registry.MCPInjectionUnsupported, mcpConfigPath, agenttest.MCPLaunchSurface{Args: args, Env: env})
	})
}

// TestBuildMCPConfigEntries_DeclaresEveryServer asserts that the
// translated entries declare every server from the generated file, in
// the runtime's own "local"/"remote" entry shapes.
func TestBuildMCPConfigEntries_DeclaresEveryServer(t *testing.T) {
	t.Parallel()

	servers := []mcpconfig.Server{
		{
			Name:      "sortie-tools",
			Transport: mcpconfig.TransportStdio,
			Command:   "/usr/local/bin/sortie",
			Args:      []string{"mcp-server"},
			Env:       map[string]string{"SORTIE_ISSUE_ID": "abc-123"},
		},
		{
			Name:      "remote-tools",
			Transport: mcpconfig.TransportHTTP,
			URL:       "https://example.invalid/mcp",
			Headers:   map[string]string{"Authorization": "Bearer token"},
		},
	}

	entries, err := buildMCPConfigEntries(servers)
	if err != nil {
		t.Fatalf("buildMCPConfigEntries() error = %v", err)
	}

	local, ok := entries["sortie-tools"]
	if !ok {
		t.Fatal("buildMCPConfigEntries() entries missing \"sortie-tools\" entry")
	}
	if local.Type != "local" {
		t.Errorf("sortie-tools entry Type = %q, want %q", local.Type, "local")
	}
	wantCommand := []string{"/usr/local/bin/sortie", "mcp-server"}
	if !slices.Equal(local.Command, wantCommand) {
		t.Errorf("sortie-tools entry Command = %v, want %v", local.Command, wantCommand)
	}
	if local.Environment["SORTIE_ISSUE_ID"] != "abc-123" {
		t.Errorf("sortie-tools entry Environment[%q] = %q, want %q", "SORTIE_ISSUE_ID", local.Environment["SORTIE_ISSUE_ID"], "abc-123")
	}
	if !local.Enabled {
		t.Error("sortie-tools entry Enabled = false, want true (nil Server.Enabled defaults true)")
	}

	remote, ok := entries["remote-tools"]
	if !ok {
		t.Fatal("buildMCPConfigEntries() entries missing \"remote-tools\" entry")
	}
	if remote.Type != "remote" {
		t.Errorf("remote-tools entry Type = %q, want %q", remote.Type, "remote")
	}
	if remote.URL != "https://example.invalid/mcp" {
		t.Errorf("remote-tools entry URL = %q, want %q", remote.URL, "https://example.invalid/mcp")
	}
	if remote.Headers["Authorization"] != "Bearer token" {
		t.Errorf("remote-tools entry Headers[%q] = %q, want %q", "Authorization", remote.Headers["Authorization"], "Bearer token")
	}
}

// TestBuildMCPConfigEntries_EnabledFalseRoundTrips asserts that an
// entry carrying enabled: false round-trips as a disabled server.
func TestBuildMCPConfigEntries_EnabledFalseRoundTrips(t *testing.T) {
	t.Parallel()

	disabled := false
	servers := []mcpconfig.Server{
		{
			Name:      "sortie-tools",
			Transport: mcpconfig.TransportStdio,
			Command:   "/usr/local/bin/sortie",
			Enabled:   &disabled,
		},
	}

	entries, err := buildMCPConfigEntries(servers)
	if err != nil {
		t.Fatalf("buildMCPConfigEntries() error = %v", err)
	}

	entry, ok := entries["sortie-tools"]
	if !ok {
		t.Fatal("buildMCPConfigEntries() entries missing \"sortie-tools\" entry")
	}
	if entry.Enabled {
		t.Error("sortie-tools entry Enabled = true, want false (Server.Enabled = false must round-trip)")
	}
}

// TestTranslateMCPServers asserts translateMCPServers returns nil for
// a remote launch, an empty path, and a configuration declaring no
// server, and otherwise returns the same entries buildMCPConfigEntries
// builds for a stdio and an HTTP server.
func TestTranslateMCPServers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sortie"), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	mcpConfigPath := filepath.Join(dir, ".sortie", "mcp.json")
	const generatedConfig = `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server"]},"remote-tools":{"type":"http","url":"https://example.invalid/mcp"}}}`
	if err := os.WriteFile(mcpConfigPath, []byte(generatedConfig), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	emptyConfigPath := filepath.Join(dir, ".sortie", "empty.json")
	if err := os.WriteFile(emptyConfigPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	nilCases := []struct {
		name   string
		path   string
		remote bool
	}{
		{name: "remote launch", path: mcpConfigPath, remote: true},
		{name: "empty path", path: "", remote: false},
		{name: "no server configured", path: emptyConfigPath, remote: false},
	}
	for _, tt := range nilCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := translateMCPServers(tt.path, tt.remote)
			if err != nil {
				t.Fatalf("translateMCPServers() error = %v", err)
			}
			if got != nil {
				t.Errorf("translateMCPServers(%q, remote=%v) = %v, want nil", tt.path, tt.remote, got)
			}
		})
	}

	t.Run("local launch matches buildMCPConfigEntries", func(t *testing.T) {
		t.Parallel()

		got, err := translateMCPServers(mcpConfigPath, false)
		if err != nil {
			t.Fatalf("translateMCPServers() error = %v", err)
		}
		want, err := buildMCPConfigEntries([]mcpconfig.Server{
			{Name: "sortie-tools", Transport: mcpconfig.TransportStdio, Command: "/usr/local/bin/sortie", Args: []string{"mcp-server"}},
			{Name: "remote-tools", Transport: mcpconfig.TransportHTTP, URL: "https://example.invalid/mcp"},
		})
		if err != nil {
			t.Fatalf("buildMCPConfigEntries() error = %v", err)
		}

		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("json.Marshal(got): %v", err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal(want): %v", err)
		}
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("translateMCPServers() = %s, want %s", gotJSON, wantJSON)
		}
	})
}

// TestBuildInlineConfig asserts the 2.x inline document's marshaled
// shape: share is always present as "disabled"; permission, compaction,
// and mcp are each present only when their source data is non-empty.
func TestBuildInlineConfig(t *testing.T) {
	t.Parallel()

	servers := map[string]mcpConfigDocumentEntry{
		"sortie-tools": {Type: "local", Command: []string{"/usr/local/bin/sortie"}, Enabled: true},
	}

	tests := []struct {
		name        string
		pt          passthroughConfig
		servers     map[string]mcpConfigDocumentEntry
		wantPerm    bool
		wantCompact bool
		wantMCP     bool
	}{
		{name: "bare configuration carries only share"},
		{name: "a tool policy adds permission", pt: passthroughConfig{AllowedTools: []string{"read"}}, wantPerm: true},
		{name: "disable_autocompact adds compaction", pt: passthroughConfig{DisableAutocompact: true}, wantCompact: true},
		{name: "servers add mcp", servers: servers, wantMCP: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := buildInlineConfig(tt.pt, tt.servers)
			if err != nil {
				t.Fatalf("buildInlineConfig() error = %v", err)
			}

			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(encoded), &doc); err != nil {
				t.Fatalf("json.Unmarshal(%q): %v", encoded, err)
			}

			if string(doc["share"]) != `"disabled"` {
				t.Errorf("buildInlineConfig() share = %s, want %q", doc["share"], "disabled")
			}
			if _, ok := doc["permission"]; ok != tt.wantPerm {
				t.Errorf("buildInlineConfig() permission present = %v, want %v", ok, tt.wantPerm)
			}
			compaction, hasCompaction := doc["compaction"]
			if hasCompaction != tt.wantCompact {
				t.Errorf("buildInlineConfig() compaction present = %v, want %v", hasCompaction, tt.wantCompact)
			} else if hasCompaction && string(compaction) != `{"auto":false}` {
				t.Errorf("buildInlineConfig() compaction = %s, want %s", compaction, `{"auto":false}`)
			}
			if _, ok := doc["mcp"]; ok != tt.wantMCP {
				t.Errorf("buildInlineConfig() mcp present = %v, want %v", ok, tt.wantMCP)
			}
		})
	}
}

// TestRunTurn_InheritedOpencodeConfigContentScrubbed asserts that an
// inherited OPENCODE_CONFIG_CONTENT value from the parent process is
// scrubbed from the turn environment, so only the adapter's own
// rendered document, appended after buildRunEnv, can ever set it.
func TestRunTurn_InheritedOpencodeConfigContentScrubbed(t *testing.T) {
	t.Parallel()

	base := []string{"OPENCODE_CONFIG_CONTENT=inherited-from-parent-process", "PATH=/usr/bin"}

	env, err := buildRunEnv(base, passthroughConfig{}, major1)
	if err != nil {
		t.Fatalf("buildRunEnv() error = %v", err)
	}

	assertEnvAbsent(t, env, "OPENCODE_CONFIG_CONTENT")

	// The adapter's own document is appended only after buildRunEnv
	// returns, so the final turn environment carries exactly the
	// adapter's own value, never the inherited one.
	env = append(env, "OPENCODE_CONFIG_CONTENT=own-document")
	assertEnvPresent(t, env, "OPENCODE_CONFIG_CONTENT", "own-document")
}

func TestBuildRunArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		sessionID   string
		pt          passthroughConfig
		prompt      string
		wantPresent []string
		wantPairs   [][2]string
		wantAbsent  []string
	}{
		{
			name:       "fresh_session",
			sessionID:  "",
			pt:         passthroughConfig{},
			prompt:     "do work",
			wantAbsent: []string{"--session"},
		},
		{
			name:      "resume_session",
			sessionID: "ses_abc",
			pt:        passthroughConfig{},
			prompt:    "continue",
			wantPairs: [][2]string{{"--session", "ses_abc"}},
		},
		{
			name:        "skip_permissions_default",
			sessionID:   "",
			pt:          passthroughConfig{DangerousSkipPermissions: true},
			prompt:      "work",
			wantPresent: []string{"--dangerously-skip-permissions"},
		},
		{
			name:       "skip_permissions_disabled",
			sessionID:  "",
			pt:         passthroughConfig{DangerousSkipPermissions: false},
			prompt:     "work",
			wantAbsent: []string{"--dangerously-skip-permissions"},
		},
		{
			name:      "model_flag",
			sessionID: "",
			pt:        passthroughConfig{Model: "anthropic/claude-3-5-sonnet"},
			prompt:    "work",
			wantPairs: [][2]string{{"--model", "anthropic/claude-3-5-sonnet"}},
		},
		{
			name:      "prompt_after_dashdash",
			sessionID: "",
			pt:        passthroughConfig{},
			prompt:    "my --prompt with flags",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newTestSessionState("/tmp/workspace", tt.sessionID)
			args := buildRunArgs(state, tt.prompt, tt.pt)

			for _, flag := range tt.wantPresent {
				assertHasFlag(t, args, flag)
			}
			for _, pair := range tt.wantPairs {
				assertHasArgPair(t, args, pair[0], pair[1])
			}
			for _, flag := range tt.wantAbsent {
				assertNoFlag(t, args, flag)
			}

			// Prompt must be the last argument, after "--".
			if len(args) < 2 {
				t.Fatalf("args too short: %v", args)
			}
			lastTwo := args[len(args)-2:]
			if lastTwo[0] != "--" {
				t.Errorf("second-to-last arg = %q, want %q", lastTwo[0], "--")
			}
			if lastTwo[1] != tt.prompt {
				t.Errorf("last arg = %q, want prompt %q", lastTwo[1], tt.prompt)
			}
		})
	}
}

// TestBuildRunArgs_DefaultConfigurationSkipsPermissions asserts that the
// full configuration path, from an empty WORKFLOW.md opencode sub-object
// through buildRunArgs, passes --dangerously-skip-permissions. This is the
// launch posture the finalizeExitedTurn refusal path assumes: no reply
// channel, so a recognized permission warning always reads
// AnswerRuntimeRefused.
func TestBuildRunArgs_DefaultConfigurationSkipsPermissions(t *testing.T) {
	t.Parallel()

	pt, err := parsePassthroughConfig(map[string]any{})
	if err != nil {
		t.Fatalf("parsePassthroughConfig(map[string]any{}) error = %v", err)
	}

	state := newTestSessionState("/tmp/workspace", "")
	args := buildRunArgs(state, "work", pt)

	assertHasFlag(t, args, "--dangerously-skip-permissions")
}

// TestSortedEnvVars asserts that sortedEnvVars converts a managed map
// into a name-sorted []sshutil.EnvVar slice, nil for an empty or nil
// map.
func TestSortedEnvVars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		managed map[string]string
		want    []sshutil.EnvVar
	}{
		{
			name:    "nil map yields nil",
			managed: nil,
			want:    nil,
		},
		{
			name:    "empty map yields nil",
			managed: map[string]string{},
			want:    nil,
		},
		{
			name: "sorted by name regardless of map iteration order",
			managed: map[string]string{
				"OPENCODE_DISABLE_AUTOUPDATE": "true",
				"OPENCODE_AUTO_SHARE":         "false",
			},
			want: []sshutil.EnvVar{
				{Name: "OPENCODE_AUTO_SHARE", Value: "false"},
				{Name: "OPENCODE_DISABLE_AUTOUPDATE", Value: "true"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := sortedEnvVars(tt.managed)
			if !slices.Equal(got, tt.want) {
				t.Errorf("sortedEnvVars(%v) = %+v, want %+v", tt.managed, got, tt.want)
			}
		})
	}
}

func TestBuildRunEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		base      []string
		pt        passthroughConfig
		checkFunc func(t *testing.T, env []string)
	}{
		{
			name: "baseline_always_set",
			base: []string{},
			pt:   passthroughConfig{},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_AUTO_SHARE", "false")
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOUPDATE", "true")
				assertEnvPresent(t, env, "OPENCODE_DISABLE_LSP_DOWNLOAD", "true")
			},
		},
		{
			name: "autocompact_default_true",
			base: []string{},
			pt:   passthroughConfig{DisableAutocompact: true},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOCOMPACT", "true")
			},
		},
		{
			name: "autocompact_disabled",
			base: []string{},
			pt:   passthroughConfig{DisableAutocompact: false},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOCOMPACT", "false")
			},
		},
		{
			name: "inherited_permission_removed",
			base: []string{"OPENCODE_PERMISSION=old_value", "OTHER_VAR=keep"},
			pt:   passthroughConfig{},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvAbsent(t, env, "OPENCODE_PERMISSION")
				assertEnvPresent(t, env, "OTHER_VAR", "keep")
			},
		},
		{
			name: "allowed_tools_policy",
			base: []string{},
			pt:   passthroughConfig{AllowedTools: []string{"read"}},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				raw, ok := envLookup(env, "OPENCODE_PERMISSION")
				if !ok {
					t.Fatal("OPENCODE_PERMISSION absent")
				}
				var policy map[string]string
				if err := json.Unmarshal([]byte(raw), &policy); err != nil {
					t.Fatalf("OPENCODE_PERMISSION unmarshal: %v", err)
				}
				if policy["read"] != "allow" {
					t.Errorf("OPENCODE_PERMISSION[read] = %q, want %q", policy["read"], "allow")
				}
				if policy["bash"] != "deny" {
					t.Errorf("OPENCODE_PERMISSION[bash] = %q, want %q", policy["bash"], "deny")
				}
			},
		},
		{
			name: "denied_tools_policy",
			base: []string{},
			pt:   passthroughConfig{DeniedTools: []string{"bash"}},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				raw, ok := envLookup(env, "OPENCODE_PERMISSION")
				if !ok {
					t.Fatal("OPENCODE_PERMISSION absent")
				}
				var policy map[string]string
				if err := json.Unmarshal([]byte(raw), &policy); err != nil {
					t.Fatalf("OPENCODE_PERMISSION unmarshal: %v", err)
				}
				if policy["bash"] != "deny" {
					t.Errorf("OPENCODE_PERMISSION[bash] = %q, want %q", policy["bash"], "deny")
				}
			},
		},
		{
			name: "no_policy_no_permission_key",
			base: []string{},
			pt:   passthroughConfig{},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvAbsent(t, env, "OPENCODE_PERMISSION")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env, err := buildRunEnv(tt.base, tt.pt, major1)
			if err != nil {
				t.Fatalf("buildRunEnv() error = %v", err)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, env)
			}
		})
	}
}
