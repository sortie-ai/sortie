package persistence

import (
	"context"
	"slices"
	"testing"
)

func mustListHandedOff(t *testing.T, s *Store, issueID, kind string) []string {
	t.Helper()
	got, err := s.ListReactionHandedOffComments(context.Background(), issueID, kind)
	if err != nil {
		t.Fatalf("ListReactionHandedOffComments(%q, %q): %v", issueID, kind, err)
	}
	slices.Sort(got)
	return got
}

func TestAddReactionHandedOffComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		batches [][]string
		want    []string
	}{
		{"one batch", [][]string{{"c2", "c1"}}, []string{"c1", "c2"}},
		{"empty slice writes nothing", [][]string{{}}, []string{}},
		{"nil slice writes nothing", [][]string{nil}, []string{}},
		{"repeated ID in one batch", [][]string{{"c1", "c1", "c2"}}, []string{"c1", "c2"}},
		{"later batch adds only absent IDs", [][]string{{"c1", "c2"}, {"c2", "c3"}}, []string{"c1", "c2", "c3"}},
		{"identical batch twice", [][]string{{"c1"}, {"c1"}}, []string{"c1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := mustOpenStore(t)
			ctx := context.Background()

			for _, batch := range tt.batches {
				if err := s.AddReactionHandedOffComments(ctx, "ISS-1", "review", batch); err != nil {
					t.Fatalf("AddReactionHandedOffComments(%v) = %v, want nil", batch, err)
				}
			}

			got := mustListHandedOff(t, s, "ISS-1", "review")
			if !slices.Equal(got, tt.want) {
				t.Errorf("ListReactionHandedOffComments(ISS-1, review) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListReactionHandedOffComments_EmptySetIsNotNil(t *testing.T) {
	t.Parallel()

	s := mustOpenStore(t)

	got, err := s.ListReactionHandedOffComments(context.Background(), "ISS-NONE", "review")

	if err != nil {
		t.Fatalf("ListReactionHandedOffComments(ISS-NONE, review) = %v, want nil error", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("ListReactionHandedOffComments(ISS-NONE, review) = %#v, want empty non-nil slice", got)
	}
}

func TestReactionHandedOffComments_SetsAreKeyedByIssueAndKind(t *testing.T) {
	t.Parallel()

	s := mustOpenStore(t)
	ctx := context.Background()
	for _, add := range []struct{ issueID, kind, commentID string }{
		{"ISS-1", "review", "a"},
		{"ISS-1", "bot-review", "b"},
		{"ISS-2", "review", "c"},
	} {
		if err := s.AddReactionHandedOffComments(ctx, add.issueID, add.kind, []string{add.commentID}); err != nil {
			t.Fatalf("AddReactionHandedOffComments(%q, %q): %v", add.issueID, add.kind, err)
		}
	}

	for _, want := range []struct {
		issueID, kind string
		ids           []string
	}{
		{"ISS-1", "review", []string{"a"}},
		{"ISS-1", "bot-review", []string{"b"}},
		{"ISS-2", "review", []string{"c"}},
		{"ISS-2", "bot-review", []string{}},
	} {
		got := mustListHandedOff(t, s, want.issueID, want.kind)
		if !slices.Equal(got, want.ids) {
			t.Errorf("ListReactionHandedOffComments(%q, %q) = %v, want %v", want.issueID, want.kind, got, want.ids)
		}
	}
}

func TestReactionHandedOffComments_SurviveFingerprintDeletion(t *testing.T) {
	t.Parallel()

	s := mustOpenStore(t)
	ctx := context.Background()
	if err := s.AddReactionHandedOffComments(ctx, "ISS-1", "review", []string{"a", "b"}); err != nil {
		t.Fatalf("AddReactionHandedOffComments: %v", err)
	}
	if err := s.UpsertReactionFingerprint(ctx, "ISS-1", "review", "fp"); err != nil {
		t.Fatalf("UpsertReactionFingerprint: %v", err)
	}

	if err := s.DeleteReactionFingerprint(ctx, "ISS-1", "review"); err != nil {
		t.Fatalf("DeleteReactionFingerprint: %v", err)
	}

	got := mustListHandedOff(t, s, "ISS-1", "review")
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("ListReactionHandedOffComments(ISS-1, review) after DeleteReactionFingerprint = %v, want %v", got, want)
	}
}

func TestReactionHandedOffComments_CanceledContext(t *testing.T) {
	t.Parallel()

	s := mustOpenStore(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	addErr := s.AddReactionHandedOffComments(canceled, "ISS-1", "review", []string{"a"})
	_, listErr := s.ListReactionHandedOffComments(canceled, "ISS-1", "review")

	if addErr == nil {
		t.Error("AddReactionHandedOffComments(canceled ctx) = nil, want error")
	}
	if listErr == nil {
		t.Error("ListReactionHandedOffComments(canceled ctx) = nil error, want error")
	}
	if got := mustListHandedOff(t, s, "ISS-1", "review"); len(got) != 0 {
		t.Errorf("ListReactionHandedOffComments(ISS-1, review) after a canceled add = %v, want empty", got)
	}
}
