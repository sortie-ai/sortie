// Package sshutil provides SSH invocation utilities shared by agent
// adapters that launch coding agents on remote hosts via SSH.
package sshutil

import "strings"

// ShellQuote quotes s for safe inclusion in a POSIX shell command.
// Uses single-quoting with embedded single-quote escaping to prevent
// shell injection when SSH passes the remote command through the
// remote shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// SSHOptions configures SSH transport behavior for remote agent
// execution. Adapters populate this from orchestrator-provided
// configuration. Zero-value fields select safe defaults.
type SSHOptions struct {
	// StrictHostKeyChecking is the OpenSSH StrictHostKeyChecking
	// value. When empty, defaults to "accept-new" (TOFU).
	StrictHostKeyChecking string
}

// BuildSSHArgs constructs SSH invocation arguments for remote agent
// execution. The workspace path sets the remote cwd via cd. remoteCommand
// is treated as a pre-formed POSIX shell fragment and appended verbatim;
// callers are responsible for any quoting within that fragment. agentArgs
// are individually shell-quoted and appended after remoteCommand.
//
// SSH options applied (unless overridden via opts):
//   - StrictHostKeyChecking=accept-new (TOFU) — configurable via opts
//   - BatchMode=yes (no interactive prompts)
//   - ConnectTimeout=30
//   - ServerAliveInterval=15
//   - ServerAliveCountMax=3
func BuildSSHArgs(host, workspacePath, remoteCommand string, agentArgs []string, opts SSHOptions) []string {
	strictHostKey := opts.StrictHostKeyChecking
	if strictHostKey == "" {
		strictHostKey = "accept-new"
	}

	sshOpts := []string{
		"-o", "StrictHostKeyChecking=" + strictHostKey,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=30",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"--",
		strings.TrimSpace(host),
	}

	var parts []string
	parts = append(parts, "cd", "--", ShellQuote(workspacePath), "&&")
	parts = append(parts, remoteCommand)
	for _, arg := range agentArgs {
		parts = append(parts, ShellQuote(arg))
	}
	remoteCmd := strings.Join(parts, " ")

	return append(sshOpts, remoteCmd)
}
