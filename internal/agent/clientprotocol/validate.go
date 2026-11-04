package clientprotocol

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks this kind's own configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or launch a subprocess: it reports the
// same fault a session start would raise, so the session-start refusal and
// the offline verdict carry byte-identical text.
func validateConfig(fields registry.AgentConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	if _, fault := typeutil.StringField(fields.Passthrough, mcpConfigKey); fault != nil {
		diags = append(diags, registry.ValidationDiag{
			Severity: "error",
			Check:    "agent-client-protocol.mcp_config.wrong_type",
			Message:  fault.Error(),
		})
	}

	if _, fault := registry.EffortSetting(fields.Passthrough); fault != nil {
		diags = append(diags, registry.ValidationDiag{
			Severity: string(registry.SeverityError),
			Check:    "agent-client-protocol.effort.wrong_type",
			Message:  fault.Error(),
		})
	}

	if variant, present := fields.Passthrough[variantKey]; present && variant != nil {
		diags = append(diags, registry.ValidationDiag{
			Severity: string(registry.SeverityWarning),
			Check:    "agent-client-protocol.variant.not_read",
			Message:  "agent-client-protocol.variant has no effect: this kind reads the reasoning level from agent-client-protocol.effort",
		})
	}

	if _, fault := envSetting(fields.Passthrough); fault != nil {
		diags = append(diags, faultDiag(fault))
	}

	if _, fault := permissionsSetting(fields.Passthrough); fault != nil {
		diags = append(diags, faultDiag(fault))
	}

	if _, fault := configOptionsSetting(fields.Passthrough); fault != nil {
		diags = append(diags, faultDiag(fault))
	}

	return diags
}

func faultDiag(fault *settingFault) registry.ValidationDiag {
	return registry.ValidationDiag{Severity: "error", Check: fault.check, Message: fault.message}
}
