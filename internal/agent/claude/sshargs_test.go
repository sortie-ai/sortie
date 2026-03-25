package claude

import (
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", "''"},
		{"simple word", "hello", "'hello'"},
		{"with spaces", "hello world", "'hello world'"},
		{"with single quote", "it's", "'it'\\''s'"},
		{"multiple single quotes", "a'b'c", "'a'\\''b'\\''c'"},
		{"special chars", "$(rm -rf /)", "'$(rm -rf /)'"},
		{"backticks", "`id`", "'`id`'"},
		{"semicolons and pipes", "a; b | c", "'a; b | c'"},
		{"double quotes", `say "hi"`, `'say "hi"'`},
		{"newline", "a\nb", "'a\nb'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildSSHArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		host           string
		workspacePath  string
		remoteCommand  string
		agentArgs      []string
		wantContains   []string
		wantLastPrefix string
	}{
		{
			name:          "basic invocation",
			host:          "dev-host",
			workspacePath: "/home/user/project",
			remoteCommand: "claude",
			agentArgs:     []string{"--model", "opus"},
			wantContains: []string{
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "BatchMode=yes",
				"-o", "ConnectTimeout=30",
				"-o", "ServerAliveInterval=15",
				"-o", "ServerAliveCountMax=3",
				"--",
				"dev-host",
			},
		},
		{
			name:          "whitespace-padded host is trimmed",
			host:          "  padded-host  ",
			workspacePath: "/ws",
			remoteCommand: "claude",
			agentArgs:     nil,
			wantContains: []string{
				"--",
				"padded-host",
			},
		},
		{
			name:          "empty agent args",
			host:          "worker-1",
			workspacePath: "/ws",
			remoteCommand: "claude",
			agentArgs:     nil,
		},
		{
			name:          "workspace with spaces",
			host:          "worker-1",
			workspacePath: "/tmp/my project",
			remoteCommand: "claude",
			agentArgs:     []string{"--flag"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildSSHArgs(tt.host, tt.workspacePath, tt.remoteCommand, tt.agentArgs)

			// Verify SSH options are present.
			joined := strings.Join(got, " ")
			for _, want := range tt.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("buildSSHArgs() missing %q in %v", want, got)
				}
			}

			// Last element is the remote command string.
			remoteCmd := got[len(got)-1]

			// Must start with cd <quoted-workspace> &&
			if !strings.HasPrefix(remoteCmd, "cd ") {
				t.Errorf("remote command = %q, want prefix \"cd \"", remoteCmd)
			}
			if !strings.Contains(remoteCmd, "&&") {
				t.Errorf("remote command = %q, want to contain \"&&\"", remoteCmd)
			}

			// Must contain the quoted remote command.
			if !strings.Contains(remoteCmd, shellQuote(tt.remoteCommand)) {
				t.Errorf("remote command = %q, want to contain %q", remoteCmd, shellQuote(tt.remoteCommand))
			}

			// Must contain quoted workspace path.
			if !strings.Contains(remoteCmd, shellQuote(tt.workspacePath)) {
				t.Errorf("remote command = %q, want to contain %q", remoteCmd, shellQuote(tt.workspacePath))
			}

			// Agent args are quoted in the remote command.
			for _, arg := range tt.agentArgs {
				if !strings.Contains(remoteCmd, shellQuote(arg)) {
					t.Errorf("remote command = %q, want to contain quoted arg %q", remoteCmd, arg)
				}
			}

			// Host (trimmed) should appear at second-to-last position,
			// and "--" immediately before it.
			trimmedHost := strings.TrimSpace(tt.host)
			hostIdx := -1
			for i, a := range got {
				if a == trimmedHost {
					hostIdx = i
					break
				}
			}
			if hostIdx < 0 {
				t.Errorf("host %q not found in args %v", trimmedHost, got)
			} else {
				if hostIdx != len(got)-2 {
					t.Errorf("host at index %d, want %d (second-to-last)", hostIdx, len(got)-2)
				}
				// "--" must immediately precede the host.
				if hostIdx > 0 && got[hostIdx-1] != "--" {
					t.Errorf("arg before host = %q, want %q", got[hostIdx-1], "--")
				}
			}
		})
	}
}
