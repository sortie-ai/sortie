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
}

// AgentMeta holds optional agent-adapter-declared properties queried
// by the orchestrator at preflight time. Zero value means no special
// requirements.
type AgentMeta struct {
	// RequiresCommand indicates the agent adapter requires a
	// non-empty agent.command config value.
	RequiresCommand bool
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
