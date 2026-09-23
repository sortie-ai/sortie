package agentcore

import (
	"errors"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
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

	if err := workspacekit.VerifyDir(absPath); err != nil {
		message := "workspace path does not exist"
		switch {
		case errors.Is(err, workspacekit.ErrLink):
			message = "workspace path is a symbolic link"
		case errors.Is(err, workspacekit.ErrNotDirectory):
			message = "workspace path is not a directory"
		}
		return "", &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: message,
			Err:     err,
		}
	}

	return absPath, nil
}
