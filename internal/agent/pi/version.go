package pi

import (
	"context"
	"fmt"

	"golang.org/x/mod/semver"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

// minPiVersion is the lowest Pi release the adapter supports. Releases
// under the previous npm name all fall below it.
const minPiVersion = "0.99.0"

// piPackage names the npm package that publishes supported Pi
// releases. The previous name tops out below the minimum, so the
// refusal names this one.
const piPackage = "@earendil-works/pi-coding-agent"

// checkRuntimeVersion refuses a command that reports no readable version
// or a version below [minPiVersion]. The query re-verifies the workspace,
// so a session start on a removed workspace fails here rather than at the
// first turn.
func checkRuntimeVersion(ctx context.Context, state *sessionState) *domain.AgentError {
	reported, agentErr := agentcore.QueryRuntimeVersion(ctx, state.target, state.agentConfig, state.logger(), nil)
	if agentErr != nil {
		return agentErr
	}
	if reported.Version == "" {
		message := "no output"
		if reported.Line != "" {
			message = redact.Truncate(reported.Line, 200)
		}
		return &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "the configured Pi command reported no version Sortie can read: " + message,
		}
	}
	if semver.Compare("v"+reported.Version, "v"+minPiVersion) < 0 {
		return &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: fmt.Sprintf("Pi %s is not supported; install Pi %s or later, published on npm as %s", reported.Version, minPiVersion, piPackage),
		}
	}
	return nil
}
