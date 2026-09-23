package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/workspacekit"
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
	return workspacekit.WriteSortieFile(workspacePath, dispatchIdentityFile, data)
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

	content, err := workspacekit.ReadSortieFile(workspacePath, dispatchIdentityFile, maxDispatchIdentityBytes)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return ""
		case errors.Is(err, workspacekit.ErrLink):
			warn("symlink", nil)
		case errors.Is(err, workspacekit.ErrNotDirectory):
			warn("not_directory", nil)
		case errors.Is(err, workspacekit.ErrNotPlainFile):
			warn("not_regular", nil)
		case errors.Is(err, workspacekit.ErrChanged):
			warn("changed", nil)
		case errors.Is(err, workspacekit.ErrTooLarge):
			warn("oversized", nil)
		default:
			warn("unreadable", err)
		}
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
