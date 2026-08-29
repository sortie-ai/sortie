package copilot

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks copilot-cli-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess.
//
// A configured allowed_tools replaces the unconditional --allow-all
// buildArgs passes otherwise, because the grant would subsume the
// allow-list. denied_tools, available_tools, and excluded_tools compose
// with the grant instead and draw no diagnostic. The diagnostic is a
// warning rather than an error because the configuration is honored
// exactly as written and no turn stalls waiting on it.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	allowed := typeutil.StringFrom(fields.Passthrough, "allowed_tools")
	if blanketGrantApplies(allowed) {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "warning",
		Check:    "copilot-cli.allowed_tools.auto_deny",
		Message: "copilot-cli.allowed_tools replaces the --allow-all grant, so only a call the list matches " +
			"is approved; every other permissioned call is denied without a prompt, the turn continues, " +
			"and a turn whose calls were all denied still reports success",
	}}
}
