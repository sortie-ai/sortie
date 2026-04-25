package agentcore

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestResolveBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		command   string
		wantKind  domain.AgentErrorKind
		wantMsg   string
		wantNoErr bool
	}{
		{
			name:      "binary on PATH",
			command:   "sh",
			wantNoErr: true,
		},
		{
			name:     "binary not found",
			command:  "sortie-no-such-binary-xyzzy",
			wantKind: domain.ErrAgentNotFound,
			wantMsg:  `agent command "sortie-no-such-binary-xyzzy" not found`,
		},
		{
			name:     "multi-token command with space",
			command:  "codex app-server",
			wantKind: domain.ErrAgentNotFound,
			wantMsg:  `agent command must be a single token, got "codex app-server"`,
		},
		{
			name:     "multi-token command with tab",
			command:  "codex\tapp-server",
			wantKind: domain.ErrAgentNotFound,
			wantMsg:  "agent command must be a single token, got \"codex\\tapp-server\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, agentErr := ResolveBinary(tt.command)

			if tt.wantNoErr {
				if agentErr != nil {
					t.Fatalf("ResolveBinary(%q) unexpected error: %v", tt.command, agentErr)
				}
				if got == "" {
					t.Errorf("ResolveBinary(%q) = %q, want non-empty path", tt.command, got)
				}
				return
			}

			if agentErr == nil {
				t.Fatalf("ResolveBinary(%q) = %q, want error with kind %q", tt.command, got, tt.wantKind)
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
