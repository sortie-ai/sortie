package linear

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
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

	// The declared fallback lists must be the variables NewLinearAdapter
	// substitutes, because the dispatch preflight rules on the declared ones.
	if !slices.Equal(meta.DefaultActiveStates, defaultActiveStates) {
		t.Errorf("Trackers.Meta(%q).DefaultActiveStates = %v, want %v", "linear", meta.DefaultActiveStates, defaultActiveStates)
	}
	if !slices.Equal(meta.DefaultTerminalStates, defaultTerminalStates) {
		t.Errorf("Trackers.Meta(%q).DefaultTerminalStates = %v, want %v", "linear", meta.DefaultTerminalStates, defaultTerminalStates)
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

// endpointGuardConfig builds a config map for [TestNewLinearAdapterEndpointGuard].
// api_key is deliberately never set: every case in that test isolates the
// endpoint gate by relying on the next required-field check to fire only
// once the endpoint has been accepted.
func endpointGuardConfig(endpointKeyPresent bool, endpoint string) map[string]any {
	cfg := map[string]any{"project": "SOR"}
	if endpointKeyPresent {
		cfg["endpoint"] = endpoint
	}
	return cfg
}

func TestNewLinearAdapterEndpointGuard(t *testing.T) {
	t.Parallel()

	t.Run("endpoint accepted", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			config map[string]any
		}{
			{"absent endpoint key defaults to the hosted host", endpointGuardConfig(false, "")},
			{"empty endpoint value defaults to the hosted host", endpointGuardConfig(true, "")},
			{"whitespace-only endpoint value defaults to the hosted host", endpointGuardConfig(true, "   ")},
			{"valid custom endpoint is accepted", endpointGuardConfig(true, "  https://self-hosted.example.com/graphql  ")},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				_, err := NewLinearAdapter(tt.config)

				assertTrackerErrorKind(t, err, domain.ErrMissingTrackerAPIKey)
			})
		}
	})

	t.Run("endpoint rejected", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			endpoint string
		}{
			{"unsupported scheme", "ftp://example.com/graphql"},
			{"not a url at all", "not a url at all"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				_, err := NewLinearAdapter(endpointGuardConfig(true, tt.endpoint))

				assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
			})
		}
	})

	t.Run("credential-bearing malformed endpoint redacts the message", func(t *testing.T) {
		t.Parallel()

		const raw = "https://operator:s3cr3t@fd00::1:3000/graphql"

		_, err := NewLinearAdapter(endpointGuardConfig(true, raw))

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("NewLinearAdapter(%q) error type = %T, want *domain.TrackerError", raw, err)
		}
		if te.Kind != domain.ErrTrackerPayload {
			t.Errorf("NewLinearAdapter(%q) TrackerError.Kind = %q, want %q", raw, te.Kind, domain.ErrTrackerPayload)
		}
		msg := te.Error()
		if strings.Contains(msg, "operator") {
			t.Errorf("NewLinearAdapter(%q) error message %q leaks the username", raw, msg)
		}
		if strings.Contains(msg, "s3cr3t") {
			t.Errorf("NewLinearAdapter(%q) error message %q leaks the password", raw, msg)
		}
	})
}

// TestEndpointVerdictAgreesWithConstructor is a drift guard: both
// validateEndpoint and NewLinearAdapter defer their pass/fail verdict to
// [resolveEndpoint], so the two must never disagree for the same value.
func TestEndpointVerdictAgreesWithConstructor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
	}{
		{"empty", ""},
		{"whitespace-only", "   "},
		{"default hosted url", "https://api.linear.app/graphql"},
		{"valid self-hosted url with whitespace", "  https://self-hosted.example.com/graphql  "},
		{"unsupported scheme", "ftp://example.com/graphql"},
		{"not a url at all", "not a url at all"},
		{"credential-bearing malformed url", "https://operator:s3cr3t@fd00::1:3000/graphql"},
		{"scheme with no host", "https://"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			validatePass := len(validateEndpoint(tt.endpoint)) == 0

			_, err := NewLinearAdapter(endpointGuardConfig(true, tt.endpoint))
			constructorPass := !isPayloadError(err)

			if validatePass != constructorPass {
				t.Errorf("endpoint %q: validateEndpoint pass = %v, NewLinearAdapter pass = %v, want agreement", tt.endpoint, validatePass, constructorPass)
			}
		})
	}
}

func TestNewAdapterSuccess(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	seedPreflight(f, t)

	got, err := newAdapter(f, "SOR", defaultActiveStates, defaultTerminalStates, "in review", nil, nil)
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

func TestParseQueryFilter(t *testing.T) {
	t.Parallel()

	t.Run("rejects malformed and reserved filters as payload errors", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			raw         string
			wantKeyword string
		}{
			{"non-json string", "not json", ""},
			{"json array", `["a", "b"]`, ""},
			{"json number", "42", ""},
			{"json string", `"backend"`, ""},
			{"json boolean", "true", ""},
			{"json null", "null", ""},
			{"reserved team key", `{"team": {"key": {"eq": "ENG"}}}`, "team"},
			{"reserved state key", `{"state": {"name": {"in": ["Done"]}}}`, "state"},
			{"both reserved keys report team first", `{"team": {}, "state": {}}`, "team"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got, err := parseQueryFilter(tt.raw)

				if got != nil {
					t.Errorf("parseQueryFilter(%q) = %v, want nil map on error", tt.raw, got)
				}
				assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
				if tt.wantKeyword != "" {
					var te *domain.TrackerError
					if errors.As(err, &te) && !strings.Contains(te.Message, tt.wantKeyword) {
						t.Errorf("parseQueryFilter(%q) message = %q, want it to name %q", tt.raw, te.Message, tt.wantKeyword)
					}
				}
			})
		}
	})

	t.Run("empty string is the unset passthrough", func(t *testing.T) {
		t.Parallel()

		got, err := parseQueryFilter("")
		if err != nil {
			t.Fatalf("parseQueryFilter(%q) = %v, want nil error", "", err)
		}
		if got != nil {
			t.Errorf("parseQueryFilter(%q) = %v, want nil map", "", got)
		}
	})

	t.Run("valid object is returned decoded", func(t *testing.T) {
		t.Parallel()

		raw := `{"labels": {"some": {"name": {"eq": "backend"}}}}`
		got, err := parseQueryFilter(raw)
		if err != nil {
			t.Fatalf("parseQueryFilter(%q) = %v, want nil error", raw, err)
		}
		want := map[string]any{
			"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eq": "backend"}}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseQueryFilter(%q) = %v, want %v", raw, got, want)
		}
	})

	t.Run("empty object is a valid no-op map", func(t *testing.T) {
		t.Parallel()

		got, err := parseQueryFilter("{}")
		if err != nil {
			t.Fatalf("parseQueryFilter(%q) = %v, want nil error", "{}", err)
		}
		if got == nil {
			t.Fatalf("parseQueryFilter(%q) = nil, want non-nil empty map", "{}")
		}
		if len(got) != 0 {
			t.Errorf("parseQueryFilter(%q) = %v, want empty map", "{}", got)
		}
	})
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
			adaptertest.AssertIssueNormalized(t, iss)
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

	t.Run("sends configured active states inside the merged filter", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "issues_empty.json"))

		adapter := newTestAdapter(t, f)

		if _, err := adapter.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		call := f.callsFor("query CandidateIssues")[0]
		filter := assertFilterVar(t, call)
		assertTeamFilter(t, filter)
		assertStateFilter(t, filter, []string{"Backlog", "Todo", "In Progress"})
		assertNoTopLevelVar(t, call, "teamKey")
		assertNoTopLevelVar(t, call, "states")
	})

	t.Run("operator filter merges with team and state and returns the fixture issues", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "candidates_filtered.json"))

		operatorFilter := map[string]any{
			"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eq": "backend"}}},
		}
		adapter := newTestAdapterWithFilter(t, f, operatorFilter)

		issues, err := adapter.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		gotOrder := identifiers(issues)
		wantOrder := []string{"SOR-7", "SOR-5"}
		if !reflect.DeepEqual(gotOrder, wantOrder) {
			t.Errorf("FetchCandidateIssues order = %v, want %v (exactly the fixture issues)", gotOrder, wantOrder)
		}

		call := f.callsFor("query CandidateIssues")[0]
		filter := assertFilterVar(t, call)
		assertTeamFilter(t, filter)
		assertStateFilter(t, filter, []string{"Backlog", "Todo", "In Progress"})
		if !reflect.DeepEqual(filter["labels"], operatorFilter["labels"]) {
			t.Errorf("filter[labels] = %v, want %v (operator fragment copied verbatim)", filter["labels"], operatorFilter["labels"])
		}
		assertFilterKeys(t, filter, []string{"team", "state", "labels"})
	})

	t.Run("unset filter passes through exactly team and state", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "issues_empty.json"))

		adapter := newTestAdapterWithFilter(t, f, nil)

		if _, err := adapter.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		call := f.callsFor("query CandidateIssues")[0]
		filter := assertFilterVar(t, call)
		assertTeamFilter(t, filter)
		assertStateFilter(t, filter, []string{"Backlog", "Todo", "In Progress"})
		assertFilterKeys(t, filter, []string{"team", "state"})
	})

	t.Run("empty-object filter is a no-op equal to the passthrough object", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "issues_empty.json"))

		parsed, err := parseQueryFilter("{}")
		if err != nil {
			t.Fatalf("parseQueryFilter(%q) = %v, want nil error", "{}", err)
		}
		adapter := newTestAdapterWithFilter(t, f, parsed)

		if _, err := adapter.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		call := f.callsFor("query CandidateIssues")[0]
		filter := assertFilterVar(t, call)
		assertTeamFilter(t, filter)
		assertStateFilter(t, filter, []string{"Backlog", "Todo", "In Progress"})
		assertFilterKeys(t, filter, []string{"team", "state"})
	})

	t.Run("multi-page filtered fetch carries the cursor and the same filter on page two", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query CandidateIssues", loadFixture(t, "candidates_page1.json"))
		f.queueBody("query CandidateIssues", loadFixture(t, "candidates_page2.json"))

		operatorFilter := map[string]any{
			"assignee": map[string]any{"isMe": map[string]any{"eq": true}},
		}
		adapter := newTestAdapterWithFilter(t, f, operatorFilter)

		if _, err := adapter.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		calls := f.callsFor("query CandidateIssues")
		if len(calls) != 2 {
			t.Fatalf("CandidateIssues call count = %d, want 2 (one per page)", len(calls))
		}

		wantCursor := "eyJrZXkiOiJiMmQ5ZTRhMS0zYzdmLTRiOGUtOWQyYS0xZjZjNWI0ZTNhMmQiLCJjcmVhdGVkQXQiOiIyMDI2LTA2LTA5In0="
		if got := calls[1].variables["after"]; got != wantCursor {
			t.Errorf("second page after = %v, want %q (page-1 end cursor)", got, wantCursor)
		}

		filter := assertFilterVar(t, calls[1])
		assertTeamFilter(t, filter)
		assertStateFilter(t, filter, []string{"Backlog", "Todo", "In Progress"})
		if !reflect.DeepEqual(filter["assignee"], operatorFilter["assignee"]) {
			t.Errorf("second page filter[assignee] = %v, want %v (filter reused across pages)", filter["assignee"], operatorFilter["assignee"])
		}
		assertFilterKeys(t, filter, []string{"team", "state", "assignee"})
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
		adaptertest.AssertNoRequestOnEmptyInput(t, len(f.callsFor("query IssuesByStates")), "FetchIssuesByStates")
	})

	t.Run("canonical-cases supplied states inside the merged filter", func(t *testing.T) {
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
		filter := assertFilterVar(t, calls[0])
		assertTeamFilter(t, filter)
		assertStateFilter(t, filter, []string{"Done", "Canceled"})
		assertNoTopLevelVar(t, calls[0], "states")
		assertNoTopLevelVar(t, calls[0], "teamKey")
	})

	t.Run("operator filter merges with team and supplied states", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssuesByStates", loadFixture(t, "issues_empty.json"))

		operatorFilter := map[string]any{
			"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eq": "backend"}}},
		}
		adapter := newTestAdapterWithFilter(t, f, operatorFilter)

		if _, err := adapter.FetchIssuesByStates(context.Background(), []string{"done", "CANCELED"}); err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}

		calls := f.callsFor("query IssuesByStates")
		if len(calls) != 1 {
			t.Fatalf("IssuesByStates call count = %d, want 1", len(calls))
		}
		filter := assertFilterVar(t, calls[0])
		assertTeamFilter(t, filter)
		assertStateFilter(t, filter, []string{"Done", "Canceled"})
		if !reflect.DeepEqual(filter["labels"], operatorFilter["labels"]) {
			t.Errorf("filter[labels] = %v, want %v (operator fragment copied verbatim)", filter["labels"], operatorFilter["labels"])
		}
		assertFilterKeys(t, filter, []string{"team", "state", "labels"})
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
		adaptertest.AssertCommentsAscending(t, issue.Comments)

		continuation := f.callsFor("query IssueComments")
		if len(continuation) != 1 {
			t.Fatalf("continuation calls = %d, want 1", len(continuation))
		}
		if continuation[0].variables["after"] != "comment-0002" {
			t.Errorf("continuation after = %v, want %q (inline endCursor, not page 1)", continuation[0].variables["after"], "comment-0002")
		}
	})

	t.Run("inline page hasNextPage without end cursor is missing cursor", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueByID", loadFixture(t, "issue_missing_comment_cursor.json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.FetchIssueByID(context.Background(), "SOR-5")

		assertTrackerErrorKind(t, err, domain.ErrTrackerMissingCursor)
		if calls := f.callsFor("query IssueComments"); len(calls) != 0 {
			t.Errorf("continuation calls = %d, want 0 (fail fast before pagination)", len(calls))
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
		adaptertest.AssertEmptyNonNil(t, comments, "FetchIssueComments")
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
		adaptertest.AssertStateMapOmitsMissing(t, ids, states)

		call := f.callsFor("query IssueStatesByIDs")[0]
		gotIDs, ok := call.variables["ids"].([]string)
		if !ok {
			t.Fatalf("ids variable type = %T, want []string", call.variables["ids"])
		}
		if !reflect.DeepEqual(gotIDs, ids) {
			t.Errorf("ids variable = %v, want %v", gotIDs, ids)
		}
	})

	t.Run("a configured filter introduces no filter variable", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueStatesByIDs", loadFixture(t, "states_by_ids.json"))

		operatorFilter := map[string]any{
			"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eq": "backend"}}},
		}
		adapter := newTestAdapterWithFilter(t, f, operatorFilter)

		if _, err := adapter.FetchIssueStatesByIDs(context.Background(), []string{"a7c4f8e2-1b9d-4e3a-8f2c-6d5e4a3b2c1f"}); err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}

		call := f.callsFor("query IssueStatesByIDs")[0]
		assertVarKeys(t, call, []string{"ids", "first"})
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

	t.Run("a configured filter introduces no filter variable and keeps teamKey", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("query IssueStatesByNumbers", loadFixture(t, "states_by_numbers.json"))

		operatorFilter := map[string]any{
			"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eq": "backend"}}},
		}
		adapter := newTestAdapterWithFilter(t, f, operatorFilter)

		if _, err := adapter.FetchIssueStatesByIdentifiers(context.Background(), []string{"SOR-5", "SOR-7"}); err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}

		call := f.callsFor("query IssueStatesByNumbers")[0]
		assertVarKeys(t, call, []string{"teamKey", "numbers", "first"})
		if call.variables["teamKey"] != "SOR" {
			t.Errorf("teamKey = %v, want %q (reconciliation query is unchanged)", call.variables["teamKey"], "SOR")
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

func TestRunMutation(t *testing.T) {
	t.Parallel()

	t.Run("success returns nil", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("IssueUpdateState", loadFixture(t, "issue_update_success.json"))

		err := runMutation(context.Background(), f, newTextLogger(&bytes.Buffer{}),
			queryIssueUpdateState, map[string]any{"id": "SOR-5", "stateId": "state-inreview"},
			decodeIssueUpdateSuccess)
		if err != nil {
			t.Fatalf("runMutation success path = %v, want nil", err)
		}
	})

	t.Run("non-empty errors array is a failure regardless of success", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("IssueUpdateState", loadFixture(t, "mutation_invalid_input.json"))

		err := runMutation(context.Background(), f, newTextLogger(&bytes.Buffer{}),
			queryIssueUpdateState, map[string]any{"id": "SOR-5", "stateId": "state-inreview"},
			decodeIssueUpdateSuccess)

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("success false with no errors is API error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("IssueUpdateState", loadFixture(t, "mutation_success_false.json"))

		err := runMutation(context.Background(), f, newTextLogger(&bytes.Buffer{}),
			queryIssueUpdateState, map[string]any{"id": "SOR-5", "stateId": "state-inreview"},
			decodeIssueUpdateSuccess)

		assertTrackerErrorKind(t, err, domain.ErrTrackerAPI)
	})

	t.Run("malformed body is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("IssueUpdateState", []byte("{not json"))

		err := runMutation(context.Background(), f, newTextLogger(&bytes.Buffer{}),
			queryIssueUpdateState, map[string]any{"id": "SOR-5", "stateId": "state-inreview"},
			decodeIssueUpdateSuccess)

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("transport error from Execute is returned unchanged", func(t *testing.T) {
		t.Parallel()

		transportErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "dial tcp: timeout"}
		f := newFakeClient()
		f.queueResponse("IssueUpdateState", fakeResponse{err: transportErr})

		err := runMutation(context.Background(), f, newTextLogger(&bytes.Buffer{}),
			queryIssueUpdateState, map[string]any{"id": "SOR-5", "stateId": "state-inreview"},
			decodeIssueUpdateSuccess)

		if !errors.Is(err, transportErr) {
			t.Errorf("runMutation transport error = %v, want %v unchanged", err, transportErr)
		}
	})
}

func TestTransitionIssue(t *testing.T) {
	t.Parallel()

	t.Run("resolves target then applies the resolved state id", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveStateID", loadFixture(t, "state_resolve_hit.json"))
		f.queueBody("IssueUpdateState", loadFixture(t, "issue_update_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.TransitionIssue(context.Background(), "SOR-5", "In Review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}

		resolves := f.callsFor("ResolveStateID")
		if len(resolves) != 1 {
			t.Fatalf("ResolveStateID call count = %d, want 1", len(resolves))
		}
		if got := resolves[0].variables["stateName"]; got != "In Review" {
			t.Errorf("ResolveStateID stateName = %v, want %q (verbatim target)", got, "In Review")
		}
		if got := resolves[0].variables["issueId"]; got != "SOR-5" {
			t.Errorf("ResolveStateID issueId = %v, want %q (verbatim)", got, "SOR-5")
		}

		mutations := f.callsFor("IssueUpdateState")
		if len(mutations) != 1 {
			t.Fatalf("IssueUpdateState call count = %d, want 1", len(mutations))
		}
		if got := mutations[0].variables["stateId"]; got != "state-inreview" {
			t.Errorf("IssueUpdateState stateId = %v, want %q (resolved id)", got, "state-inreview")
		}
		if got := mutations[0].variables["id"]; got != "SOR-5" {
			t.Errorf("IssueUpdateState id = %v, want %q (verbatim)", got, "SOR-5")
		}
	})

	t.Run("handoff name resolves case-insensitively and returns nil", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveStateID", loadFixture(t, "state_resolve_hit.json"))
		f.queueBody("IssueUpdateState", loadFixture(t, "issue_update_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.TransitionIssue(context.Background(), "SOR-5", "in review"); err != nil {
			t.Fatalf("TransitionIssue handoff name: %v", err)
		}

		resolves := f.callsFor("ResolveStateID")
		if len(resolves) != 1 {
			t.Fatalf("ResolveStateID call count = %d, want 1", len(resolves))
		}
		if got := resolves[0].variables["stateName"]; got != "in review" {
			t.Errorf("ResolveStateID stateName = %v, want %q (caller value sent verbatim, no casing cache)", got, "in review")
		}
	})

	t.Run("empty states resolve is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveStateID", loadFixture(t, "state_resolve_empty.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.TransitionIssue(context.Background(), "SOR-5", "Nonexistent")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
		if calls := f.callsFor("IssueUpdateState"); len(calls) != 0 {
			t.Errorf("IssueUpdateState call count = %d, want 0 when no state resolves", len(calls))
		}
	})

	t.Run("not found issue is not found", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveStateID", loadFixture(t, "issue_not_found.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.TransitionIssue(context.Background(), "SOR-999", "In Review")

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
		if calls := f.callsFor("IssueUpdateState"); len(calls) != 0 {
			t.Errorf("IssueUpdateState call count = %d, want 0 for a missing issue", len(calls))
		}
	})

	t.Run("null issue with no errors on resolve is not found", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveStateID", loadFixture(t, "state_resolve_null_issue.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.TransitionIssue(context.Background(), "SOR-999", "In Review")

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
		if calls := f.callsFor("IssueUpdateState"); len(calls) != 0 {
			t.Errorf("IssueUpdateState call count = %d, want 0 for a null issue", len(calls))
		}
	})

	t.Run("transport error on resolve propagates and skips the mutation", func(t *testing.T) {
		t.Parallel()

		transportErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "dial tcp: timeout"}
		f := newFakeClient()
		seedPreflight(f, t)
		f.queueResponse("ResolveStateID", fakeResponse{err: transportErr})

		adapter := newTestAdapter(t, f)

		err := adapter.TransitionIssue(context.Background(), "SOR-5", "In Review")

		if !errors.Is(err, transportErr) {
			t.Errorf("TransitionIssue resolve transport error = %v, want %v unchanged", err, transportErr)
		}
		if calls := f.callsFor("IssueUpdateState"); len(calls) != 0 {
			t.Errorf("IssueUpdateState call count = %d, want 0 when the resolve fails", len(calls))
		}
	})

	t.Run("malformed resolve body is a payload error and skips the mutation", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveStateID", []byte("{not json"))

		adapter := newTestAdapter(t, f)

		err := adapter.TransitionIssue(context.Background(), "SOR-5", "In Review")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
		if calls := f.callsFor("IssueUpdateState"); len(calls) != 0 {
			t.Errorf("IssueUpdateState call count = %d, want 0 when the resolve body is malformed", len(calls))
		}
	})
}

func TestCommentIssue(t *testing.T) {
	t.Parallel()

	t.Run("sends the markdown body verbatim and returns nil", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("CommentCreate", loadFixture(t, "comment_create_success.json"))

		adapter := newTestAdapter(t, f)

		body := "CI failed on `make test`.\n\n- step: build\n- see **logs** for details"
		if err := adapter.CommentIssue(context.Background(), "SOR-5", body); err != nil {
			t.Fatalf("CommentIssue: %v", err)
		}

		calls := f.callsFor("CommentCreate")
		if len(calls) != 1 {
			t.Fatalf("CommentCreate call count = %d, want 1", len(calls))
		}
		if got := calls[0].variables["body"]; got != body {
			t.Errorf("CommentCreate body = %q, want %q (verbatim markdown, no ADF wrapping)", got, body)
		}
		if got := calls[0].variables["issueId"]; got != "SOR-5" {
			t.Errorf("CommentCreate issueId = %v, want %q (verbatim)", got, "SOR-5")
		}
	})

	t.Run("rejected create surfaces the classified error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("CommentCreate", loadFixture(t, "mutation_auth_error.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.CommentIssue(context.Background(), "SOR-5", "body")

		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})
}

func TestAddLabel(t *testing.T) {
	t.Parallel()

	t.Run("existing label attaches without creating and never calls LabelCreate", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_hit.json"))
		f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}

		if calls := f.callsFor("LabelCreate"); len(calls) != 0 {
			t.Errorf("LabelCreate call count = %d, want 0 when the label already exists", len(calls))
		}
		if calls := f.callsFor("ResolveIssueTeam"); len(calls) != 0 {
			t.Errorf("ResolveIssueTeam call count = %d, want 0 when the label already exists", len(calls))
		}

		attaches := f.callsFor("IssueAddLabel")
		if len(attaches) != 1 {
			t.Fatalf("IssueAddLabel call count = %d, want 1", len(attaches))
		}
		labelIDs, ok := attaches[0].variables["labelIds"].([]string)
		if !ok {
			t.Fatalf("IssueAddLabel labelIds type = %T, want []string", attaches[0].variables["labelIds"])
		}
		want := []string{"label-team-needs-human"}
		if !reflect.DeepEqual(labelIDs, want) {
			t.Errorf("IssueAddLabel addedLabelIds = %v, want %v (resolved id, append)", labelIDs, want)
		}
		if got := attaches[0].variables["id"]; got != "SOR-5" {
			t.Errorf("IssueAddLabel id = %v, want %q (verbatim)", got, "SOR-5")
		}

		// The mutation sends addedLabelIds carrying only the new label, so
		// it appends to the issue's existing labels rather than replacing
		// them.
		before := []string{"existing"}
		after := append(slices.Clone(before), "needs-human")
		adaptertest.AssertLabelAddIsAdditive(t, before, after, "needs-human")
	})

	t.Run("prefers the team-scoped label over the workspace-scoped label", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_team_preference.json"))
		f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}

		attaches := f.callsFor("IssueAddLabel")
		if len(attaches) != 1 {
			t.Fatalf("IssueAddLabel call count = %d, want 1", len(attaches))
		}
		labelIDs := attaches[0].variables["labelIds"].([]string)
		want := []string{"label-team-needs-human"}
		if !reflect.DeepEqual(labelIDs, want) {
			t.Errorf("IssueAddLabel addedLabelIds = %v, want %v (team-scoped preferred over workspace)", labelIDs, want)
		}
	})

	t.Run("falls back to the workspace-scoped label when no team-scoped label exists", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_workspace_only.json"))
		f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}

		if calls := f.callsFor("LabelCreate"); len(calls) != 0 {
			t.Errorf("LabelCreate call count = %d, want 0 when a workspace label exists", len(calls))
		}
		if calls := f.callsFor("ResolveIssueTeam"); len(calls) != 0 {
			t.Errorf("ResolveIssueTeam call count = %d, want 0 when a workspace label exists", len(calls))
		}

		attaches := f.callsFor("IssueAddLabel")
		if len(attaches) != 1 {
			t.Fatalf("IssueAddLabel call count = %d, want 1", len(attaches))
		}
		labelIDs := attaches[0].variables["labelIds"].([]string)
		want := []string{"label-workspace-needs-human"}
		if !reflect.DeepEqual(labelIDs, want) {
			t.Errorf("IssueAddLabel addedLabelIds = %v, want %v (workspace fallback)", labelIDs, want)
		}
	})

	t.Run("missing label resolves team, creates, then attaches the created id", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "label_create_success.json"))
		f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}

		creates := f.callsFor("LabelCreate")
		if len(creates) != 1 {
			t.Fatalf("LabelCreate call count = %d, want 1", len(creates))
		}
		if got := creates[0].variables["teamId"]; got != "team-uuid-sor" {
			t.Errorf("LabelCreate teamId = %v, want %q (resolved team UUID)", got, "team-uuid-sor")
		}
		if got := creates[0].variables["name"]; got != "needs-human" {
			t.Errorf("LabelCreate name = %v, want %q (verbatim)", got, "needs-human")
		}

		attaches := f.callsFor("IssueAddLabel")
		if len(attaches) != 1 {
			t.Fatalf("IssueAddLabel call count = %d, want 1", len(attaches))
		}
		labelIDs := attaches[0].variables["labelIds"].([]string)
		want := []string{"label-created-needs-human"}
		if !reflect.DeepEqual(labelIDs, want) {
			t.Errorf("IssueAddLabel addedLabelIds = %v, want %v (created id)", labelIDs, want)
		}
	})

	t.Run("concurrent create race re-resolves and attaches the re-resolved id", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_hit.json"))
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "label_create_conflict.json"))
		f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human"); err != nil {
			t.Fatalf("AddLabel race path = %v, want nil (re-resolve hides the spurious conflict)", err)
		}

		resolves := f.callsFor("ResolveLabel")
		if len(resolves) != 2 {
			t.Fatalf("ResolveLabel call count = %d, want 2 (initial miss then post-create re-resolve)", len(resolves))
		}

		attaches := f.callsFor("IssueAddLabel")
		if len(attaches) != 1 {
			t.Fatalf("IssueAddLabel call count = %d, want 1", len(attaches))
		}
		labelIDs := attaches[0].variables["labelIds"].([]string)
		want := []string{"label-team-needs-human"}
		if !reflect.DeepEqual(labelIDs, want) {
			t.Errorf("IssueAddLabel addedLabelIds = %v, want %v (re-resolved id from the concurrent create)", labelIDs, want)
		}
	})

	t.Run("payload create error that still resolves nothing surfaces the create error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "label_create_conflict.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
		if calls := f.callsFor("IssueAddLabel"); len(calls) != 0 {
			t.Errorf("IssueAddLabel call count = %d, want 0 when the create error is genuine", len(calls))
		}
	})

	t.Run("forbidden create is auth error and does not re-resolve", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "mutation_forbidden.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human")

		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)

		if calls := f.callsFor("ResolveLabel"); len(calls) != 1 {
			t.Errorf("ResolveLabel call count = %d, want 1 (no re-resolve on a forbidden create)", len(calls))
		}
		if calls := f.callsFor("IssueAddLabel"); len(calls) != 0 {
			t.Errorf("IssueAddLabel call count = %d, want 0 on a forbidden create", len(calls))
		}
	})

	t.Run("create success with null label id is a payload error when re-resolve still misses", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "label_create_success_no_id.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)

		creates := f.callsFor("LabelCreate")
		if len(creates) != 1 {
			t.Fatalf("LabelCreate call count = %d, want 1", len(creates))
		}
		if resolves := f.callsFor("ResolveLabel"); len(resolves) != 2 {
			t.Errorf("ResolveLabel call count = %d, want 2 (the empty-id payload error triggers one re-resolve)", len(resolves))
		}
		if calls := f.callsFor("IssueAddLabel"); len(calls) != 0 {
			t.Errorf("IssueAddLabel call count = %d, want 0 when the create returns no id and re-resolve misses", len(calls))
		}
	})

	t.Run("create success with null label id recovers via re-resolve and attaches the re-resolved id", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_hit.json"))
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "label_create_success_no_id.json"))
		f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human"); err != nil {
			t.Fatalf("AddLabel empty-id recovery path = %v, want nil (re-resolve recovers from the empty-id payload error)", err)
		}

		if resolves := f.callsFor("ResolveLabel"); len(resolves) != 2 {
			t.Fatalf("ResolveLabel call count = %d, want 2 (empty-id payload error triggers one re-resolve)", len(resolves))
		}

		attaches := f.callsFor("IssueAddLabel")
		if len(attaches) != 1 {
			t.Fatalf("IssueAddLabel call count = %d, want 1", len(attaches))
		}
		labelIDs := attaches[0].variables["labelIds"].([]string)
		want := []string{"label-team-needs-human"}
		if !reflect.DeepEqual(labelIDs, want) {
			t.Errorf("IssueAddLabel addedLabelIds = %v, want %v (re-resolved id after the empty-id create)", labelIDs, want)
		}
	})

	t.Run("initial resolve transport error propagates and skips create and attach", func(t *testing.T) {
		t.Parallel()

		transportErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "dial tcp: timeout"}
		f := newFakeClient()
		seedPreflight(f, t)
		f.queueResponse("ResolveLabel", fakeResponse{err: transportErr})

		adapter := newTestAdapter(t, f)

		err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human")

		if !errors.Is(err, transportErr) {
			t.Errorf("AddLabel resolve transport error = %v, want %v unchanged", err, transportErr)
		}
		if calls := f.callsFor("ResolveIssueTeam"); len(calls) != 0 {
			t.Errorf("ResolveIssueTeam call count = %d, want 0 when the initial resolve fails", len(calls))
		}
		if calls := f.callsFor("LabelCreate"); len(calls) != 0 {
			t.Errorf("LabelCreate call count = %d, want 0 when the initial resolve fails", len(calls))
		}
		if calls := f.callsFor("IssueAddLabel"); len(calls) != 0 {
			t.Errorf("IssueAddLabel call count = %d, want 0 when the initial resolve fails", len(calls))
		}
	})

	t.Run("team resolve error on a missing label propagates and skips create and attach", func(t *testing.T) {
		t.Parallel()

		transportErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "dial tcp: timeout"}
		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueResponse("ResolveIssueTeam", fakeResponse{err: transportErr})

		adapter := newTestAdapter(t, f)

		err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human")

		if !errors.Is(err, transportErr) {
			t.Errorf("AddLabel team resolve transport error = %v, want %v unchanged", err, transportErr)
		}
		if calls := f.callsFor("LabelCreate"); len(calls) != 0 {
			t.Errorf("LabelCreate call count = %d, want 0 when the team resolve fails", len(calls))
		}
		if calls := f.callsFor("IssueAddLabel"); len(calls) != 0 {
			t.Errorf("IssueAddLabel call count = %d, want 0 when the team resolve fails", len(calls))
		}
	})

	t.Run("re-resolve transport error after a payload create error propagates", func(t *testing.T) {
		t.Parallel()

		transportErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "dial tcp: timeout"}
		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_miss.json"))
		f.queueResponse("ResolveLabel", fakeResponse{err: transportErr})
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "label_create_conflict.json"))

		adapter := newTestAdapter(t, f)

		err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human")

		if !errors.Is(err, transportErr) {
			t.Errorf("AddLabel re-resolve transport error = %v, want %v unchanged (re-resolve failure overrides the create error)", err, transportErr)
		}
		if resolves := f.callsFor("ResolveLabel"); len(resolves) != 2 {
			t.Errorf("ResolveLabel call count = %d, want 2 (initial miss then failing re-resolve)", len(resolves))
		}
		if calls := f.callsFor("IssueAddLabel"); len(calls) != 0 {
			t.Errorf("IssueAddLabel call count = %d, want 0 when the re-resolve fails", len(calls))
		}
	})

	t.Run("missing label with no workspace fallback creates then attaches", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_other_team.json"))
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))
		f.queueBody("LabelCreate", loadFixture(t, "label_create_success.json"))
		f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))

		adapter := newTestAdapter(t, f)

		if err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}

		if creates := f.callsFor("LabelCreate"); len(creates) != 1 {
			t.Fatalf("LabelCreate call count = %d, want 1 (a label scoped to another team is not a match)", len(creates))
		}
		attaches := f.callsFor("IssueAddLabel")
		if len(attaches) != 1 {
			t.Fatalf("IssueAddLabel call count = %d, want 1", len(attaches))
		}
		labelIDs := attaches[0].variables["labelIds"].([]string)
		want := []string{"label-created-needs-human"}
		if !reflect.DeepEqual(labelIDs, want) {
			t.Errorf("IssueAddLabel addedLabelIds = %v, want %v (created id; the other-team label is ignored)", labelIDs, want)
		}
	})
}

func TestResolveTeamID(t *testing.T) {
	t.Parallel()

	t.Run("transport error from Execute is returned unchanged", func(t *testing.T) {
		t.Parallel()

		transportErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "dial tcp: timeout"}
		f := newFakeClient()
		seedPreflight(f, t)
		f.queueResponse("ResolveIssueTeam", fakeResponse{err: transportErr})

		adapter := newTestAdapter(t, f)

		_, err := adapter.resolveTeamID(context.Background(), "SOR-5")

		if !errors.Is(err, transportErr) {
			t.Errorf("resolveTeamID transport error = %v, want %v unchanged", err, transportErr)
		}
	})

	t.Run("malformed body is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveIssueTeam", []byte("{not json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.resolveTeamID(context.Background(), "SOR-5")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("graphql errors array is classified", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveIssueTeam", loadFixture(t, "mutation_invalid_input.json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.resolveTeamID(context.Background(), "SOR-5")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("null issue with no errors is not found", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_null_issue.json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.resolveTeamID(context.Background(), "SOR-999")

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("resolves the owning team id verbatim", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveIssueTeam", loadFixture(t, "team_resolve_hit.json"))

		adapter := newTestAdapter(t, f)

		teamID, err := adapter.resolveTeamID(context.Background(), "SOR-5")
		if err != nil {
			t.Fatalf("resolveTeamID: %v", err)
		}
		if teamID != "team-uuid-sor" {
			t.Errorf("resolveTeamID = %q, want %q", teamID, "team-uuid-sor")
		}
		if got := f.callsFor("ResolveIssueTeam")[0].variables["issueId"]; got != "SOR-5" {
			t.Errorf("ResolveIssueTeam issueId = %v, want %q (verbatim)", got, "SOR-5")
		}
	})
}

func TestResolveLabelID(t *testing.T) {
	t.Parallel()

	t.Run("transport error from Execute is returned unchanged", func(t *testing.T) {
		t.Parallel()

		transportErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "dial tcp: timeout"}
		f := newFakeClient()
		seedPreflight(f, t)
		f.queueResponse("ResolveLabel", fakeResponse{err: transportErr})

		adapter := newTestAdapter(t, f)

		_, _, err := adapter.resolveLabelID(context.Background(), "needs-human")

		if !errors.Is(err, transportErr) {
			t.Errorf("resolveLabelID transport error = %v, want %v unchanged", err, transportErr)
		}
	})

	t.Run("malformed body is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", []byte("{not json"))

		adapter := newTestAdapter(t, f)

		_, _, err := adapter.resolveLabelID(context.Background(), "needs-human")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("graphql errors array is classified", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "mutation_auth_error.json"))

		adapter := newTestAdapter(t, f)

		_, _, err := adapter.resolveLabelID(context.Background(), "needs-human")

		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})

	t.Run("a label scoped to a different team reports not found", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("ResolveLabel", loadFixture(t, "label_resolve_other_team.json"))

		adapter := newTestAdapter(t, f)

		labelID, found, err := adapter.resolveLabelID(context.Background(), "needs-human")
		if err != nil {
			t.Fatalf("resolveLabelID: %v", err)
		}
		if found {
			t.Errorf("resolveLabelID found = %v, want false (the only match belongs to another team)", found)
		}
		if labelID != "" {
			t.Errorf("resolveLabelID id = %q, want empty string", labelID)
		}
	})
}

func TestCreateLabel(t *testing.T) {
	t.Parallel()

	t.Run("success with no label id is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("LabelCreate", loadFixture(t, "label_create_success_no_id.json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.createLabel(context.Background(), "team-uuid-sor", "needs-human")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("malformed body is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("LabelCreate", []byte("{not json"))

		adapter := newTestAdapter(t, f)

		_, err := adapter.createLabel(context.Background(), "team-uuid-sor", "needs-human")

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("success returns the created label id", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)
		f.queueBody("LabelCreate", loadFixture(t, "label_create_success.json"))

		adapter := newTestAdapter(t, f)

		labelID, err := adapter.createLabel(context.Background(), "team-uuid-sor", "needs-human")
		if err != nil {
			t.Fatalf("createLabel: %v", err)
		}
		if labelID != "label-created-needs-human" {
			t.Errorf("createLabel = %q, want %q", labelID, "label-created-needs-human")
		}
	})
}

func TestDecodeCommentCreateSuccess(t *testing.T) {
	t.Parallel()

	t.Run("malformed body is a payload error", func(t *testing.T) {
		t.Parallel()

		_, _, err := decodeCommentCreateSuccess([]byte("{not json"))

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("well-formed success body reports success with no error", func(t *testing.T) {
		t.Parallel()

		errs, success, err := decodeCommentCreateSuccess(loadFixture(t, "comment_create_success.json"))
		if err != nil {
			t.Fatalf("decodeCommentCreateSuccess: %v", err)
		}
		if !success {
			t.Errorf("decodeCommentCreateSuccess success = %v, want true", success)
		}
		if len(errs) != 0 {
			t.Errorf("decodeCommentCreateSuccess errs = %v, want empty", errs)
		}
	})
}

func TestIsPayloadError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"payload tracker error", &domain.TrackerError{Kind: domain.ErrTrackerPayload}, true},
		{"non-payload tracker error", &domain.TrackerError{Kind: domain.ErrTrackerAuth}, false},
		{"plain error is not a payload error", errors.New("boom"), false},
		{"nil error is not a payload error", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isPayloadError(tt.err); got != tt.want {
				t.Errorf("isPayloadError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestWriteErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fixture  string
		wantKind domain.TrackerErrorKind
	}{
		{"authentication error maps to auth", "mutation_auth_error.json", domain.ErrTrackerAuth},
		{"feature not accessible maps to auth", "mutation_forbidden.json", domain.ErrTrackerAuth},
		{"invalid input maps to payload", "mutation_invalid_input.json", domain.ErrTrackerPayload},
		{"ratelimited maps to API", "mutation_ratelimited.json", domain.ErrTrackerAPI},
		{"success false with no errors maps to API", "mutation_success_false.json", domain.ErrTrackerAPI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeClient()
			f.queueBody("CommentCreate", loadFixture(t, tt.fixture))

			err := runMutation(context.Background(), f, newTextLogger(&bytes.Buffer{}),
				queryCommentCreate, map[string]any{"issueId": "SOR-5", "body": "x"},
				decodeCommentCreateSuccess)

			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

// --- local helpers ---

// assertFilterVar returns the recorded filter operation variable as a map,
// failing the test when it is absent or not a map.
func assertFilterVar(t *testing.T, call recordedCall) map[string]any {
	t.Helper()
	filter, ok := call.variables["filter"].(map[string]any)
	if !ok {
		t.Fatalf("filter variable type = %T, want map[string]any", call.variables["filter"])
	}
	return filter
}

// assertTeamFilter asserts the adapter-owned team constraint nested as
// team.key.eq inside the merged filter. The offline harness always configures
// team SOR, so the expected key is fixed.
func assertTeamFilter(t *testing.T, filter map[string]any) {
	t.Helper()
	const wantKey = "SOR"
	team, ok := filter["team"].(map[string]any)
	if !ok {
		t.Fatalf("filter[team] type = %T, want map[string]any", filter["team"])
	}
	key, ok := team["key"].(map[string]any)
	if !ok {
		t.Fatalf("filter[team][key] type = %T, want map[string]any", team["key"])
	}
	if key["eq"] != wantKey {
		t.Errorf("filter[team][key][eq] = %v, want %q", key["eq"], wantKey)
	}
}

// assertStateFilter asserts the adapter-owned state constraint nested as
// state.name.in inside the merged filter. The in value is the canonical-cased
// Go []string the adapter builds, not a []any.
func assertStateFilter(t *testing.T, filter map[string]any, wantStates []string) {
	t.Helper()
	state, ok := filter["state"].(map[string]any)
	if !ok {
		t.Fatalf("filter[state] type = %T, want map[string]any", filter["state"])
	}
	name, ok := state["name"].(map[string]any)
	if !ok {
		t.Fatalf("filter[state][name] type = %T, want map[string]any", state["name"])
	}
	in, ok := name["in"].([]string)
	if !ok {
		t.Fatalf("filter[state][name][in] type = %T, want []string", name["in"])
	}
	if !reflect.DeepEqual(in, wantStates) {
		t.Errorf("filter[state][name][in] = %v, want %v", in, wantStates)
	}
}

// assertFilterKeys asserts the merged filter has exactly the expected top-level
// keys and no others.
func assertFilterKeys(t *testing.T, filter map[string]any, want []string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(filter))
	wantSorted := slices.Clone(want)
	slices.Sort(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Errorf("filter top-level keys = %v, want %v", got, wantSorted)
	}
}

// assertVarKeys asserts a recorded call's variable map has exactly the expected
// keys and no others, so a stray filter variable is caught.
func assertVarKeys(t *testing.T, call recordedCall, want []string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(call.variables))
	wantSorted := slices.Clone(want)
	slices.Sort(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Errorf("variable keys = %v, want %v", got, wantSorted)
	}
}

// assertNoTopLevelVar asserts the named variable is absent from the recorded
// call, used to confirm the folded teamKey/states no longer travel top-level.
func assertNoTopLevelVar(t *testing.T, call recordedCall, name string) {
	t.Helper()
	if _, present := call.variables[name]; present {
		t.Errorf("variable %q present = %v, want absent (folded into filter)", name, call.variables[name])
	}
}

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
