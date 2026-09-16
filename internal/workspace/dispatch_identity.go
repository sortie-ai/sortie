package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

const dispatchIdentityFile = "dispatch.json"

const maxDispatchIdentityBytes = 4096

// DispatchIdentity associates the current session with a dispatch.
type DispatchIdentity struct {
	DispatchID string `json:"dispatch_id"`
	SessionID  string `json:"session_id"`
}

// WriteDispatchIdentity writes the workspace's dispatch identity record.
func WriteDispatchIdentity(workspacePath string, identity DispatchIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("marshal dispatch identity: %w", err)
	}
	return WriteSortieFile(workspacePath, dispatchIdentityFile, data)
}

// ReadDispatchSessionID returns the session ID for dispatchID, or an empty
// string when the record is absent, invalid, or belongs to another dispatch.
// Invalid records are logged and treated as absent.
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
	defer root.Close() //nolint:errcheck // A read-only root has no actionable close error.

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
	defer f.Close() //nolint:errcheck // Data is already read when this deferred close runs.

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
