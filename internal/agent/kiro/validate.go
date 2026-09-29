package kiro

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// trustToolsConflictMessage is the message [validateTrustToolsConflict]
// reports when trust_all_tools and a non-empty trust_tools are both
// configured.
const trustToolsConflictMessage = "trust_all_tools and trust_tools are mutually exclusive"

// trustToolsUntrustedMessage is the diagnostic [validateTrustToolsUntrusted]
// reports for a configuration whose resolved trust posture falls short of
// full trust. The converted launch appends -a, which trusts every tool, so
// a narrower posture cannot be honored.
const trustToolsUntrustedMessage = `a converted launch appends "acp -a" to the command, and -a trusts every tool, more than this trust setting allows; to convert, set trust_all_tools: true and remove trust_tools, or remove both; to keep the narrower set, name agent kind "agent-client-protocol" and put "kiro-cli acp --trust-tools=<names>" in agent.command`

// validateTrustToolsConflict reports an error when trust_all_tools is
// true and trust_tools is also non-empty.
func validateTrustToolsConflict(passthrough map[string]any) []registry.ValidationDiag {
	trustAllTools, _ := passthrough["trust_all_tools"].(bool)
	trustTools := typeutil.ExtractStringSlice(passthrough["trust_tools"])
	if !trustAllTools || len(trustTools) == 0 {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "kiro.trust_tools.conflict",
		Message:  trustToolsConflictMessage,
	}}
}

// validateTrustToolsUntrusted reports an error when the effective trust
// posture, resolved by [resolveTrustPosture], leaves any tool untrusted.
// The converted launch trusts every tool, so a narrower posture is refused
// rather than widened silently.
func validateTrustToolsUntrusted(passthrough map[string]any) []registry.ValidationDiag {
	trustAllTools, _ := resolveTrustPosture(passthrough)
	if trustAllTools {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "kiro.trust_tools.untrusted",
		Message:  trustToolsUntrustedMessage,
	}}
}
