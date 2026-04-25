package codex

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

func TestBuildSSHRemoteCmd(t *testing.T) {
	t.Parallel()

	const base = "codex app-server"

	tests := []struct {
		name          string
		remoteCommand string
		apiKey        string
		want          string
	}{
		{
			name:          "empty key returns command unchanged",
			remoteCommand: base,
			apiKey:        "",
			want:          base,
		},
		{
			name:          "simple alphanumeric key",
			remoteCommand: base,
			apiKey:        "sk-abc123",
			want:          "CODEX_API_KEY='sk-abc123' " + base,
		},
		{
			name:          "key with dollar sign",
			remoteCommand: base,
			apiKey:        "sk-$secret",
			want:          "CODEX_API_KEY='sk-$secret' " + base,
		},
		{
			name:          "key with single quote",
			remoteCommand: base,
			apiKey:        "it's",
			want:          "CODEX_API_KEY='it'\\''s' " + base,
		},
		{
			name:          "key with combined metacharacters (spec example)",
			remoteCommand: base,
			apiKey:        "'foo'$bar",
			want:          "CODEX_API_KEY=''\\''foo'\\''$bar' " + base,
		},
		{
			name:          "key with semicolon",
			remoteCommand: base,
			apiKey:        "key;rm -rf /",
			want:          "CODEX_API_KEY='key;rm -rf /' " + base,
		},
		{
			name:          "key with backtick",
			remoteCommand: base,
			apiKey:        "key`whoami`",
			want:          "CODEX_API_KEY='key`whoami`' " + base,
		},
		{
			name:          "key with double quote",
			remoteCommand: base,
			apiKey:        `key"value"`,
			want:          `CODEX_API_KEY='key"value"' ` + base,
		},
		{
			name:          "key with spaces",
			remoteCommand: base,
			apiKey:        "key with spaces",
			want:          "CODEX_API_KEY='key with spaces' " + base,
		},
		{
			name:          "remote command preserved unchanged",
			remoteCommand: "codex app-server --flag value",
			apiKey:        "sk-abc",
			want:          "CODEX_API_KEY='sk-abc' codex app-server --flag value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildSSHRemoteCmd(tt.remoteCommand, tt.apiKey)
			if got != tt.want {
				t.Errorf("buildSSHRemoteCmd(%q, %q) = %q, want %q", tt.remoteCommand, tt.apiKey, got, tt.want)
			}
		})
	}
}

// TestBuildSSHRemoteCmd_MetacharactersDoNotLeakIntoSSHArgs verifies that
// the full pipeline (buildSSHRemoteCmd → BuildSSHArgs) does not produce a
// final SSH remote-command argument that contains the raw API key value
// when the key includes shell metacharacters. An unquoted metacharacter
// would allow injection through the remote shell.
func TestBuildSSHRemoteCmd_MetacharactersDoNotLeakIntoSSHArgs(t *testing.T) {
	t.Parallel()

	metacharKeys := []struct {
		name   string
		apiKey string
	}{
		{"single quote", "'secret'"},
		{"dollar sign", "$SECRET"},
		{"combined spec example", "'foo'$bar"},
		{"semicolon injection attempt", "key; rm -rf /"},
		{"backtick injection attempt", "key`whoami`"},
		{"subshell injection attempt", "$(cat /etc/passwd)"},
	}

	const (
		base      = "codex app-server"
		host      = "remote.host"
		workspace = "/workspace"
	)

	for _, tc := range metacharKeys {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remoteCmd := buildSSHRemoteCmd(base, tc.apiKey)
			sshArgs := sshutil.BuildSSHArgs(host, workspace, remoteCmd, nil, sshutil.SSHOptions{})

			// The last SSH arg is the full remote command string passed to the
			// remote shell. It must not contain the raw API key value, because
			// that would mean the key was concatenated without quoting.
			finalArg := sshArgs[len(sshArgs)-1]
			rawKeyAssignment := "CODEX_API_KEY=" + tc.apiKey
			if strings.Contains(finalArg, rawKeyAssignment) {
				t.Errorf("SSH remote-command arg contains unquoted API key assignment %q in %q",
					rawKeyAssignment, finalArg)
			}

			// The final arg must still carry the CODEX_API_KEY= prefix so the
			// environment variable is set on the remote host.
			if !strings.Contains(finalArg, "CODEX_API_KEY=") {
				t.Errorf("SSH remote-command arg missing CODEX_API_KEY= prefix in %q", finalArg)
			}

			// The base command must be present so the agent binary is invoked.
			if !strings.Contains(finalArg, base) {
				t.Errorf("SSH remote-command arg missing base command %q in %q", base, finalArg)
			}
		})
	}
}
