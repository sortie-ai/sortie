package codex

import (
	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks codex-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	pt, fault := parsePassthroughConfig(fields.Passthrough)
	if fault != nil {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "codex." + fault.Key + ".wrong_type",
			Message:  fault.Error(),
		}}
	}

	if pt.ApprovalPolicy == "" || pt.ApprovalPolicy == "never" {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "codex.approval_policy.interactive",
		Message: "codex.approval_policy is set to a value that lets the agent stop and ask for " +
			"approval, and an unattended run has no one to answer; only \"never\" is supported",
	}}
}
