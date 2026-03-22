package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- Test helpers ---

type testConstructor func() string

func newTestRegistry() *Registry[testConstructor] {
	return NewRegistry[testConstructor]("test")
}

func dummyConstructor() testConstructor {
	return func() string { return "marker" }
}

// --- Tests ---

func TestRegisterAndGet(t *testing.T) {
	t.Parallel()

	r := newTestRegistry()
	r.Register("alpha", dummyConstructor())

	got, err := r.Get("alpha")
	if err != nil {
		t.Fatalf("Get(%q) unexpected error: %v", "alpha", err)
	}
	if got() != "marker" {
		t.Errorf("Get(%q)() = %q, want %q", "alpha", got(), "marker")
	}
}

func TestGet_UnknownKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		register      []string
		lookup        string
		wantAvailable []string
		wantMsgPart   string
	}{
		{
			name:          "empty registry",
			register:      nil,
			lookup:        "nonexistent",
			wantAvailable: []string{},
			wantMsgPart:   "no adapters registered",
		},
		{
			name:          "lists available kinds sorted",
			register:      []string{"beta", "alpha"},
			lookup:        "gamma",
			wantAvailable: []string{"alpha", "beta"},
			wantMsgPart:   "alpha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTestRegistry()
			for _, k := range tt.register {
				r.Register(k, dummyConstructor())
			}

			_, err := r.Get(tt.lookup)
			if err == nil {
				t.Fatalf("Get(%q) returned nil error, want *RegistryError", tt.lookup)
			}

			var re *RegistryError
			if !errors.As(err, &re) {
				t.Fatalf("Get(%q) error type = %T, want *RegistryError", tt.lookup, err)
			}
			if re.Dimension != "test" {
				t.Errorf("RegistryError.Dimension = %q, want %q", re.Dimension, "test")
			}
			if re.Kind != tt.lookup {
				t.Errorf("RegistryError.Kind = %q, want %q", re.Kind, tt.lookup)
			}
			if re.Available == nil {
				t.Fatal("RegistryError.Available is nil, want non-nil slice")
			}
			if len(re.Available) != len(tt.wantAvailable) {
				t.Fatalf("RegistryError.Available = %v, want %v", re.Available, tt.wantAvailable)
			}
			for i, v := range tt.wantAvailable {
				if re.Available[i] != v {
					t.Errorf("RegistryError.Available[%d] = %q, want %q", i, re.Available[i], v)
				}
			}
			if msg := err.Error(); !strings.Contains(msg, tt.wantMsgPart) {
				t.Errorf("Error() = %q, want it to contain %q", msg, tt.wantMsgPart)
			}
		})
	}
}

func TestRegister_Panics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    string
		setup   func(*Registry[testConstructor])
		wantMsg string
	}{
		{
			name:    "empty kind",
			kind:    "",
			setup:   nil,
			wantMsg: "empty",
		},
		{
			name: "duplicate kind",
			kind: "dup",
			setup: func(r *Registry[testConstructor]) {
				r.Register("dup", dummyConstructor())
			},
			wantMsg: "duplicate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTestRegistry()
			if tt.setup != nil {
				tt.setup(r)
			}

			defer func() {
				v := recover()
				if v == nil {
					t.Fatalf("Register(%q) did not panic", tt.kind)
				}
				msg := fmt.Sprint(v)
				if !strings.Contains(msg, tt.wantMsg) {
					t.Errorf("Register(%q) panic = %q, want it to contain %q", tt.kind, msg, tt.wantMsg)
				}
			}()

			r.Register(tt.kind, dummyConstructor())
		})
	}
}

func TestKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		register []string
		want     []string
	}{
		{
			name:     "empty registry",
			register: nil,
			want:     []string{},
		},
		{
			name:     "returns sorted",
			register: []string{"zulu", "alpha", "mike"},
			want:     []string{"alpha", "mike", "zulu"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTestRegistry()
			for _, k := range tt.register {
				r.Register(k, dummyConstructor())
			}

			got := r.Kinds()
			if got == nil {
				t.Fatal("Kinds() returned nil, want non-nil slice")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Kinds() = %v, want %v", got, tt.want)
			}
			for i, v := range tt.want {
				if got[i] != v {
					t.Errorf("Kinds()[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

func TestRegisterWithMeta_AndMeta(t *testing.T) {
	t.Parallel()

	t.Run("stores and retrieves metadata", func(t *testing.T) {
		t.Parallel()

		r := newTestRegistry()
		r.RegisterWithMeta("alpha", dummyConstructor(), AdapterMeta{
			RequiresProject: true,
			RequiresCommand: true,
		})

		got := r.Meta("alpha")
		if !got.RequiresProject {
			t.Errorf("Meta(%q).RequiresProject = false, want true", "alpha")
		}
		if !got.RequiresCommand {
			t.Errorf("Meta(%q).RequiresCommand = false, want true", "alpha")
		}
	})

	t.Run("Get returns constructor from RegisterWithMeta", func(t *testing.T) {
		t.Parallel()

		r := newTestRegistry()
		r.RegisterWithMeta("beta", dummyConstructor(), AdapterMeta{RequiresProject: true})

		got, err := r.Get("beta")
		if err != nil {
			t.Fatalf("Get(%q) unexpected error: %v", "beta", err)
		}
		if got() != "marker" {
			t.Errorf("Get(%q)() = %q, want %q", "beta", got(), "marker")
		}
	})

	t.Run("plain Register returns zero-value meta", func(t *testing.T) {
		t.Parallel()

		r := newTestRegistry()
		r.Register("gamma", dummyConstructor())

		got := r.Meta("gamma")
		if got.RequiresProject {
			t.Errorf("Meta(%q).RequiresProject = true, want false for plain Register", "gamma")
		}
		if got.RequiresCommand {
			t.Errorf("Meta(%q).RequiresCommand = true, want false for plain Register", "gamma")
		}
	})

	t.Run("unregistered kind returns zero-value meta", func(t *testing.T) {
		t.Parallel()

		r := newTestRegistry()

		got := r.Meta("nonexistent")
		if got.RequiresProject {
			t.Errorf("Meta(%q).RequiresProject = true, want false for unregistered kind", "nonexistent")
		}
		if got.RequiresCommand {
			t.Errorf("Meta(%q).RequiresCommand = true, want false for unregistered kind", "nonexistent")
		}
	})
}

func TestRegisterWithMeta_Panics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    string
		setup   func(*Registry[testConstructor])
		wantMsg string
	}{
		{
			name:    "empty kind",
			kind:    "",
			setup:   nil,
			wantMsg: "empty",
		},
		{
			name: "duplicate kind",
			kind: "dup",
			setup: func(r *Registry[testConstructor]) {
				r.RegisterWithMeta("dup", dummyConstructor(), AdapterMeta{})
			},
			wantMsg: "duplicate",
		},
		{
			name: "duplicate kind across Register and RegisterWithMeta",
			kind: "dup",
			setup: func(r *Registry[testConstructor]) {
				r.Register("dup", dummyConstructor())
			},
			wantMsg: "duplicate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTestRegistry()
			if tt.setup != nil {
				tt.setup(r)
			}

			defer func() {
				v := recover()
				if v == nil {
					t.Fatalf("RegisterWithMeta(%q) did not panic", tt.kind)
				}
				msg := fmt.Sprint(v)
				if !strings.Contains(msg, tt.wantMsg) {
					t.Errorf("RegisterWithMeta(%q) panic = %q, want it to contain %q", tt.kind, msg, tt.wantMsg)
				}
			}()

			r.RegisterWithMeta(tt.kind, dummyConstructor(), AdapterMeta{RequiresProject: true})
		})
	}
}

func TestRegistryError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *RegistryError
		want string
	}{
		{
			name: "no available adapters",
			err: &RegistryError{
				Dimension: "tracker",
				Kind:      "linear",
				Available: []string{},
			},
			want: `unknown tracker adapter kind "linear"; no adapters registered`,
		},
		{
			name: "with available adapters",
			err: &RegistryError{
				Dimension: "agent",
				Kind:      "foo",
				Available: []string{"claude-code", "mock"},
			},
			want: `unknown agent adapter kind "foo"; registered: [claude-code, mock]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.err.Error(); got != tt.want {
				t.Errorf("RegistryError.Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Type-specific constructor tests ---

var _ domain.TrackerAdapter = (*mockTrackerAdapter)(nil)

type mockTrackerAdapter struct{}

func (m *mockTrackerAdapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (m *mockTrackerAdapter) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}

func TestTrackerRegistry(t *testing.T) {
	t.Parallel()

	r := NewRegistry[TrackerConstructor]("tracker")
	r.Register("mock", func(_ map[string]any) (domain.TrackerAdapter, error) {
		return &mockTrackerAdapter{}, nil
	})

	constructor, err := r.Get("mock")
	if err != nil {
		t.Fatalf("Get(%q) unexpected error: %v", "mock", err)
	}

	adapter, err := constructor(nil)
	if err != nil {
		t.Fatalf("TrackerConstructor() unexpected error: %v", err)
	}
	if adapter == nil {
		t.Fatal("TrackerConstructor() returned nil adapter")
	}
}

var _ domain.AgentAdapter = (*mockAgentAdapter)(nil)

type mockAgentAdapter struct{}

func (m *mockAgentAdapter) StartSession(_ context.Context, _ domain.StartSessionParams) (domain.Session, error) {
	return domain.Session{}, nil
}

func (m *mockAgentAdapter) RunTurn(_ context.Context, _ domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
	return domain.TurnResult{}, nil
}

func (m *mockAgentAdapter) StopSession(_ context.Context, _ domain.Session) error {
	return nil
}

func (m *mockAgentAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}

func TestAgentRegistry(t *testing.T) {
	t.Parallel()

	r := NewRegistry[AgentConstructor]("agent")
	r.Register("mock", func(_ map[string]any) (domain.AgentAdapter, error) {
		return &mockAgentAdapter{}, nil
	})

	constructor, err := r.Get("mock")
	if err != nil {
		t.Fatalf("Get(%q) unexpected error: %v", "mock", err)
	}

	adapter, err := constructor(nil)
	if err != nil {
		t.Fatalf("AgentConstructor() unexpected error: %v", err)
	}
	if adapter == nil {
		t.Fatal("AgentConstructor() returned nil adapter")
	}
}

func TestPackageLevelRegistries(t *testing.T) {
	t.Parallel()

	t.Run("Trackers", func(t *testing.T) {
		t.Parallel()

		if Trackers == nil {
			t.Fatal("Trackers is nil")
		}
		if kinds := Trackers.Kinds(); kinds == nil {
			t.Fatal("Trackers.Kinds() returned nil, want non-nil slice")
		}
	})

	t.Run("Agents", func(t *testing.T) {
		t.Parallel()

		if Agents == nil {
			t.Fatal("Agents is nil")
		}
		if kinds := Agents.Kinds(); kinds == nil {
			t.Fatal("Agents.Kinds() returned nil, want non-nil slice")
		}
	})
}
