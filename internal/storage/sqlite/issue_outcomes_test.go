package sqlite

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/storage"
)

func TestRecordIssueOutcome_Rule(t *testing.T) {
	t.Parallel()

	const first, second = "2026-02-24T21:00:00Z", "2026-02-24T21:05:00Z"
	outcome := func(kind, observedAt string) storage.IssueOutcome {
		o := storage.IssueOutcome{IssueID: "id-1", Kind: kind, ObservedAt: observedAt}
		switch kind {
		case storage.IssueOutcomeMerged:
			o.MergeReactionKind, o.PRNumber, o.PRRepository = "merge", 88, "acme/api"
		case storage.IssueOutcomeHandedOff:
			o.State = "In Review"
		case storage.IssueOutcomeTerminal:
			o.State = "Done"
		}
		return o
	}

	tests := []struct {
		name, stored, incoming string
		wantIncoming           bool
	}{
		{"none then handed off", "", storage.IssueOutcomeHandedOff, true},
		{"none then merged", "", storage.IssueOutcomeMerged, true},
		{"none then terminal", "", storage.IssueOutcomeTerminal, true},
		{"handed off then handed off", storage.IssueOutcomeHandedOff, storage.IssueOutcomeHandedOff, true},
		{"handed off then merged", storage.IssueOutcomeHandedOff, storage.IssueOutcomeMerged, true},
		{"handed off then terminal", storage.IssueOutcomeHandedOff, storage.IssueOutcomeTerminal, true},
		{"merged then handed off", storage.IssueOutcomeMerged, storage.IssueOutcomeHandedOff, true},
		{"merged then merged", storage.IssueOutcomeMerged, storage.IssueOutcomeMerged, false},
		{"merged then terminal", storage.IssueOutcomeMerged, storage.IssueOutcomeTerminal, false},
		{"terminal then handed off", storage.IssueOutcomeTerminal, storage.IssueOutcomeHandedOff, true},
		{"terminal then merged", storage.IssueOutcomeTerminal, storage.IssueOutcomeMerged, true},
		{"terminal then terminal", storage.IssueOutcomeTerminal, storage.IssueOutcomeTerminal, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			ctx := t.Context()
			if tt.stored != "" {
				if err := s.RecordIssueOutcome(ctx, outcome(tt.stored, first)); err != nil {
					t.Fatalf("RecordIssueOutcome(stored %s): %v", tt.stored, err)
				}
			}

			if err := s.RecordIssueOutcome(ctx, outcome(tt.incoming, second)); err != nil {
				t.Fatalf("RecordIssueOutcome(incoming %s): %v", tt.incoming, err)
			}
			got, err := s.IssueOutcomes(ctx, []string{"id-1"})
			if err != nil {
				t.Fatalf("IssueOutcomes: %v", err)
			}

			want := outcome(tt.incoming, second)
			if !tt.wantIncoming {
				want = outcome(tt.stored, first)
			}
			if got["id-1"] != want {
				t.Errorf("IssueOutcomes after %s = %+v, want %+v", tt.name, got["id-1"], want)
			}
		})
	}
}
