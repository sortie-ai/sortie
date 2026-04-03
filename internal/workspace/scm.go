package workspace

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/domain"
)

// scmMetadataMaxBytes is the maximum number of bytes read from the SCM
// metadata file. Legitimate metadata is under 500 bytes; 4 KiB bounds
// memory usage while providing headroom.
const scmMetadataMaxBytes = 4096

// rejectSCMSymlinks checks .sortie/ and .sortie/scm.json for
// symlinks. It returns true if either component is a symbolic link or
// if a non-ENOENT error occurs while statting either path.
func rejectSCMSymlinks(workspacePath string, logger *slog.Logger) bool {
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
		logger.Warn("symlink detected at .sortie directory, rejecting scm metadata",
			slog.String("workspace", workspacePath),
		)
		return true
	}

	scmPath := filepath.Join(dotSortiePath, "scm.json")
	symlink, err = isSymlink(scmPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to stat .sortie/scm.json",
				slog.String("workspace", workspacePath),
				slog.Any("error", err),
			)
		}
		return !errors.Is(err, fs.ErrNotExist)
	}
	if symlink {
		logger.Warn("symlink detected at .sortie/scm.json, rejecting scm metadata",
			slog.String("workspace", workspacePath),
		)
		return true
	}

	return false
}

// ReadSCMMetadata reads the workspace SCM metadata from
// <workspacePath>/.sortie/scm.json.
//
// Returns a zero-value [domain.SCMMetadata] when the file is absent,
// unreadable, oversized, malformed, or when either .sortie/ or
// scm.json is a symbolic link. Read errors are logged at warn level;
// the function never returns an error to the caller.
func ReadSCMMetadata(workspacePath string, logger *slog.Logger) domain.SCMMetadata {
	if logger == nil {
		logger = slog.Default()
	}

	if rejectSCMSymlinks(workspacePath, logger) {
		return domain.SCMMetadata{}
	}

	scmPath := filepath.Join(workspacePath, ".sortie", "scm.json")

	f, err := os.Open(scmPath) //nolint:gosec // path assembled from operator-controlled workspace root and literal .sortie/scm.json; Lstat checks precede this open
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to open .sortie/scm.json",
				slog.String("workspace", workspacePath),
				slog.Any("error", err),
			)
		}
		return domain.SCMMetadata{}
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable after data is read

	data, err := io.ReadAll(io.LimitReader(f, scmMetadataMaxBytes+1))
	if err != nil {
		logger.Warn("failed to read .sortie/scm.json",
			slog.String("workspace", workspacePath),
			slog.Any("error", err),
		)
		return domain.SCMMetadata{}
	}

	if len(data) > scmMetadataMaxBytes {
		logger.Warn("oversized .sortie/scm.json",
			slog.String("workspace", workspacePath),
			slog.String("reason", "oversized"),
		)
		return domain.SCMMetadata{}
	}

	var meta domain.SCMMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		logger.Warn("malformed .sortie/scm.json",
			slog.String("workspace", workspacePath),
			slog.Any("error", err),
		)
		return domain.SCMMetadata{}
	}

	if meta.Branch == "" {
		return domain.SCMMetadata{}
	}

	return meta
}
