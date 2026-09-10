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

// AgentConstructor creates a [domain.AgentAdapter] from opaque
// adapter-specific configuration. The config parameter is the raw map
// from the adapter's pass-through config sub-object. Implementations
// must validate their config and return an error if required fields
// are missing.
type AgentConstructor func(config map[string]any) (domain.AgentAdapter, error)

// Agents is the default agent adapter registry. Adapter packages
// register themselves via [Registry.Register] in their init functions;
// the orchestrator resolves adapters via [Registry.Get] at runtime.
var Agents = NewRegistry[AgentConstructor, AgentMeta]("agent")

// CIProviderConstructor creates a [domain.CIStatusProvider] from
// a maximum log-line count and opaque adapter-specific configuration.
// The maxLogLines parameter controls how many tail lines of CI log
// output to include for failing checks (0 disables log fetching).
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
// the sidecar resolves backends via [Registry.Get] at runtime.
var Notifiers = NewRegistry[NotifierConstructor, struct{}]("notifier")

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

	// DefaultActiveStates is the active-state list the tracker adapter
	// applies when tracker.active_states is absent or empty. An empty or
	// nil value means the adapter applies no default. The slice may alias
	// the registering adapter's own package-level variable, so callers
	// must treat it as read-only and copy before sorting or filtering.
	DefaultActiveStates []string

	// DefaultTerminalStates is the terminal-state list the tracker
	// adapter applies when tracker.terminal_states is absent or empty. An
	// empty or nil value means the adapter applies no default. The slice
	// may alias the registering adapter's own package-level variable, so
	// callers must treat it as read-only and copy before sorting or
	// filtering.
	DefaultTerminalStates []string

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

// AgentMeta holds optional agent-adapter-declared properties queried
// by the orchestrator at preflight time. Zero value means no special
// requirements.
type AgentMeta struct {
	// RequiresCommand indicates the agent adapter requires a
	// non-empty agent.command config value.
	RequiresCommand bool

	// ValidateAgentConfig is an optional function the preflight
	// pipeline calls to run agent-specific config validation. Nil
	// means no adapter-specific validation.
	ValidateAgentConfig func(fields AgentConfigFields) []ValidationDiag

	// MCPInjection declares what this adapter does with the
	// worker-generated MCP config path today, not what the underlying
	// CLI is capable of. The empty value means undeclared.
	MCPInjection MCPInjection

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

	// UsageArrival and UsageAttribution declare the disposition of a
	// locally launched session of this kind, constructed from an
	// empty passthrough. The empty values mean undeclared.
	UsageArrival     UsageArrival
	UsageAttribution UsageAttribution

	// UsageSessionRules are evaluated in order; the first rule whose
	// When reports true supplies the pair. Empty means the declared
	// pair holds for every session of this kind. UsageSessionRules
	// MUST leave the declared pair reachable: at least one
	// (passthrough, remote) combination MUST match none of the
	// rules. This is what keeps a conformance assertion's coverage
	// requirement satisfiable for every registry-conformant kind,
	// including one whose rules are numerous.
	UsageSessionRules []UsageSessionRule
}

// UsageArrival declares when a usage figure for the agent session
// becomes available to the orchestrator. The empty value means
// undeclared.
type UsageArrival string

const (
	// UsageArrivalUndeclared is the zero value: the adapter has not
	// declared when its usage figures arrive.
	UsageArrivalUndeclared UsageArrival = ""

	// UsageArrivalIncremental declares that the adapter emits one
	// domain.EventTokenUsage carrying the run-cumulative figure per
	// model API request the turn makes, while the turn's work is
	// still in flight. A turn making N requests delivers N events.
	// api_request_count is a request count for a session of this
	// arrival only once a figure has arrived: a runtime that stops
	// delivering the per-request event leaves the count at zero
	// while the declaration still promises one.
	UsageArrivalIncremental UsageArrival = "incremental"

	// UsageArrivalTurnEnd declares that the adapter emits at most
	// one domain.EventTokenUsage per turn, and only after the
	// turn's work is over. api_request_count is not a count of API
	// requests for a kind declaring this value.
	UsageArrivalTurnEnd UsageArrival = "turn_end"

	// UsageArrivalNone declares that no usage figure is ever
	// produced: no domain.EventTokenUsage event, no non-zero
	// domain.TokenUsage on any event, domain.TurnResult.UsageMeasured
	// false on every turn.
	UsageArrivalNone UsageArrival = "none"
)

// UsageAttribution declares what a usage figure from this adapter
// attributes to. The empty value means undeclared.
type UsageAttribution string

const (
	// UsageAttributionUndeclared is the zero value: the adapter has
	// not declared what its usage figures attribute to.
	UsageAttributionUndeclared UsageAttribution = ""

	// UsageAttributionPerModel declares that at least one event
	// carrying a non-zero Usage also carries a non-empty Model, so a
	// figure can be attributed to the model that produced it.
	UsageAttributionPerModel UsageAttribution = "per_model"

	// UsageAttributionSessionTotal declares that no event carrying a
	// non-zero Usage carries a Model: figures are session-level
	// totals with no model attribution.
	UsageAttributionSessionTotal UsageAttribution = "session_total"

	// UsageAttributionNone declares that there is no figure to
	// attribute.
	UsageAttributionNone UsageAttribution = "none"
)

// UsageSessionRule states the pair in force for a session that meets
// a condition the declared pair does not describe. When reads only
// the resolved adapter passthrough and the launch mode; it reaches no
// adapter state and performs no I/O, MUST NOT retain or mutate the
// passthrough map, and MUST NOT close over adapter state.
type UsageSessionRule struct {
	When        func(passthrough map[string]any, remote bool) bool
	Arrival     UsageArrival
	Attribution UsageAttribution
}

// UsageDisposition returns the arrival and attribution in force for
// one session of this kind, given its launch mode and its resolved
// adapter passthrough. remote is true when the session runs over SSH.
// It applies the first UsageSessionRules entry whose When reports
// true, and returns the declared pair when no rule matches.
//
// A resolved pair MUST satisfy INV-1 (arrival is UsageArrivalNone if
// and only if attribution is UsageAttributionNone) and MUST satisfy
// INV-3 (every resolvable pair, declared or ruled, names a value
// inside the declared set). A declaration reflects the code path
// that always runs, not one a runtime release can starve (INV-2).
func (m AgentMeta) UsageDisposition(passthrough map[string]any, remote bool) (UsageArrival, UsageAttribution) {
	for _, rule := range m.UsageSessionRules {
		if rule.When != nil && rule.When(passthrough, remote) {
			return rule.Arrival, rule.Attribution
		}
	}
	return m.UsageArrival, m.UsageAttribution
}

// ReportsDuringTurn reports whether a figure of this arrival reaches
// the orchestrator while a turn is running. True for
// UsageArrivalIncremental; false for every other value, including a
// value outside the declared set.
func (a UsageArrival) ReportsDuringTurn() bool {
	return a == UsageArrivalIncremental
}

// ReportsAnyFigure reports whether a session of this arrival ever
// produces a usage figure. True for UsageArrivalIncremental and
// UsageArrivalTurnEnd; false for every other value, including a value
// outside the declared set.
func (a UsageArrival) ReportsAnyFigure() bool {
	return a == UsageArrivalIncremental || a == UsageArrivalTurnEnd
}

// NamesModel reports whether a figure of this attribution names the
// model that produced it. True for UsageAttributionPerModel; false
// for every other value, including a value outside the declared set.
func (t UsageAttribution) NamesModel() bool {
	return t == UsageAttributionPerModel
}

// MCPInjection declares what an agent adapter does with the
// worker-generated MCP config path. The value describes what the
// adapter does with the path today, not what the underlying CLI is
// capable of, and the empty value means undeclared.
type MCPInjection string

const (
	// MCPInjectionUndeclared is the zero value: the adapter has not
	// declared a disposition toward the generated MCP config path.
	MCPInjectionUndeclared MCPInjection = ""

	// MCPInjectionSupported declares that the adapter hands the
	// generated MCP config path to the agent process.
	MCPInjectionSupported MCPInjection = "supported"

	// MCPInjectionTranslated declares that the adapter re-expresses
	// the servers declared in the generated configuration in the
	// form its runtime parses, and delivers that to the agent
	// process.
	MCPInjectionTranslated MCPInjection = "translated"

	// MCPInjectionUnsupported declares that the adapter never hands
	// the generated MCP config path to the agent process.
	MCPInjectionUnsupported MCPInjection = "unsupported"
)

// DeliversTools reports whether a session of this disposition,
// launched in the given mode, can reach the servers declared in the
// generated configuration. remote is true when the session runs over
// SSH. It is total over the value set: true for
// MCPInjectionSupported; true for MCPInjectionTranslated only when
// remote is false; false for every other value, including a value
// outside the declared set.
func (m MCPInjection) DeliversTools(remote bool) bool {
	switch m {
	case MCPInjectionSupported:
		return true
	case MCPInjectionTranslated:
		return !remote
	default:
		return false
	}
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
	mu       sync.RWMutex
	adapters map[string]T
	meta     map[string]M
}

// NewRegistry creates an empty [Registry] with the given dimension
// name. The name appears in error messages produced by [Registry.Get]
// (e.g. "tracker", "agent").
func NewRegistry[T any, M any](name string) *Registry[T, M] {
	return &Registry[T, M]{
		name:     name,
		adapters: make(map[string]T),
		meta:     make(map[string]M),
	}
}

// Register associates a kind string with a constructor function.
// Panics if kind is empty or already registered. Registration is
// expected during init(); duplicate registration is a programming
// error. The zero value of M is stored as metadata so that [Meta]
// returns (zero, true) for kinds registered without explicit metadata.
func (r *Registry[T, M]) Register(kind string, constructor T) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if kind == "" {
		panic("registry: kind must not be empty")
	}
	if _, exists := r.adapters[kind]; exists {
		panic(fmt.Sprintf("registry: duplicate registration for kind %q", kind))
	}
	r.adapters[kind] = constructor
	var zero M
	r.meta[kind] = zero
}

// RegisterWithMeta associates a kind string with a constructor and
// declared metadata. Panics on the same conditions as [Register].
func (r *Registry[T, M]) RegisterWithMeta(kind string, constructor T, meta M) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if kind == "" {
		panic("registry: kind must not be empty")
	}
	if _, exists := r.adapters[kind]; exists {
		panic(fmt.Sprintf("registry: duplicate registration for kind %q", kind))
	}
	r.adapters[kind] = constructor
	r.meta[kind] = meta
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
