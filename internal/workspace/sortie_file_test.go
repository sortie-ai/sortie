package workspace

import (
	"os"
	"path/filepath"
	"runtime"
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

func listSortieDir(t *testing.T, workspacePath string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(workspacePath, sortieDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", sortieDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteSortieFile_RoundTripAndReplace(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}

	if err := WriteSortieFile(ws, "hello.txt", []byte("first")); err != nil {
		t.Fatalf("WriteSortieFile(first) = %v, want nil", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, sortieDir, "hello.txt"))
	if err != nil {
		t.Fatalf("ReadFile after first write: %v", err)
	}
	if string(got) != "first" {
		t.Errorf("content after first write = %q, want %q", got, "first")
	}

	if err := WriteSortieFile(ws, "hello.txt", []byte("second")); err != nil {
		t.Fatalf("WriteSortieFile(second) = %v, want nil", err)
	}
	got, err = os.ReadFile(filepath.Join(ws, sortieDir, "hello.txt"))
	if err != nil {
		t.Fatalf("ReadFile after second write: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content after second write (replace) = %q, want %q", got, "second")
	}
}

func TestWriteSortieFile_ModeIsOwnerReadWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file mode bits are not meaningful on Windows")
	}
	t.Parallel()

	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}
	if err := WriteSortieFile(ws, "mode.txt", []byte("x")); err != nil {
		t.Fatalf("WriteSortieFile: %v", err)
	}
	fi, err := os.Stat(filepath.Join(ws, sortieDir, "mode.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("WriteSortieFile file mode = %o, want %o", got, 0o600)
	}
}

func TestWriteSortieFile_InvalidNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
	}{
		{name: ""},
		{name: "."},
		{name: ".."},
		{name: "a/b"},
		{name: `a\b`},
	}

	for _, tt := range tests {
		t.Run("name_"+tt.name, func(t *testing.T) {
			t.Parallel()

			ws := t.TempDir()
			if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
				t.Fatalf("Mkdir(.sortie): %v", err)
			}

			before := listSortieDir(t, ws)
			err := WriteSortieFile(ws, tt.name, []byte("x"))
			if err == nil {
				t.Fatalf("WriteSortieFile(name=%q) = nil, want error", tt.name)
			}
			after := listSortieDir(t, ws)
			if len(after) != len(before) {
				t.Errorf("WriteSortieFile(name=%q) changed .sortie entries: before=%v, after=%v", tt.name, before, after)
			}
		})
	}
}

func TestWriteSortieFile_SortieDirAbsent(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	if err := WriteSortieFile(ws, "f.txt", []byte("x")); err == nil {
		t.Fatal("WriteSortieFile(.sortie absent) = nil, want error")
	}
	if _, err := os.Stat(filepath.Join(ws, sortieDir)); !os.IsNotExist(err) {
		t.Errorf(".sortie exists after a failed write, want it to remain absent (stat err = %v)", err)
	}
}

func TestWriteSortieFile_SortieIsRegularFile(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	sortiePath := filepath.Join(ws, sortieDir)
	if err := os.WriteFile(sortiePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(.sortie as regular file): %v", err)
	}

	if err := WriteSortieFile(ws, "f.txt", []byte("x")); err == nil {
		t.Fatal("WriteSortieFile(.sortie is a regular file) = nil, want error")
	}
	data, err := os.ReadFile(sortiePath)
	if err != nil {
		t.Fatalf("ReadFile(.sortie): %v", err)
	}
	if string(data) != "not a directory" {
		t.Errorf(".sortie content changed to %q, want unchanged", data)
	}
}

func TestWriteSortieFile_SortieIsSymlink(t *testing.T) {
	t.Parallel()

	t.Run("target inside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		target := filepath.Join(ws, "real-sortie")
		if err := os.Mkdir(target, 0o750); err != nil {
			t.Fatalf("Mkdir(target): %v", err)
		}
		mustSymlink(t, target, filepath.Join(ws, sortieDir))

		if err := WriteSortieFile(ws, "f.txt", []byte("x")); err == nil {
			t.Fatal("WriteSortieFile(.sortie is a symlink, target inside workspace) = nil, want error")
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatalf("ReadDir(target): %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("symlink target directory gained entries: %v, want none", entries)
		}
	})

	t.Run("target outside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		target := t.TempDir()
		mustSymlink(t, target, filepath.Join(ws, sortieDir))

		if err := WriteSortieFile(ws, "f.txt", []byte("x")); err == nil {
			t.Fatal("WriteSortieFile(.sortie is a symlink, target outside workspace) = nil, want error")
		}
		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatalf("ReadDir(target): %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("symlink target directory (outside workspace) gained entries: %v, want none", entries)
		}
	})
}

func TestWriteSortieFile_SymlinkAtNameReplacedNotFollowed(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}

	sibling := filepath.Join(ws, sortieDir, "sibling.txt")
	if err := os.WriteFile(sibling, []byte("sibling-content"), 0o600); err != nil {
		t.Fatalf("WriteFile(sibling): %v", err)
	}

	outsideDir := t.TempDir()
	targetPath := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(targetPath, []byte("outside-content"), 0o600); err != nil {
		t.Fatalf("WriteFile(target): %v", err)
	}

	linkPath := filepath.Join(ws, sortieDir, "planted.txt")
	mustSymlink(t, targetPath, linkPath)

	if err := WriteSortieFile(ws, "planted.txt", []byte("written-content")); err != nil {
		t.Fatalf("WriteSortieFile(symlink at name) = %v, want nil (link replaced, not followed)", err)
	}

	targetData, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile(target): %v", err)
	}
	if string(targetData) != "outside-content" {
		t.Errorf("symlink target content = %q, want unchanged %q", targetData, "outside-content")
	}

	fi, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat(destination): %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("destination is still a symlink, want a regular file (link replaced)")
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("destination mode = %v, want a regular file", fi.Mode())
	}
	destData, err := os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("ReadFile(destination): %v", err)
	}
	if string(destData) != "written-content" {
		t.Errorf("destination content = %q, want %q", destData, "written-content")
	}

	siblingData, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatalf("ReadFile(sibling): %v", err)
	}
	if string(siblingData) != "sibling-content" {
		t.Errorf("sibling content = %q, want unchanged %q", siblingData, "sibling-content")
	}
}

func TestWriteSortieFile_NonEmptyDirectoryAtName(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	blockDir := filepath.Join(ws, sortieDir, "blocked")
	if err := os.MkdirAll(blockDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(blocked): %v", err)
	}
	innerFile := filepath.Join(blockDir, "inner.txt")
	if err := os.WriteFile(innerFile, []byte("inner"), 0o600); err != nil {
		t.Fatalf("WriteFile(inner): %v", err)
	}

	if err := WriteSortieFile(ws, "blocked", []byte("x")); err == nil {
		t.Fatal("WriteSortieFile(non-empty directory at name) = nil, want error")
	}

	data, err := os.ReadFile(innerFile)
	if err != nil {
		t.Fatalf("ReadFile(inner) after failed write: %v", err)
	}
	if string(data) != "inner" {
		t.Errorf("inner file content = %q, want unchanged %q", data, "inner")
	}
}

func TestWriteSortieFile_NoStrayEntries(t *testing.T) {
	t.Parallel()

	t.Run("on success", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
			t.Fatalf("Mkdir(.sortie): %v", err)
		}
		before := listSortieDir(t, ws)

		if err := WriteSortieFile(ws, "new.txt", []byte("x")); err != nil {
			t.Fatalf("WriteSortieFile: %v", err)
		}

		after := listSortieDir(t, ws)
		wantExtra := map[string]bool{"new.txt": true}
		gotExtra := diffEntries(before, after)
		if len(gotExtra) != len(wantExtra) || !gotExtra["new.txt"] {
			t.Errorf("new .sortie entries after success = %v, want exactly %v", gotExtra, wantExtra)
		}
	})

	t.Run("on failure", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		if err := os.Mkdir(filepath.Join(ws, sortieDir), 0o750); err != nil {
			t.Fatalf("Mkdir(.sortie): %v", err)
		}
		before := listSortieDir(t, ws)

		if err := WriteSortieFile(ws, "..", []byte("x")); err == nil {
			t.Fatal("WriteSortieFile(name=\"..\") = nil, want error")
		}

		after := listSortieDir(t, ws)
		gotExtra := diffEntries(before, after)
		if len(gotExtra) != 0 {
			t.Errorf("new .sortie entries after a failed write = %v, want none", gotExtra)
		}
	})
}

func diffEntries(before, after []string) map[string]bool {
	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	extra := make(map[string]bool)
	for _, n := range after {
		if !beforeSet[n] {
			extra[n] = true
		}
	}
	return extra
}
