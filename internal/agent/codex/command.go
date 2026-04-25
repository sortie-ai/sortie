package codex

import "github.com/sortie-ai/sortie/internal/typeutil"

// passthroughConfig holds Codex-specific settings extracted from the
// "codex" sub-object in WORKFLOW.md. All fields are optional with
// zero-value meaning "not configured."
type passthroughConfig struct {
	Model             string
	Effort            string
	ApprovalPolicy    string
	ThreadSandbox     string
	TurnSandboxPolicy map[string]any
	Personality       string
	SkipGitRepoCheck  bool
}

// parsePassthroughConfig extracts Codex-specific settings from the
// raw config map. Missing or wrong-typed keys use zero-value defaults.
func parsePassthroughConfig(config map[string]any) passthroughConfig {
	return passthroughConfig{
		Model:             typeutil.StringFrom(config, "model"),
		Effort:            typeutil.StringFrom(config, "effort"),
		ApprovalPolicy:    typeutil.StringFrom(config, "approval_policy"),
		ThreadSandbox:     typeutil.StringFrom(config, "thread_sandbox"),
		TurnSandboxPolicy: typeutil.MapFrom(config, "turn_sandbox_policy"),
		Personality:       typeutil.StringFrom(config, "personality"),
		SkipGitRepoCheck:  typeutil.BoolFrom(config, "skip_git_repo_check", false),
	}
}
