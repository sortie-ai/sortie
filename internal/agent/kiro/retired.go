package kiro

import (
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// This file holds every runtime fact the retirement declaration needs,
// so it stands without the adapter's own files.

// defaultCommand is the executable a session launches when it is given
// no command.
const defaultCommand = "kiro-cli"

// replacementKind is the agent kind a configuration naming "kiro"
// converts onto.
const replacementKind = "agent-client-protocol"

// credentialEnv declares the variable the runtime reads as its
// credential.
var credentialEnv = registry.DeclareCredentialEnv("KIRO_API_KEY")

func init() {
	registry.RetiredAgents.Register("kiro", registry.RetiredAgent{
		Replacement:   replacementKind,
		CredentialEnv: credentialEnv,
		Convert:       convertRetired,
	})
}

// convertRetired maps the kiro command and settings onto the protocol
// entry point of the same executable. It refuses exactly the settings
// [validateConfig] refuses, by running the same checks.
func convertRetired(in registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
	settings := in.Settings
	if settings == nil {
		settings = map[string]any{}
	}

	pt, typeFault := parsePassthroughConfig(settings)
	if typeFault != nil {
		return registry.AgentConversion{}, &registry.AgentConversionFault{Key: typeFault.Key, Message: typeFault.Error()}
	}
	for _, check := range []func(map[string]any) []registry.ValidationDiag{validateTrustToolsConflict, validateTrustToolsUntrusted} {
		if diags := check(settings); len(diags) > 0 {
			return registry.AgentConversion{}, &registry.AgentConversionFault{Key: faultKey(diags[0].Check), Message: diags[0].Message}
		}
	}

	args := []string{"acp", "-a"}
	if pt.Model != "" {
		args = append(args, "--model", pt.Model)
	}
	if pt.Agent != "" {
		args = append(args, "--agent", pt.Agent)
	}

	base := in.Command
	conversion := registry.AgentConversion{Args: args, Carried: carriedKeys(settings)}
	if !base.NamesExecutable() {
		base = domain.AgentCommand{Line: defaultCommand}
		conversion.DefaultCommand = defaultCommand
	}
	conversion.Command = agentcore.AppendCommandArgs(base, in.Remote, args...)
	return conversion, nil
}

// faultKey reduces a diagnostic check name to the bare settings key it
// reports: "kiro.trust_tools.conflict" yields "trust_tools".
func faultKey(check string) string {
	key := strings.TrimPrefix(check, "kiro.")
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = key[:i]
	}
	return key
}

func carriedKeys(settings map[string]any) []string {
	var carried []string
	for _, key := range []string{"model", "agent", "trust_all_tools", "trust_tools"} {
		if _, present := settings[key]; present {
			carried = append(carried, key)
		}
	}
	return carried
}
