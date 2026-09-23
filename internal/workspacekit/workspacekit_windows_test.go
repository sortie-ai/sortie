//go:build windows

package workspacekit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mustJunction creates an NTFS junction at link pointing to target using
// mklink /J, skipping the test when the host cannot create one.
func mustJunction(t *testing.T, target, link string) {
	t.Helper()
	cmd := exec.Command("cmd", "/C", "mklink", "/J", link, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable in this test environment: %v\n%s", err, out)
	}
}

func TestVerifyDir_JunctionRefused(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-junction")
	mustJunction(t, target, link)

	if err := VerifyDir(link); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("VerifyDir(junction) = %v, want wrapping ErrNotDirectory", err)
	}
}

func TestOpenSortieDir_JunctionAtWorkspacePathRefused(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-junction")
	mustJunction(t, target, link)

	if _, err := OpenSortieDir(link, false); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("OpenSortieDir(junction workspace) = %v, want wrapping ErrNotDirectory", err)
	}
}

func TestOpenSortieDir_JunctionAtSortiePathRefused(t *testing.T) {
	t.Parallel()

	ws := prepareWorkspace(t)
	if err := os.Remove(filepath.Join(ws, SortieDir)); err != nil {
		t.Fatalf("Remove(%s): %v", SortieDir, err)
	}
	target := t.TempDir()
	mustJunction(t, target, filepath.Join(ws, SortieDir))

	if _, err := OpenSortieDir(ws, false); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("OpenSortieDir(junction .sortie) = %v, want wrapping ErrNotDirectory", err)
	}
}
