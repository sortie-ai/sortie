//go:build unix

package workspacekit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func mustMkfifo(t *testing.T, path string) {
	t.Helper()
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("Mkfifo unavailable in this test environment: %v", err)
	}
}

// runWithDeadline runs fn on its own goroutine and fails the test if fn has
// not returned within d, proving an open that could block on a FIFO with no
// writer does not.
func runWithDeadline(t *testing.T, d time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("call blocked past the deadline, want a non-blocking open")
		return nil
	}
}

func TestOpenSortieDir_FIFOAtWorkspacePathRefusedWithoutHanging(t *testing.T) {
	t.Parallel()

	ws := filepath.Join(t.TempDir(), "workspace-fifo")
	mustMkfifo(t, ws)

	err := runWithDeadline(t, 5*time.Second, func() error {
		_, err := OpenSortieDir(ws, false)
		return err
	})
	if !errors.Is(err, ErrNotDirectory) {
		t.Errorf("OpenSortieDir(FIFO workspace) = %v, want wrapping ErrNotDirectory", err)
	}
}

func TestOpenSortieFile_FIFOAtSortiePathRefusedWithoutHanging(t *testing.T) {
	t.Parallel()

	ws := prepareWorkspace(t)
	mustMkfifo(t, filepath.Join(ws, SortieDir, "f.txt"))

	err := runWithDeadline(t, 5*time.Second, func() error {
		_, err := OpenSortieFile(ws, "f.txt")
		return err
	})
	if !errors.Is(err, ErrNotPlainFile) {
		t.Errorf("OpenSortieFile(FIFO) = %v, want wrapping ErrNotPlainFile", err)
	}
}

// TestSeamDrivenSymlinkSwapChanged proves open_dir, OpenSubdir, and
// open_plain each catch an entry swapped for a symbolic link in the window
// between the verifying Lstat and the open that follows it.
func TestSeamDrivenSymlinkSwapChanged(t *testing.T) {
	t.Run("open_dir", func(t *testing.T) {
		ws := t.TempDir()
		elsewhere := t.TempDir()

		original := seam
		seam = func() {
			if err := os.RemoveAll(ws); err != nil {
				t.Fatalf("RemoveAll(ws): %v", err)
			}
			if err := os.Symlink(elsewhere, ws); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
		}
		t.Cleanup(func() { seam = original })

		if _, err := openDir(ws); !errors.Is(err, ErrChanged) {
			t.Errorf("openDir(seam-swapped for symlink) = %v, want wrapping ErrChanged", err)
		}
	})

	t.Run("OpenSubdir", func(t *testing.T) {
		// The swapped-in symlink targets a directory inside ws: os.Root
		// refuses a symlink that escapes the root before this package's own
		// comparison ever runs, which would prove only the Root's own
		// boundary check, not open_dir's ErrChanged comparison.
		ws := t.TempDir()
		insideTarget := filepath.Join(ws, "inside-target")
		if err := os.Mkdir(insideTarget, 0o750); err != nil {
			t.Fatalf("Mkdir(inside-target): %v", err)
		}
		if err := os.Mkdir(filepath.Join(ws, "child"), 0o750); err != nil {
			t.Fatalf("Mkdir(child): %v", err)
		}
		parent, err := openDir(ws)
		if err != nil {
			t.Fatalf("openDir: %v", err)
		}
		t.Cleanup(func() { parent.Close() }) //nolint:errcheck // test cleanup

		original := seam
		seam = func() {
			if err := os.RemoveAll(filepath.Join(ws, "child")); err != nil {
				t.Fatalf("RemoveAll(child): %v", err)
			}
			if err := os.Symlink("inside-target", filepath.Join(ws, "child")); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
		}
		t.Cleanup(func() { seam = original })

		if _, err := OpenSubdir(parent, "child", false); !errors.Is(err, ErrChanged) {
			t.Errorf("OpenSubdir(seam-swapped for symlink) = %v, want wrapping ErrChanged", err)
		}
	})

	t.Run("open_plain", func(t *testing.T) {
		ws := t.TempDir()
		insideTarget := filepath.Join(ws, "inside-target.txt")
		if err := os.WriteFile(insideTarget, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(inside-target): %v", err)
		}
		if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("original"), 0o600); err != nil {
			t.Fatalf("WriteFile(f.txt): %v", err)
		}
		dir, err := openDir(ws)
		if err != nil {
			t.Fatalf("openDir: %v", err)
		}
		t.Cleanup(func() { dir.Close() }) //nolint:errcheck // test cleanup

		original := seam
		seam = func() {
			if err := os.Remove(filepath.Join(ws, "f.txt")); err != nil {
				t.Fatalf("Remove(f.txt): %v", err)
			}
			if err := os.Symlink("inside-target.txt", filepath.Join(ws, "f.txt")); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
		}
		t.Cleanup(func() { seam = original })

		if _, err := openPlain(dir, "f.txt"); !errors.Is(err, ErrChanged) {
			t.Errorf("openPlain(seam-swapped for symlink) = %v, want wrapping ErrChanged", err)
		}
	})
}
