package sshutil

import (
	"strings"
	"testing"
)

// sshOption returns true when args contains the pair "-o" "<key>=<val>".
func sshOption(args []string, key, val string) bool {
	target := key + "=" + val
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-o" && args[i+1] == target {
			return true
		}
	}
	return false
}

// hostAfterSep returns the element immediately following "--" in args.
func hostAfterSep(args []string) string {
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "''"},
		{"simple identifier", "hello", "'hello'"},
		{"spaces", "hello world", "'hello world'"},
		{"single quote", "it's", "'it'\\''s'"},
		{"double quote", `say "hi"`, `'say "hi"'`},
		{"backslash", `a\b`, `'a\b'`},
		{"newline", "line\nnewline", "'line\nnewline'"},
		{"tab", "tab\there", "'tab\there'"},
		{"semicolon", "cmd; rm -rf /", "'cmd; rm -rf /'"},
		{"pipe", "foo | bar", "'foo | bar'"},
		{"env var", "$HOME", "'$HOME'"},
		{"unicode", "héllo", "'héllo'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ShellQuote(tt.input)
			if got != tt.want {
				t.Errorf("ShellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildSSHArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		host      string
		workspace string
		cmd       string
		agentArgs []string
		opts      SSHOptions
		check     func(t *testing.T, args []string)
	}{
		{
			name:      "default StrictHostKeyChecking is accept-new",
			host:      "dev.host",
			workspace: "/workspace",
			cmd:       "claude",
			check: func(t *testing.T, args []string) {
				t.Helper()
				if !sshOption(args, "StrictHostKeyChecking", "accept-new") {
					t.Errorf("BuildSSHArgs() args = %v: missing StrictHostKeyChecking=accept-new", args)
				}
			},
		},
		{
			name:      "custom StrictHostKeyChecking reject",
			host:      "dev.host",
			workspace: "/workspace",
			cmd:       "copilot",
			opts:      SSHOptions{StrictHostKeyChecking: "reject"},
			check: func(t *testing.T, args []string) {
				t.Helper()
				if !sshOption(args, "StrictHostKeyChecking", "reject") {
					t.Errorf("BuildSSHArgs() args = %v: missing StrictHostKeyChecking=reject", args)
				}
				if sshOption(args, "StrictHostKeyChecking", "accept-new") {
					t.Errorf("BuildSSHArgs() args = %v: unexpected StrictHostKeyChecking=accept-new", args)
				}
			},
		},
		{
			name:      "custom StrictHostKeyChecking yes",
			host:      "dev.host",
			workspace: "/workspace",
			cmd:       "copilot",
			opts:      SSHOptions{StrictHostKeyChecking: "yes"},
			check: func(t *testing.T, args []string) {
				t.Helper()
				if !sshOption(args, "StrictHostKeyChecking", "yes") {
					t.Errorf("BuildSSHArgs() args = %v: missing StrictHostKeyChecking=yes", args)
				}
			},
		},
		{
			name:      "fixed connectivity options always present",
			host:      "host",
			workspace: "/w",
			cmd:       "cmd",
			check: func(t *testing.T, args []string) {
				t.Helper()
				for _, pair := range [][2]string{
					{"BatchMode", "yes"},
					{"ConnectTimeout", "30"},
					{"ServerAliveInterval", "15"},
					{"ServerAliveCountMax", "3"},
				} {
					if !sshOption(args, pair[0], pair[1]) {
						t.Errorf("BuildSSHArgs() args = %v: missing SSH option %s=%s", args, pair[0], pair[1])
					}
				}
			},
		},
		{
			name:      "-- separator and trimmed host",
			host:      "target.host",
			workspace: "/w",
			cmd:       "cmd",
			check: func(t *testing.T, args []string) {
				t.Helper()
				found := false
				for _, a := range args {
					if a == "--" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("BuildSSHArgs() args = %v: missing '--' separator", args)
				}
				if h := hostAfterSep(args); h != "target.host" {
					t.Errorf("host after '--' = %q, want %q", h, "target.host")
				}
			},
		},
		{
			name:      "host whitespace is trimmed",
			host:      "  spaced.host  ",
			workspace: "/w",
			cmd:       "cmd",
			check: func(t *testing.T, args []string) {
				t.Helper()
				if h := hostAfterSep(args); h != "spaced.host" {
					t.Errorf("host = %q, want %q (whitespace stripped)", h, "spaced.host")
				}
			},
		},
		{
			name:      "workspace cd in remote command",
			host:      "host",
			workspace: "/my/workspace",
			cmd:       "claude",
			check: func(t *testing.T, args []string) {
				t.Helper()
				remote := args[len(args)-1]
				if !strings.HasPrefix(remote, "cd ") {
					t.Errorf("remote cmd = %q: want prefix 'cd '", remote)
				}
				if !strings.Contains(remote, ShellQuote("/my/workspace")) {
					t.Errorf("remote cmd = %q: missing quoted workspace %q", remote, ShellQuote("/my/workspace"))
				}
				if !strings.Contains(remote, "&&") {
					t.Errorf("remote cmd = %q: missing '&&' separator", remote)
				}
			},
		},
		{
			name:      "remote command format with no agent args",
			host:      "host",
			workspace: "/workspace",
			cmd:       "claude",
			check: func(t *testing.T, args []string) {
				t.Helper()
				remote := args[len(args)-1]
				want := "cd " + ShellQuote("/workspace") + " && " + ShellQuote("claude")
				if remote != want {
					t.Errorf("remote cmd = %q, want %q", remote, want)
				}
			},
		},
		{
			name:      "agent args are all shell-quoted in remote command",
			host:      "host",
			workspace: "/w",
			cmd:       "copilot",
			agentArgs: []string{"--task", "fix it up", "--model", "gpt-4"},
			check: func(t *testing.T, args []string) {
				t.Helper()
				remote := args[len(args)-1]
				for _, arg := range []string{"--task", "fix it up", "--model", "gpt-4"} {
					if !strings.Contains(remote, ShellQuote(arg)) {
						t.Errorf("remote cmd = %q: missing quoted arg %q", remote, ShellQuote(arg))
					}
				}
			},
		},
		{
			name:      "workspace path with spaces is quoted",
			host:      "host",
			workspace: "/home/user/my workspace",
			cmd:       "claude",
			check: func(t *testing.T, args []string) {
				t.Helper()
				remote := args[len(args)-1]
				if !strings.Contains(remote, ShellQuote("/home/user/my workspace")) {
					t.Errorf("remote cmd = %q: workspace with spaces not properly quoted", remote)
				}
			},
		},
		{
			name:      "command with spaces is shell-quoted",
			host:      "host",
			workspace: "/w",
			cmd:       "/usr/local/bin/my cmd",
			check: func(t *testing.T, args []string) {
				t.Helper()
				remote := args[len(args)-1]
				if !strings.Contains(remote, ShellQuote("/usr/local/bin/my cmd")) {
					t.Errorf("remote cmd = %q: command with spaces not properly quoted", remote)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := BuildSSHArgs(tt.host, tt.workspace, tt.cmd, tt.agentArgs, tt.opts)
			tt.check(t, args)
		})
	}
}
