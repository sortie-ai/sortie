package sqlite

import (
	"context"
	"testing"

	"github.com/sortie-ai/sortie/internal/storage"
)

func TestLatestReactionTriageDisposition(t *testing.T) {
	t.Parallel()

	out := func(issue, kind, fingerprint, disposition string) storage.ReactionTriageOutcome {
		return storage.ReactionTriageOutcome{IssueID: issue, Kind: kind, Fingerprint: fingerprint, Disposition: disposition}
	}
	recorded := []storage.ReactionTriageOutcome{
		out("ISS-1", "bot-review", "fp-a", "actionable"),
		out("ISS-1", "bot-review", "fp-a", "handled"),
		out("ISS-1", "bot-review", "fp-b", "actionable"),
		out("ISS-2", "bot-review", "fp-a", "actionable"),
	}

	tests := []struct {
		name                     string
		records                  []storage.ReactionTriageOutcome
		issue, kind, fingerprint string
		want                     string
		wantFound                bool
	}{
		{name: "newest of several rows", records: recorded, issue: "ISS-1", kind: "bot-review", fingerprint: "fp-a", want: "handled", wantFound: true},
		{name: "other fingerprint keeps its own row", records: recorded, issue: "ISS-1", kind: "bot-review", fingerprint: "fp-b", want: "actionable", wantFound: true},
		{name: "different issue", records: recorded, issue: "ISS-3", kind: "bot-review", fingerprint: "fp-a"},
		{name: "different kind", records: recorded, issue: "ISS-1", kind: "review", fingerprint: "fp-a"},
		{name: "different fingerprint", records: recorded, issue: "ISS-1", kind: "bot-review", fingerprint: "fp-c"},
		{name: "empty table", issue: "ISS-1", kind: "bot-review", fingerprint: "fp-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			s := mustOpenStore(t)
			for _, r := range tt.records {
				if err := s.RecordReactionTriageOutcome(ctx, r); err != nil {
					t.Fatalf("RecordReactionTriageOutcome(%+v): %v", r, err)
				}
			}

			got, found, err := s.LatestReactionTriageDisposition(ctx, tt.issue, tt.kind, tt.fingerprint)
			if err != nil {
				t.Fatalf("LatestReactionTriageDisposition(%q, %q, %q): %v", tt.issue, tt.kind, tt.fingerprint, err)
			}

			if got != tt.want || found != tt.wantFound {
				t.Errorf("LatestReactionTriageDisposition(%q, %q, %q) = %q, %t, want %q, %t",
					tt.issue, tt.kind, tt.fingerprint, got, found, tt.want, tt.wantFound)
			}
		})
	}
}
