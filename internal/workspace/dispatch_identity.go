package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// dispatchIdentityFile is the .sortie-relative name of the dispatch
// identity record.
const dispatchIdentityFile = "dispatch.json"

// maxDispatchIdentityBytes bounds how much of the dispatch identity
// record ReadDispatchSessionID reads. A record at or under this size
// decodes; anything larger is rejected as oversized.
const maxDispatchIdentityBytes = 4096

// DispatchIdentity is the dispatch-fenced session identity record a
// worker keeps current at .sortie/dispatch.json. SessionID is accepted
// by a reader only when DispatchID matches the reader's own dispatch.
type DispatchIdentity struct {
	DispatchID string `json:"dispatch_id"`
	SessionID  string `json:"session_id"`
}

// WriteDispatchIdentity writes identity as the workspace's dispatch
// identity record through [WriteSortieFile], returning its error.
func WriteDispatchIdentity(workspacePath string, identity DispatchIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("marshal dispatch identity: %w", err)
	}
	return WriteSortieFile(workspacePath, dispatchIdentityFile, data)
}

// ReadDispatchSessionID returns the session ID recorded for dispatchID
// in the workspace's dispatch identity record, or "" when workspacePath
// or dispatchID is empty, no record exists, the record names a
// different dispatch, or the record is rejected. It never returns an
// error: a rejected or unreadable record is logged at warn level and
// treated the same as an absent one. It never waits for a writer, and
// reads at most one byte past the size bound so an oversized record is
// detected rather than silently truncated. A nil logger selects
// [slog.Default].
func ReadDispatchSessionID(workspacePath, dispatchID string, logger *slog.Logger) string {
	if workspacePath == "" || dispatchID == "" {
		return ""
	}
	if logger == nil {
		logger = slog.Default()
	}

	warn := func(reason string, err error) {
		attrs := []any{
			slog.String("workspace", workspacePath),
			slog.String("reason", reason),
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
		}
		logger.Warn("dispatch identity record unusable", attrs...)
	}

	root, err := os.OpenRoot(workspacePath)
	if err != nil {
		warn("unreadable", err)
		return ""
	}
	defer root.Close() //nolint:errcheck // read-only handle; close error is not actionable

	dirInfo, err := root.Lstat(sortieDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ""
	case err != nil:
		warn("unreadable", err)
		return ""
	case dirInfo.Mode()&os.ModeSymlink != 0:
		warn("symlink", nil)
		return ""
	case !dirInfo.IsDir():
		warn("not_directory", nil)
		return ""
	}

	recordPath := sortieDir + "/" + dispatchIdentityFile
	fileInfo, err := root.Lstat(recordPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ""
	case err != nil:
		warn("unreadable", err)
		return ""
	case fileInfo.Mode()&os.ModeSymlink != 0:
		warn("symlink", nil)
		return ""
	case !fileInfo.Mode().IsRegular():
		warn("not_regular", nil)
		return ""
	}

	f, err := root.OpenFile(recordPath, os.O_RDONLY|nonBlockingReadFlag, 0)
	if err != nil {
		warn("unreadable", err)
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable after data is read

	openedInfo, err := f.Stat()
	if err != nil {
		warn("unreadable", err)
		return ""
	}
	if !openedInfo.Mode().IsRegular() {
		warn("not_regular", nil)
		return ""
	}

	limited := io.LimitReader(f, maxDispatchIdentityBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		warn("unreadable", err)
		return ""
	}
	if len(content) > maxDispatchIdentityBytes {
		warn("oversized", nil)
		return ""
	}

	var record DispatchIdentity
	if err := json.Unmarshal(content, &record); err != nil {
		warn("malformed", err)
		return ""
	}
	if record.DispatchID != dispatchID {
		return ""
	}
	return record.SessionID
}
