package agentcore

import (
	"cmp"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
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
	// Command is the resolved path to the local binary to exec.
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

	// SSHEnvNames lists the environment variable names to carry into
	// a remote session, passed through from StartSessionParams. Empty
	// in local mode.
	SSHEnvNames []string
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
// preflights) are the caller's responsibility and must run after
// ResolveLaunchTarget returns successfully.
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
			SSHEnvNames:              params.SSHEnvNames,
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

// SSHOptions resolves t's SSH transport options for one remote
// launch. It reads each of t.SSHEnvNames with [os.LookupEnv], in
// order, carrying a name only when its value holds a non-whitespace
// character, then appends every settings entry whose Value is
// non-empty. A name already carried, whether from t.SSHEnvNames or
// from an earlier settings entry, is not carried again.
//
// A variable that is unset, empty, or only whitespace is left behind,
// so a blank value on the orchestrator cannot override the credential
// or login the remote host already holds.
//
// settings carries adapter-owned variables with computed values, such
// as a tool-server setting. It MUST NOT carry a credential: a
// credential reaches a remote launch only as a name in
// t.SSHEnvNames. SSHOptions calls os.LookupEnv on every call, so
// calling it once per launch reflects the orchestrator's environment
// at that moment.
func (t LaunchTarget) SSHOptions(settings ...sshutil.EnvVar) sshutil.SSHOptions {
	var carried []sshutil.EnvVar
	skip := make(map[string]bool, len(t.SSHEnvNames)+len(settings))
	for _, entry := range settings {
		skip[entry.Name] = true
	}

	for _, name := range t.SSHEnvNames {
		if skip[name] {
			continue
		}
		skip[name] = true
		value, present := os.LookupEnv(name)
		if !present || strings.TrimSpace(value) == "" {
			continue
		}
		carried = append(carried, sshutil.EnvVar{Name: name, Value: value})
	}

	added := make(map[string]bool, len(settings))
	for _, entry := range settings {
		if entry.Value == "" || added[entry.Name] {
			continue
		}
		added[entry.Name] = true
		carried = append(carried, entry)
	}

	return sshutil.SSHOptions{
		StrictHostKeyChecking: t.SSHStrictHostKeyChecking,
		Env:                   carried,
	}
}
