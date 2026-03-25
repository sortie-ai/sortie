package claude

import "strings"

// shellQuote quotes a string for safe inclusion in a remote shell
// command. Uses single-quoting with embedded single-quote escaping.
// This is the standard POSIX shell quoting pattern to prevent
// injection when SSH passes the remote command through the remote
// shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildSSHArgs constructs SSH invocation arguments for remote agent
// execution. The agent command and its arguments are passed as a
// shell-quoted remote command string. The workspace path is used to
// set the remote cwd via cd.
func buildSSHArgs(host, workspacePath, remoteCommand string, agentArgs []string) []string {
	sshOpts := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=30",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"--",
		strings.TrimSpace(host),
	}

	// Build remote command: cd <workspace> && <command> <args...>
	var parts []string
	parts = append(parts, "cd", shellQuote(workspacePath), "&&")
	parts = append(parts, shellQuote(remoteCommand))
	for _, arg := range agentArgs {
		parts = append(parts, shellQuote(arg))
	}
	remoteCmd := strings.Join(parts, " ")

	return append(sshOpts, remoteCmd)
}
