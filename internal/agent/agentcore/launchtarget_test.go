package agentcore

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// fakeSSHDir builds a no-op fake runtime named "ssh" in a temp directory and
// returns the directory path. The directory should be prepended to PATH via
// t.Setenv.
func fakeSSHDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	agenttest.FakeRuntime(t, dir, "ssh", agenttest.OutputScenario, agenttest.Output{})
	return dir
}

// unsetEnvForTest removes name for the duration of the test and
// restores whatever value the surrounding environment held. t.Setenv
// registers that restore before the variable is removed; a name the
// environment did not hold needs no restore.
func unsetEnvForTest(t *testing.T, name string) {
	t.Helper()
	if prior, ok := os.LookupEnv(name); ok {
		t.Setenv(name, prior)
	}
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%s): %v", name, err)
	}
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
	binPath := agenttest.FakeRuntime(t, t.TempDir(), "agent", agenttest.OutputScenario, agenttest.Output{})

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
			defaultCommand: binPath,
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
			defaultCommand: binPath + " -c",
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
			defaultCommand: binPath,
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
				t.Setenv("PATH", fakeSSHDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
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
				t.Setenv("PATH", fakeSSHDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
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
				t.Setenv("PATH", fakeSSHDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
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
				return makeParams(t, dir, "", binPath)
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

// TestResolveLaunchTarget_SSHEnvNames asserts that ResolveLaunchTarget
// sets LaunchTarget.SSHEnvNames from params.SSHEnvNames in SSH mode and
// leaves it nil in local mode, even when the caller sets it.
func TestResolveLaunchTarget_SSHEnvNames(t *testing.T) {
	// Not parallel: the SSH-mode subtest uses t.Setenv.
	dir := t.TempDir()
	binPath := agenttest.FakeRuntime(t, t.TempDir(), "agent", agenttest.OutputScenario, agenttest.Output{})

	t.Run("ssh mode carries the resolved names", func(t *testing.T) {
		t.Setenv("PATH", fakeSSHDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))

		params := makeParams(t, dir, "user@host", "claude")
		params.SSHEnvNames = []string{"EXAMPLE_TOKEN"}

		lt, agentErr := ResolveLaunchTarget(params, "claude")
		if agentErr != nil {
			t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
		}
		if !slices.Equal(lt.SSHEnvNames, []string{"EXAMPLE_TOKEN"}) {
			t.Errorf("LaunchTarget.SSHEnvNames = %v, want %v", lt.SSHEnvNames, []string{"EXAMPLE_TOKEN"})
		}
	})

	t.Run("local mode leaves SSHEnvNames nil even when params set it", func(t *testing.T) {
		t.Parallel()

		params := makeParams(t, dir, "", binPath)
		params.SSHEnvNames = []string{"EXAMPLE_TOKEN"}

		lt, agentErr := ResolveLaunchTarget(params, binPath)
		if agentErr != nil {
			t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
		}
		if lt.SSHEnvNames != nil {
			t.Errorf("LaunchTarget.SSHEnvNames = %v, want nil in local mode", lt.SSHEnvNames)
		}
	})
}

// TestLaunchTarget_SSHOptions asserts the resolution order:
// t.SSHEnvNames is walked in order, a name already claimed by
// settings or already carried is skipped, each remaining name is
// looked up and carried only when its value holds a non-whitespace
// character, then every settings entry whose Value is non-empty is
// appended, keeping the first entry for a name a later entry repeats.
func TestLaunchTarget_SSHOptions(t *testing.T) {
	// Not parallel: sets process environment via t.Setenv.
	t.Setenv("SSH_OPTIONS_TEST_B", "b-value")
	t.Setenv("SSH_OPTIONS_TEST_A", "a-value")
	t.Setenv("SSH_OPTIONS_TEST_C", "")
	t.Setenv("SSH_OPTIONS_TEST_W", " \t\r\n ")
	unsetEnvForTest(t, "SSH_OPTIONS_TEST_D")

	target := LaunchTarget{
		SSHStrictHostKeyChecking: "yes",
		SSHEnvNames: []string{
			"SSH_OPTIONS_TEST_B", "SSH_OPTIONS_TEST_A", "SSH_OPTIONS_TEST_B",
			"SSH_OPTIONS_TEST_C", "SSH_OPTIONS_TEST_W", "SSH_OPTIONS_TEST_D",
		},
	}
	settings := []sshutil.EnvVar{
		{Name: "SSH_OPTIONS_TEST_C", Value: "managed"},
		{Name: "SSH_OPTIONS_TEST_E", Value: ""},
		{Name: "SSH_OPTIONS_TEST_C", Value: "managed-again"},
	}

	got := target.SSHOptions(settings...)

	want := []sshutil.EnvVar{
		{Name: "SSH_OPTIONS_TEST_B", Value: "b-value"},
		{Name: "SSH_OPTIONS_TEST_A", Value: "a-value"},
		{Name: "SSH_OPTIONS_TEST_C", Value: "managed"},
	}
	if !slices.Equal(got.Env, want) {
		t.Errorf("SSHOptions(...).Env = %+v, want %+v", got.Env, want)
	}
	if got.StrictHostKeyChecking != "yes" {
		t.Errorf("SSHOptions(...).StrictHostKeyChecking = %q, want %q", got.StrictHostKeyChecking, "yes")
	}

	t.Run("no names and no settings yields nil Env", func(t *testing.T) {
		var empty LaunchTarget
		if got := empty.SSHOptions(); got.Env != nil {
			t.Errorf("SSHOptions() Env = %v, want nil", got.Env)
		}
	})
}
