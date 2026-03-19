package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// testConstructor is a trivial function type used to test generic
// Registry behavior without coupling to domain.TrackerAdapter.
type testConstructor func() string

func newTestRegistry() *Registry[testConstructor] {
	return NewRegistry[testConstructor]("test")
}

func dummyConstructor() testConstructor {
	return func() string { return "marker" }
}

func TestRegisterAndGet(t *testing.T) {
	r := newTestRegistry()
	r.Register("alpha", dummyConstructor())

	got, err := r.Get("alpha")
	if err != nil {
		t.Fatalf("Get(\"alpha\") returned unexpected error: %v", err)
	}
	if got() != "marker" {
		t.Errorf("constructor returned %q, want %q", got(), "marker")
	}
}

func TestGetUnknownKind_Empty(t *testing.T) {
	r := newTestRegistry()

	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("Get(\"nonexistent\") returned nil error, want *RegistryError")
	}

	var re *RegistryError
	if !errors.As(err, &re) {
		t.Fatalf("error type = %T, want *RegistryError", err)
	}
	if re.Dimension != "test" {
		t.Errorf("Dimension = %q, want %q", re.Dimension, "test")
	}
	if re.Kind != "nonexistent" {
		t.Errorf("Kind = %q, want %q", re.Kind, "nonexistent")
	}
	if re.Available == nil {
		t.Fatal("Available is nil, want non-nil empty slice")
	}
	if len(re.Available) != 0 {
		t.Errorf("len(Available) = %d, want 0", len(re.Available))
	}
	if msg := err.Error(); !strings.Contains(msg, "no adapters registered") {
		t.Errorf("error message = %q, want it to contain %q", msg, "no adapters registered")
	}
}

func TestGetUnknownKind_ListsAvailable(t *testing.T) {
	r := newTestRegistry()
	r.Register("beta", dummyConstructor())
	r.Register("alpha", dummyConstructor())

	_, err := r.Get("gamma")
	if err == nil {
		t.Fatal("Get(\"gamma\") returned nil error")
	}

	var re *RegistryError
	if !errors.As(err, &re) {
		t.Fatalf("error type = %T, want *RegistryError", err)
	}

	want := []string{"alpha", "beta"}
	if len(re.Available) != len(want) {
		t.Fatalf("Available = %v, want %v", re.Available, want)
	}
	for i, v := range want {
		if re.Available[i] != v {
			t.Errorf("Available[%d] = %q, want %q", i, re.Available[i], v)
		}
	}

	msg := err.Error()
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("error message = %q, want it to contain both %q and %q", msg, "alpha", "beta")
	}
}

func TestRegisterDuplicate_Panics(t *testing.T) {
	r := newTestRegistry()
	r.Register("dup", dummyConstructor())

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic on duplicate registration")
		}
		msg, ok := v.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string", v)
		}
		if !strings.Contains(msg, "duplicate") {
			t.Errorf("panic message = %q, want it to contain %q", msg, "duplicate")
		}
	}()

	r.Register("dup", dummyConstructor())
}

func TestRegisterEmptyKind_Panics(t *testing.T) {
	r := newTestRegistry()

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic on empty kind")
		}
		msg, ok := v.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string", v)
		}
		if !strings.Contains(msg, "empty") {
			t.Errorf("panic message = %q, want it to contain %q", msg, "empty")
		}
	}()

	r.Register("", dummyConstructor())
}

func TestKinds_Sorted(t *testing.T) {
	r := newTestRegistry()
	r.Register("zulu", dummyConstructor())
	r.Register("alpha", dummyConstructor())
	r.Register("mike", dummyConstructor())

	got := r.Kinds()
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("Kinds()[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestKinds_Empty(t *testing.T) {
	r := newTestRegistry()

	got := r.Kinds()
	if got == nil {
		t.Fatal("Kinds() returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len(Kinds()) = %d, want 0", len(got))
	}
}

func TestRegistryError_Format_NoAvailable(t *testing.T) {
	e := &RegistryError{
		Dimension: "tracker",
		Kind:      "linear",
		Available: []string{},
	}
	want := `unknown tracker adapter kind "linear"; no adapters registered`
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestRegistryError_Format_WithAvailable(t *testing.T) {
	e := &RegistryError{
		Dimension: "agent",
		Kind:      "foo",
		Available: []string{"claude-code", "mock"},
	}
	want := `unknown agent adapter kind "foo"; registered: [claude-code, mock]`
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// Compile-time interface satisfaction check.
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

func TestTrackerConstructor_Integration(t *testing.T) {
	r := NewRegistry[TrackerConstructor]("tracker")
	r.Register("mock", func(_ map[string]any) (domain.TrackerAdapter, error) {
		return &mockTrackerAdapter{}, nil
	})

	constructor, err := r.Get("mock")
	if err != nil {
		t.Fatalf("Get(\"mock\") returned unexpected error: %v", err)
	}

	adapter, err := constructor(nil)
	if err != nil {
		t.Fatalf("constructor returned unexpected error: %v", err)
	}

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues returned unexpected error: %v", err)
	}
	if issues != nil {
		t.Errorf("FetchCandidateIssues returned %v, want nil", issues)
	}
}

func TestPackageLevelTrackers_Usable(t *testing.T) {
	if Trackers == nil {
		t.Fatal("Trackers is nil")
	}
	kinds := Trackers.Kinds()
	if kinds == nil {
		t.Fatal("Trackers.Kinds() returned nil, want non-nil slice")
	}
}
