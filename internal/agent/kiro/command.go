package kiro

import (
	"slices"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// passthroughConfig holds Kiro-specific settings extracted from the
// "kiro" sub-object in WORKFLOW.md. All fields are optional; the zero
// value means "not configured".
type passthroughConfig struct {
	// Model is the value of the converted launch's --model argument.
	Model string

	// TrustAllTools is the effective trust_all_tools setting. It is mutually
	// exclusive with a non-empty TrustTools.
	TrustAllTools bool

	// TrustTools is the trust_tools allowlist. An empty slice with
	// TrustAllTools false trusts nothing.
	TrustTools []string

	// Agent is the value of the converted launch's --agent argument.
	Agent string
}

// parsePassthroughConfig extracts Kiro-specific settings from the raw
// config map. A missing key uses its zero-value default, except
// TrustAllTools, which [resolveTrustPosture] defaults to true rather than
// false when the config sets neither trust key. A key present with a
// non-string value for a string field reports a fault rather than
// defaulting. It does not check the trust_all_tools and trust_tools
// conflict; [validateTrustToolsConflict] does.
func parsePassthroughConfig(config map[string]any) (passthroughConfig, *typeutil.TypeFault) {
	model, fault := typeutil.StringField(config, "model")
	if fault != nil {
		return passthroughConfig{}, fault
	}
	agent, fault := typeutil.StringField(config, "agent")
	if fault != nil {
		return passthroughConfig{}, fault
	}

	trustAllTools, trustTools := resolveTrustPosture(config)

	return passthroughConfig{
		Model:         model,
		TrustAllTools: trustAllTools,
		TrustTools:    trustTools,
		Agent:         agent,
	}, nil
}

// resolveTrustPosture computes the effective trust_all_tools value and the
// trust_tools allowlist from the raw config map. [validateTrustToolsUntrusted]
// calls it too, so the conversion and its trust check read the same
// effective posture.
//
// trust_all_tools resolves to true when the config sets neither
// trust_all_tools nor trust_tools, because the converted launch trusts
// every tool. Once either key is set explicitly, including an explicit
// false or an empty trust_tools list, the configured value is used
// unmodified.
func resolveTrustPosture(config map[string]any) (trustAllTools bool, trustTools []string) {
	_, trustAllToolsSet := config["trust_all_tools"]
	_, trustToolsSet := config["trust_tools"]

	trustAllTools = typeutil.BoolFrom(config, "trust_all_tools", false)
	trustTools = slices.Clone(typeutil.ExtractStringSlice(config["trust_tools"]))

	if !trustAllToolsSet && !trustToolsSet {
		trustAllTools = true
	}

	return trustAllTools, trustTools
}
