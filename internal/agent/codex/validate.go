package codex

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks codex-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	policy := typeutil.StringFrom(fields.Passthrough, "approval_policy")
	if policy == "" || policy == "never" {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "codex.approval_policy.interactive",
		Message: "codex.approval_policy is set to a value that lets the agent stop and ask for " +
			"approval, and an unattended run has no one to answer; only \"never\" is supported",
	}}
}
