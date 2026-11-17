package pi

import "github.com/sortie-ai/sortie/internal/registry"

// validateConfig checks pi-specific configuration constraints and returns
// diagnostics for the sortie validate pipeline. It does not construct an
// adapter instance or launch a subprocess, and reports what
// [PiAdapter.StartSession] refuses on, because both call
// [parsePassthroughConfig].
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	_, fault := parsePassthroughConfig(fields.Passthrough)
	if fault == nil {
		return nil
	}
	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "pi." + fault.Key + ".wrong_type",
		Message:  fault.Error(),
	}}
}
