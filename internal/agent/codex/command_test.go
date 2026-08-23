package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// TestMCPInjectionConformance proves codex's real launch surface
// matches its declared disposition, on both a local and a remote
// launch. codex renders the generated MCP config as override
// arguments appended to [agentcore.ResolveLaunchTarget]'s Args; a
// non-empty AgentConfig.Command supersedes the default, so the
// "codex app-server" argument is never resolved and the check does
// not depend on a codex binary being installed on the host running
// the test.
func TestMCPInjectionConformance(t *testing.T) {
	t.Parallel()

	declared, ok := registry.Agents.Meta("codex")
	if !ok {
		t.Fatal(`registry.Agents.Meta("codex") reported not registered`)
	}

	dir := t.TempDir()
	mcpConfigPath := filepath.Join(dir, ".sortie", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpConfigPath), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	const mcpConfigContent = `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server","--workflow","/repo/WORKFLOW.md"],"env":{"SORTIE_ISSUE_ID":"abc-123"}}}}`
	if err := os.WriteFile(mcpConfigPath, []byte(mcpConfigContent), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Run("local launch delivers the translated overrides", func(t *testing.T) {
		t.Parallel()

		params := domain.StartSessionParams{
			WorkspacePath: dir,
			AgentConfig:   domain.AgentConfig{Command: "sh -c"},
			MCPConfigPath: mcpConfigPath,
		}
		target, agentErr := agentcore.ResolveLaunchTarget(params, "codex app-server")
		if agentErr != nil {
			t.Fatalf("agentcore.ResolveLaunchTarget() error = %v", agentErr)
		}
		servers, err := mcpconfig.Parse(mcpConfigPath)
		if err != nil {
			t.Fatalf("mcpconfig.Parse() error = %v", err)
		}
		overrideArgs, err := renderMCPServerOverrides(servers, os.Environ())
		if err != nil {
			t.Fatalf("renderMCPServerOverrides() error = %v", err)
		}
		target.Args = append(target.Args, overrideArgs...)

		agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{Args: target.Args})
	})

	t.Run("remote launch delivers nothing", func(t *testing.T) {
		t.Parallel()

		params := domain.StartSessionParams{
			WorkspacePath: dir,
			AgentConfig:   domain.AgentConfig{Command: "sh -c"},
			MCPConfigPath: mcpConfigPath,
			SSHHost:       "remote.example",
		}
		target, agentErr := agentcore.ResolveLaunchTarget(params, "codex app-server")
		if agentErr != nil {
			t.Fatalf("agentcore.ResolveLaunchTarget() error = %v", agentErr)
		}

		// StartSession's remote guard skips rendering entirely, so the
		// launch surface a remote session produces carries nothing to
		// append here.
		agenttest.AssertMCPInjection(t, registry.MCPInjectionUnsupported, mcpConfigPath, agenttest.MCPLaunchSurface{Args: target.Args})
	})
}

// --- renderMCPServerOverrides ---

// TestRenderMCPServerOverrides_OnePerServer asserts that a session
// whose generated file names the sortie-tools server plus one
// operator server renders one override per server.
func TestRenderMCPServerOverrides_OnePerServer(t *testing.T) {
	t.Parallel()

	servers := []mcpconfig.Server{
		{
			Name:      "operator-server",
			Transport: mcpconfig.TransportStdio,
			Command:   "/usr/local/bin/operator-mcp",
		},
		{
			Name:      "sortie-tools",
			Transport: mcpconfig.TransportStdio,
			Command:   "/usr/local/bin/sortie",
			Args:      []string{"mcp-server"},
		},
	}

	args, err := renderMCPServerOverrides(servers, nil)
	if err != nil {
		t.Fatalf("renderMCPServerOverrides() error = %v", err)
	}

	// Two servers, one "-c" flag/value pair each.
	if len(args) != 4 {
		t.Fatalf("renderMCPServerOverrides() returned %d args, want 4 (2 servers x 2 args each): %v", len(args), args)
	}

	var overrideKeys []string
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-c" {
			t.Errorf("renderMCPServerOverrides() arg[%d] = %q, want \"-c\"", i, args[i])
		}
		key, _, ok := strings.Cut(args[i+1], "=")
		if !ok {
			t.Fatalf("renderMCPServerOverrides() arg[%d] = %q, want a key=value override", i+1, args[i+1])
		}
		overrideKeys = append(overrideKeys, key)
	}

	wantKeys := []string{"mcp_servers.operator-server", "mcp_servers.sortie-tools"}
	if len(overrideKeys) != len(wantKeys) {
		t.Fatalf("renderMCPServerOverrides() keys = %v, want %v", overrideKeys, wantKeys)
	}
	for _, want := range wantKeys {
		if !slices.Contains(overrideKeys, want) {
			t.Errorf("renderMCPServerOverrides() keys = %v, missing %q", overrideKeys, want)
		}
	}
}

// TestRenderMCPServerOverrides_DottedPathPerServer asserts that each
// server is keyed under its own dotted path, so an operator's own
// [mcp_servers] table entries merge rather than being replaced by a
// single override.
func TestRenderMCPServerOverrides_DottedPathPerServer(t *testing.T) {
	t.Parallel()

	servers := []mcpconfig.Server{
		{Name: "sortie-tools", Transport: mcpconfig.TransportStdio, Command: "/usr/local/bin/sortie"},
	}

	args, err := renderMCPServerOverrides(servers, nil)
	if err != nil {
		t.Fatalf("renderMCPServerOverrides() error = %v", err)
	}
	if len(args) != 2 {
		t.Fatalf("renderMCPServerOverrides() returned %d args, want 2", len(args))
	}
	if args[0] != "-c" {
		t.Errorf("renderMCPServerOverrides() arg[0] = %q, want \"-c\"", args[0])
	}
	if !strings.HasPrefix(args[1], "mcp_servers.sortie-tools=") {
		t.Errorf("renderMCPServerOverrides() arg[1] = %q, want prefix %q", args[1], "mcp_servers.sortie-tools=")
	}
	// The override must not carry a bracketed [mcp_servers] table header,
	// which would replace the operator's whole table instead of merging
	// one entry into it.
	if strings.Contains(args[1], "[mcp_servers]") {
		t.Errorf("renderMCPServerOverrides() arg[1] = %q, contains a table header that would replace the operator's table", args[1])
	}
}

// TestRenderMCPServerOverrides_CredentialPassthrough asserts that an
// env entry whose name and value both match the adapter's own process
// environment is delivered by name only, never by value; an env entry
// with no match in the process environment is rendered literally.
func TestRenderMCPServerOverrides_CredentialPassthrough(t *testing.T) {
	t.Parallel()

	processEnv := []string{"SORTIE_TRACKER_TOKEN=super-secret-value"}

	servers := []mcpconfig.Server{
		{
			Name:      "sortie-tools",
			Transport: mcpconfig.TransportStdio,
			Command:   "/usr/local/bin/sortie",
			Env: map[string]string{
				"SORTIE_TRACKER_TOKEN": "super-secret-value",
				"SORTIE_ISSUE_ID":      "abc-123",
			},
		},
	}

	args, err := renderMCPServerOverrides(servers, processEnv)
	if err != nil {
		t.Fatalf("renderMCPServerOverrides() error = %v", err)
	}
	if len(args) != 2 {
		t.Fatalf("renderMCPServerOverrides() returned %d args, want 2", len(args))
	}
	rendered := args[1]

	if strings.Contains(rendered, "super-secret-value") {
		t.Errorf("renderMCPServerOverrides() rendered the credential value %q, want name-only passthrough: %q", "super-secret-value", rendered)
	}
	if !strings.Contains(rendered, "SORTIE_TRACKER_TOKEN") {
		t.Errorf("renderMCPServerOverrides() rendered = %q, want it to name the passthrough variable %q", rendered, "SORTIE_TRACKER_TOKEN")
	}
	if !strings.Contains(rendered, `env_vars=`) {
		t.Errorf("renderMCPServerOverrides() rendered = %q, want an env_vars passthrough list", rendered)
	}

	if !strings.Contains(rendered, "SORTIE_ISSUE_ID") || !strings.Contains(rendered, "abc-123") {
		t.Errorf("renderMCPServerOverrides() rendered = %q, want the session-only variable rendered literally", rendered)
	}
}

// --- encoding contract ---

// TestRenderMCPServerTable_EncodingContract asserts the exact
// rendered argument for each encoding case the renderer must handle.
func TestRenderMCPServerTable_EncodingContract(t *testing.T) {
	t.Parallel()

	trueVal := true
	falseVal := false

	tests := []struct {
		name       string
		server     mcpconfig.Server
		wantErr    bool
		wantSubstr []string
	}{
		{
			name: "server name outside bare-key set fails",
			server: mcpconfig.Server{
				Name:      "bad name!",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
			},
			wantErr: true,
		},
		{
			name: "structural keys rendered bare",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Args:      []string{"a"},
			},
			wantSubstr: []string{`command="/bin/echo"`, `args=["a"]`},
		},
		{
			name: "environment variable name rendered as quoted key",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Env:       map[string]string{"MY_VAR": "value"},
			},
			wantSubstr: []string{`"MY_VAR"="value"`},
		},
		{
			name: "environment variable name outside bare-key set is still a quoted key",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Env:       map[string]string{"my.var with space": "value"},
			},
			wantSubstr: []string{`"my.var with space"="value"`},
		},
		{
			name: "value with quote, backslash, space, and control character is escaped",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Env:       map[string]string{"MY_VAR": "a\"b\\c d\te"},
			},
			wantSubstr: []string{`"MY_VAR"="a\"b\\c d\te"`},
		},
		{
			name: "invalid UTF-8 value fails",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Env:       map[string]string{"MY_VAR": "\xff\xfe"},
			},
			wantErr: true,
		},
		{
			name: "Enabled true is rendered as the enable field",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Enabled:   &trueVal,
			},
			wantSubstr: []string{"enabled=true"},
		},
		{
			name: "Enabled false is rendered as the enable field",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Enabled:   &falseVal,
			},
			wantSubstr: []string{"enabled=false"},
		},
		{
			name: "nil Enabled omits the field",
			server: mcpconfig.Server{
				Name:      "sortie-tools",
				Transport: mcpconfig.TransportStdio,
				Command:   "/bin/echo",
				Enabled:   nil,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args, err := renderMCPServerOverrides([]mcpconfig.Server{tt.server}, nil)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("renderMCPServerOverrides(%+v) = %v, nil, want error", tt.server, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("renderMCPServerOverrides(%+v) unexpected error: %v", tt.server, err)
			}
			if len(args) != 2 {
				t.Fatalf("renderMCPServerOverrides(%+v) returned %d args, want 2", tt.server, len(args))
			}
			rendered := args[1]

			for _, substr := range tt.wantSubstr {
				if !strings.Contains(rendered, substr) {
					t.Errorf("renderMCPServerOverrides(%+v) = %q, want substring %q", tt.server, rendered, substr)
				}
			}
			if tt.name == "nil Enabled omits the field" && strings.Contains(rendered, "enabled=") {
				t.Errorf("renderMCPServerOverrides(%+v) = %q, want no enabled field", tt.server, rendered)
			}
		})
	}
}

func TestBuildSSHRemoteCmd(t *testing.T) {
	t.Parallel()

	const base = "codex app-server"

	tests := []struct {
		name          string
		remoteCommand string
		apiKey        string
		want          string
	}{
		{
			name:          "empty key returns command unchanged",
			remoteCommand: base,
			apiKey:        "",
			want:          base,
		},
		{
			name:          "simple alphanumeric key",
			remoteCommand: base,
			apiKey:        "sk-abc123",
			want:          "CODEX_API_KEY='sk-abc123' " + base,
		},
		{
			name:          "key with dollar sign",
			remoteCommand: base,
			apiKey:        "sk-$secret",
			want:          "CODEX_API_KEY='sk-$secret' " + base,
		},
		{
			name:          "key with single quote",
			remoteCommand: base,
			apiKey:        "it's",
			want:          "CODEX_API_KEY='it'\\''s' " + base,
		},
		{
			name:          "key with combined metacharacters (spec example)",
			remoteCommand: base,
			apiKey:        "'foo'$bar",
			want:          "CODEX_API_KEY=''\\''foo'\\''$bar' " + base,
		},
		{
			name:          "key with semicolon",
			remoteCommand: base,
			apiKey:        "key;rm -rf /",
			want:          "CODEX_API_KEY='key;rm -rf /' " + base,
		},
		{
			name:          "key with backtick",
			remoteCommand: base,
			apiKey:        "key`whoami`",
			want:          "CODEX_API_KEY='key`whoami`' " + base,
		},
		{
			name:          "key with double quote",
			remoteCommand: base,
			apiKey:        `key"value"`,
			want:          `CODEX_API_KEY='key"value"' ` + base,
		},
		{
			name:          "key with spaces",
			remoteCommand: base,
			apiKey:        "key with spaces",
			want:          "CODEX_API_KEY='key with spaces' " + base,
		},
		{
			name:          "remote command preserved unchanged",
			remoteCommand: "codex app-server --flag value",
			apiKey:        "sk-abc",
			want:          "CODEX_API_KEY='sk-abc' codex app-server --flag value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildSSHRemoteCmd(tt.remoteCommand, tt.apiKey)
			if got != tt.want {
				t.Errorf("buildSSHRemoteCmd(%q, %q) = %q, want %q", tt.remoteCommand, tt.apiKey, got, tt.want)
			}
		})
	}
}

// TestBuildSSHRemoteCmd_MetacharactersDoNotLeakIntoSSHArgs verifies that
// the full pipeline (buildSSHRemoteCmd → BuildSSHArgs) does not produce a
// final SSH remote-command argument that contains the raw API key value
// when the key includes shell metacharacters. An unquoted metacharacter
// would allow injection through the remote shell.
func TestBuildSSHRemoteCmd_MetacharactersDoNotLeakIntoSSHArgs(t *testing.T) {
	t.Parallel()

	metacharKeys := []struct {
		name   string
		apiKey string
	}{
		{"single quote", "'secret'"},
		{"dollar sign", "$SECRET"},
		{"combined spec example", "'foo'$bar"},
		{"semicolon injection attempt", "key; rm -rf /"},
		{"backtick injection attempt", "key`whoami`"},
		{"subshell injection attempt", "$(cat /etc/passwd)"},
	}

	const (
		base      = "codex app-server"
		host      = "remote.host"
		workspace = "/workspace"
	)

	for _, tc := range metacharKeys {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remoteCmd := buildSSHRemoteCmd(base, tc.apiKey)
			sshArgs := sshutil.BuildSSHArgs(host, workspace, remoteCmd, nil, sshutil.SSHOptions{})

			// The last SSH arg is the full remote command string passed to the
			// remote shell. It must not contain the raw API key value, because
			// that would mean the key was concatenated without quoting.
			finalArg := sshArgs[len(sshArgs)-1]
			rawKeyAssignment := "CODEX_API_KEY=" + tc.apiKey
			if strings.Contains(finalArg, rawKeyAssignment) {
				t.Errorf("SSH remote-command arg contains unquoted API key assignment %q in %q",
					rawKeyAssignment, finalArg)
			}

			// The final arg must still carry the CODEX_API_KEY= prefix so the
			// environment variable is set on the remote host.
			if !strings.Contains(finalArg, "CODEX_API_KEY=") {
				t.Errorf("SSH remote-command arg missing CODEX_API_KEY= prefix in %q", finalArg)
			}

			// The base command must be present so the agent binary is invoked.
			if !strings.Contains(finalArg, base) {
				t.Errorf("SSH remote-command arg missing base command %q in %q", base, finalArg)
			}
		})
	}
}
