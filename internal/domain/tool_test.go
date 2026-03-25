package domain

import (
	"context"
	"encoding/json"
	"testing"
)

// stubTool is a minimal AgentTool implementation for registry tests.
type stubTool struct {
	name string
}

var _ AgentTool = (*stubTool)(nil)

func (s *stubTool) Name() string                 { return s.name }
func (s *stubTool) Description() string          { return "stub " + s.name }
func (s *stubTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func TestNewToolRegistry(t *testing.T) {
	t.Parallel()

	reg := NewToolRegistry()
	if reg.Len() != 0 {
		t.Errorf("NewToolRegistry().Len() = %d, want 0", reg.Len())
	}
	if tools := reg.List(); len(tools) != 0 {
		t.Errorf("NewToolRegistry().List() has %d items, want 0", len(tools))
	}
}

func TestToolRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()

	reg := NewToolRegistry()
	tool := &stubTool{name: "my_tool"}
	reg.Register(tool)

	got, ok := reg.Get("my_tool")
	if !ok {
		t.Fatal("Get(\"my_tool\") returned false, want true")
	}
	if got.Name() != "my_tool" {
		t.Errorf("Get(\"my_tool\").Name() = %q, want %q", got.Name(), "my_tool")
	}
}

func TestToolRegistry_GetNotFound(t *testing.T) {
	t.Parallel()

	reg := NewToolRegistry()
	got, ok := reg.Get("nonexistent")
	if ok {
		t.Errorf("Get(\"nonexistent\") = (%v, true), want (nil, false)", got)
	}
	if got != nil {
		t.Errorf("Get(\"nonexistent\") returned non-nil tool, want nil")
	}
}

func TestToolRegistry_List(t *testing.T) {
	t.Parallel()

	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "alpha"})
	reg.Register(&stubTool{name: "beta"})

	tools := reg.List()
	if len(tools) != 2 {
		t.Fatalf("List() returned %d tools, want 2", len(tools))
	}

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name()] = true
	}
	if !names["alpha"] {
		t.Error("List() missing tool \"alpha\"")
	}
	if !names["beta"] {
		t.Error("List() missing tool \"beta\"")
	}
}

func TestToolRegistry_Len(t *testing.T) {
	t.Parallel()

	reg := NewToolRegistry()
	if reg.Len() != 0 {
		t.Errorf("Len() = %d after construction, want 0", reg.Len())
	}

	reg.Register(&stubTool{name: "a"})
	if reg.Len() != 1 {
		t.Errorf("Len() = %d after 1 register, want 1", reg.Len())
	}

	reg.Register(&stubTool{name: "b"})
	if reg.Len() != 2 {
		t.Errorf("Len() = %d after 2 registers, want 2", reg.Len())
	}
}

func TestToolRegistry_DuplicateRegisterPanics(t *testing.T) {
	t.Parallel()

	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "dup"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register duplicate did not panic, want panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string", r)
		}
		if msg != "duplicate tool registration: dup" {
			t.Errorf("panic message = %q, want %q", msg, "duplicate tool registration: dup")
		}
	}()

	reg.Register(&stubTool{name: "dup"})
}

func TestToolRegistry_ListIsSnapshot(t *testing.T) {
	t.Parallel()

	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "x"})

	snapshot := reg.List()
	reg.Register(&stubTool{name: "y"})

	if len(snapshot) != 1 {
		t.Errorf("snapshot length changed to %d after subsequent Register, want 1", len(snapshot))
	}
}
