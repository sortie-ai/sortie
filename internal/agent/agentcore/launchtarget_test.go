package agentcore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// fakeSSHDir writes a no-op executable named "ssh" to a temp directory and
// returns the directory path. The directory should be prepended to PATH via
// t.Setenv.
func fakeSSHDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("fakeSSHDir: %v", err)
	}
	return dir
}

// emptyDir returns a temp directory that contains no binaries.
func emptyDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func makeParams(t *testing.T, workspace, sshHost string, command string) domain.StartSessionParams {
	t.Helper()
	return domain.StartSessionParams{
		WorkspacePath: workspace,
		SSHHost:       sshHost,
		AgentConfig: domain.AgentConfig{
			Command: command,
		},
	}
}

func TestResolveLaunchTarget(t *testing.T) {
	// Not parallel: some subtests use t.Setenv, which is incompatible with
	// parallel parent tests. Individual subtests that do not modify env call
	// t.Parallel() themselves.
	dir := t.TempDir()

	tests := []struct {
		name           string
		setup          func(t *testing.T)
		params         func(t *testing.T) domain.StartSessionParams
		defaultCommand string
		wantKind       domain.AgentErrorKind
		wantMsg        string
		wantNoErr      bool
		check          func(t *testing.T, lt LaunchTarget)
	}{
		{
			name:           "local mode: binary present and workspace valid",
			defaultCommand: "sh",
			params:         func(t *testing.T) domain.StartSessionParams { return makeParams(t, dir, "", "") },
			wantNoErr:      true,
			check: func(t *testing.T, lt LaunchTarget) {
				t.Helper()
				if lt.RemoteCommand != "" {
					t.Errorf("RemoteCommand = %q, want empty (local mode)", lt.RemoteCommand)
				}
				if lt.Command == "" {
					t.Error("Command is empty, want resolved path")
				}
				if lt.WorkspacePath != dir {
					t.Errorf("WorkspacePath = %q, want %q", lt.WorkspacePath, dir)
				}
			},
		},
		{
			name:           "local mode: multi-token command produces Args",
			defaultCommand: "sh -c",
			params:         func(t *testing.T) domain.StartSessionParams { return makeParams(t, dir, "", "") },
			wantNoErr:      true,
			check: func(t *testing.T, lt LaunchTarget) {
				t.Helper()
				if len(lt.Args) != 1 || lt.Args[0] != "-c" {
					t.Errorf("Args = %v, want [-c]", lt.Args)
				}
			},
		},
		{
			name:           "local mode: binary absent",
			defaultCommand: "sortie-no-such-binary-xyzzy",
			params:         func(t *testing.T) domain.StartSessionParams { return makeParams(t, dir, "", "") },
			wantKind:       domain.ErrAgentNotFound,
		},
		{
			name:           "local mode: workspace invalid",
			defaultCommand: "sh",
			params: func(t *testing.T) domain.StartSessionParams {
				return makeParams(t, filepath.Join(dir, "nope"), "", "")
			},
			wantKind: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name:           "ssh mode: ssh present",
			defaultCommand: "claude",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv("PATH", fakeSSHDir(t)+":"+os.Getenv("PATH"))
			},
			params: func(t *testing.T) domain.StartSessionParams {
				return makeParams(t, dir, "user@host", "")
			},
			wantNoErr: true,
			check: func(t *testing.T, lt LaunchTarget) {
				t.Helper()
				if lt.RemoteCommand == "" {
					t.Error("RemoteCommand is empty, want ssh mode")
				}
				if lt.SSHHost != "user@host" {
					t.Errorf("SSHHost = %q, want user@host", lt.SSHHost)
				}
				if lt.Args != nil {
					t.Errorf("Args = %v, want nil in ssh mode", lt.Args)
				}
			},
		},
		{
			name:           "ssh mode: ssh absent",
			defaultCommand: "claude",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv("PATH", emptyDir(t))
			},
			params: func(t *testing.T) domain.StartSessionParams {
				return makeParams(t, dir, "user@host", "")
			},
			wantKind: domain.ErrAgentNotFound,
			wantMsg:  "ssh binary not found on orchestrator host",
		},
		{
			name:           "ssh mode: workspace invalid",
			defaultCommand: "claude",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv("PATH", fakeSSHDir(t)+":"+os.Getenv("PATH"))
			},
			params: func(t *testing.T) domain.StartSessionParams {
				return makeParams(t, filepath.Join(dir, "nope"), "user@host", "")
			},
			wantKind: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name:           "ssh mode: SSHHost whitespace trimmed",
			defaultCommand: "claude",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv("PATH", fakeSSHDir(t)+":"+os.Getenv("PATH"))
			},
			params: func(t *testing.T) domain.StartSessionParams {
				return makeParams(t, dir, "  user@host  ", "")
			},
			wantNoErr: true,
			check: func(t *testing.T, lt LaunchTarget) {
				t.Helper()
				if lt.SSHHost != "user@host" {
					t.Errorf("SSHHost = %q, want %q (whitespace trimmed)", lt.SSHHost, "user@host")
				}
			},
		},
		{
			name:           "empty command",
			defaultCommand: "",
			params:         func(t *testing.T) domain.StartSessionParams { return makeParams(t, dir, "", "") },
			wantKind:       domain.ErrAgentNotFound,
			wantMsg:        "agent command is empty or whitespace-only",
		},
		{
			name:           "whitespace-only command",
			defaultCommand: "   ",
			params:         func(t *testing.T) domain.StartSessionParams { return makeParams(t, dir, "", "") },
			wantKind:       domain.ErrAgentNotFound,
			wantMsg:        "agent command is empty or whitespace-only",
		},
		{
			name:           "ssh mode: empty command",
			defaultCommand: "",
			params:         func(t *testing.T) domain.StartSessionParams { return makeParams(t, dir, "user@host", "") },
			wantKind:       domain.ErrAgentNotFound,
			wantMsg:        "agent command is empty or whitespace-only",
		},
		{
			name:           "params command overrides default",
			defaultCommand: "sortie-no-such-binary-xyzzy",
			params: func(t *testing.T) domain.StartSessionParams {
				return makeParams(t, dir, "", "sh")
			},
			wantNoErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup == nil {
				t.Parallel()
			}

			if tt.setup != nil {
				tt.setup(t)
			}

			got, agentErr := ResolveLaunchTarget(tt.params(t), tt.defaultCommand)

			if tt.wantNoErr {
				if agentErr != nil {
					t.Fatalf("ResolveLaunchTarget unexpected error: %v", agentErr)
				}
				if tt.check != nil {
					tt.check(t, got)
				}
				return
			}

			if agentErr == nil {
				t.Fatalf("ResolveLaunchTarget returned no error, want kind %q", tt.wantKind)
			}
			if agentErr.Kind != tt.wantKind {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, tt.wantKind)
			}
			if tt.wantMsg != "" && agentErr.Message != tt.wantMsg {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, tt.wantMsg)
			}
		})
	}
}
