// Package registry provides typed adapter registries that map kind
// strings to constructor functions. Start with [Trackers] for the
// default tracker adapter registry and [Agents] for the default agent
// adapter registry.
package registry

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TrackerConstructor creates a [domain.TrackerAdapter] from opaque
// adapter-specific configuration. The config parameter is the raw map
// from the adapter's pass-through config sub-object. Implementations
// must validate their config and return an error if required fields
// are missing.
type TrackerConstructor func(config map[string]any) (domain.TrackerAdapter, error)

// Trackers is the default tracker adapter registry. Adapter packages
// register themselves via [Registry.Register] in their init functions;
// the orchestrator resolves adapters via [Registry.Get] at runtime.
var Trackers = NewRegistry[TrackerConstructor, TrackerMeta]("tracker")

// AgentConstructor creates a [domain.AgentAdapter] that holds no
// settings: each session reads [domain.StartSessionParams.Settings].
type AgentConstructor func() (domain.AgentAdapter, error)

// Agents is the default agent adapter registry. Adapter packages
// register themselves via [Registry.RegisterWithMeta] in their init
// functions; the orchestrator resolves adapters via [Registry.Get] at
// runtime. A registration whose [AgentMeta.UsageSources] fails its check
// panics, so [Registry.Register] without metadata always panics here.
var Agents = NewCheckedRegistry[AgentConstructor, AgentMeta]("agent", func(_ AgentConstructor, meta AgentMeta) error {
	return meta.UsageSources.Check()
})

// CIProviderConstructor creates a [domain.CIStatusProvider] from
// a maximum log-line count and opaque adapter-specific configuration.
// The maxLogLines parameter controls how many log lines of the excerpt
// to include for failing checks (0 disables log fetching).
// The adapterConfig parameter is the raw map from the adapter's
// pass-through config sub-object. Implementations must validate
// adapterConfig and return an error if required fields are missing.
type CIProviderConstructor func(maxLogLines int, adapterConfig map[string]any) (domain.CIStatusProvider, error)

// CIProviders is the default CI status provider registry. Adapter
// packages register themselves via [Registry.Register] in their init
// functions; the orchestrator resolves adapters via [Registry.Get] at
// runtime.
var CIProviders = NewRegistry[CIProviderConstructor, struct{}]("ci_provider")

// SCMAdapterConstructor creates a [domain.SCMAdapter] from opaque
// adapter-specific configuration. The adapterConfig parameter is the
// raw map from the adapter's pass-through config sub-object (merged
// tracker credentials when tracker kind matches, plus reaction Extra
// fields). Implementations must validate adapterConfig and return an
// error if required fields are missing.
type SCMAdapterConstructor func(adapterConfig map[string]any) (domain.SCMAdapter, error)

// SCMAdapters is the default SCM adapter registry. Adapter packages
// register themselves via [Registry.Register] in their init functions;
// the orchestrator resolves adapters via [Registry.Get] at runtime.
var SCMAdapters = NewRegistry[SCMAdapterConstructor, struct{}]("scm")

// NotifierConstructor creates a [domain.Notifier] from the backend
// entry's pass-through config map. Implementations validate their
// config and return an error when a required field is missing or a
// referenced secret resolved to an empty string.
type NotifierConstructor func(config map[string]any) (domain.Notifier, error)

// Notifiers is the default notifier adapter registry. Backend packages
// register themselves via [Registry.Register] in their init functions;
// the sidecar and the program's notification router resolve backends via
// [Registry.Get] at runtime. It reserves [domain.TrackerCommentKind],
// which names a built-in destination rather than a backend.
var Notifiers = NewRegistry[NotifierConstructor, struct{}]("notifier", domain.TrackerCommentKind)

// TrackerConfigFields holds the config values passed to adapter
// validation functions. This is a plain data struct that avoids
// coupling the registry package to the config package.
type TrackerConfigFields struct {
	Kind            string
	Project         string
	Endpoint        string
	APIKey          string
	ActiveStates    []string
	TerminalStates  []string
	HandoffState    string
	InProgressState string

	// QueryFilter is the resolved tracker.query_filter value. An adapter
	// that interprets the key checks its shape here; an adapter that
	// ignores the key leaves the field unread.
	QueryFilter string

	// APIVersion is the resolved tracker.api_version value, empty when
	// the value is absent or the config layer produced no string for
	// it. An adapter that selects an API version resolves its own
	// default from this value; an adapter without a version selector
	// leaves the field unread.
	APIVersion string
}

// ValidationDiag is a single diagnostic produced by adapter config
// validation. Adapters populate Check, Severity, and Message.
type ValidationDiag struct {
	Severity string // "error" or "warning"
	Check    string // e.g. "tracker.project.format"
	Message  string // operator-friendly description
}

// TrackerMeta holds optional tracker-adapter-declared properties
// queried by the orchestrator at preflight time. Zero value means no
// special requirements.
type TrackerMeta struct {
	// RequiresProject indicates the tracker adapter requires a
	// non-empty tracker.project config value.
	RequiresProject bool

	// RequiresAPIKey indicates the tracker adapter requires a
	// non-empty tracker.api_key config value.
	RequiresAPIKey bool

	// ValidateTrackerConfig is an optional function the preflight
	// pipeline calls to run tracker-specific config validation.
	// Nil means no adapter-specific validation.
	ValidateTrackerConfig func(fields TrackerConfigFields) []ValidationDiag

	// RequiresTerminalStates indicates the tracker adapter requires a
	// non-empty tracker.terminal_states.
	RequiresTerminalStates bool

	// BlockerSource declares where this adapter's blocker data comes
	// from. The empty value is read as BlockersFromCandidates.
	BlockerSource BlockerSource
}

// BlockerSource declares where a tracker adapter's blocker data comes
// from, so the layer between the registry and the orchestrator knows
// whether a candidate issue is gate-ready as fetched.
type BlockerSource string

const (
	// BlockersFromCandidates declares that the adapter's candidate
	// fetch already carries every blocker the tracker reports.
	BlockersFromCandidates BlockerSource = "candidates"

	// BlockersPerIssue declares that the adapter's candidate fetch
	// carries no blockers, and that one issue's blockers are read
	// through domain.BlockerReader.
	BlockersPerIssue BlockerSource = "per_issue"

	// BlockersUnsupported declares that the tracker has no blocking
	// relation, so an empty blocker list on a candidate is complete.
	BlockersUnsupported BlockerSource = "unsupported"
)

// AgentDeprecation declares that a registered agent kind is
// deprecated in favor of Replacement.
type AgentDeprecation struct {
	Replacement string
}

// AgentMeta holds the agent-adapter-declared properties queried by the
// orchestrator at preflight time.
type AgentMeta struct {
	// Deprecation is nil when the kind is not deprecated. Non-nil
	// declares the kind deprecated in favor of Deprecation.Replacement:
	// the kind stays registered, and it constructs and runs unchanged.
	// Read-only after registration.
	Deprecation *AgentDeprecation

	// RequiresCommand indicates the kind launches a command.
	RequiresCommand bool

	// DefaultCommand is the command the kind launches when its own
	// command is zero: the value its StartSession hands to the shared
	// launch resolution. Empty for a kind that has none.
	DefaultCommand string

	// ValidateAgentConfig is an optional function the preflight
	// pipeline calls to run agent-specific config validation. Nil
	// means no adapter-specific validation.
	ValidateAgentConfig func(fields AgentConfigFields) []ValidationDiag

	// MCPDialect declares the tool-server configuration syntax this kind's
	// runtime reads. Required.
	MCPDialect MCPDialect

	// MCPDialectNoneReason is required with MCPDialectNone and empty
	// otherwise.
	MCPDialectNoneReason string

	// SessionResumeBlockedBy reports which of this adapter's own
	// operator-visible config keys, under the given passthrough,
	// stops it from resuming a session across separate process
	// launches. A nil field declares that no key of this adapter
	// blocks resume. A non-nil field returns the bare key name, with
	// no agent-kind prefix, when the given passthrough leaves the
	// adapter unable to resume, and the empty string when the given
	// passthrough leaves the adapter able to resume across separate
	// process launches; it MUST return the empty string for a nil
	// map and for an empty map. It MUST be pure - no map mutation,
	// no environment read, no filesystem or network access, no
	// adapter construction, no process launch - deterministic, and
	// safe for concurrent use.
	SessionResumeBlockedBy func(passthrough map[string]any) string

	// UsageSources declares the usage sources every session of this kind
	// attaches, local or remote. The kind's arrival and attribution derive
	// from it. Required.
	UsageSources usage.Sources

	// CredentialEnv declares the environment variable names this
	// kind's runtime reads as the credential for its default
	// provider. The zero value means undeclared.
	CredentialEnv CredentialEnv

	// EffortForwarding declares what this adapter does with the effort
	// key of its own settings block. The empty value means undeclared.
	EffortForwarding EffortForwarding

	// VerificationContext declares which agent context components the
	// kind's credential-verification request still carries. The zero
	// value means undeclared.
	VerificationContext VerificationContext

	// UsageLimitRecognition declares whether the adapter recognizes a
	// provider usage-limit refusal. Required.
	UsageLimitRecognition UsageLimitRecognition

	// UsageLimitNotRecognizedReason is required with
	// UsageLimitNotRecognized and empty otherwise.
	UsageLimitNotRecognizedReason string
}

// UsageLimitRecognition declares whether a kind's adapter returns
// [domain.ErrUsageLimit] for a provider usage-limit refusal. The empty
// value means undeclared.
type UsageLimitRecognition string

const (
	// UsageLimitRecognitionUndeclared is the zero value.
	UsageLimitRecognitionUndeclared UsageLimitRecognition = ""

	// UsageLimitRecognized declares that the adapter returns
	// [domain.ErrUsageLimit] for a provider usage-limit refusal.
	UsageLimitRecognized UsageLimitRecognition = "recognized"

	// UsageLimitNotRecognized declares that the adapter does not
	// recognize the refusal and states why.
	UsageLimitNotRecognized UsageLimitRecognition = "not_recognized"
)

// CredentialEnv declares the environment variable names an agent
// kind's runtime reads as the credential for its default provider.
// The zero value is undeclared; construct a declared value with
// [DeclareCredentialEnv].
type CredentialEnv struct {
	names    []string
	declared bool
}

// DeclareCredentialEnv returns a declared [CredentialEnv] holding
// names in the given order. Called with no arguments, it declares
// that the kind carries no credential variable of its own.
func DeclareCredentialEnv(names ...string) CredentialEnv {
	return CredentialEnv{names: slices.Clone(names), declared: true}
}

// Declared reports whether c was built by [DeclareCredentialEnv]. The
// zero value reports false.
func (c CredentialEnv) Declared() bool {
	return c.declared
}

// Names returns a newly allocated copy of c's declared names, nil
// when c holds none. The caller may freely mutate the returned slice
// without affecting c or any other caller's copy.
func (c CredentialEnv) Names() []string {
	if len(c.names) == 0 {
		return nil
	}
	return slices.Clone(c.names)
}

// VerificationResidual names one context component that a kind's
// credential-verification request still carries, because its runtime
// offers no qualifying switch that drops it.
type VerificationResidual string

const (
	// VerificationKeepsSomeTools declares that the request carries
	// strictly fewer tool definitions than a working request.
	VerificationKeepsSomeTools VerificationResidual = "some_tools"

	// VerificationKeepsAllTools declares that the request carries no
	// more tool definitions than a working request.
	VerificationKeepsAllTools VerificationResidual = "all_tools"

	// VerificationKeepsProjectInstructions declares that the request
	// carries the instruction files read from the workspace.
	VerificationKeepsProjectInstructions VerificationResidual = "project_instructions"

	// VerificationKeepsUserInstructions declares that the request
	// carries the instruction files and skill catalogues read from the
	// operator's home.
	VerificationKeepsUserInstructions VerificationResidual = "user_instructions"

	// VerificationKeepsSystemPrompt declares that the request carries
	// the runtime's built-in system prompt.
	VerificationKeepsSystemPrompt VerificationResidual = "system_prompt"
)

// VerificationContext declares the residuals of a kind's
// credential-verification request: the context components it still
// carries. The zero value is undeclared; construct a declared value
// with [DeclareVerificationContext].
//
// Tool definitions of every origin count toward the tool components:
// built-in, skill, and tool-server tools.
type VerificationContext struct {
	residuals []VerificationResidual
	declared  bool
}

// DeclareVerificationContext returns a declared [VerificationContext]
// keeping the given residuals. Called with no arguments, it declares a
// request that keeps nothing. It panics when given both
// [VerificationKeepsSomeTools] and [VerificationKeepsAllTools], which
// bound the same component differently.
func DeclareVerificationContext(residuals ...VerificationResidual) VerificationContext {
	if slices.Contains(residuals, VerificationKeepsSomeTools) && slices.Contains(residuals, VerificationKeepsAllTools) {
		panic("registry: verification context declares both some_tools and all_tools")
	}
	return VerificationContext{residuals: slices.Clone(residuals), declared: true}
}

// Declared reports whether v was built by [DeclareVerificationContext].
// The zero value reports false.
func (v VerificationContext) Declared() bool {
	return v.declared
}

// Keeps reports whether v declares that the request carries r. An
// undeclared value keeps nothing.
func (v VerificationContext) Keeps(r VerificationResidual) bool {
	return slices.Contains(v.residuals, r)
}

// UsageArrival is when a session's token figure reaches the orchestrator.
type UsageArrival = usage.Arrival

// UsageAttribution is what a session's token figure names.
type UsageAttribution = usage.Attribution

const (
	// UsageArrivalUndeclared is the zero value.
	UsageArrivalUndeclared = usage.ArrivalUndeclared

	// UsageArrivalIncremental is a figure reported after each model request
	// while the turn runs.
	UsageArrivalIncremental = usage.ArrivalIncremental

	// UsageArrivalTurnEnd is at most one figure per turn, settled after the
	// turn's work is over.
	UsageArrivalTurnEnd = usage.ArrivalTurnEnd

	// UsageArrivalNone is no attached source counting tokens.
	UsageArrivalNone = usage.ArrivalNone

	// UsageAttributionUndeclared is the zero value.
	UsageAttributionUndeclared = usage.AttributionUndeclared

	// UsageAttributionPerModel is a session at least one of whose usage
	// events names a model.
	UsageAttributionPerModel = usage.AttributionPerModel

	// UsageAttributionSessionTotal is a session whose kind guarantees no
	// model name.
	UsageAttributionSessionTotal = usage.AttributionSessionTotal

	// UsageAttributionNone is no figure to attribute.
	UsageAttributionNone = usage.AttributionNone
)

// UsageDisposition returns the arrival and attribution derived from the
// kind's UsageSources.
func (m AgentMeta) UsageDisposition() (UsageArrival, UsageAttribution) {
	return m.UsageSources.Disposition()
}

// MCPDialect names the tool-server configuration syntax the shared delivery
// renders for a kind's runtime. The empty value means undeclared.
type MCPDialect string

const (
	// MCPDialectUndeclared is the zero value.
	MCPDialectUndeclared MCPDialect = ""

	// MCPDialectNone declares that the kind's runtime receives no tool server.
	MCPDialectNone MCPDialect = "none"

	// MCPDialectMCPServersJSON is an {"mcpServers":{...}} document with ${VAR}
	// header references.
	MCPDialectMCPServersJSON MCPDialect = "mcp_servers_json"

	// MCPDialectMCPServersJSONAlwaysLoad is mcp_servers_json with Sortie's own
	// member marked "alwaysLoad".
	MCPDialectMCPServersJSONAlwaysLoad MCPDialect = "mcp_servers_json_always_load"

	// MCPDialectMCPServersTOML is one "-c mcp_servers.<name>=<inline table>"
	// argument pair per server.
	MCPDialectMCPServersTOML MCPDialect = "mcp_servers_toml"

	// MCPDialectMCPMemberJSON is the "mcp" member of the runtime's inline JSON
	// configuration, with {env:VAR} header references.
	MCPDialectMCPMemberJSON MCPDialect = "mcp_member_json"

	// MCPDialectACPSession is one Agent Client Protocol McpServer object per
	// server, sent in the session-creation request.
	MCPDialectACPSession MCPDialect = "acp_session"
)

// DeliversTools reports whether a kind declaring this dialect can reach
// Sortie's tool server. It is true for every dialect but MCPDialectUndeclared
// and MCPDialectNone, including a value outside the declared set.
func (d MCPDialect) DeliversTools() bool {
	return d != MCPDialectUndeclared && d != MCPDialectNone
}

// AgentConfigFields holds the config values passed to agent adapter
// validation functions. This is a plain data struct that avoids
// coupling the registry package to the config package.
type AgentConfigFields struct {
	// Kind is the agent adapter kind whose configuration is under
	// validation. It is the default agent.kind or a kind a dispatch
	// rule routes to.
	Kind string

	// Passthrough is the effective config map an adapter of this kind
	// receives at construction. Read-only: a validator MUST NOT
	// mutate it.
	Passthrough map[string]any
}

// Registry is a typed adapter registry mapping kind strings to
// constructor functions. Safe for concurrent use; registrations are
// expected during init and lookups happen at runtime.
//
// Registry is generic over the constructor function type T and the
// metadata type M to serve all adapter dimensions with a single
// implementation.
type Registry[T any, M any] struct {
	name     string
	reserved []string
	check    func(value T, meta M) error
	mu       sync.RWMutex
	adapters map[string]T
	meta     map[string]M
}

// NewRegistry creates an empty [Registry] with the given dimension
// name. The name appears in error messages produced by [Registry.Get]
// (e.g. "tracker", "agent"). Each reserved kind is refused by
// [Registry.Register] and [Registry.RegisterWithMeta].
func NewRegistry[T any, M any](name string, reserved ...string) *Registry[T, M] {
	return &Registry[T, M]{
		name:     name,
		reserved: slices.Clone(reserved),
		adapters: make(map[string]T),
		meta:     make(map[string]M),
	}
}

// NewCheckedRegistry creates an empty [Registry] like [NewRegistry] whose
// [Registry.Register] and [Registry.RegisterWithMeta] also run check on the
// value and metadata being registered. A nil check accepts every
// registration.
func NewCheckedRegistry[T any, M any](name string, check func(value T, meta M) error, reserved ...string) *Registry[T, M] {
	r := NewRegistry[T, M](name, reserved...)
	r.check = check
	return r
}

// Register associates a kind string with a constructor function.
// Panics if kind is empty, reserved, already registered, or refused by the
// registry's check, which receives the zero metadata. Registration is
// expected during init(); duplicate registration is a programming
// error. The zero value of M is stored as metadata so that [Meta]
// returns (zero, true) for kinds registered without explicit metadata.
func (r *Registry[T, M]) Register(kind string, constructor T) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if kind == "" {
		panic("registry: kind must not be empty")
	}
	if slices.Contains(r.reserved, kind) {
		panic(fmt.Sprintf("registry: kind %q is reserved", kind))
	}
	if _, exists := r.adapters[kind]; exists {
		panic(fmt.Sprintf("registry: duplicate registration for kind %q", kind))
	}
	var zero M
	r.panicIfRefused(kind, constructor, zero)
	r.adapters[kind] = constructor
	r.meta[kind] = zero
}

// RegisterWithMeta associates a kind string with a constructor and
// declared metadata. Panics on the same conditions as [Registry.Register],
// the check receiving meta.
func (r *Registry[T, M]) RegisterWithMeta(kind string, constructor T, meta M) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if kind == "" {
		panic("registry: kind must not be empty")
	}
	if slices.Contains(r.reserved, kind) {
		panic(fmt.Sprintf("registry: kind %q is reserved", kind))
	}
	if _, exists := r.adapters[kind]; exists {
		panic(fmt.Sprintf("registry: duplicate registration for kind %q", kind))
	}
	r.panicIfRefused(kind, constructor, meta)
	r.adapters[kind] = constructor
	r.meta[kind] = meta
}

// panicIfRefused panics when the registry's check rejects the registration.
func (r *Registry[T, M]) panicIfRefused(kind string, value T, meta M) {
	if r.check == nil {
		return
	}
	if err := r.check(value, meta); err != nil {
		panic(fmt.Sprintf("registry: %s kind %q refused: %v", r.name, kind, err))
	}
}

// Meta returns the metadata for the given kind and a boolean
// indicating whether the kind is registered. Returns (zero, false)
// for unknown kinds and (meta, true) for registered kinds.
func (r *Registry[T, M]) Meta(kind string) (M, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.meta[kind]
	return m, ok
}

// Get returns the constructor for the given kind, or a
// [*RegistryError] if the kind is not registered. The kind lookup is
// exact-match (case-sensitive).
func (r *Registry[T, M]) Get(kind string) (T, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if c, ok := r.adapters[kind]; ok {
		return c, nil
	}

	var zero T
	return zero, &RegistryError{
		Dimension: r.name,
		Kind:      kind,
		Available: r.sortedKinds(),
	}
}

// Kinds returns a sorted list of all registered kind strings. The
// returned slice is always non-nil.
func (r *Registry[T, M]) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sortedKinds()
}

// Has reports whether the given kind is registered. The lookup is
// exact-match (case-sensitive) and allocates nothing. Safe for
// concurrent use.
func (r *Registry[T, M]) Has(kind string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.adapters[kind]
	return ok
}

// sortedKinds collects and sorts the registered kind strings. The
// caller must hold r.mu (read or write). Always returns a non-nil slice.
func (r *Registry[T, M]) sortedKinds() []string {
	kinds := make([]string, 0, len(r.adapters))
	for k := range r.adapters {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return kinds
}

// RegistryError is returned by [Registry.Get] when the requested kind
// is not registered.
type RegistryError struct {
	// Dimension identifies the adapter dimension (e.g. "tracker" or
	// "agent").
	Dimension string

	// Kind is the requested adapter kind that was not found.
	Kind string

	// Available lists the registered kinds in sorted order.
	Available []string
}

// Error returns a human-readable diagnostic including the requested
// kind and available alternatives.
func (e *RegistryError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("unknown %s adapter kind %q; no adapters registered", e.Dimension, e.Kind)
	}
	return fmt.Sprintf("unknown %s adapter kind %q; registered: [%s]", e.Dimension, e.Kind, strings.Join(e.Available, ", "))
}
