package agentcore

import (
	"cmp"
	"os/exec"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// LaunchTarget captures the resolved launch parameters for a single agent
// session. It is populated once by [ResolveLaunchTarget] during
// [domain.AgentAdapter.StartSession] and stored in adapter session state.
// RunTurn uses it to construct the subprocess command.
//
// When RemoteCommand is non-empty the session runs in SSH mode: Command is
// the local ssh binary, SSHHost is the remote destination, and RemoteCommand
// is the agent command to execute there. When RemoteCommand is empty the
// session runs locally: Command is the resolved agent binary and Args contains
// any initial CLI arguments (e.g., ["app-server"]).
type LaunchTarget struct {
	// Command is the resolved absolute path to the local binary to exec.
	// In SSH mode this is the path to the ssh binary.
	// In local mode this is the path to the agent binary.
	Command string

	// Args contains initial CLI arguments inserted before per-turn
	// arguments. Non-empty only in local mode when the configured command
	// contains multiple tokens (e.g., "codex app-server" yields
	// Args: ["app-server"]). Empty in SSH mode.
	Args []string

	// WorkspacePath is the validated absolute path to the agent workspace
	// directory. Both local and SSH mode set this field.
	WorkspacePath string

	// RemoteCommand is the agent command string to run on the SSH host.
	// Non-empty signals SSH mode. Empty in local mode.
	RemoteCommand string

	// SSHHost is the SSH destination string after whitespace trimming.
	// Non-empty when RemoteCommand is non-empty.
	SSHHost string

	// SSHStrictHostKeyChecking is the OpenSSH StrictHostKeyChecking value
	// passed through from StartSessionParams. Empty means the SSH caller
	// defaults to "accept-new".
	SSHStrictHostKeyChecking string
}

// ResolveLaunchTarget resolves the workspace path and agent binary for a
// session, choosing between SSH and local launch modes based on
// params.SSHHost. It returns a populated [LaunchTarget] and a nil error on
// success, or a zero [LaunchTarget] and a [*domain.AgentError] on failure.
//
// defaultCommand is the fallback agent command when params.AgentConfig.Command
// is empty (e.g., "claude", "copilot", "codex app-server").
//
// Adapter-specific post-resolution steps (canary version checks, auth
// preflights, environment variable prefixing for the remote command) are the
// caller's responsibility and must run after ResolveLaunchTarget returns
// successfully.
func ResolveLaunchTarget(params domain.StartSessionParams, defaultCommand string) (LaunchTarget, *domain.AgentError) {
	absPath, agentErr := ResolveWorkspace(params.WorkspacePath)
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}

	command := cmp.Or(params.AgentConfig.Command, defaultCommand)
	sshHost := strings.TrimSpace(params.SSHHost)

	if strings.TrimSpace(command) == "" {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "agent command is empty or whitespace-only",
		}
	}

	if sshHost != "" {
		sshPath, lookErr := exec.LookPath("ssh")
		if lookErr != nil {
			return LaunchTarget{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "ssh binary not found on orchestrator host",
				Err:     lookErr,
			}
		}
		return LaunchTarget{
			Command:                  sshPath,
			Args:                     nil,
			WorkspacePath:            absPath,
			RemoteCommand:            command,
			SSHHost:                  sshHost,
			SSHStrictHostKeyChecking: params.SSHStrictHostKeyChecking,
		}, nil
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "agent command is empty or whitespace-only",
		}
	}

	resolved, agentErr := ResolveBinary(parts[0])
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}

	return LaunchTarget{
		Command:       resolved,
		Args:          slices.Clone(parts[1:]),
		WorkspacePath: absPath,
	}, nil
}
