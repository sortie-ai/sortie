package workspace

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// StatusSignal represents the parsed A2O status file value. The
// orchestrator reads <workspace>/.sortie/status after each completed
// turn to detect agent-reported blockage. Unrecognized or absent
// values are represented by [StatusNone].
type StatusSignal string

const (
	// StatusNone indicates no status file was found, the file was
	// unreadable, empty after trimming, or contained an unrecognized
	// value. This is the default; it does not trigger a soft stop.
	StatusNone StatusSignal = ""

	// StatusBlocked indicates the agent self-assessed further work as
	// futile and wrote "blocked" to .sortie/status.
	StatusBlocked StatusSignal = "blocked"

	// StatusNeedsHumanReview indicates the agent determined that human
	// review is required before further automated work can proceed.
	StatusNeedsHumanReview StatusSignal = "needs-human-review"
)

// IsRecognized reports whether s is a recognized A2O status signal
// that triggers a soft stop.
func (s StatusSignal) IsRecognized() bool {
	return s == StatusBlocked || s == StatusNeedsHumanReview
}

// statusFileMaxBytes is the maximum number of bytes read from the
// status file. Legitimate tokens are under 30 bytes; 1 KiB provides
// headroom while bounding memory usage.
const statusFileMaxBytes = 1024

// isSymlink reports whether the file at path is a symbolic link.
// The caller must handle the returned error; non-existence is
// reported as a non-nil error wrapping [fs.ErrNotExist].
func isSymlink(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}

// rejectStatusSymlinks checks .sortie/ and .sortie/status for
// symlinks. It returns true if either component is a symbolic link or
// if a non-ENOENT error occurs while statting either path.
func rejectStatusSymlinks(workspacePath string, logger *slog.Logger) bool {
	dotSortiePath := filepath.Join(workspacePath, ".sortie")

	symlink, err := isSymlink(dotSortiePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to stat .sortie directory",
				slog.String("workspace", workspacePath),
				slog.Any("error", err),
			)
		}
		return !errors.Is(err, fs.ErrNotExist)
	}
	if symlink {
		logger.Warn("symlink detected at .sortie directory, rejecting status file",
			slog.String("workspace", workspacePath),
		)
		return true
	}

	statusPath := filepath.Join(dotSortiePath, "status")
	symlink, err = isSymlink(statusPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to stat .sortie/status",
				slog.String("workspace", workspacePath),
				slog.Any("error", err),
			)
		}
		return !errors.Is(err, fs.ErrNotExist)
	}
	if symlink {
		logger.Warn("symlink detected at .sortie/status, rejecting status file",
			slog.String("workspace", workspacePath),
		)
		return true
	}

	return false
}

// ReadStatusFile reads the A2O status file from the workspace
// directory and returns the parsed status signal. The file path is
// <workspacePath>/.sortie/status.
//
// Returns [StatusNone] when the file is absent, unreadable, empty
// after trimming, or when either .sortie/ or status is a symbolic
// link. Read errors are logged at warn level; the function never
// returns an error to the caller. All failure modes degrade to
// StatusNone.
func ReadStatusFile(workspacePath string, logger *slog.Logger) StatusSignal {
	if logger == nil {
		logger = slog.Default()
	}

	if rejectStatusSymlinks(workspacePath, logger) {
		return StatusNone
	}

	statusPath := filepath.Join(workspacePath, ".sortie", "status")

	f, err := os.Open(statusPath) //nolint:gosec // path assembled from operator-controlled workspace root and literal .sortie/status; Lstat checks precede this open
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to open .sortie/status",
				slog.String("workspace", workspacePath),
				slog.Any("error", err),
			)
		}
		return StatusNone
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable after data is read

	data, err := io.ReadAll(io.LimitReader(f, statusFileMaxBytes))
	if err != nil {
		logger.Warn("failed to read .sortie/status",
			slog.String("workspace", workspacePath),
			slog.Any("error", err),
		)
		return StatusNone
	}

	parts := bytes.SplitN(data, []byte("\n"), 2)
	token := string(bytes.TrimSpace(parts[0]))
	if token == "" {
		return StatusNone
	}

	switch token {
	case "blocked":
		return StatusBlocked
	case "needs-human-review":
		return StatusNeedsHumanReview
	default:
		logger.Warn("unrecognized .sortie/status value",
			slog.String("workspace", workspacePath),
			slog.String("value", token),
		)
		return StatusNone
	}
}

// CleanupStatusFile removes the .sortie/status file from the
// workspace directory if it exists. The removal is best-effort:
// errors are logged and ignored, never propagated. Symlinks at
// either .sortie/ or status are rejected via Lstat — the file is
// not removed and a warning is logged.
func CleanupStatusFile(workspacePath string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	if rejectStatusSymlinks(workspacePath, logger) {
		return
	}

	statusPath := filepath.Join(workspacePath, ".sortie", "status")
	if err := os.Remove(statusPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("status file cleanup failed",
			slog.String("workspace", workspacePath),
			slog.Any("error", err),
		)
	}
}
