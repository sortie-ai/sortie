package sqlite

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

func sameEscalation(a, b storage.ReactionEscalation) bool {
	if (a.EscalatedAt == nil) != (b.EscalatedAt == nil) {
		return false
	}
	if a.EscalatedAt != nil && !a.EscalatedAt.Equal(*b.EscalatedAt) {
		return false
	}
	a.EscalatedAt, b.EscalatedAt = nil, nil
	return a == b
}

func standingKeys(t *testing.T, s *Store) []string {
	t.Helper()
	recs, err := s.ListStandingReactionEscalations(context.Background())
	if err != nil {
		t.Fatalf("ListStandingReactionEscalations: %v", err)
	}
	keys := []string{}
	for _, rec := range recs {
		keys = append(keys, rec.IssueID+"/"+rec.Kind)
	}
	return keys
}

func TestReactionEscalationLifecycle(t *testing.T) {
	t.Parallel()

	escalatedAt := time.Date(2026, 3, 1, 14, 0, 5, 123456789, time.FixedZone("UTC+2", 2*60*60))
	saved := storage.ReactionEscalation{
		IssueID: "ISS-1", Kind: "ci", Subject: "head-abc", EscalatedAt: &escalatedAt,
		Trigger: "budget", Attempts: 3, PRNumber: 42, PROwner: "acme", PRRepo: "widgets", PRBranch: "feat/x",
	}
	ended := saved
	ended.EscalatedAt = nil

	tests := []struct {
		name         string
		act          func(ctx context.Context, s *Store) error
		wantRec      *storage.ReactionEscalation
		wantStanding []string
	}{
		{
			name:         "saved record reads back at nanosecond precision",
			act:          func(context.Context, *Store) error { return nil },
			wantRec:      &saved,
			wantStanding: []string{"ISS-1/ci", "ISS-1/review", "ISS-2/ci"},
		},
		{
			name:         "ended record keeps its subject and leaves the standing list",
			act:          func(ctx context.Context, s *Store) error { return s.EndReactionEscalation(ctx, "ISS-1", "ci") },
			wantRec:      &ended,
			wantStanding: []string{"ISS-1/review", "ISS-2/ci"},
		},
		{
			name:         "delete by issue removes every kind",
			act:          func(ctx context.Context, s *Store) error { return s.DeleteReactionEscalations(ctx, "ISS-1") },
			wantStanding: []string{"ISS-2/ci"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := mustOpenStore(t)
			ctx := context.Background()
			other := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
			for _, rec := range []storage.ReactionEscalation{
				{IssueID: "ISS-2", Kind: "ci", Subject: "head-z", EscalatedAt: &other},
				{IssueID: "ISS-1", Kind: "review", Subject: "set-1", EscalatedAt: &other},
				saved,
			} {
				if err := s.SaveReactionEscalation(ctx, rec); err != nil {
					t.Fatalf("SaveReactionEscalation(%s/%s) = %v, want nil", rec.IssueID, rec.Kind, err)
				}
			}

			if err := tt.act(ctx, s); err != nil {
				t.Fatalf("act = %v, want nil", err)
			}

			got, found, err := s.GetReactionEscalation(ctx, "ISS-1", "ci")
			if err != nil {
				t.Fatalf("GetReactionEscalation(ISS-1, ci): %v", err)
			}
			if found != (tt.wantRec != nil) {
				t.Fatalf("GetReactionEscalation(ISS-1, ci) found = %v, want %v", found, tt.wantRec != nil)
			}
			if tt.wantRec != nil && !sameEscalation(got, *tt.wantRec) {
				t.Errorf("GetReactionEscalation(ISS-1, ci) = %+v, want %+v", got, *tt.wantRec)
			}
			if _, found, err := s.GetReactionEscalation(ctx, "ISS-1", "review"); err != nil {
				t.Fatalf("GetReactionEscalation(ISS-1, review): %v", err)
			} else if wantReview := slices.Contains(tt.wantStanding, "ISS-1/review"); found != wantReview {
				t.Errorf("GetReactionEscalation(ISS-1, review) found = %v, want %v", found, wantReview)
			}
			if gotStanding := standingKeys(t, s); !slices.Equal(gotStanding, tt.wantStanding) {
				t.Errorf("ListStandingReactionEscalations() = %v, want %v", gotStanding, tt.wantStanding)
			}
		})
	}
}
