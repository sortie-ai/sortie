package copilot

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
		{"word with spaces", "hello world", "'hello world'"},
		{"single quote embedded", "it's", "'it'\\''s'"},
		{"multiple single quotes", "a'b'c", "'a'\\''b'\\''c'"},
		{"shell metacharacters prevented", "$(rm -rf /)", "'$(rm -rf /)'"},
		{"backticks prevented", "`id`", "'`id`'"},
		{"semicolons and pipes prevented", "a; b | c", "'a; b | c'"},
		{"double quotes inside are safe", `say "hi"`, `'say "hi"'`},
		{"newline preserved literally", "a\nb", "'a\nb'"},
		{"path with spaces", "/my workspace/project", "'/my workspace/project'"},
		{"path with no special chars", "/home/user/workspace", "'/home/user/workspace'"},
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
		name          string
		host          string
		workspacePath string
		remoteCommand string
		agentArgs     []string
		checkArgs     func(t *testing.T, args []string)
	}{
		{
			name:          "basic invocation has required SSH options",
			host:          "dev-host",
			workspacePath: "/home/user/project",
			remoteCommand: "copilot",
			agentArgs:     []string{"--model", "gpt-5"},
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				joined := strings.Join(args, " ")
				for _, opt := range []string{
					"StrictHostKeyChecking=accept-new",
					"BatchMode=yes",
					"ConnectTimeout=30",
					"ServerAliveInterval=15",
					"ServerAliveCountMax=3",
				} {
					if !strings.Contains(joined, opt) {
						t.Errorf("buildSSHArgs() missing SSH option %q in %v", opt, args)
					}
				}
				// Host appears after "--"
				if !strings.Contains(joined, "-- dev-host") {
					t.Errorf("buildSSHArgs() missing \"-- dev-host\" in %v", args)
				}
			},
		},
		{
			name:          "whitespace-padded host is trimmed",
			host:          "  padded-host  ",
			workspacePath: "/ws",
			remoteCommand: "copilot",
			agentArgs:     nil,
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				joined := strings.Join(args, " ")
				if !strings.Contains(joined, "padded-host") {
					t.Errorf("buildSSHArgs() missing trimmed host in %v", args)
				}
				if strings.Contains(joined, "  padded-host") {
					t.Errorf("buildSSHArgs() host not trimmed in %v", args)
				}
			},
		},
		{
			name:          "remote command starts with cd and workspace path",
			host:          "worker-1",
			workspacePath: "/home/ubuntu/workspace",
			remoteCommand: "copilot",
			agentArgs:     []string{"-p", "fix bug"},
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				// The last element is the remote command string.
				remoteCmd := args[len(args)-1]
				if !strings.HasPrefix(remoteCmd, "cd ") {
					t.Errorf("remote command %q does not start with \"cd \"", remoteCmd)
				}
				if !strings.Contains(remoteCmd, "&&") {
					t.Errorf("remote command %q missing \"&&\" separator", remoteCmd)
				}
				// The quoted workspace path appears in the cd fragment.
				quotedPath := shellQuote("/home/ubuntu/workspace")
				if !strings.Contains(remoteCmd, quotedPath) {
					t.Errorf("remote command %q missing quoted workspace %q", remoteCmd, quotedPath)
				}
				// The remote command binary appears after &&.
				quotedCmd := shellQuote("copilot")
				if !strings.Contains(remoteCmd, quotedCmd) {
					t.Errorf("remote command %q missing quoted copilot binary %q", remoteCmd, quotedCmd)
				}
			},
		},
		{
			name:          "workspace path with spaces is quoted",
			host:          "worker-1",
			workspacePath: "/my workspace/issue 42",
			remoteCommand: "copilot",
			agentArgs:     nil,
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				remoteCmd := args[len(args)-1]
				quotedPath := shellQuote("/my workspace/issue 42")
				if !strings.Contains(remoteCmd, quotedPath) {
					t.Errorf("remote command %q missing quoted path %q", remoteCmd, quotedPath)
				}
			},
		},
		{
			name:          "workspace path with single quotes is safely escaped",
			host:          "worker-1",
			workspacePath: "/tmp/it's/here",
			remoteCommand: "copilot",
			agentArgs:     nil,
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				remoteCmd := args[len(args)-1]
				quotedPath := shellQuote("/tmp/it's/here")
				if !strings.Contains(remoteCmd, quotedPath) {
					t.Errorf("remote command %q missing escaped path %q", remoteCmd, quotedPath)
				}
			},
		},
		{
			name:          "agent args are shell-quoted in remote command",
			host:          "h",
			workspacePath: "/ws",
			remoteCommand: "copilot",
			agentArgs:     []string{"-p", "fix the 'bug' today"},
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				remoteCmd := args[len(args)-1]
				quotedPrompt := shellQuote("fix the 'bug' today")
				if !strings.Contains(remoteCmd, quotedPrompt) {
					t.Errorf("remote command %q missing quoted prompt %q", remoteCmd, quotedPrompt)
				}
			},
		},
		{
			name:          "no agent args still constructs valid remote command",
			host:          "worker-1",
			workspacePath: "/ws",
			remoteCommand: "copilot",
			agentArgs:     nil,
			checkArgs: func(t *testing.T, args []string) {
				t.Helper()
				remoteCmd := args[len(args)-1]
				if !strings.HasPrefix(remoteCmd, "cd ") {
					t.Errorf("remote command %q does not start with \"cd \"", remoteCmd)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildSSHArgs(tt.host, tt.workspacePath, tt.remoteCommand, tt.agentArgs)
			if len(got) == 0 {
				t.Fatal("buildSSHArgs() returned empty slice")
			}
			tt.checkArgs(t, got)
		})
	}
}
