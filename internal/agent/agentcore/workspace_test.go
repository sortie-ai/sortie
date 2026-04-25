package agentcore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestResolveWorkspace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "somefile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(dir, symlink); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires elevated privileges on Windows")
		}
		t.Fatalf("setup symlink: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		wantKind  domain.AgentErrorKind
		wantMsg   string
		wantNoErr bool
	}{
		{
			name:     "empty path",
			path:     "",
			wantKind: domain.ErrInvalidWorkspaceCwd,
			wantMsg:  "empty workspace path",
		},
		{
			name:      "valid directory",
			path:      dir,
			wantNoErr: true,
		},
		{
			name:      "relative path resolving to valid directory",
			path:      ".",
			wantNoErr: true,
		},
		{
			name:     "non-existent path",
			path:     filepath.Join(dir, "does-not-exist"),
			wantKind: domain.ErrInvalidWorkspaceCwd,
			wantMsg:  "workspace path does not exist",
		},
		{
			name:     "file not a directory",
			path:     file,
			wantKind: domain.ErrInvalidWorkspaceCwd,
			wantMsg:  "workspace path is not a directory",
		},
		{
			name:      "symlink to valid directory",
			path:      symlink,
			wantNoErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, agentErr := ResolveWorkspace(tt.path)

			if tt.wantNoErr {
				if agentErr != nil {
					t.Fatalf("ResolveWorkspace(%q) unexpected error: %v", tt.path, agentErr)
				}
				if got == "" {
					t.Errorf("ResolveWorkspace(%q) = %q, want non-empty path", tt.path, got)
				}
				if !filepath.IsAbs(got) {
					t.Errorf("ResolveWorkspace(%q) = %q, want absolute path", tt.path, got)
				}
				return
			}

			if agentErr == nil {
				t.Fatalf("ResolveWorkspace(%q) = %q, want error with kind %q", tt.path, got, tt.wantKind)
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
