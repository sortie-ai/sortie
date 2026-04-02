package domain

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
)

// AgentTool defines a client-side tool that Sortie exposes to agents
// during sessions. Tools are instantiated per-orchestrator and scoped
// per-session via the [ToolRegistry]. The orchestrator advertises
// registered tools in the agent prompt and dispatches tool calls to
// the matching implementation.
type AgentTool interface {
	// Name returns the stable tool identifier used for matching
	// tool call requests to implementations. Must be unique within
	// a [ToolRegistry].
	Name() string

	// Description returns a human-readable summary of what the tool
	// does, suitable for inclusion in agent prompts.
	Description() string

	// InputSchema returns the JSON Schema describing the tool's
	// expected input format. Used for MCP tool registration and
	// prompt-based documentation.
	InputSchema() json.RawMessage

	// Execute runs the tool with the given JSON input and returns
	// a structured JSON result. The returned [json.RawMessage] is
	// always a structured response for the agent. The error return
	// is reserved for internal failures (nil adapter, marshal
	// failure) — domain-level errors are encoded in the JSON result.
	Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// ToolRegistry holds the set of tools available to agent sessions.
// Safe for concurrent reads after construction. Do not call [Register]
// after passing the registry to the orchestrator — concurrent
// Register + Get is a data race.
type ToolRegistry struct {
	tools map[string]AgentTool
}

// NewToolRegistry returns an empty [ToolRegistry].
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]AgentTool)}
}

// Register adds a tool to the registry. Panics if tool is nil, has
// an empty name, or a tool with the same name is already registered
// (programming error, not runtime input).
func (r *ToolRegistry) Register(tool AgentTool) {
	if tool == nil {
		panic("tool registration: tool must not be nil")
	}
	name := tool.Name()
	if name == "" {
		panic("tool registration: tool name must not be empty")
	}
	if _, exists := r.tools[name]; exists {
		panic("duplicate tool registration: " + name)
	}
	r.tools[name] = tool
}

// Get returns the tool with the given name and true, or nil and
// false if no tool is registered under that name.
func (r *ToolRegistry) Get(name string) (AgentTool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List returns all registered tools sorted by name. The returned
// slice is a snapshot; mutations to the registry after List returns
// are not reflected.
func (r *ToolRegistry) List() []AgentTool {
	registered := make([]AgentTool, 0, len(r.tools))
	for _, t := range r.tools {
		registered = append(registered, t)
	}
	slices.SortFunc(registered, func(a, b AgentTool) int {
		return cmp.Compare(a.Name(), b.Name())
	})
	return registered
}

// Len returns the number of registered tools.
func (r *ToolRegistry) Len() int {
	return len(r.tools)
}
