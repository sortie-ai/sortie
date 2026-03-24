package file

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func fixture(name string) string {
	return filepath.Join("testdata", name)
}

func newAdapter(t *testing.T, path string, activeStates []any) *FileAdapter {
	t.Helper()
	config := map[string]any{"path": path}
	if activeStates != nil {
		config["active_states"] = activeStates
	}
	a, err := NewFileAdapter(config)
	if err != nil {
		t.Fatalf("NewFileAdapter: %v", err)
	}
	return a.(*FileAdapter)
}

func requireTrackerError(t *testing.T, err error) {
	t.Helper()
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != domain.ErrTrackerPayload {
		t.Fatalf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerPayload)
	}
}

func requireTrackerErrorKind(t *testing.T, err error, kind domain.TrackerErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", kind)
	}
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != kind {
		t.Fatalf("TrackerError.Kind = %q, want %q", te.Kind, kind)
	}
}

// --- Constructor tests ---

func TestNewFileAdapter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name:   "valid path",
			config: map[string]any{"path": fixture("basic.json")},
		},
		{
			name:    "missing path key",
			config:  map[string]any{},
			wantErr: true,
		},
		{
			name:    "empty path value",
			config:  map[string]any{"path": ""},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := NewFileAdapter(tt.config)
			if tt.wantErr {
				requireTrackerError(t, err)
				if a != nil {
					t.Error("adapter should be nil on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if a == nil {
				t.Fatal("adapter is nil")
			}
		})
	}
}

func TestNewFileAdapter_YAMLAnySliceExtraction(t *testing.T) {
	t.Parallel()

	// YAML decoders produce []any, not []string. Verify the constructor
	// handles this without panic and lowercases state values.
	config := map[string]any{
		"path":          fixture("basic.json"),
		"active_states": []any{"To Do", "In Progress"},
	}
	a, err := NewFileAdapter(config)
	if err != nil {
		t.Fatalf("NewFileAdapter: %v", err)
	}
	fa := a.(*FileAdapter)
	if !fa.activeStates["to do"] {
		t.Error("active_states missing 'to do'")
	}
	if !fa.activeStates["in progress"] {
		t.Error("active_states missing 'in progress'")
	}
}

// --- FetchCandidateIssues tests ---

func TestFetchCandidateIssues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("filters by active states", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), []any{"to do", "in progress"})
		issues, err := a.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if len(issues) != 2 {
			t.Fatalf("got %d issues, want 2", len(issues))
		}
		ids := map[string]bool{}
		for _, iss := range issues {
			ids[iss.Identifier] = true
		}
		if !ids["PROJ-1"] || !ids["PROJ-2"] {
			t.Errorf("unexpected issues: %v", ids)
		}
	})

	t.Run("no active states returns all", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		issues, err := a.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if len(issues) != 3 {
			t.Fatalf("got %d issues, want 3", len(issues))
		}
	})

	t.Run("empty file", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("empty.json"), nil)
		issues, err := a.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if issues == nil {
			t.Fatal("issues is nil, want non-nil empty slice")
		}
		if len(issues) != 0 {
			t.Fatalf("got %d issues, want 0", len(issues))
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("malformed.json"), nil)
		_, err := a.FetchCandidateIssues(ctx)
		requireTrackerError(t, err)
	})

	t.Run("nonexistent file", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, "testdata/does_not_exist.json", nil)
		_, err := a.FetchCandidateIssues(ctx)
		requireTrackerError(t, err)
	})

	t.Run("comments are nil", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		issues, err := a.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		for _, iss := range issues {
			if iss.Comments != nil {
				t.Errorf("issue %s: Comments = %v, want nil", iss.Identifier, iss.Comments)
			}
		}
	})

	t.Run("case-insensitive state filtering", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), []any{"TO DO"})
		issues, err := a.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
		if issues[0].Identifier != "PROJ-1" {
			t.Errorf("got %s, want PROJ-1", issues[0].Identifier)
		}
	})
}

// --- FetchIssueByID tests ---

func TestFetchIssueByID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := newAdapter(t, fixture("basic.json"), nil)

	t.Run("fully populated issue", func(t *testing.T) {
		t.Parallel()

		iss, err := a.FetchIssueByID(ctx, "10001")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}

		if iss.ID != "10001" {
			t.Errorf("ID = %q, want %q", iss.ID, "10001")
		}
		if iss.Identifier != "PROJ-1" {
			t.Errorf("Identifier = %q, want %q", iss.Identifier, "PROJ-1")
		}
		if iss.Title != "Implement login" {
			t.Errorf("Title = %q", iss.Title)
		}
		if iss.Description != "Add OAuth2 login flow." {
			t.Errorf("Description = %q", iss.Description)
		}
		if iss.Priority == nil || *iss.Priority != 2 {
			t.Errorf("Priority = %v, want 2", iss.Priority)
		}
		if iss.State != "To Do" {
			t.Errorf("State = %q, want %q", iss.State, "To Do")
		}
		if iss.BranchName != "feature/PROJ-1" {
			t.Errorf("BranchName = %q", iss.BranchName)
		}
		if iss.URL != "https://tracker.example.com/PROJ-1" {
			t.Errorf("URL = %q", iss.URL)
		}
		if iss.Assignee != "alice" {
			t.Errorf("Assignee = %q", iss.Assignee)
		}
		if iss.IssueType != "Story" {
			t.Errorf("IssueType = %q", iss.IssueType)
		}
		if iss.CreatedAt != "2026-02-28T09:00:00Z" {
			t.Errorf("CreatedAt = %q", iss.CreatedAt)
		}
		if iss.UpdatedAt != "2026-03-01T12:00:00Z" {
			t.Errorf("UpdatedAt = %q", iss.UpdatedAt)
		}

		// Labels lowercased.
		if len(iss.Labels) != 2 || iss.Labels[0] != "feature" || iss.Labels[1] != "auth" {
			t.Errorf("Labels = %v, want [feature auth]", iss.Labels)
		}

		// Parent populated.
		if iss.Parent == nil || iss.Parent.ID != "10000" || iss.Parent.Identifier != "PROJ-0" {
			t.Errorf("Parent = %v", iss.Parent)
		}

		// Comments non-nil and populated.
		if iss.Comments == nil {
			t.Fatal("Comments is nil, want non-nil")
		}
		if len(iss.Comments) != 1 {
			t.Fatalf("len(Comments) = %d, want 1", len(iss.Comments))
		}
		if iss.Comments[0].Author != "bob" {
			t.Errorf("Comments[0].Author = %q, want %q", iss.Comments[0].Author, "bob")
		}

		// BlockedBy populated.
		if len(iss.BlockedBy) != 1 || iss.BlockedBy[0].ID != "10002" {
			t.Errorf("BlockedBy = %v", iss.BlockedBy)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		_, err := a.FetchIssueByID(ctx, "99999")
		requireTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("empty comments array", func(t *testing.T) {
		t.Parallel()

		iss, err := a.FetchIssueByID(ctx, "10002")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}
		if iss.Comments == nil {
			t.Fatal("Comments is nil, want non-nil empty slice")
		}
		if len(iss.Comments) != 0 {
			t.Errorf("len(Comments) = %d, want 0", len(iss.Comments))
		}
	})
}

// --- FetchIssuesByStates tests ---

func TestFetchIssuesByStates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := newAdapter(t, fixture("basic.json"), nil)

	t.Run("match by original case", func(t *testing.T) {
		t.Parallel()

		issues, err := a.FetchIssuesByStates(ctx, []string{"Done"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
		if issues[0].Identifier != "PROJ-3" {
			t.Errorf("got %s, want PROJ-3", issues[0].Identifier)
		}
	})

	t.Run("case-insensitive matching", func(t *testing.T) {
		t.Parallel()

		issues, err := a.FetchIssuesByStates(ctx, []string{"done"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
	})

	t.Run("empty states slice", func(t *testing.T) {
		t.Parallel()

		issues, err := a.FetchIssuesByStates(ctx, []string{})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if issues == nil {
			t.Fatal("issues is nil, want non-nil empty slice")
		}
		if len(issues) != 0 {
			t.Fatalf("got %d issues, want 0", len(issues))
		}
	})

	t.Run("no matching states", func(t *testing.T) {
		t.Parallel()

		issues, err := a.FetchIssuesByStates(ctx, []string{"Nonexistent"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 0 {
			t.Fatalf("got %d issues, want 0", len(issues))
		}
	})
}

// --- FetchIssueStatesByIDs tests ---

func TestFetchIssueStatesByIDs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := newAdapter(t, fixture("basic.json"), nil)

	t.Run("multiple found", func(t *testing.T) {
		t.Parallel()

		m, err := a.FetchIssueStatesByIDs(ctx, []string{"10001", "10002"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if len(m) != 2 {
			t.Fatalf("got %d entries, want 2", len(m))
		}
		if m["10001"] != "To Do" {
			t.Errorf("10001 state = %q, want %q", m["10001"], "To Do")
		}
		if m["10002"] != "In Progress" {
			t.Errorf("10002 state = %q, want %q", m["10002"], "In Progress")
		}
	})

	t.Run("missing ID omitted", func(t *testing.T) {
		t.Parallel()

		m, err := a.FetchIssueStatesByIDs(ctx, []string{"10001", "99999"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if len(m) != 1 {
			t.Fatalf("got %d entries, want 1", len(m))
		}
		if _, ok := m["99999"]; ok {
			t.Error("missing ID should be omitted from result")
		}
	})

	t.Run("empty IDs", func(t *testing.T) {
		t.Parallel()

		m, err := a.FetchIssueStatesByIDs(ctx, []string{})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if m == nil {
			t.Fatal("map is nil, want non-nil")
		}
		if len(m) != 0 {
			t.Fatalf("got %d entries, want 0", len(m))
		}
	})
}

// --- FetchIssueStatesByIdentifiers tests ---

func TestFetchIssueStatesByIdentifiers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := newAdapter(t, fixture("basic.json"), nil)

	t.Run("multiple found by identifier", func(t *testing.T) {
		t.Parallel()

		m, err := a.FetchIssueStatesByIdentifiers(ctx, []string{"PROJ-1", "PROJ-2"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}
		if len(m) != 2 {
			t.Fatalf("got %d entries, want 2", len(m))
		}
		if m["PROJ-1"] != "To Do" {
			t.Errorf("PROJ-1 state = %q, want %q", m["PROJ-1"], "To Do")
		}
		if m["PROJ-2"] != "In Progress" {
			t.Errorf("PROJ-2 state = %q, want %q", m["PROJ-2"], "In Progress")
		}
	})

	t.Run("missing identifier omitted", func(t *testing.T) {
		t.Parallel()

		m, err := a.FetchIssueStatesByIdentifiers(ctx, []string{"PROJ-1", "NONEXISTENT"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}
		if len(m) != 1 {
			t.Fatalf("got %d entries, want 1", len(m))
		}
		if _, ok := m["NONEXISTENT"]; ok {
			t.Error("missing identifier should be omitted from result")
		}
	})

	t.Run("empty identifiers", func(t *testing.T) {
		t.Parallel()

		m, err := a.FetchIssueStatesByIdentifiers(ctx, []string{})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}
		if m == nil {
			t.Fatal("map is nil, want non-nil")
		}
		if len(m) != 0 {
			t.Fatalf("got %d entries, want 0", len(m))
		}
	})
}

// --- FetchIssueComments tests ---

func TestFetchIssueComments(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := newAdapter(t, fixture("basic.json"), nil)

	t.Run("issue with comments", func(t *testing.T) {
		t.Parallel()

		comments, err := a.FetchIssueComments(ctx, "10001")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		if len(comments) != 1 {
			t.Fatalf("got %d comments, want 1", len(comments))
		}
		c := comments[0]
		if c.ID != "c1" {
			t.Errorf("ID = %q, want %q", c.ID, "c1")
		}
		if c.Author != "bob" {
			t.Errorf("Author = %q, want %q", c.Author, "bob")
		}
		if c.Body != "Needs SSO support." {
			t.Errorf("Body = %q", c.Body)
		}
		if c.CreatedAt != "2026-03-01T10:00:00Z" {
			t.Errorf("CreatedAt = %q", c.CreatedAt)
		}
	})

	t.Run("empty comments array", func(t *testing.T) {
		t.Parallel()

		comments, err := a.FetchIssueComments(ctx, "10002")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		if comments == nil {
			t.Fatal("comments is nil, want non-nil empty slice")
		}
		if len(comments) != 0 {
			t.Fatalf("got %d comments, want 0", len(comments))
		}
	})

	t.Run("null comments coerced to empty", func(t *testing.T) {
		t.Parallel()

		comments, err := a.FetchIssueComments(ctx, "10003")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		if comments == nil {
			t.Fatal("comments is nil, want non-nil empty slice")
		}
		if len(comments) != 0 {
			t.Fatalf("got %d comments, want 0", len(comments))
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		_, err := a.FetchIssueComments(ctx, "99999")
		requireTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

// --- Normalization tests ---

func TestNormalization(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a := newAdapter(t, fixture("normalization.json"), nil)
	issues, err := a.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	issueByID := make(map[string]domain.Issue, len(issues))
	for _, iss := range issues {
		issueByID[iss.ID] = iss
	}

	t.Run("labels lowercased", func(t *testing.T) {
		t.Parallel()

		iss := issueByID["n1"]
		want := []string{"bug", "high-priority"}
		if len(iss.Labels) != len(want) {
			t.Fatalf("Labels = %v, want %v", iss.Labels, want)
		}
		for i, w := range want {
			if iss.Labels[i] != w {
				t.Errorf("Labels[%d] = %q, want %q", i, iss.Labels[i], w)
			}
		}
	})

	t.Run("string priority is nil", func(t *testing.T) {
		t.Parallel()

		if issueByID["n2"].Priority != nil {
			t.Errorf("Priority = %v, want nil", *issueByID["n2"].Priority)
		}
	})

	t.Run("float priority is nil", func(t *testing.T) {
		t.Parallel()

		if issueByID["n3"].Priority != nil {
			t.Errorf("Priority = %v, want nil", *issueByID["n3"].Priority)
		}
	})

	t.Run("boolean priority is nil", func(t *testing.T) {
		t.Parallel()

		if issueByID["n4"].Priority != nil {
			t.Errorf("Priority = %v, want nil", *issueByID["n4"].Priority)
		}
	})

	t.Run("integer priority", func(t *testing.T) {
		t.Parallel()

		p := issueByID["n5"].Priority
		if p == nil {
			t.Fatal("Priority is nil, want 1")
		}
		if *p != 1 {
			t.Errorf("Priority = %d, want 1", *p)
		}
	})

	t.Run("null priority is nil", func(t *testing.T) {
		t.Parallel()

		if issueByID["n6"].Priority != nil {
			t.Errorf("Priority = %v, want nil", *issueByID["n6"].Priority)
		}
	})

	t.Run("absent priority is nil", func(t *testing.T) {
		t.Parallel()

		if issueByID["n7"].Priority != nil {
			t.Errorf("Priority = %v, want nil", *issueByID["n7"].Priority)
		}
	})

	t.Run("absent labels is non-nil empty", func(t *testing.T) {
		t.Parallel()

		iss := issueByID["n7"]
		if iss.Labels == nil {
			t.Fatal("Labels is nil, want non-nil empty slice")
		}
		if len(iss.Labels) != 0 {
			t.Errorf("len(Labels) = %d, want 0", len(iss.Labels))
		}
	})

	t.Run("absent blocked_by is non-nil empty", func(t *testing.T) {
		t.Parallel()

		iss := issueByID["n7"]
		if iss.BlockedBy == nil {
			t.Fatal("BlockedBy is nil, want non-nil empty slice")
		}
		if len(iss.BlockedBy) != 0 {
			t.Errorf("len(BlockedBy) = %d, want 0", len(iss.BlockedBy))
		}
	})

	t.Run("absent parent is nil", func(t *testing.T) {
		t.Parallel()

		if issueByID["n7"].Parent != nil {
			t.Errorf("Parent = %v, want nil", issueByID["n7"].Parent)
		}
	})

	t.Run("absent description is empty string", func(t *testing.T) {
		t.Parallel()

		if issueByID["n7"].Description != "" {
			t.Errorf("Description = %q, want empty", issueByID["n7"].Description)
		}
	})
}

// --- Registry integration test ---

func TestRegistryIntegration(t *testing.T) {
	t.Parallel()

	ctor, err := registry.Trackers.Get("file")
	if err != nil {
		t.Fatalf("registry.Trackers.Get(\"file\"): %v", err)
	}

	adapter, err := ctor(map[string]any{"path": fixture("basic.json")})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("expected issues from registry-constructed adapter")
	}
}

// --- TransitionIssue tests ---

func TestTransitionIssue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("successful transition", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), []any{"to do", "in progress"})
		if err := a.TransitionIssue(ctx, "10001", "Human Review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}

		iss, err := a.FetchIssueByID(ctx, "10001")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}
		if iss.State != "Human Review" {
			t.Errorf("State = %q, want %q", iss.State, "Human Review")
		}
	})

	t.Run("reflected in FetchCandidateIssues", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), []any{"to do", "in progress"})
		if err := a.TransitionIssue(ctx, "10001", "Human Review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}

		issues, err := a.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("got %d issues, want 1", len(issues))
		}
		if issues[0].Identifier != "PROJ-2" {
			t.Errorf("got %s, want PROJ-2", issues[0].Identifier)
		}
	})

	t.Run("reflected in FetchIssueStatesByIDs", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		if err := a.TransitionIssue(ctx, "10001", "Human Review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}

		m, err := a.FetchIssueStatesByIDs(ctx, []string{"10001"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if m["10001"] != "Human Review" {
			t.Errorf("state = %q, want %q", m["10001"], "Human Review")
		}
	})

	t.Run("reflected in FetchIssueStatesByIdentifiers", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		if err := a.TransitionIssue(ctx, "10001", "Human Review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}

		m, err := a.FetchIssueStatesByIdentifiers(ctx, []string{"PROJ-1"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}
		if m["PROJ-1"] != "Human Review" {
			t.Errorf("state = %q, want %q", m["PROJ-1"], "Human Review")
		}
	})

	t.Run("reflected in FetchIssuesByStates", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		if err := a.TransitionIssue(ctx, "10001", "Human Review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}

		found, err := a.FetchIssuesByStates(ctx, []string{"Human Review"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates(Human Review): %v", err)
		}
		if len(found) != 1 {
			t.Fatalf("got %d issues, want 1", len(found))
		}
		if found[0].ID != "10001" {
			t.Errorf("ID = %q, want %q", found[0].ID, "10001")
		}

		old, err := a.FetchIssuesByStates(ctx, []string{"To Do"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates(To Do): %v", err)
		}
		if len(old) != 0 {
			t.Errorf("got %d issues for old state, want 0", len(old))
		}
	})

	t.Run("issue not found", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		err := a.TransitionIssue(ctx, "99999", "Human Review")
		requireTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("file read error", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, "testdata/does_not_exist.json", nil)
		err := a.TransitionIssue(ctx, "10001", "Human Review")
		requireTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("multiple transitions", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		if err := a.TransitionIssue(ctx, "10001", "Review"); err != nil {
			t.Fatalf("TransitionIssue(Review): %v", err)
		}
		if err := a.TransitionIssue(ctx, "10001", "Done"); err != nil {
			t.Fatalf("TransitionIssue(Done): %v", err)
		}

		iss, err := a.FetchIssueByID(ctx, "10001")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}
		if iss.State != "Done" {
			t.Errorf("State = %q, want %q", iss.State, "Done")
		}
	})

	t.Run("transition does not affect other issues", func(t *testing.T) {
		t.Parallel()

		a := newAdapter(t, fixture("basic.json"), nil)
		if err := a.TransitionIssue(ctx, "10001", "Human Review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}

		iss, err := a.FetchIssueByID(ctx, "10002")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}
		if iss.State != "In Progress" {
			t.Errorf("State = %q, want %q", iss.State, "In Progress")
		}
	})
}
