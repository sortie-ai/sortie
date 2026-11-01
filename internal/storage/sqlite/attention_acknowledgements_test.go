package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

func listAcknowledgementTimes(t *testing.T, s *Store) map[string]time.Time {
	t.Helper()
	rows, err := s.ListAttentionAcknowledgements(context.Background())
	if err != nil {
		t.Fatalf("ListAttentionAcknowledgements: %v", err)
	}
	times := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		times[row.ItemID] = row.AcknowledgedAt
	}
	return times
}

func TestAttentionAcknowledgementRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)
	saved := map[string]time.Time{
		"464f9ff8ea54c4c6cfb43af394234d57": time.Date(2026, 2, 24, 20, 13, 5, 0, time.UTC),
		"0b8e1c52a7d94f3e8a61c9d27f4b5e30": time.Date(2026, 2, 24, 21, 0, 0, 0, time.UTC),
		"9c3d7a10e5b84f26b1a0d8e47c2f6a95": time.Date(2026, 2, 25, 8, 30, 0, 0, time.UTC),
	}
	for id, at := range saved {
		if err := s.SaveAttentionAcknowledgement(ctx, storage.AttentionAcknowledgement{ItemID: id, AcknowledgedAt: at}); err != nil {
			t.Fatalf("SaveAttentionAcknowledgement(%q): %v", id, err)
		}
	}

	got := listAcknowledgementTimes(t, s)

	if len(got) != len(saved) {
		t.Fatalf("ListAttentionAcknowledgements() returned %d rows, want %d", len(got), len(saved))
	}
	for id, want := range saved {
		if at, ok := got[id]; !ok || !at.Equal(want) {
			t.Errorf("ListAttentionAcknowledgements()[%q] = %v, want %v", id, at, want)
		}
	}

	if err := s.DeleteAttentionAcknowledgements(ctx, []string{
		"464f9ff8ea54c4c6cfb43af394234d57",
		"0b8e1c52a7d94f3e8a61c9d27f4b5e30",
	}); err != nil {
		t.Fatalf("DeleteAttentionAcknowledgements: %v", err)
	}

	got = listAcknowledgementTimes(t, s)

	keep := "9c3d7a10e5b84f26b1a0d8e47c2f6a95"
	if len(got) != 1 {
		t.Fatalf("ListAttentionAcknowledgements() after delete = %v, want only %q", got, keep)
	}
	if at, ok := got[keep]; !ok || !at.Equal(saved[keep]) {
		t.Errorf("ListAttentionAcknowledgements()[%q] after delete = %v, want %v", keep, at, saved[keep])
	}
}
