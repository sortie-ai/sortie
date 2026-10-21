package pi

import (
	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

type passthroughConfig struct {
	Model   string
	Approve bool
}

// parsePassthroughConfig extracts pi-specific settings from the raw config
// map. A key it does not read is accepted and ignored.
func parsePassthroughConfig(config map[string]any) (passthroughConfig, *typeutil.TypeFault) {
	model, fault := registry.ModelSetting(config)
	if fault != nil {
		return passthroughConfig{}, fault
	}

	approve := false
	if v := config["approve"]; v != nil {
		b, ok := v.(bool)
		if !ok {
			return passthroughConfig{}, &typeutil.TypeFault{Key: "approve", Want: "boolean", Got: typeutil.DescribeYAMLType(v)}
		}
		approve = b
	}

	return passthroughConfig{Model: model, Approve: approve}, nil
}

// buildRunArgs returns the arguments of one turn. The prompt is read from
// standard input, so a prompt beginning with "@", which pi treats as a file
// to include, and a prompt longer than the argument limit never reach the
// command line. A credential verification session passes --no-session so it
// leaves no session file behind, and a reduced one also drops tools,
// instruction files, and skills, replaces the system prompt, and suppresses
// the discovery of APPEND_SYSTEM.md, which no other flag disables. The empty
// --tools list follows --no-tools because pi lets a --tools list override
// --no-tools, and the configured command may carry one.
func buildRunArgs(state *sessionState, pt passthroughConfig) []string {
	args := []string{"-p", "--mode", "json"}

	if state.credentialVerification {
		args = append(args, "--no-session")
	} else if state.sessionID != "" {
		args = append(args, "--session", state.sessionID)
	}
	if state.reducedVerificationContext {
		args = append(args, "--no-tools", "--tools", "", "--no-context-files", "--no-skills", "--system-prompt", agentcore.VerificationSystemPrompt, "--append-system-prompt", "")
	}
	if pt.Model != "" {
		args = append(args, "--model", pt.Model)
	}
	if pt.Approve {
		args = append(args, "--approve")
	} else {
		args = append(args, "--no-approve")
	}

	return args
}
