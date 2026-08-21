package copilot

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks copilot-cli-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess.
//
// A configured allowed_tools, denied_tools, available_tools, or
// excluded_tools displaces the unconditional --allow-all buildArgs passes
// otherwise. The CLI's own non-interactive permission policy denies and
// continues past a tool call that scoping excludes, so the diagnostic is a
// warning rather than an error: the configuration narrows what the agent
// may do, but does not defeat the non-interactive launch posture.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	hasToolScoping := typeutil.StringFrom(fields.Passthrough, "allowed_tools") != "" ||
		typeutil.StringFrom(fields.Passthrough, "denied_tools") != "" ||
		typeutil.StringFrom(fields.Passthrough, "available_tools") != "" ||
		typeutil.StringFrom(fields.Passthrough, "excluded_tools") != ""
	if !hasToolScoping {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "warning",
		Check:    "copilot-cli.tool_scoping.interactive",
		Message: "copilot-cli tool scoping (allowed_tools, denied_tools, available_tools, or excluded_tools) " +
			"displaces --allow-all; a tool call the scoping excludes is denied and the run continues, " +
			"rather than being auto-approved",
	}}
}
