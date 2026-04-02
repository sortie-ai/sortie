//go:build unix

package workspace

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Helpers ---

// captureLogger returns an slog.Logger that writes to the provided buffer.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// writeStatusFile creates <wsPath>/.sortie/status with the given content.
func writeStatusFile(t *testing.T, wsPath string, content []byte) {
	t.Helper()
	dir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), content, 0o644); err != nil {
		t.Fatalf("WriteFile(status): %v", err)
	}
}

// makeDotSortieDir creates an empty .sortie directory without a status file.
func makeDotSortieDir(t *testing.T, wsPath string) {
	t.Helper()
	dir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}
}

// --- TestReadStatusFile ---

func TestReadStatusFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, wsPath string)
		want    StatusSignal
		wantLog string // substring expected in log output (empty = no log required)
	}{
		{
			name:  "file absent no dot-sortie dir",
			setup: func(_ *testing.T, _ string) {},
			want:  StatusNone,
		},
		{
			name:  "file absent dot-sortie dir exists",
			setup: makeDotSortieDir,
			want:  StatusNone,
		},
		{
			name: "blocked with newline",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("blocked\n"))
			},
			want: StatusBlocked,
		},
		{
			name: "needs-human-review with newline",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("needs-human-review\n"))
			},
			want: StatusNeedsHumanReview,
		},
		{
			name: "blocked no trailing newline",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("blocked"))
			},
			want: StatusBlocked,
		},
		{
			name: "blocked with surrounding whitespace",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("  blocked  \n"))
			},
			want: StatusBlocked,
		},
		{
			name: "wrong case Blocked",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("Blocked\n"))
			},
			want:    StatusNone,
			wantLog: "unrecognized",
		},
		{
			name: "unknown value",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("unknown-value\n"))
			},
			want:    StatusNone,
			wantLog: "unrecognized",
		},
		{
			name: "empty file",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte(""))
			},
			want: StatusNone,
		},
		{
			name: "extra lines after blocked",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("blocked\nextra line\nmore"))
			},
			want: StatusBlocked,
		},
		{
			name: "blank first line blocked on second",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("\nblocked"))
			},
			// Empty first line after TrimSpace → StatusNone silently (no warn).
			want:    StatusNone,
			wantLog: "",
		},
		{
			name: "CRLF line ending blocked",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("blocked\r\n"))
			},
			want: StatusBlocked,
		},
		{
			name: "CRLF with blank first line",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte("\r\nblocked"))
			},
			// \r before \n; TrimSpace removes \r → empty token → StatusNone silently.
			want:    StatusNone,
			wantLog: "",
		},
		{
			name: "file larger than 1 KiB starting with blocked",
			setup: func(t *testing.T, wsPath string) {
				// Write 2 KiB: "blocked\n" + padding.
				content := []byte("blocked\n")
				padding := bytes.Repeat([]byte("x"), 2048)
				writeStatusFile(t, wsPath, append(content, padding...))
			},
			want: StatusBlocked,
		},
		{
			name: "binary non-UTF-8 content",
			setup: func(t *testing.T, wsPath string) {
				writeStatusFile(t, wsPath, []byte{0xFF, 0xFE, 0x00})
			},
			want:    StatusNone,
			wantLog: "unrecognized",
		},
		{
			name: "permission denied on file",
			setup: func(t *testing.T, wsPath string) {
				if os.Getuid() == 0 {
					t.Skip("skipping: test requires non-root to enforce file permissions")
				}
				writeStatusFile(t, wsPath, []byte("blocked\n"))
				if err := os.Chmod(filepath.Join(wsPath, ".sortie", "status"), 0o000); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				t.Cleanup(func() {
					_ = os.Chmod(filepath.Join(wsPath, ".sortie", "status"), 0o644)
				})
			},
			want:    StatusNone,
			wantLog: "failed to open",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wsPath := t.TempDir()
			tt.setup(t, wsPath)

			var logBuf bytes.Buffer
			logger := captureLogger(&logBuf)

			got := ReadStatusFile(wsPath, logger)

			if got != tt.want {
				t.Errorf("ReadStatusFile() = %q, want %q", got, tt.want)
			}
			if tt.wantLog != "" && !strings.Contains(logBuf.String(), tt.wantLog) {
				t.Errorf("log output = %q, want to contain %q", logBuf.String(), tt.wantLog)
			}
		})
	}
}

func TestReadStatusFile_NilLogger(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	// Should not panic with nil logger.
	got := ReadStatusFile(wsPath, nil)
	if got != StatusNone {
		t.Errorf("ReadStatusFile(nil logger) = %q, want %q", got, StatusNone)
	}
}

func TestReadStatusFile_SymlinkAtDotSortie(t *testing.T) {
	t.Parallel()

	// Create a real workspace and a directory with a status file.
	wsPath := t.TempDir()
	target := t.TempDir()

	// Put a valid status file inside the target directory.
	if err := os.WriteFile(filepath.Join(target, "status"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// .sortie is a symlink pointing to a directory outside the workspace.
	if err := os.Symlink(target, filepath.Join(wsPath, ".sortie")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	var logBuf bytes.Buffer
	got := ReadStatusFile(wsPath, captureLogger(&logBuf))

	if got != StatusNone {
		t.Errorf("ReadStatusFile() = %q, want %q (symlink escape)", got, StatusNone)
	}
	if !strings.Contains(logBuf.String(), "symlink detected at .sortie directory") {
		t.Errorf("log = %q, want to contain %q", logBuf.String(), "symlink detected at .sortie directory")
	}
}

func TestReadStatusFile_SymlinkAtStatusFile(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	makeDotSortieDir(t, wsPath)

	// Create a file outside the workspace.
	outsideFile := filepath.Join(t.TempDir(), "outside_status")
	if err := os.WriteFile(outsideFile, []byte("blocked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// status is a symlink pointing to a file outside the workspace.
	statusPath := filepath.Join(wsPath, ".sortie", "status")
	if err := os.Symlink(outsideFile, statusPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	var logBuf bytes.Buffer
	got := ReadStatusFile(wsPath, captureLogger(&logBuf))

	if got != StatusNone {
		t.Errorf("ReadStatusFile() = %q, want %q (status symlink escape)", got, StatusNone)
	}
	if !strings.Contains(logBuf.String(), "symlink detected at .sortie/status") {
		t.Errorf("log = %q, want to contain %q", logBuf.String(), "symlink detected at .sortie/status")
	}
}

func TestReadStatusFile_SymlinkToFileInsideWorkspace(t *testing.T) {
	t.Parallel()

	// Regression: a status symlink pointing inside the workspace was previously
	// accepted; Lstat rejection must catch it regardless of target location.
	wsPath := t.TempDir()
	makeDotSortieDir(t, wsPath)

	// Place the real file inside the workspace.
	realFile := filepath.Join(wsPath, "real_status")
	if err := os.WriteFile(realFile, []byte("blocked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// status is a symlink pointing to a file inside the workspace.
	statusPath := filepath.Join(wsPath, ".sortie", "status")
	if err := os.Symlink(realFile, statusPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	var logBuf bytes.Buffer
	got := ReadStatusFile(wsPath, captureLogger(&logBuf))
	if got != StatusNone {
		t.Errorf("ReadStatusFile() = %q, want %q (intra-workspace symlink must be rejected)", got, StatusNone)
	}
	if !strings.Contains(logBuf.String(), "symlink detected at .sortie/status") {
		t.Errorf("log = %q, want to contain %q", logBuf.String(), "symlink detected at .sortie/status")
	}
}

func TestReadStatusFile_DotSortieLstatErrorLogsWarn(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("skipping: test requires non-root to enforce directory permissions")
	}

	wsPath := t.TempDir()
	writeStatusFile(t, wsPath, []byte("blocked\n"))
	dotSortiePath := filepath.Join(wsPath, ".sortie")
	// Removing all permissions from .sortie prevents os.Lstat from traversing
	// into it, causing isSymlink(".sortie/status") to fail with EACCES.
	if err := os.Chmod(dotSortiePath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dotSortiePath, 0o755)
	})

	var logBuf bytes.Buffer
	got := ReadStatusFile(wsPath, captureLogger(&logBuf))

	if got != StatusNone {
		t.Errorf("ReadStatusFile() = %q, want %q", got, StatusNone)
	}
	log := logBuf.String()
	if !strings.Contains(log, "failed to stat") {
		t.Errorf("log = %q, want to contain %q", log, "failed to stat")
	}
	if !strings.Contains(log, "level=WARN") {
		t.Errorf("log = %q, want level=WARN", log)
	}
}

// --- TestIsRecognized ---

func TestStatusSignal_IsRecognized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		signal StatusSignal
		want   bool
	}{
		{StatusNone, false},
		{StatusBlocked, true},
		{StatusNeedsHumanReview, true},
		{StatusSignal("anything-else"), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.signal), func(t *testing.T) {
			t.Parallel()
			got := tt.signal.IsRecognized()
			if got != tt.want {
				t.Errorf("StatusSignal(%q).IsRecognized() = %v, want %v", tt.signal, got, tt.want)
			}
		})
	}
}

// --- TestCleanupStatusFile ---

func TestCleanupStatusFile(t *testing.T) {
	t.Parallel()

	t.Run("no dot-sortie dir no panic", func(t *testing.T) {
		t.Parallel()
		wsPath := t.TempDir()
		// Must not panic.
		CleanupStatusFile(wsPath, slog.Default())
	})

	t.Run("file exists is removed", func(t *testing.T) {
		t.Parallel()
		wsPath := t.TempDir()
		writeStatusFile(t, wsPath, []byte("blocked\n"))

		statusPath := filepath.Join(wsPath, ".sortie", "status")
		if _, err := os.Stat(statusPath); err != nil {
			t.Fatalf("pre-condition: status file should exist: %v", err)
		}

		CleanupStatusFile(wsPath, slog.Default())

		if _, err := os.Stat(statusPath); !os.IsNotExist(err) {
			t.Errorf("status file still exists after CleanupStatusFile")
		}
	})

	t.Run("file already absent no panic", func(t *testing.T) {
		t.Parallel()
		wsPath := t.TempDir()
		makeDotSortieDir(t, wsPath)
		// status file was never created.
		CleanupStatusFile(wsPath, slog.Default())
	})

	t.Run("dot-sortie is symlink skips cleanup", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		target := t.TempDir()

		// Place a status file in the target directory.
		if err := os.WriteFile(filepath.Join(target, "status"), []byte("blocked\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// .sortie is a symlink to an outside directory.
		if err := os.Symlink(target, filepath.Join(wsPath, ".sortie")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		var logBuf bytes.Buffer
		CleanupStatusFile(wsPath, captureLogger(&logBuf))

		// The outside file must NOT have been removed.
		outsideStatus := filepath.Join(target, "status")
		if _, err := os.Stat(outsideStatus); err != nil {
			t.Errorf("outside status file was removed, want preserved: %v", err)
		}
		if !strings.Contains(logBuf.String(), "symlink detected at .sortie directory") {
			t.Errorf("log = %q, want to contain %q", logBuf.String(), "symlink detected at .sortie directory")
		}
	})

	t.Run("status is symlink outside workspace skips cleanup", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		makeDotSortieDir(t, wsPath)

		outsideFile := filepath.Join(t.TempDir(), "status")
		if err := os.WriteFile(outsideFile, []byte("blocked\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// status points to a file outside the workspace.
		statusLink := filepath.Join(wsPath, ".sortie", "status")
		if err := os.Symlink(outsideFile, statusLink); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		var logBuf bytes.Buffer
		CleanupStatusFile(wsPath, captureLogger(&logBuf))

		// Outside file not removed.
		if _, err := os.Stat(outsideFile); err != nil {
			t.Errorf("outside file was removed, want preserved: %v", err)
		}
		if !strings.Contains(logBuf.String(), "symlink detected at .sortie/status") {
			t.Errorf("log = %q, want to contain %q", logBuf.String(), "symlink detected at .sortie/status")
		}
	})

	t.Run("nil logger no panic", func(t *testing.T) {
		t.Parallel()
		wsPath := t.TempDir()
		CleanupStatusFile(wsPath, nil)
	})

	t.Run("status is symlink inside workspace skips cleanup", func(t *testing.T) {
		t.Parallel()

		// Regression: a symlink at status that resolves inside the workspace
		// must not be followed during cleanup — the symlink target must be preserved.
		wsPath := t.TempDir()
		makeDotSortieDir(t, wsPath)

		// Real file inside the workspace.
		realFile := filepath.Join(wsPath, "real_status")
		if err := os.WriteFile(realFile, []byte("blocked\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// status is a symlink pointing inside the workspace.
		statusLink := filepath.Join(wsPath, ".sortie", "status")
		if err := os.Symlink(realFile, statusLink); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		var logBuf bytes.Buffer
		CleanupStatusFile(wsPath, captureLogger(&logBuf))

		// The symlink target inside the workspace must NOT have been deleted.
		if _, err := os.Stat(realFile); err != nil {
			t.Errorf("intra-workspace symlink target was removed, want preserved: %v", err)
		}
		if !strings.Contains(logBuf.String(), "symlink detected at .sortie/status") {
			t.Errorf("log = %q, want to contain %q", logBuf.String(), "symlink detected at .sortie/status")
		}
	})

	t.Run("dot-sortie is symlink inside workspace skips cleanup", func(t *testing.T) {
		t.Parallel()

		// Regression: a .sortie symlink pointing inside the workspace must also
		// be rejected — nothing in the target directory may be removed.
		wsPath := t.TempDir()

		// Real directory with a status file, both inside the workspace.
		realDir := filepath.Join(wsPath, "real_sortie")
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		realStatus := filepath.Join(realDir, "status")
		if err := os.WriteFile(realStatus, []byte("blocked\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// .sortie is a symlink pointing to the real directory inside the workspace.
		if err := os.Symlink(realDir, filepath.Join(wsPath, ".sortie")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		var logBuf bytes.Buffer
		CleanupStatusFile(wsPath, captureLogger(&logBuf))

		// The file inside the target directory must NOT have been removed.
		if _, err := os.Stat(realStatus); err != nil {
			t.Errorf("target status file was removed, want preserved: %v", err)
		}
		if !strings.Contains(logBuf.String(), "symlink detected at .sortie directory") {
			t.Errorf("log = %q, want to contain %q", logBuf.String(), "symlink detected at .sortie directory")
		}
	})

	t.Run("remove fails logs warn", func(t *testing.T) {
		t.Parallel()

		if os.Getuid() == 0 {
			t.Skip("skipping: test requires non-root to enforce directory permissions")
		}

		wsPath := t.TempDir()
		writeStatusFile(t, wsPath, []byte("blocked\n"))
		dotSortiePath := filepath.Join(wsPath, ".sortie")
		// Remove write permission from .sortie so os.Remove on the status
		// file fails with EACCES; execute is retained so Lstat can traverse.
		if err := os.Chmod(dotSortiePath, 0o555); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(dotSortiePath, 0o755)
		})

		var logBuf bytes.Buffer
		CleanupStatusFile(wsPath, captureLogger(&logBuf))

		log := logBuf.String()
		if !strings.Contains(log, "status file cleanup failed") {
			t.Errorf("log = %q, want to contain %q", log, "status file cleanup failed")
		}
		if !strings.Contains(log, "level=WARN") {
			t.Errorf("log = %q, want level=WARN", log)
		}
	})
}
