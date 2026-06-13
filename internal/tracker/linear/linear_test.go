package linear

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestLinearAdapterRegistration(t *testing.T) {
	t.Parallel()

	meta, ok := registry.Trackers.Meta("linear")
	if !ok {
		t.Fatalf("Trackers.Meta(%q) reported not registered", "linear")
	}
	if !meta.RequiresProject {
		t.Errorf("Trackers.Meta(%q).RequiresProject = %v, want %v", "linear", meta.RequiresProject, true)
	}
	if !meta.RequiresAPIKey {
		t.Errorf("Trackers.Meta(%q).RequiresAPIKey = %v, want %v", "linear", meta.RequiresAPIKey, true)
	}

	if _, err := registry.Trackers.Get("linear"); err != nil {
		t.Errorf("Trackers.Get(%q) = %v, want registered constructor", "linear", err)
	}
}

func TestNewLinearAdapter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   map[string]any
		wantKind domain.TrackerErrorKind
	}{
		{
			name:     "missing api_key",
			config:   map[string]any{"project": "SOR"},
			wantKind: domain.ErrMissingTrackerAPIKey,
		},
		{
			name:     "missing project",
			config:   map[string]any{"api_key": "lin_api_test"},
			wantKind: domain.ErrMissingTrackerProject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewLinearAdapter(tt.config)

			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

func TestNewAdapterSuccess(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	seedPreflight(f, t)

	got, err := newAdapter(f, "SOR", defaultActiveStates, defaultTerminalStates, "in review", nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}

	adapter, ok := got.(*LinearAdapter)
	if !ok {
		t.Fatalf("newAdapter returned %T, want *LinearAdapter", got)
	}
	if adapter.handoffState != "In Review" {
		t.Errorf("handoffState = %q, want %q (canonical casing from preflight cache)", adapter.handoffState, "In Review")
	}
	wantActive := []string{"Backlog", "Todo", "In Progress"}
	if !reflect.DeepEqual(adapter.activeStates, wantActive) {
		t.Errorf("activeStates = %v, want %v", adapter.activeStates, wantActive)
	}
}

func TestFetchCandidateIssues(t *testing.T) {
	t.Parallel()

	t.Run("paginates to exhaustion and sorts by priority then created", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "candidates_page1.json"))
		f.queueBody("query CandidateIssues", loadFixture(t, "candidates_page2.json"))

		adapter := newTestAdapter(t, f)

		issues, err := adapter.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		gotOrder := identifiers(issues)
		wantOrder := []string{"SOR-7", "SOR-5", "SOR-8"}
		if !reflect.DeepEqual(gotOrder, wantOrder) {
			t.Errorf("FetchCandidateIssues order = %v, want %v (priority asc, nil last, then createdAt)", gotOrder, wantOrder)
		}

		for _, iss := range issues {
			if iss.Comments != nil {
				t.Errorf("issue %s: Comments = %v, want nil for candidate fetch", iss.Identifier, iss.Comments)
			}
		}

		calls := f.callsFor("query CandidateIssues")
		if len(calls) != 2 {
			t.Fatalf("CandidateIssues call count = %d, want 2 (one per page)", len(calls))
		}
		if calls[0].variables["after"] != nil {
			t.Errorf("first page after = %v, want nil", calls[0].variables["after"])
		}
		if calls[1].variables["after"] == nil || calls[1].variables["after"] == "" {
			t.Errorf("second page after = %v, want the page-1 end cursor", calls[1].variables["after"])
		}
	})

	t.Run("returns non-nil empty slice when no candidates", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "issues_empty.json"))

		adapter := newTestAdapter(t, f)

		issues, err := adapter.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if issues == nil {
			t.Fatal("FetchCandidateIssues = nil, want non-nil empty slice")
		}
		if len(issues) != 0 {
			t.Errorf("FetchCandidateIssues len = %d, want 0", len(issues))
		}
	})

	t.Run("sends configured active states", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "issues_empty.json"))

		adapter := newTestAdapter(t, f)

		if _, err := adapter.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		call := f.callsFor("query CandidateIssues")[0]
		states, ok := call.variables["states"].([]string)
		if !ok {
			t.Fatalf("states variable type = %T, want []string", call.variables["states"])
		}
		want := []string{"Backlog", "Todo", "In Progress"}
		if !reflect.DeepEqual(states, want) {
			t.Errorf("states = %v, want %v", states, want)
		}
		if call.variables["teamKey"] != "SOR" {
			t.Errorf("teamKey = %v, want %q", call.variables["teamKey"], "SOR")
		}
	})
}

func TestFetchIssuesByStates(t *testing.T) {
	t.Parallel()

	t.Run("empty states returns empty slice with no API call", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		adapter := newTestAdapter(t, f)

		issues, err := adapter.FetchIssuesByStates(context.Background(), nil)
		if err != nil {
			t.Fatalf("FetchIssuesByStates(nil): %v", err)
		}
		if issues == nil || len(issues) != 0 {
			t.Errorf("FetchIssuesByStates(nil) = %v, want non-nil empty slice", issues)
		}
		if calls := f.callsFor("query IssuesByStates"); len(calls) != 0 {
			t.Errorf("IssuesByStates call count = %d, want 0 for empty input", len(calls))
		}
	})

	t.Run("canonical-cases supplied states and omits sort", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssuesByStates", loadFixture(t, "issues_empty.json"))

		adapter := newTestAdapter(t, f)

		if _, err := adapter.FetchIssuesByStates(context.Background(), []string{"done", "CANCELED"}); err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}

		calls := f.callsFor("query IssuesByStates")
		if len(calls) != 1 {
			t.Fatalf("IssuesByStates call count = %d, want 1", len(calls))
		}
		states, ok := calls[0].variables["states"].([]string)
		if !ok {
			t.Fatalf("states variable type = %T, want []string", calls[0].variables["states"])
		}
		want := []string{"Done", "Canceled"}
		if !reflect.DeepEqual(states, want) {
			t.Errorf("states = %v, want %v (canonical casing)", states, want)
		}
	})
}

func TestFetchIssueByID(t *testing.T) {
	t.Parallel()

	t.Run("merges inline and continuation comments sorted ascending", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueByID", loadFixture(t, "issue_by_id.json"))
		f.queueBody("query IssueComments", loadFixture(t, "comments_continuation.json"))

		adapter := newTestAdapter(t, f)

		issue, err := adapter.FetchIssueByID(context.Background(), "SOR-5")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}

		if issue.Comments == nil {
			t.Fatal("Comments = nil, want non-nil slice for fully populated issue")
		}
		gotIDs := commentIDs(issue.Comments)
		wantIDs := []string{"comment-0001", "comment-0002", "comment-0003"}
		if !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Errorf("comment order = %v, want %v (ascending createdAt over merged set)", gotIDs, wantIDs)
		}
		if issue.Identifier != "SOR-5" {
			t.Errorf("Identifier = %q, want %q", issue.Identifier, "SOR-5")
		}
	})

	t.Run("passes id verbatim", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name string
			id   string
		}{
			{"uuid", "a7c4f8e2-1b9d-4e3a-8f2c-6d5e4a3b2c1f"},
			{"identifier", "SOR-5"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				f := newFakeClient()
				seedPreflight(f, t)
				f.queueBody("query IssueByID", loadFixture(t, "issue_single_comment_page.json"))

				adapter := newTestAdapter(t, f)

				if _, err := adapter.FetchIssueByID(context.Background(), tt.id); err != nil {
					t.Fatalf("FetchIssueByID(%q): %v", tt.id, err)
				}

				call := f.callsFor("query IssueByID")[0]
				if call.variables["id"] != tt.id {
					t.Errorf("id variable = %v, want %q (verbatim)", call.variables["id"], tt.id)
				}
			})
		}
	})

	t.Run("top-level data.issue null is not found", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueByID", loadFixture(t, "issue_not_found.json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.FetchIssueByID(context.Background(), "SOR-999")

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("mid-continuation not found surfaces as not found", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueByID", loadFixture(t, "issue_by_id.json"))
		f.queueBody("query IssueComments", loadFixture(t, "issue_not_found.json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.FetchIssueByID(context.Background(), "SOR-5")

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

func TestFetchIssueComments(t *testing.T) {
	t.Parallel()

	t.Run("returns comments sorted ascending", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueComments", loadFixture(t, "comments_single_page.json"))

		adapter := newTestAdapter(t, f)

		comments, err := adapter.FetchIssueComments(context.Background(), "SOR-5")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}

		gotIDs := commentIDs(comments)
		wantIDs := []string{"comment-a", "comment-b"}
		if !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Errorf("comment order = %v, want %v (ascending createdAt)", gotIDs, wantIDs)
		}
	})

	t.Run("empty connection returns non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueComments", loadFixture(t, "comments_empty.json"))

		adapter := newTestAdapter(t, f)

		comments, err := adapter.FetchIssueComments(context.Background(), "SOR-5")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		if comments == nil {
			t.Fatal("FetchIssueComments = nil, want non-nil empty slice")
		}
		if len(comments) != 0 {
			t.Errorf("FetchIssueComments len = %d, want 0", len(comments))
		}
	})

	t.Run("nonexistent issue is not found", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueComments", loadFixture(t, "issue_not_found.json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.FetchIssueComments(context.Background(), "SOR-999")

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

func TestFetchIssueStatesByIDs(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty map with no API call", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		adapter := newTestAdapter(t, f)

		states, err := adapter.FetchIssueStatesByIDs(context.Background(), nil)
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs(nil): %v", err)
		}
		if states == nil || len(states) != 0 {
			t.Errorf("FetchIssueStatesByIDs(nil) = %v, want empty non-nil map", states)
		}
		if calls := f.callsFor("query IssueStatesByIDs"); len(calls) != 0 {
			t.Errorf("call count = %d, want 0 for empty input", len(calls))
		}
	})

	t.Run("keys by id and omits missing ids", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueStatesByIDs", loadFixture(t, "states_by_ids.json"))

		adapter := newTestAdapter(t, f)

		ids := []string{
			"a7c4f8e2-1b9d-4e3a-8f2c-6d5e4a3b2c1f",
			"d4f1a6c3-5e9b-4daf-bf4c-3b8e7d6a5c4f",
			"00000000-0000-0000-0000-000000000000",
		}
		states, err := adapter.FetchIssueStatesByIDs(context.Background(), ids)
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}

		want := map[string]string{
			"a7c4f8e2-1b9d-4e3a-8f2c-6d5e4a3b2c1f": "Todo",
			"d4f1a6c3-5e9b-4daf-bf4c-3b8e7d6a5c4f": "Done",
		}
		if !reflect.DeepEqual(states, want) {
			t.Errorf("FetchIssueStatesByIDs = %v, want %v (missing id omitted)", states, want)
		}

		call := f.callsFor("query IssueStatesByIDs")[0]
		gotIDs, ok := call.variables["ids"].([]string)
		if !ok {
			t.Fatalf("ids variable type = %T, want []string", call.variables["ids"])
		}
		if !reflect.DeepEqual(gotIDs, ids) {
			t.Errorf("ids variable = %v, want %v", gotIDs, ids)
		}
	})

	t.Run("checks context cancellation before each chunk", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		adapter := newTestAdapter(t, f)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := adapter.FetchIssueStatesByIDs(ctx, []string{"some-id"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("FetchIssueStatesByIDs cancelled error = %v, want %v", err, context.Canceled)
		}
		if calls := f.callsFor("query IssueStatesByIDs"); len(calls) != 0 {
			t.Errorf("call count = %d, want 0 when context already cancelled", len(calls))
		}
	})
}

func TestFetchIssueStatesByIdentifiers(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty map with no API call", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		adapter := newTestAdapter(t, f)

		states, err := adapter.FetchIssueStatesByIdentifiers(context.Background(), nil)
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers(nil): %v", err)
		}
		if states == nil || len(states) != 0 {
			t.Errorf("FetchIssueStatesByIdentifiers(nil) = %v, want empty non-nil map", states)
		}
		if calls := f.callsFor("query IssueStatesByNumbers"); len(calls) != 0 {
			t.Errorf("call count = %d, want 0 for empty input", len(calls))
		}
	})

	t.Run("splits identifiers, scopes to team, keys by identifier", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueStatesByNumbers", loadFixture(t, "states_by_numbers.json"))

		adapter := newTestAdapter(t, f)

		states, err := adapter.FetchIssueStatesByIdentifiers(context.Background(), []string{"SOR-5", "SOR-7", "SOR-99999"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}

		want := map[string]string{"SOR-5": "Todo", "SOR-7": "Backlog"}
		if !reflect.DeepEqual(states, want) {
			t.Errorf("FetchIssueStatesByIdentifiers = %v, want %v (missing number omitted)", states, want)
		}

		call := f.callsFor("query IssueStatesByNumbers")[0]
		if call.variables["teamKey"] != "SOR" {
			t.Errorf("teamKey = %v, want %q", call.variables["teamKey"], "SOR")
		}
		numbers, ok := call.variables["numbers"].([]float64)
		if !ok {
			t.Fatalf("numbers variable type = %T, want []float64", call.variables["numbers"])
		}
		wantNumbers := []float64{5, 7, 99999}
		if !reflect.DeepEqual(numbers, wantNumbers) {
			t.Errorf("numbers = %v, want %v", numbers, wantNumbers)
		}
	})

	t.Run("non-integer trailing part is skipped, no API call when none parse", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		adapter := newTestAdapter(t, f)

		states, err := adapter.FetchIssueStatesByIdentifiers(context.Background(), []string{"SOR-abc", "NODASH"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}
		if len(states) != 0 {
			t.Errorf("FetchIssueStatesByIdentifiers = %v, want empty map", states)
		}
		if calls := f.callsFor("query IssueStatesByNumbers"); len(calls) != 0 {
			t.Errorf("call count = %d, want 0 when no identifier yields a number", len(calls))
		}
	})

	t.Run("checks context cancellation before each chunk", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		adapter := newTestAdapter(t, f)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := adapter.FetchIssueStatesByIdentifiers(ctx, []string{"SOR-5"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("FetchIssueStatesByIdentifiers cancelled error = %v, want %v", err, context.Canceled)
		}
		if calls := f.callsFor("query IssueStatesByNumbers"); len(calls) != 0 {
			t.Errorf("call count = %d, want 0 when context already cancelled", len(calls))
		}
	})
}

func TestWriteStubs(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	seedPreflight(f, t)
	adapter := newTestAdapter(t, f)
	callsBefore := len(f.calls)

	tests := []struct {
		name string
		call func() error
	}{
		{"TransitionIssue", func() error { return adapter.TransitionIssue(context.Background(), "SOR-5", "Done") }},
		{"CommentIssue", func() error { return adapter.CommentIssue(context.Background(), "SOR-5", "hello") }},
		{"AddLabel", func() error { return adapter.AddLabel(context.Background(), "SOR-5", "needs-human") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()

			assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
		})
	}

	if len(f.calls) != callsBefore {
		t.Errorf("write stubs issued %d GraphQL calls, want 0", len(f.calls)-callsBefore)
	}
}

func TestSetMetricsAcceptsNil(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	seedPreflight(f, t)
	f.queueBody("query CandidateIssues", loadFixture(t, "issues_empty.json"))

	adapter := newTestAdapter(t, f)
	adapter.SetMetrics(nil)

	if _, err := adapter.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("FetchCandidateIssues with nil metrics: %v", err)
	}
}

// --- local helpers ---

func identifiers(issues []domain.Issue) []string {
	out := make([]string, len(issues))
	for i, iss := range issues {
		out[i] = iss.Identifier
	}
	return out
}

func commentIDs(comments []domain.Comment) []string {
	out := make([]string, len(comments))
	for i, c := range comments {
		out[i] = c.ID
	}
	return out
}
