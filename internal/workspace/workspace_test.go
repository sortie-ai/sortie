package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSanitizeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
		wantOp  string
	}{
		{"simple alphanumeric with hyphen", "ABC-123", "ABC-123", false, ""},
		{"dots and underscores preserved", "my.task_1", "my.task_1", false, ""},
		{"slashes replaced", "PROJ/sub-task", "PROJ_sub-task", false, ""},
		{"spaces replaced", "My Task 42", "My_Task_42", false, ""},
		{"unicode replaced", "日本語-タスク", "___-___", false, ""},
		{"special chars replaced", "a@b#c$d%e", "a_b_c_d_e", false, ""},
		{"all replaced chars", "///", "___", false, ""},
		{"single valid char", "a", "a", false, ""},
		{"consecutive replacements not collapsed", "A//B", "A__B", false, ""},
		{"backslash replaced", `A\B`, "A_B", false, ""},
		{"null byte replaced", "A\x00B", "A_B", false, ""},
		{"empty input", "", "", true, "sanitize"},
		{"result is dot", ".", "", true, "sanitize"},
		{"result is dotdot", "..", "", true, "sanitize"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := SanitizeKey(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("SanitizeKey(%q) = %q, want error", tt.input, got)
				}
				var pe *PathError
				if !errors.As(err, &pe) {
					t.Fatalf("SanitizeKey(%q) error type = %T, want *PathError", tt.input, err)
				}
				if pe.Op != tt.wantOp {
					t.Errorf("PathError.Op = %q, want %q", pe.Op, tt.wantOp)
				}
				return
			}

			if err != nil {
				t.Fatalf("SanitizeKey(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("SanitizeKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestComputePath(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		res, err := ComputePath(root, "ABC-123")
		if err != nil {
			t.Fatalf("ComputePath(%q, %q) error: %v", root, "ABC-123", err)
		}
		if res.Key != "ABC-123" {
			t.Errorf("Key = %q, want %q", res.Key, "ABC-123")
		}
		wantPath := filepath.Join(root, "ABC-123")
		if res.Path != wantPath {
			t.Errorf("Path = %q, want %q", res.Path, wantPath)
		}
		if !filepath.IsAbs(res.Path) {
			t.Errorf("Path %q is not absolute", res.Path)
		}
	})

	t.Run("root with trailing slash", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		res, err := ComputePath(root+"/", "X-1")
		if err != nil {
			t.Fatalf("ComputePath(%q, %q) error: %v", root+"/", "X-1", err)
		}
		wantPath := filepath.Join(root, "X-1")
		if res.Path != wantPath {
			t.Errorf("Path = %q, want %q", res.Path, wantPath)
		}
	})

	t.Run("identifier needs sanitization", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		res, err := ComputePath(root, "A/B#C")
		if err != nil {
			t.Fatalf("ComputePath(%q, %q) error: %v", root, "A/B#C", err)
		}
		if res.Key != "A_B_C" {
			t.Errorf("Key = %q, want %q", res.Key, "A_B_C")
		}
		wantPath := filepath.Join(root, "A_B_C")
		if res.Path != wantPath {
			t.Errorf("Path = %q, want %q", res.Path, wantPath)
		}
	})

	t.Run("empty root", func(t *testing.T) {
		t.Parallel()

		_, err := ComputePath("", "ABC-123")
		if err == nil {
			t.Fatal("ComputePath(\"\", \"ABC-123\") error = nil, want error")
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
		if pe.Op != "resolve" {
			t.Errorf("PathError.Op = %q, want %q", pe.Op, "resolve")
		}
	})

	t.Run("empty identifier", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		_, err := ComputePath(root, "")
		if err == nil {
			t.Fatalf("ComputePath(%q, \"\") error = nil, want error", root)
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
		if pe.Op != "sanitize" {
			t.Errorf("PathError.Op = %q, want %q", pe.Op, "sanitize")
		}
	})

	t.Run("root does not exist yet", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		nonexistent := filepath.Join(base, "nonexistent", "sub")

		res, err := ComputePath(nonexistent, "X-1")
		if err != nil {
			t.Fatalf("ComputePath(%q, %q) error: %v", nonexistent, "X-1", err)
		}
		if res.Key != "X-1" {
			t.Errorf("Key = %q, want %q", res.Key, "X-1")
		}
		wantPath := filepath.Join(nonexistent, "X-1")
		if res.Path != wantPath {
			t.Errorf("Path = %q, want %q", res.Path, wantPath)
		}
	})

	// root="/" must not cause false rejection.
	t.Run("root is filesystem root", func(t *testing.T) {
		t.Parallel()

		res, err := ComputePath("/", "X-1")
		if err != nil {
			t.Fatalf("ComputePath(%q, %q) error: %v", "/", "X-1", err)
		}
		if res.Path != "/X-1" {
			t.Errorf("Path = %q, want %q", res.Path, "/X-1")
		}
		if res.Key != "X-1" {
			t.Errorf("Key = %q, want %q", res.Key, "X-1")
		}
	})

	// Workspace path must always stay inside the workspace root.
	t.Run("path is always under root", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		identifiers := []string{"ABC-123", "A/B#C", "日本語", "my.task_1"}
		for _, id := range identifiers {
			t.Run(id, func(t *testing.T) {
				t.Parallel()

				res, err := ComputePath(root, id)
				if err != nil {
					t.Fatalf("ComputePath(%q, %q) error: %v", root, id, err)
				}

				dir := filepath.Dir(res.Path)
				if dir != root {
					t.Errorf("ComputePath(%q, %q): Dir(Path) = %q, want %q", root, id, dir, root)
				}
				if filepath.Base(res.Path) != res.Key {
					t.Errorf("ComputePath(%q, %q): Base(Path) = %q, want Key %q", root, id, filepath.Base(res.Path), res.Key)
				}
			})
		}
	})

	t.Run("special name identifiers rejected", func(t *testing.T) {
		t.Parallel()

		for _, id := range []string{".", ".."} {
			t.Run(id, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()

				_, err := ComputePath(root, id)
				if err == nil {
					t.Fatalf("ComputePath(%q, %q) error = nil, want error", root, id)
				}
				var pe *PathError
				if !errors.As(err, &pe) {
					t.Fatalf("error type = %T, want *PathError", err)
				}
				if pe.Op != "sanitize" {
					t.Errorf("PathError.Op = %q, want %q", pe.Op, "sanitize")
				}
			})
		}
	})
}

func TestComputePath_SymlinkRoot(t *testing.T) {
	t.Parallel()

	realRoot := t.TempDir()
	symlinkDir := t.TempDir()
	symlinkPath := filepath.Join(symlinkDir, "symlink-root")

	if err := os.Symlink(realRoot, symlinkPath); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	res, err := ComputePath(symlinkPath, "X-1")
	if err != nil {
		t.Fatalf("ComputePath(%q, %q) error: %v", symlinkPath, "X-1", err)
	}

	// Path must be under the real root, not the symlink path
	if strings.HasPrefix(res.Path, symlinkPath) {
		t.Errorf("Path %q is under symlink path %q, should be under real root %q", res.Path, symlinkPath, realRoot)
	}

	wantPath := filepath.Join(realRoot, "X-1")
	if res.Path != wantPath {
		t.Errorf("Path = %q, want %q", res.Path, wantPath)
	}
}

func TestEnsure(t *testing.T) {
	t.Parallel()

	t.Run("create new workspace", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		res, err := Ensure(root, "ABC-123")
		if err != nil {
			t.Fatalf("Ensure(%q, %q) error: %v", root, "ABC-123", err)
		}
		if !res.CreatedNow {
			t.Error("CreatedNow = false, want true")
		}
		if res.Key != "ABC-123" {
			t.Errorf("Key = %q, want %q", res.Key, "ABC-123")
		}
		wantPath := filepath.Join(root, "ABC-123")
		if res.Path != wantPath {
			t.Errorf("Path = %q, want %q", res.Path, wantPath)
		}
		if !filepath.IsAbs(res.Path) {
			t.Errorf("Path %q is not absolute", res.Path)
		}

		info, err := os.Stat(res.Path)
		if err != nil {
			t.Fatalf("os.Stat(%q) error: %v", res.Path, err)
		}
		if !info.IsDir() {
			t.Errorf("path %q is not a directory", res.Path)
		}
	})

	t.Run("reuse existing directory", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsPath := filepath.Join(root, "EXIST-1")

		if err := os.Mkdir(wsPath, 0o750); err != nil {
			t.Fatalf("setup: os.Mkdir: %v", err)
		}

		res, err := Ensure(root, "EXIST-1")
		if err != nil {
			t.Fatalf("Ensure(%q, %q) error: %v", root, "EXIST-1", err)
		}
		if res.CreatedNow {
			t.Error("CreatedNow = true, want false for existing directory")
		}
		if res.Key != "EXIST-1" {
			t.Errorf("Key = %q, want %q", res.Key, "EXIST-1")
		}
		if res.Path != wsPath {
			t.Errorf("Path = %q, want %q", res.Path, wsPath)
		}

		info, err := os.Stat(wsPath)
		if err != nil {
			t.Fatalf("directory should still exist: %v", err)
		}
		if !info.IsDir() {
			t.Error("path should still be a directory")
		}
	})

	t.Run("non-directory at workspace path returns conflict error", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		filePath := filepath.Join(root, "CONFLICT-1")

		if err := os.WriteFile(filePath, []byte("occupied"), 0o644); err != nil {
			t.Fatalf("setup: os.WriteFile: %v", err)
		}

		_, err := Ensure(root, "CONFLICT-1")
		if err == nil {
			t.Fatalf("Ensure(%q, %q) error = nil, want error", root, "CONFLICT-1")
		}

		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
		if pe.Op != "conflict" {
			t.Errorf("PathError.Op = %q, want %q", pe.Op, "conflict")
		}

		// File must NOT be deleted — non-destructive behavior.
		info, statErr := os.Lstat(filePath)
		if statErr != nil {
			t.Fatalf("file should still exist after conflict error: %v", statErr)
		}
		if info.IsDir() {
			t.Error("file should not have been replaced with a directory")
		}
	})

	t.Run("root does not exist yet", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		deepRoot := filepath.Join(base, "deep", "nested")

		res, err := Ensure(deepRoot, "NEW-1")
		if err != nil {
			t.Fatalf("Ensure(%q, %q) error: %v", deepRoot, "NEW-1", err)
		}
		if !res.CreatedNow {
			t.Error("CreatedNow = false, want true")
		}

		info, err := os.Stat(deepRoot)
		if err != nil {
			t.Fatalf("root directory should exist: %v", err)
		}
		if !info.IsDir() {
			t.Error("root path should be a directory")
		}

		info, err = os.Stat(res.Path)
		if err != nil {
			t.Fatalf("workspace directory should exist: %v", err)
		}
		if !info.IsDir() {
			t.Error("workspace path should be a directory")
		}
	})

	t.Run("symlink at workspace path rejected", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		target := t.TempDir()
		symlinkPath := filepath.Join(root, "SYM-1")

		if err := os.Symlink(target, symlinkPath); err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}

		_, err := Ensure(root, "SYM-1")
		if err == nil {
			t.Fatalf("Ensure(%q, %q) error = nil, want error", root, "SYM-1")
		}

		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
		if pe.Op != "containment" {
			t.Errorf("PathError.Op = %q, want %q", pe.Op, "containment")
		}
	})

	t.Run("empty identifier", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		_, err := Ensure(root, "")
		if err == nil {
			t.Fatalf("Ensure(%q, \"\") error = nil, want error", root)
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
		if pe.Op != "sanitize" {
			t.Errorf("PathError.Op = %q, want %q", pe.Op, "sanitize")
		}
	})

	t.Run("empty root", func(t *testing.T) {
		t.Parallel()

		_, err := Ensure("", "X-1")
		if err == nil {
			t.Fatal("Ensure(\"\", \"X-1\") error = nil, want error")
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
		if pe.Op != "resolve" {
			t.Errorf("PathError.Op = %q, want %q", pe.Op, "resolve")
		}
	})

	t.Run("reuse preserves directory contents", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsPath := filepath.Join(root, "KEEP-1")

		if err := os.Mkdir(wsPath, 0o750); err != nil {
			t.Fatalf("setup: os.Mkdir: %v", err)
		}
		markerPath := filepath.Join(wsPath, "marker.txt")
		if err := os.WriteFile(markerPath, []byte("keep me"), 0o644); err != nil {
			t.Fatalf("setup: os.WriteFile: %v", err)
		}

		res, err := Ensure(root, "KEEP-1")
		if err != nil {
			t.Fatalf("Ensure(%q, %q) error: %v", root, "KEEP-1", err)
		}
		if res.CreatedNow {
			t.Error("CreatedNow = true, want false for existing directory")
		}

		data, err := os.ReadFile(markerPath)
		if err != nil {
			t.Fatalf("marker file should still exist: %v", err)
		}
		if string(data) != "keep me" {
			t.Errorf("marker file content = %q, want %q", string(data), "keep me")
		}
	})

	t.Run("concurrent Ensure yields exactly one CreatedNow", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		const goroutines = 10
		results := make([]EnsureResult, goroutines)
		errs := make([]error, goroutines)

		var ready sync.WaitGroup
		ready.Add(goroutines)
		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		done.Add(goroutines)

		for i := range goroutines {
			go func(idx int) {
				defer done.Done()
				ready.Done()
				start.Wait() // all goroutines launch together
				results[idx], errs[idx] = Ensure(root, "RACE-1")
			}(i)
		}

		ready.Wait() // wait for all goroutines to be ready
		start.Done() // release them simultaneously
		done.Wait()  // wait for all to finish

		createdCount := 0
		for i := range goroutines {
			if errs[i] != nil {
				t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
				continue
			}
			if results[i].CreatedNow {
				createdCount++
			}
		}

		if createdCount != 1 {
			t.Errorf("CreatedNow=true count = %d, want exactly 1", createdCount)
		}
	})
}

func TestPathError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  PathError
		want string
	}{
		{
			"op with root and identifier",
			PathError{Op: "containment", Root: "/tmp/root", Identifier: "ABC-123", Err: errors.New("escaped")},
			`workspace containment: root="/tmp/root" identifier="ABC-123": escaped`,
		},
		{
			"op with identifier only",
			PathError{Op: "sanitize", Identifier: "bad-id", Err: errors.New("empty")},
			`workspace sanitize: identifier="bad-id": empty`,
		},
		{
			"op with root only",
			PathError{Op: "resolve", Root: "/tmp/root", Err: errors.New("bad")},
			`workspace resolve: root="/tmp/root": bad`,
		},
		{
			"op only",
			PathError{Op: "resolve", Err: errors.New("missing")},
			`workspace resolve: missing`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.err.Error()
			if got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPathError_Unwrap(t *testing.T) {
	t.Parallel()

	inner := errors.New("inner")
	pe := &PathError{Op: "test", Err: inner}

	if !errors.Is(pe, inner) {
		t.Error("errors.Is should find the wrapped error")
	}
}

func TestListWorkspaceKeys(t *testing.T) {
	t.Parallel()

	t.Run("directories returned files skipped", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		// Create two directories and one regular file.
		for _, dir := range []string{"PROJ-1", "PROJ-2"} {
			if err := os.Mkdir(filepath.Join(root, dir), 0o750); err != nil {
				t.Fatalf("setup mkdir %s: %v", dir, err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "not-a-dir.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("setup writefile: %v", err)
		}

		got, err := ListWorkspaceKeys(root)
		if err != nil {
			t.Fatalf("ListWorkspaceKeys() error: %v", err)
		}

		want := map[string]bool{"PROJ-1": true, "PROJ-2": true}
		if len(got) != len(want) {
			t.Fatalf("ListWorkspaceKeys() returned %d keys, want %d: %v", len(got), len(want), got)
		}
		for _, k := range got {
			if !want[k] {
				t.Errorf("ListWorkspaceKeys() unexpected key %q", k)
			}
		}
	})

	t.Run("symlinks skipped", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(root, "link-dir")); err != nil {
			t.Skip("symlinks not supported:", err)
		}
		if err := os.Mkdir(filepath.Join(root, "real-dir"), 0o750); err != nil {
			t.Fatalf("setup: %v", err)
		}

		got, err := ListWorkspaceKeys(root)
		if err != nil {
			t.Fatalf("ListWorkspaceKeys() error: %v", err)
		}

		if len(got) != 1 || got[0] != "real-dir" {
			t.Errorf("ListWorkspaceKeys() = %v, want [real-dir]", got)
		}
	})

	t.Run("nonexistent root returns empty", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "does-not-exist")

		got, err := ListWorkspaceKeys(root)
		if err != nil {
			t.Fatalf("ListWorkspaceKeys() error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("ListWorkspaceKeys() = %v, want empty slice", got)
		}
	})

	t.Run("empty directory returns empty", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		got, err := ListWorkspaceKeys(root)
		if err != nil {
			t.Fatalf("ListWorkspaceKeys() error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("ListWorkspaceKeys() = %v, want empty slice", got)
		}
	})

	t.Run("empty root rejected", func(t *testing.T) {
		t.Parallel()

		got, err := ListWorkspaceKeys("")
		if err == nil {
			t.Fatalf("ListWorkspaceKeys(\"\") error = nil, want non-nil, keys = %v", got)
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
		if pe.Op != "resolve" {
			t.Errorf("PathError.Op = %q, want %q", pe.Op, "resolve")
		}
	})
}
