package registry

import "github.com/sortie-ai/sortie/internal/domain"

// RetiredAgent declares that an agent kind has no adapter and that a
// configuration naming it converts onto Replacement at load.
type RetiredAgent struct {
	// Replacement is the kind the configuration converts onto. It is
	// registered in [Agents] with a nil Deprecation, absent from
	// [RetiredAgents], and not the retired kind itself.
	Replacement string

	// CredentialEnv declares the environment variable names the retired
	// kind's runtime reads as its credential.
	CredentialEnv CredentialEnv

	// Convert maps the retired kind's command and settings onto the
	// replacement kind. It MUST be non-nil, pure, deterministic and safe
	// for concurrent use: no environment read, no filesystem or network
	// access, no process launch, and no mutation of in.Settings. It
	// returns a conversion or a fault, never both.
	Convert func(in AgentConversionInput) (AgentConversion, *AgentConversionFault)
}

// AgentConversionInput is what [RetiredAgent.Convert] reads.
type AgentConversionInput struct {
	// Command is agent.command in its form when the retired kind is the
	// default agent kind, zero otherwise.
	Command domain.AgentCommand

	// Settings is the retired kind's settings block with $VAR
	// references resolved, nil unless the block is a mapping.
	Settings map[string]any

	// Remote is the launch mode the command is built for.
	Remote bool
}

// AgentConversion is what [RetiredAgent.Convert] returns on success.
type AgentConversion struct {
	// Command is non-zero: what the replacement kind's sessions launch
	// in the input's launch mode when the conversion governs them.
	Command domain.AgentCommand

	// Args lists the arguments the conversion added to the command, in
	// order. It serves display only.
	Args []string

	// DefaultCommand is the executable the conversion started from when
	// the input command named none, and empty otherwise. It serves
	// display only.
	DefaultCommand string

	// Settings holds keys for the replacement kind's settings block.
	// Nil adds none.
	Settings map[string]any

	// Carried lists the keys of the input settings consumed into
	// Command or Settings; every other key is reported as not carried.
	Carried []string
}

// AgentConversionFault reports a setting the conversion cannot carry.
type AgentConversionFault struct {
	// Key is a bare key of the retired kind's settings block.
	Key string

	// Message describes the fault in operator-friendly terms.
	Message string
}

// RetiredAgents is the retired agent kind registry. Adapter packages
// register declarations in their init functions; the configuration
// layer reads them through [RetiredAgentOf].
var RetiredAgents = NewRegistry[RetiredAgent, struct{}]("retired agent")

// RetiredAgentOf returns the declaration [RetiredAgents] holds for
// kind. It reports false when [RetiredAgents] holds none, and when
// [Agents] registers kind: a name registered both ways runs as the
// live kind. Safe for concurrent use.
func RetiredAgentOf(kind string) (RetiredAgent, bool) {
	if Agents.Has(kind) {
		return RetiredAgent{}, false
	}
	decl, err := RetiredAgents.Get(kind)
	if err != nil {
		return RetiredAgent{}, false
	}
	return decl, true
}
