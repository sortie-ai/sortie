package agentcore

import (
	"os"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/domain"
)

// ResolveWorkspace validates path for use as an agent workspace directory.
// It performs four checks in order: non-empty, resolvable to absolute form,
// existence, and directory kind. On success it returns the absolute resolved
// path. On failure it returns a [*domain.AgentError] with Kind
// [domain.ErrInvalidWorkspaceCwd].
//
// ResolveWorkspace does not check workspace root containment; that invariant
// is enforced by the workspace manager before StartSession is called.
func ResolveWorkspace(path string) (string, *domain.AgentError) {
	if path == "" {
		return "", &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "empty workspace path",
		}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "cannot resolve workspace path",
			Err:     err,
		}
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		return "", &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path does not exist",
			Err:     err,
		}
	}

	if !fi.IsDir() {
		return "", &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path is not a directory",
		}
	}

	return absPath, nil
}
