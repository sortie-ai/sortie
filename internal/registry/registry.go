// Package registry provides typed adapter registries that map kind
// strings to constructor functions. Start with [Trackers] for the
// default tracker adapter registry and [Agents] for the default agent
// adapter registry.
package registry

import (
	"fmt"
	"sort"
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
var Trackers = NewRegistry[TrackerConstructor]("tracker")

// AgentConstructor creates a [domain.AgentAdapter] from opaque
// adapter-specific configuration. The config parameter is the raw map
// from the adapter's pass-through config sub-object. Implementations
// must validate their config and return an error if required fields
// are missing.
type AgentConstructor func(config map[string]any) (domain.AgentAdapter, error)

// Agents is the default agent adapter registry. Adapter packages
// register themselves via [Registry.Register] in their init functions;
// the orchestrator resolves adapters via [Registry.Get] at runtime.
var Agents = NewRegistry[AgentConstructor]("agent")

// AdapterMeta holds optional adapter-declared properties queried by
// the orchestrator at preflight time. Zero value means no special
// requirements.
type AdapterMeta struct {
	// RequiresProject indicates the tracker adapter requires a
	// non-empty tracker.project config value.
	RequiresProject bool

	// RequiresAPIKey indicates the tracker adapter requires a
	// non-empty tracker.api_key config value.
	RequiresAPIKey bool

	// RequiresCommand indicates the agent adapter requires a
	// non-empty agent.command config value.
	RequiresCommand bool
}

// Registry is a typed adapter registry mapping kind strings to
// constructor functions. Safe for concurrent use; registrations are
// expected during init and lookups happen at runtime.
//
// Registry is generic over the constructor function type to serve
// both tracker and agent dimensions with a single implementation.
type Registry[T any] struct {
	name     string
	mu       sync.RWMutex
	adapters map[string]T
	meta     map[string]AdapterMeta
}

// NewRegistry creates an empty [Registry] with the given dimension
// name. The name appears in error messages produced by [Registry.Get]
// (e.g. "tracker", "agent").
func NewRegistry[T any](name string) *Registry[T] {
	return &Registry[T]{
		name:     name,
		adapters: make(map[string]T),
		meta:     make(map[string]AdapterMeta),
	}
}

// Register associates a kind string with a constructor function.
// Panics if kind is empty or already registered. Registration is
// expected during init(); duplicate registration is a programming
// error.
func (r *Registry[T]) Register(kind string, constructor T) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if kind == "" {
		panic("registry: kind must not be empty")
	}
	if _, exists := r.adapters[kind]; exists {
		panic(fmt.Sprintf("registry: duplicate registration for kind %q", kind))
	}
	r.adapters[kind] = constructor
}

// RegisterWithMeta associates a kind string with a constructor and
// declared metadata. Panics on the same conditions as [Register].
func (r *Registry[T]) RegisterWithMeta(kind string, constructor T, meta AdapterMeta) {
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

// Meta returns the metadata for the given kind. Returns zero-value
// [AdapterMeta] if the kind is not registered or was registered
// without metadata.
func (r *Registry[T]) Meta(kind string) AdapterMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.meta[kind]
}

// Get returns the constructor for the given kind, or a
// [*RegistryError] if the kind is not registered. The kind lookup is
// exact-match (case-sensitive).
func (r *Registry[T]) Get(kind string) (T, error) {
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
func (r *Registry[T]) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sortedKinds()
}

// sortedKinds collects and sorts the registered kind strings. The
// caller must hold r.mu (read or write).
func (r *Registry[T]) sortedKinds() []string {
	kinds := make([]string, 0, len(r.adapters))
	for k := range r.adapters {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
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
