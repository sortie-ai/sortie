package clientprotocol

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks this kind's own configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess: it reports the
// same fault [NewClientProtocolAdapter] would raise, so the
// constructor's refusal and the offline verdict carry byte-identical
// text.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	_, fault := typeutil.StringField(fields.Passthrough, mcpConfigKey)
	if fault == nil {
		return nil
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "agent-client-protocol.mcp_config.wrong_type",
		Message:  fault.Error(),
	}}
}
