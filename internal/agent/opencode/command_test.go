package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const toolChildScenario = "opencode.toolchild"

func init() {
	fakeScenarios[toolChildScenario] = agenttest.Typed(func(args []string, _ struct{}) int {
		if !slices.Contains(args, "--http") {
			return 2
		}
		fmt.Println(`{"listening":"127.0.0.1:45678"}`)
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	})
}

func resolveRemoteState(t *testing.T) *sessionState {
	t.Helper()

	workspace := t.TempDir()
	configPath := filepath.Join(workspace, ".sortie", "mcp.json")
	if err := os.Mkdir(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("Mkdir(%q): %v", filepath.Dir(configPath), err)
	}
	tool := agenttest.FakeRuntime(t, t.TempDir(), "sortie", toolChildScenario, struct{}{})
	config := fmt.Sprintf(`{"mcpServers":{"sortie-tools":{"command":%q,"args":["mcp-server"]}}}`, tool)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", configPath, err)
	}
	target := agentcore.LaunchTarget{
		Command:       agenttest.NewSSHStandIn(t, t.TempDir(), agenttest.SSHStandIn{PATH: os.Getenv("PATH"), Forward: agenttest.SSHForwardAllocated}),
		SSHHost:       "user@stand-in-host",
		WorkspacePath: workspace,
		RemoteCommand: "opencode",
	}
	tools, agentErr := agentcore.ResolveToolDelivery(waittest.Context(t), domain.StartSessionParams{MCPConfigPath: configPath}, target, registry.MCPDialectMCPMemberJSON, nil)
	if agentErr != nil {
		t.Fatalf("ResolveToolDelivery() = %v, want nil", agentErr)
	}
	t.Cleanup(func() { _ = tools.Close() })

	pt, fault := parsePassthroughConfig(map[string]any{})
	if fault != nil {
		t.Fatalf("parsePassthroughConfig() = %v, want nil", fault)
	}
	return &sessionState{target: target, tools: tools, passthrough: pt}
}

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

func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if after, ok := strings.CutPrefix(e, prefix); ok {
			return after, true
		}
	}
	return "", false
}

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

func assertEnvAbsent(t *testing.T, env []string, key string) {
	t.Helper()
	if _, ok := envLookup(env, key); ok {
		t.Errorf("env %q is present, want absent", key)
	}
}

const titleAgentDisabledMember = `{"title":{"disable":true}}`

func decodeConfigDocument(t *testing.T, content string) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", content, err)
	}
	return doc
}

func assertTitleAgentDisabled(t *testing.T, doc map[string]json.RawMessage) {
	t.Helper()
	if got := string(doc["agent"]); got != titleAgentDisabledMember {
		t.Errorf("document agent = %s, want %s", got, titleAgentDisabledMember)
	}
}

func assertHasArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("buildRunArgs() missing %q %q in [%s]", flag, value, strings.Join(args, " "))
}

func assertHasFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		return
	}
	t.Errorf("buildRunArgs() missing flag %q in [%s]", flag, strings.Join(args, " "))
}

func assertNoFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		t.Errorf("buildRunArgs() unexpected flag %q in [%s]", flag, strings.Join(args, " "))
		return
	}
}

func newTestSessionState(workspacePath, sessionID string) *sessionState {
	return &sessionState{
		target: agentcore.LaunchTarget{
			WorkspacePath: workspacePath,
		},
		sessionID: sessionID,
	}
}

func TestParsePassthroughConfigAndCrossField(t *testing.T) {
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

			pt, fault := parsePassthroughConfig(tt.config)
			if fault != nil {
				t.Fatalf("parsePassthroughConfig(%v) fault = %v", tt.config, fault)
			}
			err := checkCrossField(pt)

			if tt.wantErr {
				if err == nil {
					t.Fatal("checkCrossField() error = nil, want error")
				}
				if !strings.Contains(err.Error(), "bash") {
					t.Errorf("error = %q, want it to mention %q", err.Error(), "bash")
				}
				return
			}

			if err != nil {
				t.Fatalf("checkCrossField() error = %v", err)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, pt)
			}
		})
	}
}

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

func TestMCPInjectionConformance(t *testing.T) {
	t.Parallel()

	declared, ok := registry.Agents.Meta("opencode")
	if !ok {
		t.Fatal(`registry.Agents.Meta("opencode") reported not registered`)
	}
	workspace := t.TempDir()
	mcpConfigPath := agenttest.WriteSeededMCPConfig(t, workspace)

	a, _ := NewOpenCodeAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: agenttest.FakeRuntime(t, t.TempDir(), "opencode", agenttest.OutputScenario, agenttest.Output{Version: "2.0.18"})},
		MCPConfigPath: mcpConfigPath,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })
	state := session.Internal.(*sessionState)

	agenttest.AssertMCPInjection(t, declared.MCPDialect, mcpConfigPath, agenttest.MCPLaunchSurface{
		Args: buildRunArgs(state, state.passthrough),
		Env:  append(state.tools.LocalEnv(), buildTurnEnv(state)...),
	})
}

func TestStartSession_RejectsMalformedMCPConfig(t *testing.T) {
	t.Parallel()

	command := agenttest.FakeRuntime(t, t.TempDir(), "opencode", agenttest.OutputScenario, agenttest.Output{Version: "2.0.18"})
	agenttest.AssertMalformedMCPConfigRejected(t, func(mcpConfigPath string) error {
		a, _ := NewOpenCodeAdapter()
		_, err := a.StartSession(context.Background(), domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: command},
			MCPConfigPath: mcpConfigPath,
		})
		return err
	})
}

func TestBuildInlineConfig(t *testing.T) {
	t.Parallel()

	const mcp = `{"sortie-tools":{"type":"local","command":["/usr/local/bin/sortie"]}}`

	tests := []struct {
		name        string
		pt          passthroughConfig
		mcp         string
		wantPerm    string
		wantCompact bool
		wantMCP     bool
	}{
		{name: "bypass on without lists carries only share", pt: passthroughConfig{DangerousSkipPermissions: true}},
		{name: "bypass off without lists denies every tool", wantPerm: `{"*":"deny"}`},
		{
			name:     "bypass off with allowed_tools asks first, allows the listed tool and denies the other known keys",
			pt:       passthroughConfig{AllowedTools: []string{"read"}},
			wantPerm: `{"*":"deny","bash":"deny","codesearch":"deny","doom_loop":"deny","edit":"deny","external_directory":"deny","glob":"deny","grep":"deny","list":"deny","lsp":"deny","opencode_list_mcp_resources":"deny","opencode_models":"deny","opencode_read_mcp_resource":"deny","opencode_session_move":"deny","opencode_session_rename":"deny","question":"deny","read":"allow","skill":"deny","task":"deny","todowrite":"deny","webfetch":"deny","websearch":"deny"}`,
		},
		{
			name:     "bypass off with denied_tools only keeps the denies and asks for the rest",
			pt:       passthroughConfig{DeniedTools: []string{"bash"}},
			wantPerm: `{"*":"deny","bash":"deny"}`,
		},
		{
			name:     "a denied list entry keeps its own action beside the base ask",
			pt:       passthroughConfig{DeniedTools: []string{"external_directory"}},
			wantPerm: `{"*":"deny","external_directory":"deny"}`,
		},
		{name: "disable_autocompact adds compaction", pt: passthroughConfig{DangerousSkipPermissions: true, DisableAutocompact: true}, wantCompact: true},
		{name: "delivery document becomes the mcp member verbatim", pt: passthroughConfig{DangerousSkipPermissions: true}, mcp: mcp, wantMCP: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := buildInlineConfig(tt.pt, []byte(tt.mcp))
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
			assertTitleAgentDisabled(t, doc)
			if got := string(doc["permission"]); got != tt.wantPerm {
				t.Errorf("buildInlineConfig(%+v) permission = %q, want %q", tt.pt, got, tt.wantPerm)
			}
			compaction, hasCompaction := doc["compaction"]
			if hasCompaction != tt.wantCompact {
				t.Errorf("buildInlineConfig() compaction present = %v, want %v", hasCompaction, tt.wantCompact)
			} else if hasCompaction && string(compaction) != `{"auto":false}` {
				t.Errorf("buildInlineConfig() compaction = %s, want %s", compaction, `{"auto":false}`)
			}
			if got, ok := doc["mcp"]; ok != tt.wantMCP || (ok && string(got) != tt.mcp) {
				t.Errorf("buildInlineConfig() mcp = %s (present %v), want %q (present %v)", got, ok, tt.mcp, tt.wantMCP)
			}
		})
	}
}

func TestRemoteLaunch_ConfigDocumentArrivesAsLateEntry(t *testing.T) {
	t.Parallel()

	state := resolveRemoteState(t)
	forward := state.tools.Forward(state.placeTools)
	if forward == nil {
		t.Fatal("Forward() = nil, want the relay's reverse forward")
	}

	late := forward.Render(sshutil.ForwardOutcome{Status: sshutil.ForwardAllocated, RemotePort: 45678})
	_, static := state.turnEnv()

	var document string
	for _, entry := range late.Env {
		if entry.Name == "OPENCODE_CONFIG_CONTENT" {
			document = entry.Value
		}
	}
	if document == "" {
		t.Fatalf("late env = %v, want an OPENCODE_CONFIG_CONTENT entry", late.Env)
	}
	doc := decodeConfigDocument(t, document)
	assertTitleAgentDisabled(t, doc)
	if !strings.Contains(string(doc["mcp"]), "45678") {
		t.Errorf("late OPENCODE_CONFIG_CONTENT mcp = %s, want the tool server at the allocated remote port", doc["mcp"])
	}
	if i := slices.IndexFunc(static, func(entry sshutil.EnvVar) bool { return entry.Name == "OPENCODE_CONFIG_CONTENT" }); i >= 0 {
		t.Errorf("turnEnv() static settings carry OPENCODE_CONFIG_CONTENT, want it only as a late entry")
	}
	agenttest.AssertForwardRenderingsFit(t, forward)
}

func TestStartSession_VerificationPermissionPolicy(t *testing.T) {
	t.Parallel()

	const operatorPolicy = `{"*":"deny","bash":"deny"}`
	operator := map[string]any{"dangerously_skip_permissions": false, "denied_tools": []any{"bash"}}

	tests := []struct {
		name               string
		settings           map[string]any
		verification, full bool
		wantPermission     string
	}{
		{name: "working launch", settings: operator, wantPermission: operatorPolicy},
		{
			name:           "verification launch",
			settings:       map[string]any{"dangerously_skip_permissions": false, "denied_tools": []any{"bash"}, "allowed_tools": []any{"read"}},
			verification:   true,
			wantPermission: `{"*":"deny"}`,
		},
		{name: "verification launch with full context", settings: operator, verification: true, full: true, wantPermission: operatorPolicy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			command := agenttest.FakeRuntime(t, t.TempDir(), "opencode", agenttest.OutputScenario, agenttest.Output{Version: "2.0.18"})
			adapter, _ := NewOpenCodeAdapter()
			session, err := adapter.StartSession(t.Context(), domain.StartSessionParams{
				WorkspacePath:           t.TempDir(),
				AgentConfig:             domain.AgentConfig{Command: command},
				CredentialVerification:  tt.verification,
				VerificationFullContext: tt.full,
				Settings:                tt.settings,
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}

			doc := decodeConfigDocument(t, session.Internal.(*sessionState).turnConfigContent)

			if got := string(doc["permission"]); got != tt.wantPermission {
				t.Errorf("StartSession(%v) permission = %s, want %s", tt.settings, got, tt.wantPermission)
			}
			if got, ok := doc["mcp"]; ok {
				t.Errorf("StartSession(%v) mcp = %s, want no mcp member without a tool configuration", tt.settings, got)
			}
		})
	}
}

func TestBuildTurnEnv_CarriesTitleAgentDisabled(t *testing.T) {
	t.Parallel()

	content, err := buildInlineConfig(passthroughConfig{}, nil)
	if err != nil {
		t.Fatalf("buildInlineConfig() error = %v", err)
	}
	state := newTestSessionState("/workspace", "")
	state.turnConfigContent = content

	env := buildTurnEnv(state)

	carried, ok := envLookup(env, "OPENCODE_CONFIG_CONTENT")
	if !ok {
		t.Fatalf("buildTurnEnv() env lacks OPENCODE_CONFIG_CONTENT, want the turn document")
	}
	assertTitleAgentDisabled(t, decodeConfigDocument(t, carried))
}

func TestRunTurn_InheritedOpencodeConfigContentScrubbed(t *testing.T) {
	t.Parallel()

	base := []string{"OPENCODE_CONFIG_CONTENT=inherited-from-parent-process", "PATH=/usr/bin"}

	env := buildRunEnv(base)

	assertEnvAbsent(t, env, "OPENCODE_CONFIG_CONTENT")

	env = append(env, "OPENCODE_CONFIG_CONTENT=own-document")
	assertEnvPresent(t, env, "OPENCODE_CONFIG_CONTENT", "own-document")
}

func TestBuildRunArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		sessionID   string
		pt          passthroughConfig
		wantPresent []string
		wantPairs   [][2]string
		wantAbsent  []string
		wantArgs    []string
	}{
		{
			name:     "minimal_vector_carries_no_directory_variant_or_prompt",
			pt:       passthroughConfig{},
			wantArgs: []string{"run", "--format", "json", "--standalone"},
		},
		{
			name:     "effort_rides_the_model_suffix",
			pt:       passthroughConfig{Model: "provider/model", Effort: "high"},
			wantArgs: []string{"run", "--format", "json", "--standalone", "--model", "provider/model#high"},
		},
		{
			name:       "fresh_session",
			sessionID:  "",
			pt:         passthroughConfig{},
			wantAbsent: []string{"--session"},
		},
		{
			name:      "resume_session",
			sessionID: "ses_abc",
			pt:        passthroughConfig{},
			wantPairs: [][2]string{{"--session", "ses_abc"}},
		},
		{
			name:        "skip_permissions_default",
			sessionID:   "",
			pt:          passthroughConfig{DangerousSkipPermissions: true},
			wantPresent: []string{"--dangerously-skip-permissions"},
		},
		{
			name:       "skip_permissions_disabled",
			sessionID:  "",
			pt:         passthroughConfig{DangerousSkipPermissions: false},
			wantAbsent: []string{"--dangerously-skip-permissions"},
		},
		{
			name:      "model_flag",
			sessionID: "",
			pt:        passthroughConfig{Model: "anthropic/claude-3-5-sonnet"},
			wantPairs: [][2]string{{"--model", "anthropic/claude-3-5-sonnet"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newTestSessionState("/tmp/workspace", tt.sessionID)
			args := buildRunArgs(state, tt.pt)

			assertHasFlag(t, args, "--standalone")
			for _, flag := range tt.wantPresent {
				assertHasFlag(t, args, flag)
			}
			for _, pair := range tt.wantPairs {
				assertHasArgPair(t, args, pair[0], pair[1])
			}
			for _, flag := range tt.wantAbsent {
				assertNoFlag(t, args, flag)
			}
			if tt.wantArgs != nil && !slices.Equal(args, tt.wantArgs) {
				t.Errorf("buildRunArgs(%+v) = %v, want %v", tt.pt, args, tt.wantArgs)
			}
		})
	}
}

func TestBuildRunArgs_DefaultConfigurationSkipsPermissions(t *testing.T) {
	t.Parallel()

	pt, err := parsePassthroughConfig(map[string]any{})
	if err != nil {
		t.Fatalf("parsePassthroughConfig(map[string]any{}) error = %v", err)
	}

	state := newTestSessionState("/tmp/workspace", "")
	args := buildRunArgs(state, pt)

	assertHasFlag(t, args, "--dangerously-skip-permissions")
}

func TestBuildRunEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		base      []string
		checkFunc func(t *testing.T, env []string)
	}{
		{
			name: "baseline_always_set",
			base: []string{},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOUPDATE", "true")
			},
		},
		{
			name: "inherited_autoupdate_overridden",
			base: []string{"OPENCODE_DISABLE_AUTOUPDATE=false"},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOUPDATE", "true")
			},
		},
		{
			name: "inherited_permission_removed",
			base: []string{"OPENCODE_PERMISSION=old_value", "OTHER_VAR=keep"},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvAbsent(t, env, "OPENCODE_PERMISSION")
				assertEnvPresent(t, env, "OTHER_VAR", "keep")
			},
		},
		{
			name: "inherited_policy_variables_removed",
			base: []string{
				"OPENCODE_AUTO_SHARE=true",
				"OPENCODE_DISABLE_AUTOCOMPACT=false",
				"OPENCODE_DISABLE_LSP_DOWNLOAD=false",
			},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvAbsent(t, env, "OPENCODE_AUTO_SHARE")
				assertEnvAbsent(t, env, "OPENCODE_DISABLE_AUTOCOMPACT")
				assertEnvAbsent(t, env, "OPENCODE_DISABLE_LSP_DOWNLOAD")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := buildRunEnv(tt.base)

			tt.checkFunc(t, env)
		})
	}
}
