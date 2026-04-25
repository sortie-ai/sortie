package agentcore

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// ResolveBinary resolves command to an absolute executable path using
// exec.LookPath. It returns a [*domain.AgentError] with Kind
// [domain.ErrAgentNotFound] when the binary cannot be found on PATH.
//
// command must be a single token (no embedded spaces). If command contains
// whitespace, ResolveBinary returns ErrAgentNotFound immediately with a
// message indicating the caller must split the command string first. For
// multi-token commands such as "codex app-server", callers must split on
// whitespace and pass only the first token.
func ResolveBinary(command string) (string, *domain.AgentError) {
	if strings.ContainsAny(command, " \t") {
		return "", &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: fmt.Sprintf("agent command must be a single token, got %q", command),
		}
	}

	absPath, err := exec.LookPath(command)
	if err != nil {
		return "", &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: fmt.Sprintf("agent command %q not found", command),
			Err:     err,
		}
	}

	return absPath, nil
}
