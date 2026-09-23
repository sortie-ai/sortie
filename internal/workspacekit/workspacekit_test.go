package workspacekit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires elevated privileges on Windows")
		}
		t.Fatalf("Symlink(%q, %q): %v", target, link, err)
	}
}

func prepareWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, SortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(%s): %v", SortieDir, err)
	}
	return ws
}

// swapForSymlink renames path aside so it keeps existing on disk, unreferenced,
// then plants a symbolic link to target in its place, modeling a workspace
// directory replaced after preparation.
func swapForSymlink(t *testing.T, path, target string) {
	t.Helper()
	aside := path + "-aside"
	if err := os.Rename(path, aside); err != nil {
		t.Fatalf("Rename(%q, %q): %v", path, aside, err)
	}
	mustSymlink(t, target, path)
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func TestVerifyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "link")
	mustSymlink(t, dir, link)

	tests := []struct {
		name      string
		path      string
		wantErr   error
		wantNoNil bool
	}{
		{name: "real directory", path: dir},
		{name: "empty path", path: "", wantNoNil: true},
		{name: "absent path", path: filepath.Join(dir, "missing"), wantErr: fs.ErrNotExist},
		{name: "regular file", path: file, wantErr: ErrNotDirectory},
		{name: "symbolic link to a directory", path: link, wantErr: ErrLink},
		{name: "symbolic link with a trailing separator", path: link + string(filepath.Separator), wantErr: ErrLink},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := VerifyDir(tt.path)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("VerifyDir(%q) = %v, want wrapping %v", tt.path, err, tt.wantErr)
				}
			case tt.wantNoNil:
				if err == nil {
					t.Errorf("VerifyDir(%q) = nil, want non-nil", tt.path)
				}
			default:
				if err != nil {
					t.Fatalf("VerifyDir(%q) = %v, want nil", tt.path, err)
				}
			}
		})
	}
}

func TestVerifyDir_EmptyPathIsNeverTheProcessDirectory(t *testing.T) {
	t.Parallel()

	cwdErr := VerifyDir(".")
	emptyErr := VerifyDir("")

	if cwdErr != nil {
		t.Fatalf("VerifyDir(\".\") = %v, want nil (the process directory is a real directory)", cwdErr)
	}
	if emptyErr == nil {
		t.Fatal("VerifyDir(\"\") = nil, want an error distinct from checking the process directory")
	}
}

func TestOpenSortieDir(t *testing.T) {
	t.Parallel()

	t.Run("workspace absent", func(t *testing.T) {
		t.Parallel()
		ws := filepath.Join(t.TempDir(), "missing")
		if _, err := OpenSortieDir(ws, false); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenSortieDir(absent workspace) = %v, want wrapping fs.ErrNotExist", err)
		}
	})

	t.Run("workspace is a symbolic link", func(t *testing.T) {
		t.Parallel()
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "workspace-link")
		mustSymlink(t, target, link)
		if _, err := OpenSortieDir(link, false); !errors.Is(err, ErrLink) {
			t.Errorf("OpenSortieDir(linked workspace) = %v, want wrapping ErrLink", err)
		}
	})

	t.Run(".sortie absent, create false", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if _, err := OpenSortieDir(ws, false); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenSortieDir(.sortie absent, create=false) = %v, want wrapping fs.ErrNotExist", err)
		}
	})

	t.Run(".sortie absent, create true", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		root, err := OpenSortieDir(ws, true)
		if err != nil {
			t.Fatalf("OpenSortieDir(.sortie absent, create=true) = %v, want nil", err)
		}
		defer root.Close() //nolint:errcheck // test cleanup

		info, statErr := os.Lstat(filepath.Join(ws, SortieDir))
		if statErr != nil || !info.IsDir() {
			t.Errorf("Lstat(%s) = %v, %v, want a created directory", SortieDir, info, statErr)
		}
	})

	t.Run(".sortie is a symbolic link", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		target := t.TempDir()
		mustSymlink(t, target, filepath.Join(ws, SortieDir))
		if _, err := OpenSortieDir(ws, false); !errors.Is(err, ErrLink) {
			t.Errorf("OpenSortieDir(.sortie is a symlink) = %v, want wrapping ErrLink", err)
		}
	})

	t.Run(".sortie is a regular file", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, SortieDir), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(.sortie): %v", err)
		}
		if _, err := OpenSortieDir(ws, false); !errors.Is(err, ErrNotDirectory) {
			t.Errorf("OpenSortieDir(.sortie is a regular file) = %v, want wrapping ErrNotDirectory", err)
		}
	})

	t.Run("workspace exists but .sortie already exists", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		root, err := OpenSortieDir(ws, true)
		if err != nil {
			t.Fatalf("OpenSortieDir(create=true, already exists) = %v, want nil", err)
		}
		root.Close() //nolint:errcheck // test cleanup
	})
}

func TestOpenSubdir(t *testing.T) {
	t.Parallel()

	openParent := func(t *testing.T, ws string) *os.Root {
		t.Helper()
		root, err := openDir(ws)
		if err != nil {
			t.Fatalf("openDir(%q): %v", ws, err)
		}
		t.Cleanup(func() { root.Close() }) //nolint:errcheck // test cleanup
		return root
	}

	t.Run("invalid names are rejected", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		parent := openParent(t, ws)

		for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
			if _, err := OpenSubdir(parent, name, true); err == nil {
				t.Errorf("OpenSubdir(%q) error = nil, want non-nil", name)
			}
		}
	})

	t.Run("create false, absent", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		parent := openParent(t, ws)
		if _, err := OpenSubdir(parent, "child", false); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenSubdir(absent, create=false) = %v, want wrapping fs.ErrNotExist", err)
		}
	})

	t.Run("create true, absent", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		parent := openParent(t, ws)
		sub, err := OpenSubdir(parent, "child", true)
		if err != nil {
			t.Fatalf("OpenSubdir(absent, create=true) = %v, want nil", err)
		}
		sub.Close() //nolint:errcheck // test cleanup
	})

	t.Run("create true, concurrent-create tolerates ErrExist", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		parent := openParent(t, ws)
		if err := parent.Mkdir("child", 0o750); err != nil {
			t.Fatalf("Mkdir(child): %v", err)
		}
		sub, err := OpenSubdir(parent, "child", true)
		if err != nil {
			t.Fatalf("OpenSubdir(create=true, already exists) = %v, want nil", err)
		}
		sub.Close() //nolint:errcheck // test cleanup
	})

	t.Run("name is a symbolic link", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		target := t.TempDir()
		mustSymlink(t, target, filepath.Join(ws, "child"))
		parent := openParent(t, ws)
		if _, err := OpenSubdir(parent, "child", false); !errors.Is(err, ErrLink) {
			t.Errorf("OpenSubdir(symlink) = %v, want wrapping ErrLink", err)
		}
	})

	t.Run("name is a regular file", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "child"), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(child): %v", err)
		}
		parent := openParent(t, ws)
		if _, err := OpenSubdir(parent, "child", false); !errors.Is(err, ErrNotDirectory) {
			t.Errorf("OpenSubdir(regular file) = %v, want wrapping ErrNotDirectory", err)
		}
	})
}

func TestOpenSortieFile(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		if err := os.WriteFile(filepath.Join(ws, SortieDir, "f.txt"), []byte("hello"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := OpenSortieFile(ws, "f.txt")
		if err != nil {
			t.Fatalf("OpenSortieFile: %v", err)
		}
		defer f.Close() //nolint:errcheck // test cleanup
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		if _, err := OpenSortieFile(ws, "f.txt"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenSortieFile(absent) = %v, want wrapping fs.ErrNotExist", err)
		}
	})

	t.Run("symbolic link at name", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		outside := filepath.Join(t.TempDir(), "elsewhere.txt")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatalf("WriteFile(outside): %v", err)
		}
		mustSymlink(t, outside, filepath.Join(ws, SortieDir, "f.txt"))
		if _, err := OpenSortieFile(ws, "f.txt"); !errors.Is(err, ErrLink) {
			t.Errorf("OpenSortieFile(symlink) = %v, want wrapping ErrLink", err)
		}
	})

	t.Run("directory at name", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		if err := os.Mkdir(filepath.Join(ws, SortieDir, "f.txt"), 0o750); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if _, err := OpenSortieFile(ws, "f.txt"); !errors.Is(err, ErrNotPlainFile) {
			t.Errorf("OpenSortieFile(directory) = %v, want wrapping ErrNotPlainFile", err)
		}
	})
}

func TestReadSortieFile(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		want := []byte("hello world")
		if err := os.WriteFile(filepath.Join(ws, SortieDir, "f.txt"), want, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := ReadSortieFile(ws, "f.txt", 4096)
		if err != nil {
			t.Fatalf("ReadSortieFile: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("ReadSortieFile() = %q, want %q", got, want)
		}
	})

	t.Run("oversized content", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		if err := os.WriteFile(filepath.Join(ws, SortieDir, "f.txt"), []byte("0123456789"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := ReadSortieFile(ws, "f.txt", 5); !errors.Is(err, ErrTooLarge) {
			t.Errorf("ReadSortieFile(oversized) = %v, want wrapping ErrTooLarge", err)
		}
	})

	t.Run("content exactly at the limit is accepted", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		want := []byte("01234")
		if err := os.WriteFile(filepath.Join(ws, SortieDir, "f.txt"), want, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := ReadSortieFile(ws, "f.txt", int64(len(want)))
		if err != nil {
			t.Fatalf("ReadSortieFile(at limit) = %v, want nil", err)
		}
		if string(got) != string(want) {
			t.Errorf("ReadSortieFile(at limit) = %q, want %q", got, want)
		}
	})

	t.Run("non-positive maxBytes is rejected", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		if _, err := ReadSortieFile(ws, "f.txt", 0); err == nil {
			t.Error("ReadSortieFile(maxBytes=0) error = nil, want non-nil")
		}
		if _, err := ReadSortieFile(ws, "f.txt", -1); err == nil {
			t.Error("ReadSortieFile(maxBytes=-1) error = nil, want non-nil")
		}
	})
}

func TestWriteSortieFile(t *testing.T) {
	t.Parallel()

	t.Run("round trip", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		want := []byte("payload")
		if err := WriteSortieFile(ws, "f.txt", want); err != nil {
			t.Fatalf("WriteSortieFile: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(ws, SortieDir, "f.txt"))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("written content = %q, want %q", got, want)
		}
	})

	t.Run("overwrites existing content atomically", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		if err := WriteSortieFile(ws, "f.txt", []byte("old")); err != nil {
			t.Fatalf("WriteSortieFile(old): %v", err)
		}
		if err := WriteSortieFile(ws, "f.txt", []byte("new")); err != nil {
			t.Fatalf("WriteSortieFile(new): %v", err)
		}
		got, err := os.ReadFile(filepath.Join(ws, SortieDir, "f.txt"))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "new" {
			t.Errorf("written content = %q, want %q", got, "new")
		}
		if names := listNames(t, filepath.Join(ws, SortieDir)); len(names) != 1 {
			t.Errorf(".sortie entries = %v, want exactly {f.txt}, no leftover temp file", names)
		}
	})

	t.Run("absent .sortie is an error", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		if err := WriteSortieFile(ws, "f.txt", []byte("x")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("WriteSortieFile(absent .sortie) = %v, want wrapping fs.ErrNotExist", err)
		}
	})
}

func TestRemoveSortieFile(t *testing.T) {
	t.Parallel()

	t.Run("removes an existing plain file", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		path := filepath.Join(ws, SortieDir, "f.txt")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := RemoveSortieFile(ws, "f.txt"); err != nil {
			t.Fatalf("RemoveSortieFile: %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Lstat(removed file) = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("absent wraps fs.ErrNotExist", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		if err := RemoveSortieFile(ws, "f.txt"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("RemoveSortieFile(absent) = %v, want wrapping fs.ErrNotExist", err)
		}
	})

	t.Run("symbolic link is refused and not removed", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		outside := t.TempDir()
		target := filepath.Join(outside, "elsewhere.txt")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(target): %v", err)
		}
		link := filepath.Join(ws, SortieDir, "f.txt")
		mustSymlink(t, target, link)

		if err := RemoveSortieFile(ws, "f.txt"); !errors.Is(err, ErrLink) {
			t.Errorf("RemoveSortieFile(symlink) = %v, want wrapping ErrLink", err)
		}
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("Lstat(link) = %v, want the link to remain in place", err)
		}
		if _, err := os.Lstat(target); err != nil {
			t.Errorf("Lstat(target) = %v, want the link target untouched", err)
		}
	})

	t.Run("directory is refused and not removed", func(t *testing.T) {
		t.Parallel()
		ws := prepareWorkspace(t)
		path := filepath.Join(ws, SortieDir, "f.txt")
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := RemoveSortieFile(ws, "f.txt"); !errors.Is(err, ErrNotPlainFile) {
			t.Errorf("RemoveSortieFile(directory) = %v, want wrapping ErrNotPlainFile", err)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("Lstat(directory) = %v, want it to remain in place", err)
		}
	})
}

// TestWriteSortieFile_WorkspaceReplacedBySymlinkRefused reproduces the
// redirect this package closes: a workspace directory prepared, then
// renamed aside and replaced by a symbolic link to an outside directory.
func TestWriteSortieFile_WorkspaceReplacedBySymlinkRefused(t *testing.T) {
	t.Parallel()

	ws := prepareWorkspace(t)
	target := t.TempDir()
	swapForSymlink(t, ws, target)

	if err := WriteSortieFile(ws, "f.txt", []byte("attacker-controlled")); !errors.Is(err, ErrLink) {
		t.Fatalf("WriteSortieFile(swapped workspace) = %v, want wrapping ErrLink", err)
	}
	if names := listNames(t, target); len(names) != 0 {
		t.Errorf("link target gained entries %v, want none", names)
	}
}

// TestLinkedSortieDirRefusesReadersWithoutTouchingTarget covers a linked
// .sortie holding planted content: every reader refuses it and none of the
// target's content is read back or altered.
func TestLinkedSortieDirRefusesReadersWithoutTouchingTarget(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	target := t.TempDir()
	planted := filepath.Join(target, "status")
	if err := os.WriteFile(planted, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("WriteFile(planted): %v", err)
	}
	mustSymlink(t, target, filepath.Join(ws, SortieDir))

	if _, err := OpenSortieFile(ws, "status"); !errors.Is(err, ErrLink) {
		t.Errorf("OpenSortieFile(linked .sortie) = %v, want wrapping ErrLink", err)
	}
	if _, err := ReadSortieFile(ws, "status", 4096); !errors.Is(err, ErrLink) {
		t.Errorf("ReadSortieFile(linked .sortie) = %v, want wrapping ErrLink", err)
	}
	if err := RemoveSortieFile(ws, "status"); !errors.Is(err, ErrLink) {
		t.Errorf("RemoveSortieFile(linked .sortie) = %v, want wrapping ErrLink", err)
	}

	got, err := os.ReadFile(planted)
	if err != nil {
		t.Fatalf("ReadFile(planted) after refusal: %v", err)
	}
	if string(got) != "blocked" {
		t.Errorf("planted content = %q, want unchanged %q", got, "blocked")
	}
}

// TestReplaceFile_LandsInHandleDirectoryAfterSortieDirRenamed proves a
// verified handle keeps acting on the directory it opened even after that
// directory is renamed away and a symbolic link takes its former name.
func TestReplaceFile_LandsInHandleDirectoryAfterSortieDirRenamed(t *testing.T) {
	t.Parallel()

	ws := prepareWorkspace(t)
	sortiePath := filepath.Join(ws, SortieDir)

	handle, err := OpenSortieDir(ws, false)
	if err != nil {
		t.Fatalf("OpenSortieDir: %v", err)
	}
	defer handle.Close() //nolint:errcheck // test cleanup

	renamedAside := sortiePath + "-real"
	if err := os.Rename(sortiePath, renamedAside); err != nil {
		t.Fatalf("Rename(.sortie aside): %v", err)
	}
	elsewhere := t.TempDir()
	mustSymlink(t, elsewhere, sortiePath)

	if err := ReplaceFile(handle, "f.txt", []byte("via-handle")); err != nil {
		t.Fatalf("ReplaceFile(through pre-opened handle): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(renamedAside, "f.txt"))
	if err != nil {
		t.Fatalf("ReadFile(renamed-aside directory): %v", err)
	}
	if string(got) != "via-handle" {
		t.Errorf("content = %q, want %q", got, "via-handle")
	}
	if names := listNames(t, elsewhere); len(names) != 0 {
		t.Errorf("symlink target gained entries %v, want none", names)
	}
}

func TestIsPlainMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{name: "plain file", mode: 0, want: true},
		{name: "irregular is allowed", mode: os.ModeIrregular, want: true},
		{name: "symbolic link", mode: os.ModeSymlink, want: false},
		{name: "directory", mode: os.ModeDir, want: false},
		{name: "named pipe", mode: os.ModeNamedPipe, want: false},
		{name: "socket", mode: os.ModeSocket, want: false},
		{name: "device", mode: os.ModeDevice, want: false},
		{name: "char device", mode: os.ModeCharDevice, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isPlainMode(tt.mode); got != tt.want {
				t.Errorf("isPlainMode(%v) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

// TestOpenDir_SeamDrivenRealDirectorySwapChanged proves open_dir binds check
// and use: a real directory swapped in at the workspace path, between the
// verifying Lstat and the open, is refused on Linux and macOS because the
// comparison catches the swap, and accepted on Windows because a
// non-reparse-point directory's identity there is not settled until the
// comparison runs (D1 accepts this platform split).
func TestOpenDir_SeamDrivenRealDirectorySwapChanged(t *testing.T) {
	ws := t.TempDir()
	swapped := t.TempDir()

	original := seam
	seam = func() {
		if err := os.RemoveAll(ws); err != nil {
			t.Fatalf("RemoveAll(original): %v", err)
		}
		if err := os.Rename(swapped, ws); err != nil {
			t.Fatalf("Rename(swapped into place): %v", err)
		}
	}
	t.Cleanup(func() { seam = original })

	_, err := openDir(ws)

	if runtime.GOOS == "windows" {
		if err != nil {
			t.Errorf("openDir(swapped directory) on windows = %v, want nil (D1)", err)
		}
		return
	}
	if !errors.Is(err, ErrChanged) {
		t.Errorf("openDir(swapped directory) = %v, want wrapping ErrChanged", err)
	}
}

// openFDCount reports the number of open file descriptors this process
// currently holds, or skips the test when the count is unavailable.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("fd accounting via /proc/self/fd is unavailable on this platform")
	}
	return len(entries)
}

// TestNoHandleLeak proves every exported function closes each intermediate
// handle it opened, on both its success and every error path: the process's
// open file descriptor count is the same before and after each call.
func TestNoHandleLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd accounting via /proc/self/fd is linux-specific")
	}

	ws := prepareWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws, SortieDir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	linkedWS := t.TempDir()
	mustSymlink(t, t.TempDir(), filepath.Join(linkedWS, SortieDir))

	calls := []struct {
		name string
		run  func() error
	}{
		{"VerifyDir success", func() error { return VerifyDir(ws) }},
		{"VerifyDir absent", func() error { return VerifyDir(filepath.Join(ws, "missing")) }},
		{"OpenSortieDir success", func() error {
			root, err := OpenSortieDir(ws, false)
			if err == nil {
				root.Close() //nolint:errcheck // test cleanup
			}
			return err
		}},
		{"OpenSortieDir linked", func() error {
			_, err := OpenSortieDir(linkedWS, false)
			return err
		}},
		{"OpenSortieFile success", func() error {
			f, err := OpenSortieFile(ws, "f.txt")
			if err == nil {
				f.Close() //nolint:errcheck // test cleanup
			}
			return err
		}},
		{"OpenSortieFile absent", func() error {
			_, err := OpenSortieFile(ws, "missing.txt")
			return err
		}},
		{"ReadSortieFile success", func() error {
			_, err := ReadSortieFile(ws, "f.txt", 4096)
			return err
		}},
		{"WriteSortieFile success", func() error {
			return WriteSortieFile(ws, "f.txt", []byte("y"))
		}},
		{"RemoveSortieFile absent", func() error {
			return RemoveSortieFile(ws, "missing.txt")
		}},
	}

	for _, c := range calls {
		before := openFDCount(t)
		_ = c.run()
		after := openFDCount(t)
		if after != before {
			t.Errorf("%s: open fd count = %d before, %d after, want equal", c.name, before, after)
		}
	}
}
