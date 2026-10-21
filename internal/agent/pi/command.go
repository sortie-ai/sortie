package pi

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

type passthroughConfig struct {
	Model        string
	ProjectTrust projectTrust
}

// projectTrust is the operator's decision about whether pi loads the
// workspace's own project-local files and packages.
type projectTrust string

const (
	// trustIgnore keeps project-local files and packages out of the run.
	// It is the default, because an unattended turn has no operator to
	// answer a trust prompt and must not silently adopt whatever the
	// workspace declares.
	trustIgnore projectTrust = "ignore"

	// trustApprove loads them.
	trustApprove projectTrust = "approve"
)

// knownConfigKeys is every key the pi block of WORKFLOW.md accepts. A
// key outside it is refused rather than ignored: pi has no passthrough
// channel, so an unrecognized key would otherwise look configured while
// doing nothing. effort is accepted and never read: the kind declares
// [registry.EffortNotForwarded], so a set effort draws the
// orchestrator's advisory instead of this refusal.
var knownConfigKeys = map[string]struct{}{
	"model":         {},
	"effort":        {},
	"mcp_config":    {},
	"project_trust": {},
}

// genericConfigKeys are the keys the agent block contributes to every
// adapter's config map, whichever kind it names. They are not pi block
// keys, so the unknown-key check exempts them: refusing them would
// reject every valid workflow rather than one misspelled option.
var genericConfigKeys = map[string]struct{}{
	"kind":             {},
	"command":          {},
	"turn_timeout_ms":  {},
	"read_timeout_ms":  {},
	"stall_timeout_ms": {},
	"stop_grace_ms":    {},
}

// configFault is one reason a pi passthrough is refused. Check is the
// diagnostic key, Message the text both the constructor and the
// registered validator report, so the two can never disagree. Err is the
// wrapped cause when there is one.
type configFault struct {
	Check   string
	Message string
	Err     error
}

// fault builds one configFault under the adapter's diagnostic namespace.
func fault(check, format string, args ...any) *configFault {
	return &configFault{Check: "pi." + check, Message: fmt.Sprintf(format, args...)}
}

// typeFault builds the configFault for a key holding the wrong YAML type.
func typeFault(f *typeutil.TypeFault) *configFault {
	return &configFault{Check: "pi." + f.Key + ".wrong_type", Message: f.Error(), Err: f}
}

// parsePassthroughConfig extracts pi-specific settings from the raw
// config map, reporting every fault it finds rather than only the first,
// so one pass tells an operator everything that needs fixing.
func parsePassthroughConfig(config map[string]any) (passthroughConfig, []*configFault) {
	var faults []*configFault

	for _, key := range unknownConfigKeys(config) {
		faults = append(faults, fault(key+".unknown_key",
			"unknown pi config key %q; the pi block accepts %s",
			key, strings.Join(sortedConfigKeys(), ", ")))
	}

	pt := passthroughConfig{}

	model, modelFault := registry.ModelSetting(config)
	if modelFault != nil {
		faults = append(faults, typeFault(modelFault))
	}
	pt.Model = model

	_, mcpConfigFault := typeutil.StringField(config, "mcp_config")
	if mcpConfigFault != nil {
		faults = append(faults, typeFault(mcpConfigFault))
	}

	pt.ProjectTrust = trustIgnore
	trust, trustFault := typeutil.StringField(config, "project_trust")
	switch {
	case trustFault != nil:
		faults = append(faults, typeFault(trustFault))
	case trust == "":
	case projectTrust(trust) == trustIgnore, projectTrust(trust) == trustApprove:
		pt.ProjectTrust = projectTrust(trust)
	default:
		faults = append(faults, fault("project_trust.invalid_value",
			"project_trust: %q is not a pi trust setting; valid values are %s, %s",
			trust, trustIgnore, trustApprove))
	}

	if len(faults) > 0 {
		return passthroughConfig{}, faults
	}
	return pt, nil
}

// unknownConfigKeys returns the config keys pi does not accept, sorted.
func unknownConfigKeys(config map[string]any) []string {
	var unknown []string
	for key := range config {
		if _, ok := knownConfigKeys[key]; ok {
			continue
		}
		if _, ok := genericConfigKeys[key]; ok {
			continue
		}
		unknown = append(unknown, key)
	}
	slices.Sort(unknown)
	return unknown
}

// sortedConfigKeys returns knownConfigKeys' keys in a stable order, so
// the unknown-key diagnostic renders identically on every run.
func sortedConfigKeys() []string {
	keys := make([]string, 0, len(knownConfigKeys))
	for key := range knownConfigKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// pi runs non-interactive with -p --mode json and reads the prompt from
// standard input, so neither a prompt beginning with "@" (which pi treats
// as a file to include) nor a prompt longer than the kernel's argument
// limit reaches the command line. cmd.Dir is already the workspace, so
// no --dir. A credential-verification session is ephemeral: --no-session
// leaves no session file behind.
func buildRunArgs(state *sessionState, pt passthroughConfig) []string {
	args := []string{"-p", "--mode", "json"}

	if state.credentialVerification {
		args = append(args, "--no-session")
	} else if state.sessionID != "" {
		args = append(args, "--session", state.sessionID)
	}
	if pt.Model != "" {
		args = append(args, "--model", pt.Model)
	}
	if pt.ProjectTrust == trustApprove {
		args = append(args, "--approve")
	} else {
		args = append(args, "--no-approve")
	}

	return args
}

// buildTurnStdin returns the reader a turn's subprocess reads from:
// prompt, preceded by preamble (nil on a local launch, the SSH preamble
// otherwise).
func buildTurnStdin(prompt string, preamble io.Reader) io.Reader {
	promptReader := strings.NewReader(prompt)
	if preamble == nil {
		return promptReader
	}
	return io.MultiReader(preamble, promptReader)
}
