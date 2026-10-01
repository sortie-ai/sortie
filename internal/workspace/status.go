package workspace

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/workspacekit"
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

	// StatusNoChangeNeeded indicates the agent determined that the
	// requested outcome already held and nothing needed changing.
	StatusNoChangeNeeded StatusSignal = "no-change-needed"
)

// IsRecognized reports whether s is a recognized A2O status signal
// that triggers a soft stop.
func (s StatusSignal) IsRecognized() bool {
	return s == StatusBlocked || s == StatusNeedsHumanReview || s == StatusNoChangeNeeded
}

// StatusStatement is the text an agent wrote after the first line of the
// status file.
type StatusStatement struct {
	// Text is the bytes after the first line feed, verbatim.
	Text string

	// Truncated reports that the file was longer than the read bound, so
	// Text was cut.
	Truncated bool
}

// StatusFile is one read of the status file.
type StatusFile struct {
	Signal StatusSignal

	// Statement is zero unless Signal is recognized.
	Statement StatusStatement
}

// statusFileMaxBytes is the maximum number of bytes of the status file
// that token and statement are taken from. Legitimate tokens are under
// 30 bytes; 1 KiB provides headroom for a short reason while bounding
// memory usage.
const statusFileMaxBytes = 1024

// ReadStatusFile reads the A2O status file from the workspace
// directory and returns the parsed status signal and, for a recognized
// signal, the statement after its first line. The file path is
// <workspacePath>/.sortie/status.
//
// Returns a zero [StatusFile] when the file is absent, unreadable,
// empty after trimming, or when the workspace directory, .sortie, or
// status is a symbolic link or otherwise refused. Read errors, other
// than absence, are logged at warn level; the function never returns an
// error to the caller.
func ReadStatusFile(workspacePath string, logger *slog.Logger) StatusFile {
	if logger == nil {
		logger = slog.Default()
	}

	f, err := workspacekit.OpenSortieFile(workspacePath, "status")
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to open .sortie/status",
				slog.String("workspace", workspacePath),
				slog.Any("error", err),
			)
		}
		return StatusFile{}
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable after data is read

	// One byte past the bound tells a file of exactly the bound from a
	// longer one.
	data, err := io.ReadAll(io.LimitReader(f, statusFileMaxBytes+1))
	if err != nil {
		logger.Warn("failed to read .sortie/status",
			slog.String("workspace", workspacePath),
			slog.Any("error", err),
		)
		return StatusFile{}
	}
	truncated := len(data) > statusFileMaxBytes
	data = data[:min(len(data), statusFileMaxBytes)]

	parts := bytes.SplitN(data, []byte("\n"), 2)
	token := string(bytes.TrimSpace(parts[0]))
	if token == "" {
		return StatusFile{}
	}

	var signal StatusSignal
	switch token {
	case "blocked":
		signal = StatusBlocked
	case "needs-human-review":
		signal = StatusNeedsHumanReview
	case "no-change-needed":
		signal = StatusNoChangeNeeded
	default:
		logger.Warn("unrecognized .sortie/status value",
			slog.String("workspace", workspacePath),
			slog.String("value", token),
		)
		return StatusFile{}
	}

	statement := StatusStatement{Truncated: truncated}
	if len(parts) == 2 {
		statement.Text = string(parts[1])
	}
	return StatusFile{Signal: signal, Statement: statement}
}

// CleanupStatusFile removes the .sortie/status file from the
// workspace directory if it exists. The removal is best-effort:
// errors are logged and ignored, never propagated. A symbolic link or
// any other refusal at the workspace directory, .sortie, or status
// leaves the file in place.
func CleanupStatusFile(workspacePath string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	if err := workspacekit.RemoveSortieFile(workspacePath, "status"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("status file cleanup failed",
			slog.String("workspace", workspacePath),
			slog.Any("error", err),
		)
	}
}
