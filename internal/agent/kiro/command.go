package kiro

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// passthroughConfig holds Kiro-specific settings extracted from the
// "kiro" sub-object in WORKFLOW.md. All fields are optional; the zero
// value means "not configured".
type passthroughConfig struct {
	// Model is pinned with --model on every turn because the /model slash
	// command is unavailable headless.
	Model string

	// TrustAllTools passes --trust-all-tools when true. Mutually exclusive
	// with TrustTools.
	TrustAllTools bool

	// TrustTools is the allowlist passed as --trust-tools=<names>. An empty
	// slice with TrustAllTools false trusts nothing.
	TrustTools []string

	// Agent is the optional custom-agent selector passed as --agent.
	Agent string
}

// parsePassthroughConfig extracts Kiro-specific settings from the raw
// config map. Missing or wrong-typed keys use zero-value defaults.
//
// It returns a non-nil error when both trust_all_tools is true and
// trust_tools is non-empty, because the two trust modes are mutually
// exclusive.
func parsePassthroughConfig(config map[string]any) (passthroughConfig, error) {
	pt := passthroughConfig{
		Model:         typeutil.StringFrom(config, "model"),
		TrustAllTools: typeutil.BoolFrom(config, "trust_all_tools", false),
		TrustTools:    slices.Clone(typeutil.ExtractStringSlice(config["trust_tools"])),
		Agent:         typeutil.StringFrom(config, "agent"),
	}

	if pt.TrustAllTools && len(pt.TrustTools) > 0 {
		return passthroughConfig{}, fmt.Errorf("trust_all_tools and trust_tools are mutually exclusive")
	}

	return pt, nil
}

// buildArgs constructs the CLI argument slice for one headless Kiro turn.
// The arguments are passed directly to exec.Command, avoiding shell
// interpolation. The prompt is passed after a -- separator as a single
// positional argument.
func buildArgs(state *sessionState, turn int, prompt string, pt passthroughConfig) []string { //nolint:unparam // turn mirrors the ForkPerTurnHooks.BuildArgs signature; kiro decides resume via state.resumeRequested
	args := []string{"chat", "--no-interactive", "--wrap", "never"}

	if pt.Model != "" {
		args = append(args, "--model", pt.Model)
	}

	if pt.TrustAllTools {
		args = append(args, "--trust-all-tools")
	} else {
		args = append(args, "--trust-tools="+strings.Join(pt.TrustTools, ","))
	}

	if pt.Agent != "" {
		args = append(args, "--agent", pt.Agent)
	}

	if state.resumeRequested {
		args = append(args, "--resume")
	}

	args = append(args, "--", prompt)
	return args
}

// buildSSHRemoteCmd returns the remote command string for SSH mode.
// When apiKey is non-empty, KIRO_API_KEY is prepended and the value is
// shell-quoted, because OpenSSH drops the orchestrator's local environment
// and a key containing shell metacharacters would otherwise be misparsed by
// the remote shell.
func buildSSHRemoteCmd(remoteCommand, apiKey string) string {
	if apiKey == "" {
		return remoteCommand
	}
	return "KIRO_API_KEY=" + sshutil.ShellQuote(apiKey) + " " + remoteCommand
}
