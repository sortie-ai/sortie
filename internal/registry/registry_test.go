package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

type testConstructor func() string

type testMeta struct {
	RequiresProject bool
	RequiresCommand bool
}

func newTestRegistry() *Registry[testConstructor, testMeta] {
	return NewRegistry[testConstructor, testMeta]("test")
}

func dummyConstructor() testConstructor {
	return func() string { return "marker" }
}

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
		setup   func(*Registry[testConstructor, testMeta])
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
			setup: func(r *Registry[testConstructor, testMeta]) {
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

func TestRegister_ReservedKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		register func()
	}{
		{
			name: "Register on a registry that reserves the kind",
			register: func() {
				NewRegistry[testConstructor, testMeta]("test", "builtin").Register("builtin", dummyConstructor())
			},
		},
		{
			name: "RegisterWithMeta on a registry that reserves the kind",
			register: func() {
				NewRegistry[testConstructor, testMeta]("test", "builtin").RegisterWithMeta("builtin", dummyConstructor(), testMeta{})
			},
		},
		{
			name: "the notifier registry reserves the tracker comment kind",
			register: func() {
				Notifiers.Register(domain.TrackerCommentKind, func(map[string]any) (domain.Notifier, error) { return nil, nil })
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				v := recover()
				if v == nil {
					t.Fatalf("%s did not panic", tt.name)
				}
				if msg := fmt.Sprint(v); !strings.Contains(msg, "reserved") {
					t.Errorf("%s panic = %q, want it to contain %q", tt.name, msg, "reserved")
				}
			}()

			tt.register()
		})
	}
}

func TestRegister_UnreservedKindOnReservingRegistry(t *testing.T) {
	t.Parallel()

	r := NewRegistry[testConstructor, testMeta]("test", "builtin")

	r.Register("other", dummyConstructor())

	if _, err := r.Get("other"); err != nil {
		t.Errorf("Get(%q) error = %v, want nil after registering an unreserved kind", "other", err)
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
		r.RegisterWithMeta("alpha", dummyConstructor(), testMeta{
			RequiresProject: true,
			RequiresCommand: true,
		})

		got, ok := r.Meta("alpha")
		if !ok {
			t.Fatalf("Meta(%q) ok = false, want true", "alpha")
		}
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
		r.RegisterWithMeta("beta", dummyConstructor(), testMeta{RequiresProject: true})

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

		got, ok := r.Meta("gamma")
		if !ok {
			t.Fatalf("Meta(%q) ok = false, want true for plain Register", "gamma")
		}
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

		got, ok := r.Meta("nonexistent")
		if ok {
			t.Errorf("Meta(%q) ok = true, want false for unregistered kind", "nonexistent")
		}
		if got.RequiresProject {
			t.Errorf("Meta(%q).RequiresProject = true, want false for unregistered kind", "nonexistent")
		}
		if got.RequiresCommand {
			t.Errorf("Meta(%q).RequiresCommand = true, want false for unregistered kind", "nonexistent")
		}
	})
}

func dummyAgentConstructor() AgentConstructor {
	return func() (domain.AgentAdapter, error) { return nil, nil }
}

// TestRegisterWithMeta_AndMeta_AgentValidateAgentConfig pins that
// AgentMeta.ValidateAgentConfig round-trips through RegisterWithMeta and
// Meta, mirroring TestRegisterWithMeta_AndMeta for the real AgentMeta
// type rather than the generic testMeta fixture.
func TestRegisterWithMeta_AndMeta_AgentValidateAgentConfig(t *testing.T) {
	t.Parallel()

	t.Run("RegisterWithMeta with a non-nil ValidateAgentConfig round-trips through Meta", func(t *testing.T) {
		t.Parallel()

		r := NewRegistry[AgentConstructor, AgentMeta]("agent")
		validate := func(fields AgentConfigFields) []ValidationDiag {
			return []ValidationDiag{{Severity: "error", Check: "marker.check", Message: "marker " + fields.Kind}}
		}
		r.RegisterWithMeta("alpha", dummyAgentConstructor(), AgentMeta{ValidateAgentConfig: validate})

		got, ok := r.Meta("alpha")
		if !ok {
			t.Fatalf("Meta(%q) ok = false, want true", "alpha")
		}
		if got.ValidateAgentConfig == nil {
			t.Fatalf("Meta(%q).ValidateAgentConfig = nil, want non-nil", "alpha")
		}

		diags := got.ValidateAgentConfig(AgentConfigFields{Kind: "alpha"})
		if len(diags) != 1 || diags[0].Check != "marker.check" || diags[0].Message != "marker alpha" {
			t.Errorf("Meta(%q).ValidateAgentConfig(...) = %+v, want the registered function's own output", "alpha", diags)
		}
	})

	t.Run("plain Register reports nil ValidateAgentConfig", func(t *testing.T) {
		t.Parallel()

		r := NewRegistry[AgentConstructor, AgentMeta]("agent")
		r.Register("mock", dummyAgentConstructor())

		got, ok := r.Meta("mock")
		if !ok {
			t.Fatalf("Meta(%q) ok = false, want true for plain Register", "mock")
		}
		if got.ValidateAgentConfig != nil {
			t.Errorf("Meta(%q).ValidateAgentConfig = non-nil, want nil for plain Register", "mock")
		}
	})
}

func TestRegisterWithMeta_Panics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    string
		setup   func(*Registry[testConstructor, testMeta])
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
			setup: func(r *Registry[testConstructor, testMeta]) {
				r.RegisterWithMeta("dup", dummyConstructor(), testMeta{})
			},
			wantMsg: "duplicate",
		},
		{
			name: "duplicate kind across Register and RegisterWithMeta",
			kind: "dup",
			setup: func(r *Registry[testConstructor, testMeta]) {
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

			r.RegisterWithMeta(tt.kind, dummyConstructor(), testMeta{RequiresProject: true})
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

var _ domain.Notifier = (*stubNotifier)(nil)

type stubNotifier struct{}

func (s *stubNotifier) Send(_ context.Context, _ domain.Notification) error { return nil }

var _ domain.TrackerAdapter = (*mockTrackerAdapter)(nil)

type mockTrackerAdapter struct{}

func (m *mockTrackerAdapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) SetTrackerStates(func() domain.TrackerStates) {}

func (m *mockTrackerAdapter) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (m *mockTrackerAdapter) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesAndLabelsByIDs(context.Context, []string) (map[string]domain.IssueStateAndLabels, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) TransitionIssue(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockTrackerAdapter) CommentIssue(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockTrackerAdapter) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockTrackerAdapter) RemoveLabel(_ context.Context, _ string, _ string) error { return nil }

func TestTrackerRegistry(t *testing.T) {
	t.Parallel()

	r := NewRegistry[TrackerConstructor, TrackerMeta]("tracker")
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

func TestAgentRegistry(t *testing.T) {
	t.Parallel()

	r := NewRegistry[AgentConstructor, AgentMeta]("agent")
	r.Register("mock", func() (domain.AgentAdapter, error) {
		return &mockAgentAdapter{}, nil
	})

	constructor, err := r.Get("mock")
	if err != nil {
		t.Fatalf("Get(%q) unexpected error: %v", "mock", err)
	}

	adapter, err := constructor()
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

	t.Run("CIProviders", func(t *testing.T) {
		t.Parallel()

		if CIProviders == nil {
			t.Fatal("CIProviders is nil")
		}
		if kinds := CIProviders.Kinds(); kinds == nil {
			t.Fatal("CIProviders.Kinds() returned nil, want non-nil slice")
		}
	})

	t.Run("Notifiers", func(t *testing.T) {
		t.Parallel()

		if Notifiers == nil {
			t.Fatal("Notifiers is nil")
		}
		if kinds := Notifiers.Kinds(); kinds == nil {
			t.Fatal("Notifiers.Kinds() returned nil, want non-nil slice")
		}
	})
}

func TestNotifiersRegistry(t *testing.T) {
	t.Parallel()

	t.Run("register and resolve stub kind", func(t *testing.T) {
		t.Parallel()

		r := NewRegistry[NotifierConstructor, struct{}]("notifier")
		r.Register("test-notifier-stub", func(_ map[string]any) (domain.Notifier, error) {
			return &stubNotifier{}, nil
		})

		ctor, err := r.Get("test-notifier-stub")
		if err != nil {
			t.Fatalf("Get(%q) unexpected error: %v", "test-notifier-stub", err)
		}

		notifier, err := ctor(nil)
		if err != nil {
			t.Fatalf("NotifierConstructor() unexpected error: %v", err)
		}
		if notifier == nil {
			t.Fatal("NotifierConstructor() returned nil, want non-nil Notifier")
		}
	})

	t.Run("unknown kind returns RegistryError", func(t *testing.T) {
		t.Parallel()

		r := NewRegistry[NotifierConstructor, struct{}]("notifier")

		_, err := r.Get("nonexistent-notifier-kind")
		if err == nil {
			t.Fatal("Get(unknown kind) = nil, want *RegistryError")
		}

		var re *RegistryError
		if !errors.As(err, &re) {
			t.Fatalf("Get(unknown kind) error type = %T, want *RegistryError", err)
		}
		if re.Dimension != "notifier" {
			t.Errorf("RegistryError.Dimension = %q, want %q", re.Dimension, "notifier")
		}
		if re.Kind != "nonexistent-notifier-kind" {
			t.Errorf("RegistryError.Kind = %q, want %q", re.Kind, "nonexistent-notifier-kind")
		}
	})

	t.Run("package-level Notifiers uses notifier dimension", func(t *testing.T) {
		t.Parallel()

		_, err := Notifiers.Get("no-such-kind-xyz")
		if err == nil {
			t.Fatal("Notifiers.Get(unknown) = nil, want error")
		}

		var re *RegistryError
		if !errors.As(err, &re) {
			t.Fatalf("error type = %T, want *RegistryError", err)
		}
		if re.Dimension != "notifier" {
			t.Errorf("Notifiers dimension = %q, want %q", re.Dimension, "notifier")
		}
	})
}

func TestHas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		register []string
		lookup   string
		want     bool
	}{
		{
			name:     "registered kind returns true",
			register: []string{"alpha", "beta"},
			lookup:   "alpha",
			want:     true,
		},
		{
			name:     "second registered kind returns true",
			register: []string{"alpha", "beta"},
			lookup:   "beta",
			want:     true,
		},
		{
			name:     "unknown kind returns false",
			register: []string{"alpha"},
			lookup:   "gamma",
			want:     false,
		},
		{
			name:     "empty registry returns false",
			register: nil,
			lookup:   "anything",
			want:     false,
		},
		{
			name:     "case-sensitive lookup: wrong case returns false",
			register: []string{"Claude-Code"},
			lookup:   "claude-code",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTestRegistry()
			for _, k := range tt.register {
				r.Register(k, dummyConstructor())
			}

			got := r.Has(tt.lookup)
			if got != tt.want {
				t.Errorf("Has(%q) = %v, want %v", tt.lookup, got, tt.want)
			}
		})
	}
}

func TestMCPDialect_DeliversTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dialect MCPDialect
		want    bool
	}{
		{"undeclared", MCPDialectUndeclared, false},
		{"none", MCPDialectNone, false},
		{"mcp_servers_json", MCPDialectMCPServersJSON, true},
		{"mcp_servers_json_always_load", MCPDialectMCPServersJSONAlwaysLoad, true},
		{"mcp_servers_toml", MCPDialectMCPServersTOML, true},
		{"mcp_member_json", MCPDialectMCPMemberJSON, true},
		{"acp_session", MCPDialectACPSession, true},
		{"unrecognized value", MCPDialect("bogus"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.dialect.DeliversTools()
			if got != tt.want {
				t.Errorf("MCPDialect(%q).DeliversTools() = %v, want %v", tt.dialect, got, tt.want)
			}
		})
	}
}

func TestAgents_RefusesInvalidUsageSources(t *testing.T) {
	t.Parallel()

	validLive := usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative},
	}

	tests := []struct {
		name     string
		kind     string
		register func(kind string)
	}{
		{
			name: "zero usage sources with metadata",
			kind: "fixture-zero-sources",
			register: func(kind string) {
				Agents.RegisterWithMeta(kind, dummyAgentConstructor(), AgentMeta{})
			},
		},
		{
			name: "registration without metadata",
			kind: "fixture-no-meta",
			register: func(kind string) {
				Agents.Register(kind, dummyAgentConstructor())
			},
		},
		{
			name: "records source that is not a journal",
			kind: "fixture-bad-records",
			register: func(kind string) {
				records := usage.Source{Category: usage.SourceOutput, Strategy: usage.StrategyRecords, Spend: validLive.Spend}
				Agents.RegisterWithMeta(kind, dummyAgentConstructor(), AgentMeta{UsageSources: usage.Sources{Live: validLive, Records: records}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("registering agent kind %q did not panic", tt.kind)
					}
				}()
				tt.register(tt.kind)
			}()

			if Agents.Has(tt.kind) || slices.Contains(Agents.Kinds(), tt.kind) {
				t.Errorf("Agents lists kind %q after a refused registration, want it absent", tt.kind)
			}
		})
	}
}

func TestNewCheckedRegistry(t *testing.T) {
	t.Parallel()

	errRefused := errors.New("refused")
	refuseNonRequired := func(_ testConstructor, meta testMeta) error {
		if !meta.RequiresProject {
			return errRefused
		}
		return nil
	}

	tests := []struct {
		name      string
		check     func(testConstructor, testMeta) error
		register  func(*Registry[testConstructor, testMeta])
		wantStore bool
	}{
		{
			name:     "failed check on RegisterWithMeta stores nothing",
			check:    refuseNonRequired,
			register: func(r *Registry[testConstructor, testMeta]) { r.RegisterWithMeta("k", dummyConstructor(), testMeta{}) },
		},
		{
			name:     "Register passes the zero metadata to the check",
			check:    refuseNonRequired,
			register: func(r *Registry[testConstructor, testMeta]) { r.Register("k", dummyConstructor()) },
		},
		{
			name:  "passing check stores the kind",
			check: refuseNonRequired,
			register: func(r *Registry[testConstructor, testMeta]) {
				r.RegisterWithMeta("k", dummyConstructor(), testMeta{RequiresProject: true})
			},
			wantStore: true,
		},
		{
			name:      "nil check accepts every registration",
			register:  func(r *Registry[testConstructor, testMeta]) { r.Register("k", dummyConstructor()) },
			wantStore: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewCheckedRegistry("test", tt.check)

			func() {
				defer func() {
					if recovered := recover(); (recovered != nil) == tt.wantStore {
						t.Errorf("registering kind %q panicked = %v, want %v", "k", recovered != nil, !tt.wantStore)
					}
				}()
				tt.register(r)
			}()

			if r.Has("k") != tt.wantStore || slices.Contains(r.Kinds(), "k") != tt.wantStore {
				t.Errorf("Has(%q) = %v, Kinds() = %v, want stored = %v", "k", r.Has("k"), r.Kinds(), tt.wantStore)
			}
		})
	}
}

func TestCredentialEnv(t *testing.T) {
	t.Parallel()

	t.Run("zero value is undeclared with no names", func(t *testing.T) {
		t.Parallel()

		var zero CredentialEnv
		if zero.Declared() {
			t.Error("zero CredentialEnv Declared() = true, want false")
		}
		if got := zero.Names(); got != nil {
			t.Errorf("zero CredentialEnv Names() = %v, want nil", got)
		}
	})

	t.Run("no-argument declaration is declared with no names", func(t *testing.T) {
		t.Parallel()

		c := DeclareCredentialEnv()
		if !c.Declared() {
			t.Error("DeclareCredentialEnv().Declared() = false, want true")
		}
		if got := c.Names(); got != nil {
			t.Errorf("DeclareCredentialEnv().Names() = %v, want nil", got)
		}
	})

	t.Run("explicitly empty declaration is declared with no names", func(t *testing.T) {
		t.Parallel()

		c := DeclareCredentialEnv([]string{}...)
		if !c.Declared() {
			t.Error("DeclareCredentialEnv([]string{}...).Declared() = false, want true")
		}
		if got := c.Names(); got != nil {
			t.Errorf("DeclareCredentialEnv([]string{}...).Names() = %v, want nil", got)
		}
	})

	t.Run("declared names are held in order", func(t *testing.T) {
		t.Parallel()

		c := DeclareCredentialEnv("FIRST", "SECOND")
		want := []string{"FIRST", "SECOND"}
		if got := c.Names(); !slices.Equal(got, want) {
			t.Errorf("Names() = %v, want %v", got, want)
		}
	})

	t.Run("Names returns a fresh copy each call", func(t *testing.T) {
		t.Parallel()

		c := DeclareCredentialEnv("FIRST", "SECOND")

		first := c.Names()
		first[0] = "MUTATED"

		second := c.Names()
		want := []string{"FIRST", "SECOND"}
		if !slices.Equal(second, want) {
			t.Errorf("Names() after mutating a previous call's result = %v, want %v (unaffected)", second, want)
		}
	})
}
