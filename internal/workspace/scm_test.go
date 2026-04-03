//go:build unix

package workspace

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// writeSCMFile creates <wsPath>/.sortie/scm.json with the given content.
func writeSCMFile(t *testing.T, wsPath string, content []byte) {
	t.Helper()
	dir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scm.json"), content, 0o644); err != nil {
		t.Fatalf("WriteFile(scm.json): %v", err)
	}
}

// --- TestReadSCMMetadata ---

func TestReadSCMMetadata_FileAbsent(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	got := ReadSCMMetadata(wsPath, nil)

	if got != (domain.SCMMetadata{}) {
		t.Errorf("ReadSCMMetadata(absent) = %+v, want zero value", got)
	}
}

func TestReadSCMMetadata_ValidJSON(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMFile(t, wsPath, []byte(`{"branch":"feature/PROJ-42","sha":"abc123","pushed_at":"2026-04-01T12:00:00Z"}`))

	got := ReadSCMMetadata(wsPath, slog.Default())

	if got.Branch != "feature/PROJ-42" {
		t.Errorf("ReadSCMMetadata().Branch = %q, want %q", got.Branch, "feature/PROJ-42")
	}
	if got.SHA != "abc123" {
		t.Errorf("ReadSCMMetadata().SHA = %q, want %q", got.SHA, "abc123")
	}
	if got.PushedAt != "2026-04-01T12:00:00Z" {
		t.Errorf("ReadSCMMetadata().PushedAt = %q, want %q", got.PushedAt, "2026-04-01T12:00:00Z")
	}
}

func TestReadSCMMetadata_ValidJSON_SHAOnly(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMFile(t, wsPath, []byte(`{"branch":"main","sha":"deadbeef"}`))

	got := ReadSCMMetadata(wsPath, slog.Default())

	if got.Branch != "main" {
		t.Errorf("ReadSCMMetadata().Branch = %q, want %q", got.Branch, "main")
	}
	if got.SHA != "deadbeef" {
		t.Errorf("ReadSCMMetadata().SHA = %q, want %q", got.SHA, "deadbeef")
	}
	if got.PushedAt != "" {
		t.Errorf("ReadSCMMetadata().PushedAt = %q, want %q", got.PushedAt, "")
	}
}

func TestReadSCMMetadata_MalformedJSON(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMFile(t, wsPath, []byte(`{invalid`))

	got := ReadSCMMetadata(wsPath, slog.Default())

	if got != (domain.SCMMetadata{}) {
		t.Errorf("ReadSCMMetadata(malformed) = %+v, want zero value", got)
	}
}

func TestReadSCMMetadata_OversizedFile(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	// 4097 bytes exceeds the 4096-byte limit.
	oversized := make([]byte, 4097)
	for i := range oversized {
		oversized[i] = 'x'
	}
	writeSCMFile(t, wsPath, oversized)

	got := ReadSCMMetadata(wsPath, slog.Default())

	if got != (domain.SCMMetadata{}) {
		t.Errorf("ReadSCMMetadata(oversized) = %+v, want zero value", got)
	}
}

func TestReadSCMMetadata_EmptyBranchField(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMFile(t, wsPath, []byte(`{"branch":"","sha":"abc"}`))

	got := ReadSCMMetadata(wsPath, slog.Default())

	if got != (domain.SCMMetadata{}) {
		t.Errorf("ReadSCMMetadata(empty branch) = %+v, want zero value", got)
	}
}

func TestReadSCMMetadata_SymlinkAtDotSortie(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()

	// Create a real target directory with a valid scm.json.
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "scm.json"), []byte(`{"branch":"main","sha":"abc"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Make .sortie a symlink to the real directory.
	dotSortie := filepath.Join(wsPath, ".sortie")
	if err := os.Symlink(realDir, dotSortie); err != nil {
		t.Fatalf("Symlink(.sortie): %v", err)
	}

	got := ReadSCMMetadata(wsPath, slog.Default())

	if got != (domain.SCMMetadata{}) {
		t.Errorf("ReadSCMMetadata(symlink at .sortie) = %+v, want zero value", got)
	}
}

func TestReadSCMMetadata_SymlinkAtSCMJSON(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()

	// Create .sortie directory normally.
	dotSortie := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o755); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}

	// Create a real file in a different location.
	realFile := filepath.Join(t.TempDir(), "real_scm.json")
	if err := os.WriteFile(realFile, []byte(`{"branch":"main","sha":"abc"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(real_scm.json): %v", err)
	}

	// Make .sortie/scm.json a symlink to the real file.
	scmPath := filepath.Join(dotSortie, "scm.json")
	if err := os.Symlink(realFile, scmPath); err != nil {
		t.Fatalf("Symlink(scm.json): %v", err)
	}

	got := ReadSCMMetadata(wsPath, slog.Default())

	if got != (domain.SCMMetadata{}) {
		t.Errorf("ReadSCMMetadata(symlink at scm.json) = %+v, want zero value", got)
	}
}

func TestReadSCMMetadata_NilLogger(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()

	// No panic when logger is nil and file is absent.
	got := ReadSCMMetadata(wsPath, nil)

	if got != (domain.SCMMetadata{}) {
		t.Errorf("ReadSCMMetadata(nil logger, absent) = %+v, want zero value", got)
	}
}
