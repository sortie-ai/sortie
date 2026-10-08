package linear

import (
	"context"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func removedLabelIDs(f *fakeGraphQLClient) [][]string {
	var removed [][]string
	for _, call := range f.callsFor("IssueRemoveLabels") {
		ids, _ := call.variables["labelIds"].([]string)
		removed = append(removed, ids)
	}
	return removed
}

func TestRemoveLabel_SendsIssueLabelIDsAndTrustsReportedSet(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	seedPreflight(f, t)
	f.queueBody("query IssueLabels", loadFixture(t, "issue_labels_with_sibling.json"))
	f.queueBody("IssueRemoveLabels", loadFixture(t, "issue_remove_label_success.json"))
	adapter := newTestAdapter(t, f)

	if err := adapter.RemoveLabel(context.Background(), "SOR-5", "stage-plan"); err != nil {
		t.Fatalf("RemoveLabel(SOR-5, stage-plan): %v", err)
	}

	removes := f.callsFor("IssueRemoveLabels")
	if len(removes) != 1 {
		t.Fatalf("IssueRemoveLabels calls = %d, want 1", len(removes))
	}
	if got, want := removedLabelIDs(f)[0], []string{"label-id-plan"}; !slices.Equal(got, want) {
		t.Errorf("IssueRemoveLabels labelIds = %v, want %v", got, want)
	}
	if got := removes[0].variables["id"]; got != "SOR-5" {
		t.Errorf("IssueRemoveLabels id = %v, want %q verbatim", got, "SOR-5")
	}
	if got := len(f.callsFor("query IssueLabels")); got != 1 {
		t.Errorf("IssueLabels reads = %d, want 1 (the payload reports the label set)", got)
	}
}

func TestAddLabel_GroupSwap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		resolve     string
		firstAdd    string
		wantErr     bool
		wantAdds    int
		wantRemoved [][]string
	}{
		{"single-select sibling replaced by the plain add", "label_resolve_single_select_group.json", "issue_add_label_success.json", false, 1, nil},
		{"single-select add rejected", "label_resolve_single_select_group.json", "mutation_invalid_input.json", false, 2, [][]string{{"label-id-plan"}}},
		{"single-select add accepted and dropped", "label_resolve_single_select_group.json", "issue_remove_label_success.json", false, 2, [][]string{{"label-id-plan"}}},
		{"multi-select add dropped never removes", "label_resolve_multi_select_group.json", "issue_remove_label_success.json", true, 1, nil},
		{"ungrouped add dropped never removes", "label_resolve_hit.json", "issue_remove_label_success.json", true, 1, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeClient()
			seedPreflight(f, t)
			f.queueBody("ResolveLabel", loadFixture(t, tt.resolve))
			f.queueBody("IssueAddLabel", loadFixture(t, tt.firstAdd))
			f.queueBody("IssueAddLabel", loadFixture(t, "issue_add_label_success.json"))
			f.queueBody("query IssueLabels", loadFixture(t, "issue_labels_with_sibling.json"))
			f.queueBody("IssueRemoveLabels", loadFixture(t, "issue_remove_label_success.json"))
			adapter := newTestAdapter(t, f)

			err := adapter.AddLabel(context.Background(), "SOR-5", "needs-human")

			if tt.wantErr {
				assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
			} else if err != nil {
				t.Fatalf("AddLabel(SOR-5, needs-human) = %v, want nil", err)
			}
			if got := len(f.callsFor("IssueAddLabel")); got != tt.wantAdds {
				t.Errorf("IssueAddLabel calls = %d, want %d", got, tt.wantAdds)
			}
			if got := removedLabelIDs(f); !slices.EqualFunc(got, tt.wantRemoved, slices.Equal) {
				t.Errorf("IssueRemoveLabels labelIds = %v, want %v", got, tt.wantRemoved)
			}
		})
	}
}

type labelReadPath struct {
	name          string
	query         string
	fixture       string
	cursorFixture string
	fetch         func(*LinearAdapter) ([]domain.Issue, error)
}

func labelReadPaths() []labelReadPath {
	return []labelReadPath{
		{
			name:          "FetchIssueByID",
			query:         "query IssueByID",
			fixture:       "issue_by_id_many_labels.json",
			cursorFixture: "issue_by_id_labels_missing_cursor.json",
			fetch: func(a *LinearAdapter) ([]domain.Issue, error) {
				issue, err := a.FetchIssueByID(context.Background(), "SOR-5")
				return []domain.Issue{issue}, err
			},
		},
		{
			name:          "FetchCandidateIssues",
			query:         "query CandidateIssues",
			fixture:       "candidates_many_labels.json",
			cursorFixture: "candidates_labels_missing_cursor.json",
			fetch: func(a *LinearAdapter) ([]domain.Issue, error) {
				return a.FetchCandidateIssues(context.Background())
			},
		},
		{
			name:          "FetchIssuesByStates",
			query:         "query IssuesByStates",
			fixture:       "candidates_many_labels.json",
			cursorFixture: "candidates_labels_missing_cursor.json",
			fetch: func(a *LinearAdapter) ([]domain.Issue, error) {
				return a.FetchIssuesByStates(context.Background(), []string{"Todo"})
			},
		},
	}
}

func TestReads_CompleteCappedLabelLists(t *testing.T) {
	t.Parallel()

	for _, tt := range labelReadPaths() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeClient()
			seedPreflight(f, t)
			f.queueBody(tt.query, loadFixture(t, tt.fixture))
			f.queueBody("query IssueLabels", loadFixture(t, "issue_labels_rest.json"))
			adapter := newTestAdapter(t, f)

			issues, err := tt.fetch(adapter)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}

			idx := slices.IndexFunc(issues, func(issue domain.Issue) bool { return issue.Identifier == "SOR-5" })
			if idx < 0 {
				t.Fatalf("%s returned no SOR-5", tt.name)
			}
			want := []string{"label-01", "label-02", "label-03", "label-04", "label-05"}
			if got := issues[idx].Labels; !slices.Equal(got, want) {
				t.Errorf("%s labels = %v, want %v", tt.name, got, want)
			}
			completing := f.callsFor("query IssueLabels")
			if len(completing) != 1 {
				t.Fatalf("IssueLabels calls = %d, want 1 (only the capped issue is completed)", len(completing))
			}
			if got := completing[0].variables["after"]; got != "labels-cursor-03" {
				t.Errorf("IssueLabels after = %v, want the nested endCursor %q", got, "labels-cursor-03")
			}
		})
	}
}

func TestReads_CappedLabelListWithoutEndCursorIsMissingCursor(t *testing.T) {
	t.Parallel()

	for _, tt := range labelReadPaths() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeClient()
			seedPreflight(f, t)
			f.queueBody(tt.query, loadFixture(t, tt.cursorFixture))
			adapter := newTestAdapter(t, f)

			_, err := tt.fetch(adapter)

			assertTrackerErrorKind(t, err, domain.ErrTrackerMissingCursor)
			if calls := f.callsFor("query IssueLabels"); len(calls) != 0 {
				t.Errorf("IssueLabels calls = %d, want 0 (fail fast before completing)", len(calls))
			}
		})
	}
}
