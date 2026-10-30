package usagesource

import (
	"os"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/registry"
)

const geminiAgentName = "gemini-cli"

// geminiExecutableName is the name the runtime's own executable is published
// under, distinct from geminiAgentName, the name it reports at handshake time.
const geminiExecutableName = "gemini"

const geminiPackageName = "@google/gemini-cli"

func init() {
	VerificationAdditions.Register(geminiAgentName, func() VerificationAddition { return &geminiVerification{} })
}

// geminiVerification replaces the runtime's built-in system prompt for a
// verification session. The runtime reads the replacement from a file named by
// an environment variable and fails its handshake when the file is missing, so
// the file is written here, on the launching host, and removed with the
// session.
type geminiVerification struct {
	dir string
}

func (v *geminiVerification) Claim(target agentcore.LaunchTarget) (VerificationSwitches, bool) {
	if !namesRuntime(target, geminiExecutableName, geminiPackageName) {
		return VerificationSwitches{}, false
	}

	dir, err := os.MkdirTemp("", "sortie-verification-")
	if err != nil {
		return VerificationSwitches{}, false
	}
	path := filepath.Join(dir, "system.md")
	if err := os.WriteFile(path, []byte(agentcore.VerificationSystemPrompt), 0o600); err != nil {
		_ = os.RemoveAll(dir) //nolint:errcheck // best-effort; the claim is refused either way
		return VerificationSwitches{}, false
	}

	v.dir = dir
	return VerificationSwitches{Env: []string{"GEMINI_SYSTEM_MD=" + path}}, true
}

// Context keeps tools and instruction files because the runtime offers no
// switch that drops them.
func (v *geminiVerification) Context() registry.VerificationContext {
	return registry.DeclareVerificationContext(
		registry.VerificationKeepsAllTools,
		registry.VerificationKeepsProjectInstructions,
		registry.VerificationKeepsUserInstructions,
	)
}

// Close retries a removal that failed on its next call.
func (v *geminiVerification) Close() {
	if v.dir == "" {
		return
	}
	if err := os.RemoveAll(v.dir); err != nil {
		return
	}
	v.dir = ""
}
