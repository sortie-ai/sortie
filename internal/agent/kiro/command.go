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
// config map. A missing key uses its zero-value default, except
// TrustAllTools, which [resolveTrustPosture] defaults to true rather than
// false when the config sets neither trust key. A key present with a
// non-string value for a string field reports a fault rather than
// defaulting; [checkCrossField] holds the trust_all_tools and
// trust_tools conflict check.
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

// checkCrossField rejects a passthrough carrying both trust_all_tools and
// a non-empty trust_tools, because the two trust modes are mutually
// exclusive.
func checkCrossField(pt passthroughConfig) error {
	if pt.TrustAllTools && len(pt.TrustTools) > 0 {
		return fmt.Errorf("%s", trustToolsConflictMessage)
	}
	return nil
}

// resolveTrustPosture computes the effective trust_all_tools value and the
// trust_tools allowlist from the raw config map. [validateConfig] calls it
// too, so the offline verdict and the constructor read the same effective
// posture.
//
// trust_all_tools resolves to true when the config sets neither
// trust_all_tools nor trust_tools: kiro-cli's behavior on an untrusted tool
// under --no-interactive is unestablished (see "Untrusted-tool behavior"
// in docs/kiro-adapter-notes.md), and the conservative default trusts
// every tool rather than risk a wait for an approval that never arrives.
// Once either key is set explicitly, including an explicit false or an
// empty trust_tools list, the configured value is used unmodified.
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
