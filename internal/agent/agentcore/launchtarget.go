package agentcore

import (
	"cmp"
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
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
	// holds more than its executable: the words after the first of a
	// string command (e.g., "codex app-server" yields Args:
	// ["app-server"]), or the elements after element zero of a list
	// command. Empty in SSH mode.
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

	// WithheldEnv lists environment variable names no launch through this
	// target carries: a local launch runs without them, and a remote launch
	// neither carries them nor lets the remote shell pass them to the agent.
	WithheldEnv []string

	// StopGrace is the session's resolved agent.stop_grace_ms, which a remote
	// launch also grants its runtime once the SSH connection is gone. Zero in
	// local mode.
	StopGrace time.Duration

	// ConfiguredEnv is what the workflow sets for the runtime, in ascending
	// name order. [LaunchTarget.Environ] applies it to a local launch and a
	// remote launch carries it on the standard-input preamble. Callers MUST NOT
	// modify it.
	ConfiguredEnv []sshutil.EnvVar

	// RemoteArgv holds a list-form command's elements, unquoted, when the
	// command runs over SSH. Nil for a string command and for a local launch.
	RemoteArgv []string
}

// ResolveLaunchTarget resolves the workspace path and agent binary for a
// session, choosing between SSH and local launch modes based on
// params.SSHHost. It returns a populated [LaunchTarget] and a nil error on
// success, or a zero [LaunchTarget] and a [*domain.AgentError] on failure.
//
// defaultCommand is the fallback agent command when params.AgentConfig holds
// no command (e.g., "claude", "copilot", "codex app-server"). A non-empty
// params.AgentConfig.CommandArgv is the command instead: locally element zero
// resolves whole, whitespace included, and the rest become Args; over SSH
// every element is single-quoted into RemoteCommand and kept unquoted in
// RemoteArgv. This is the only reader of CommandArgv.
//
// Adapter-specific post-resolution steps (canary version checks, auth
// preflights) are the caller's responsibility and must run after
// ResolveLaunchTarget returns successfully.
func ResolveLaunchTarget(params domain.StartSessionParams, defaultCommand string) (LaunchTarget, *domain.AgentError) {
	absPath, agentErr := ResolveWorkspace(params.WorkspacePath)
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}

	sshHost := strings.TrimSpace(params.SSHHost)

	if argv := params.AgentConfig.CommandArgv; len(argv) > 0 {
		return resolveArgvLaunch(params, absPath, sshHost, argv)
	}

	command := cmp.Or(params.AgentConfig.Command, defaultCommand)

	if strings.TrimSpace(command) == "" {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "agent command is empty or whitespace-only",
		}
	}

	if sshHost != "" {
		return resolveSSHLaunch(params, absPath, sshHost, command)
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

func resolveSSHLaunch(params domain.StartSessionParams, absPath, sshHost, remoteCommand string) (LaunchTarget, *domain.AgentError) {
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
		RemoteCommand:            remoteCommand,
		SSHHost:                  sshHost,
		SSHStrictHostKeyChecking: params.SSHStrictHostKeyChecking,
		SSHEnvNames:              params.SSHEnvNames,
		StopGrace:                procutil.StopGrace(params.AgentConfig.StopGraceMS),
	}, nil
}

func resolveArgvLaunch(params domain.StartSessionParams, absPath, sshHost string, argv []string) (LaunchTarget, *domain.AgentError) {
	if sshHost != "" {
		target, agentErr := resolveSSHLaunch(params, absPath, sshHost, sshutil.QuoteArgv(argv))
		if agentErr != nil {
			return LaunchTarget{}, agentErr
		}
		target.RemoteArgv = slices.Clone(argv)
		return target, nil
	}
	resolved, agentErr := lookPath(argv[0])
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}
	return LaunchTarget{
		Command:       resolved,
		Args:          slices.Clone(argv[1:]),
		WorkspacePath: absPath,
	}, nil
}

// CommandWords returns the words of the configured agent command: Command
// then Args for a local launch; for a remote one, RemoteArgv when the command
// is a list, else the whitespace-separated words of RemoteCommand. The SSH
// executable is never among them. The slice is the caller's to modify.
func (t LaunchTarget) CommandWords() []string {
	if t.RemoteCommand != "" {
		if t.RemoteArgv != nil {
			return slices.Clone(t.RemoteArgv)
		}
		return strings.Fields(t.RemoteCommand)
	}
	return append([]string{t.Command}, t.Args...)
}

// SSHOptions resolves t's SSH transport options for one remote
// launch. It reads each of t.SSHEnvNames with [os.LookupEnv], in
// order, carrying a name only when its value holds a non-whitespace
// character, then appends t.ConfiguredEnv, then every settings entry
// whose Value is non-empty. Per name, a settings entry beats
// t.ConfiguredEnv, which beats t.SSHEnvNames. A name already carried is
// not carried again.
//
// A variable that is unset, empty, or only whitespace in t.SSHEnvNames is
// left behind, so a blank value on the orchestrator cannot override the
// credential or login the remote host already holds. A t.ConfiguredEnv
// entry is carried with an empty value too, because the workflow set it.
//
// settings carries adapter-owned variables with computed values, such
// as a tool-server setting. It MUST NOT carry a credential: a
// credential reaches a remote launch only as a name in t.SSHEnvNames or
// as an entry of t.ConfiguredEnv, which carries what the workflow sets
// for the runtime, a credential included, only on the standard-input
// preamble. SSHOptions calls os.LookupEnv on every call, so calling it
// once per launch reflects the orchestrator's environment at that moment.
//
// A name in t.WithheldEnv is never carried, whether t.SSHEnvNames,
// t.ConfiguredEnv or a settings entry names it, and is returned as the
// options' Unset list.
func (t LaunchTarget) SSHOptions(settings ...sshutil.EnvVar) sshutil.SSHOptions {
	var carried []sshutil.EnvVar
	settingNames := make(map[string]bool, len(settings))
	for _, entry := range settings {
		settingNames[entry.Name] = true
	}
	skip := make(map[string]bool, len(t.WithheldEnv)+len(t.ConfiguredEnv)+len(t.SSHEnvNames)+len(settings))
	maps.Copy(skip, settingNames)
	for _, name := range t.WithheldEnv {
		skip[name] = true
	}
	for _, entry := range t.ConfiguredEnv {
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

	for _, entry := range t.ConfiguredEnv {
		if settingNames[entry.Name] || slices.Contains(t.WithheldEnv, entry.Name) {
			continue
		}
		carried = append(carried, entry)
	}

	added := make(map[string]bool, len(settings))
	for _, entry := range settings {
		if entry.Value == "" || added[entry.Name] || slices.Contains(t.WithheldEnv, entry.Name) {
			continue
		}
		added[entry.Name] = true
		carried = append(carried, entry)
	}

	return sshutil.SSHOptions{
		StrictHostKeyChecking: t.SSHStrictHostKeyChecking,
		Env:                   carried,
		Unset:                 slices.Clone(t.WithheldEnv),
		StopGrace:             t.StopGrace,
	}
}

// RuntimeEnv returns name's value as the runtime receives it from
// Sortie: from env, or [LaunchTarget.Environ] when env is nil, for a local
// launch; for a remote launch, a name in t.ConfiguredEnv with its configured
// value, empty included, else only a variable the launch forwards over SSH,
// which is a name in t.SSHEnvNames whose value in env holds a
// non-whitespace character, the rule [LaunchTarget.SSHOptions] carries
// by. A value the remote host sets for itself is invisible. A name in
// t.WithheldEnv is never visible. When env holds a name more than once
// the last entry wins, as it does for a launched process.
func (t LaunchTarget) RuntimeEnv(env []string, name string) (string, bool) {
	if slices.Contains(t.WithheldEnv, name) {
		return "", false
	}
	remote := t.RemoteCommand != ""
	if remote {
		if i := slices.IndexFunc(t.ConfiguredEnv, func(entry sshutil.EnvVar) bool { return entry.Name == name }); i >= 0 {
			return t.ConfiguredEnv[i].Value, true
		}
		if !slices.Contains(t.SSHEnvNames, name) {
			return "", false
		}
	}
	if env == nil {
		env = t.Environ()
	}
	var value string
	var found bool
	for _, entry := range env {
		if !IsEnvEntryNamed(entry, name) {
			continue
		}
		_, value, _ = strings.Cut(entry, "=")
		found = true
	}
	if remote && strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, found
}

// launcherEnviron returns the environment of the process Sortie starts. The
// local ssh client of a remote launch keeps Sortie's own, because the configured
// variables are meant for the remote runtime and travel on the preamble.
func (t LaunchTarget) launcherEnviron() []string {
	if t.RemoteCommand != "" {
		return os.Environ()
	}
	return t.Environ()
}

// Environ returns the environment a local launch starts from: [os.Environ]
// less every entry that names a t.ConfiguredEnv variable, then t.ConfiguredEnv
// in order. A configured name replaces the inherited entry, whatever its
// letter case on Windows.
func (t LaunchTarget) Environ() []string {
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return slices.ContainsFunc(t.ConfiguredEnv, func(configured sshutil.EnvVar) bool {
			return IsEnvEntryNamed(entry, configured.Name)
		})
	})
	for _, configured := range t.ConfiguredEnv {
		env = append(env, configured.Name+"="+configured.Value)
	}
	return env
}

// BindWorkspace re-verifies t.WorkspacePath and, on success, sets
// cmd.Dir to it, sets cmd.Env when it is nil to [LaunchTarget.Environ], or to
// Sortie's own environment for the ssh client of a remote launch, and
// replaces every PWD entry already in cmd.Env with one naming the same
// verified path, so a subprocess that trusts PWD over its own working
// directory still lands in the workspace. It also removes every entry
// named in t.WithheldEnv from cmd.Env. On failure cmd.Dir and
// cmd.Env are left untouched and the verification error is returned;
// the caller must not start cmd.
//
// Callers MUST assign cmd.Env before calling BindWorkspace and MUST
// NOT assign it afterward, so the PWD this sets is never overwritten.
func (t LaunchTarget) BindWorkspace(cmd *exec.Cmd) *domain.AgentError {
	absPath, agentErr := ResolveWorkspace(t.WorkspacePath)
	if agentErr != nil {
		return agentErr
	}
	cmd.Dir = absPath
	if cmd.Env == nil {
		cmd.Env = t.launcherEnviron()
	}
	cmd.Env = dropEnvNames(cmd.Env, t.WithheldEnv)
	cmd.Env = setPWD(cmd.Env, absPath)
	return nil
}

// dropEnvNames returns env without the entries named in names, compared
// as [IsEnvEntryNamed] compares.
func dropEnvNames(env, names []string) []string {
	if len(names) == 0 {
		return env
	}
	return slices.DeleteFunc(slices.Clone(env), func(entry string) bool {
		return slices.ContainsFunc(names, func(name string) bool {
			return IsEnvEntryNamed(entry, name)
		})
	})
}

// setPWD returns env with every entry named PWD removed (case-
// insensitively on Windows, exactly elsewhere, matching the platform's
// own environment-variable name comparison) and exactly one
// "PWD=value" appended.
func setPWD(env []string, value string) []string {
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if isPWDEntry(entry) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "PWD="+value)
}

// isPWDEntry reports whether entry is an env-slice assignment of PWD.
func isPWDEntry(entry string) bool {
	return IsEnvEntryNamed(entry, "PWD")
}

// IsEnvEntryNamed reports whether entry is an env-slice assignment of
// name, comparing case-insensitively on Windows to match that platform's
// own environment-variable name resolution. It is the one name rule every
// reader of a launch environment uses.
func IsEnvEntryNamed(entry, name string) bool {
	key, _, found := strings.Cut(entry, "=")
	if !found {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(key, name)
	}
	return key == name
}

// AuxiliaryCommand builds a one-shot runtime subcommand run in the
// workspace, locally or over ssh like the session itself. A nil env
// means [BindWorkspace] supplies the launcher's environment with PWD replaced;
// stdin may be nil. Returns a non-nil [*domain.AgentError] and a nil
// *[exec.Cmd] when the workspace path fails re-verification immediately
// before launch.
func (t LaunchTarget) AuxiliaryCommand(ctx context.Context, args []string, stdin io.Reader, env []string, carried ...sshutil.EnvVar) (*exec.Cmd, *domain.AgentError) {
	var cmd *exec.Cmd
	if t.RemoteCommand == "" {
		allArgs := append(slices.Clone(t.Args), args...)
		cmd = exec.CommandContext(ctx, t.Command, allArgs...) //nolint:gosec // args are constructed programmatically
		cmd.Stdin = stdin
	} else {
		launch := sshutil.BuildSSHLaunch(t.SSHHost, t.WorkspacePath, t.RemoteCommand, args, t.SSHOptions(carried...))
		cmd = exec.CommandContext(ctx, t.Command, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
		cmd.Stdin = prependPreamble(launch.StdinReader(), stdin)
	}
	cmd.Env = env
	if agentErr := t.BindWorkspace(cmd); agentErr != nil {
		return nil, agentErr
	}
	return cmd, nil
}

func prependPreamble(preamble, stdin io.Reader) io.Reader {
	switch {
	case preamble == nil:
		return stdin
	case stdin == nil:
		return preamble
	default:
		return io.MultiReader(preamble, stdin)
	}
}

// AuxiliaryTimeout returns the bound for an auxiliary launch: twice
// cfg.ReadTimeoutMS, capped at 30 seconds, and never non-positive.
func AuxiliaryTimeout(cfg domain.AgentConfig) time.Duration {
	return min(2*ReadTimeout(cfg), 30*time.Second)
}
