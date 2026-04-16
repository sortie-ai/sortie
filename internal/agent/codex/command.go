package codex

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
		Model:             stringFrom(config, "model"),
		Effort:            stringFrom(config, "effort"),
		ApprovalPolicy:    stringFrom(config, "approval_policy"),
		ThreadSandbox:     stringFrom(config, "thread_sandbox"),
		TurnSandboxPolicy: mapFrom(config, "turn_sandbox_policy"),
		Personality:       stringFrom(config, "personality"),
		SkipGitRepoCheck:  boolFrom(config, "skip_git_repo_check", false),
	}
}

func stringFrom(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func boolFrom(m map[string]any, key string, def bool) bool {
	v, ok := m[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func mapFrom(m map[string]any, key string) map[string]any {
	v, ok := m[key]
	if !ok {
		return nil
	}
	sub, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return sub
}
