package kiro

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// trustToolsConflictMessage is the byte-identical message both
// [validateConfig] and [NewKiroAdapter] report when trust_all_tools and a
// non-empty trust_tools are both configured.
const trustToolsConflictMessage = "trust_all_tools and trust_tools are mutually exclusive"

// trustToolsUntrustedMessage is the diagnostic [validateTrustToolsUntrusted]
// reports for a configuration whose resolved trust posture falls short of
// full trust. kiro-cli's behavior on an untrusted tool under
// --no-interactive is unestablished, and the conservative reading assumes
// it waits for an approval this unattended run cannot give.
const trustToolsUntrustedMessage = "trust_all_tools does not resolve to true, and kiro-cli's behavior on an untrusted tool under --no-interactive is unestablished; the conservative assumption is that it waits for an approval this unattended run cannot give, so trust_all_tools: true (or leaving trust_all_tools and trust_tools both unset) is required"

// validateConfig checks kiro-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess. [NewKiroAdapter]
// calls this same function so the constructor's refusal and the offline
// verdict can never disagree.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateTrustToolsConflict(fields.Passthrough)...)
	diags = append(diags, validateTrustToolsUntrusted(fields.Passthrough)...)

	return diags
}

// validateTrustToolsConflict reports an error when trust_all_tools is
// true and trust_tools is also non-empty, mirroring the check
// [parsePassthroughConfig] used to enforce inline at construction.
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
// posture, resolved the same way [resolveTrustPosture] resolves it for
// [NewKiroAdapter], leaves any tool untrusted. The wait branch recorded in
// docs/kiro-adapter-notes.md is assumed for kiro-cli's untrusted-tool
// behavior, so any configuration that can still reach an untrusted tool
// call is refused rather than accepted unexamined.
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
