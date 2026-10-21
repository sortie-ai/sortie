package pi

import "github.com/sortie-ai/sortie/internal/registry"

// validateConfig checks pi-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess. It reports
// exactly what [NewPiAdapter] refuses on, because both call
// [parsePassthroughConfig]: one implementation, so the constructor's
// refusal and the offline verdict can never disagree.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	_, faults := parsePassthroughConfig(fields.Passthrough)

	diags := make([]registry.ValidationDiag, 0, len(faults))
	for _, f := range faults {
		diags = append(diags, registry.ValidationDiag{
			Severity: "error",
			Check:    f.Check,
			Message:  f.Message,
		})
	}
	return diags
}
