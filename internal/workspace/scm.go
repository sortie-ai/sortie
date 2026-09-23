package workspace

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// scmMetadataMaxBytes is the maximum number of bytes read from the SCM
// metadata file. Legitimate metadata is under 500 bytes; 4 KiB bounds
// memory usage while providing headroom.
const scmMetadataMaxBytes = 4096

// ReadSCMMetadata reads the workspace SCM metadata from
// <workspacePath>/.sortie/scm.json.
//
// Returns a zero-value [domain.SCMMetadata] when the file is absent,
// unreadable, oversized, malformed, or when the workspace directory,
// .sortie, or scm.json is a symbolic link or otherwise refused. Read
// errors, other than absence, are logged at warn level; the function
// never returns an error to the caller. Branch may be empty in the
// returned value: some reaction kinds require no checkout and
// therefore no branch, so validating Branch is the caller's
// responsibility, not this reader's.
func ReadSCMMetadata(workspacePath string, logger *slog.Logger) domain.SCMMetadata {
	if logger == nil {
		logger = slog.Default()
	}

	data, err := workspacekit.ReadSortieFile(workspacePath, "scm.json", scmMetadataMaxBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("failed to read .sortie/scm.json",
				slog.String("workspace", workspacePath),
				slog.Any("error", err),
			)
		}
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

	return meta
}
