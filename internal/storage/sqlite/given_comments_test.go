package sqlite

import (
	"cmp"
	"context"
	"crypto/sha256"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

func mustOpenFileStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err == nil {
		err = s.Migrate(context.Background())
	}
	if err != nil {
		t.Fatalf("Open and Migrate(%q): %v", path, err)
	}
	return s
}

func givenComment(conversation, commentID string, hash byte, channel string, at time.Time) storage.GivenComment {
	return storage.GivenComment{ConversationID: conversation, Source: "tracker", CommentID: commentID,
		BodySHA256: sha256.Sum256([]byte{hash}), Channel: channel, GivenAt: at}
}

func assertGivenRows(t *testing.T, step string, got, want []storage.GivenComment) {
	t.Helper()
	byKey := func(a, b storage.GivenComment) int {
		return cmp.Or(cmp.Compare(a.ConversationID, b.ConversationID), cmp.Compare(a.CommentID, b.CommentID))
	}
	got, want = slices.SortedFunc(slices.Values(got), byKey), slices.SortedFunc(slices.Values(want), byKey)
	if len(got) != len(want) {
		t.Fatalf("%s returned %d rows, want %d: got %+v, want %+v", step, len(got), len(want), got, want)
	}
	for i, w := range want {
		g := got[i]
		if !g.GivenAt.Equal(w.GivenAt) || g.GivenAt.Location() != time.UTC {
			t.Errorf("%s row %s/%s GivenAt = %v, want instant %v in UTC", step, w.ConversationID, w.CommentID, g.GivenAt, w.GivenAt)
		}
		g.GivenAt, w.GivenAt = time.Time{}, time.Time{}
		if g != w {
			t.Errorf("%s row %s/%s = {hash %x channel %q}, want {hash %x channel %q}", step, w.ConversationID, w.CommentID, g.BodySHA256, g.Channel, w.BodySHA256, w.Channel)
		}
	}
}

func TestGivenComments(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	offsetNanos := time.Date(2026, 11, 1, 12, 30, 0, 123456789, time.FixedZone("UTC+2", 2*3600))
	a1 := givenComment("A", "c1", 1, "prompt", at)
	a2 := givenComment("A", "c2", 2, "turn", offsetNanos)
	b1 := givenComment("B", "c1", 3, "prompt", at.Add(time.Minute))
	c1 := givenComment("C", "c3", 4, "turn", at.Add(2*time.Minute))
	jA1 := givenComment("A", "c1", 5, "prompt", at.Add(3*time.Minute))

	path := filepath.Join(t.TempDir(), "sortie.db")
	first := mustOpenFileStore(t, path)
	for issue, rows := range map[string][]storage.GivenComment{"I": {a1, a2, b1, c1}, "J": {jA1}} {
		if err := first.RecordGivenComments(ctx, issue, rows); err != nil {
			t.Fatalf("RecordGivenComments(%q) = %v, want nil", issue, err)
		}
	}
	closeStore(t, first)
	s := mustOpenFileStore(t, path)
	defer closeStore(t, s)

	read := func(step string, earlier bool, issue, convo string) []storage.GivenComment {
		t.Helper()
		list := s.ListGivenComments
		if earlier {
			list = s.ListEarlierGivenComments
		}
		got, err := list(ctx, issue, convo)
		if err != nil {
			t.Fatalf("%s(%q, %q) = %v, want nil", step, issue, convo, err)
		}
		return got
	}

	reads := []struct {
		name         string
		earlier      bool
		issue, convo string
		want         []storage.GivenComment
	}{
		{"conversation A reads its own rows", false, "I", "A", []storage.GivenComment{a1, a2}},
		{"conversation B reads its own rows", false, "I", "B", []storage.GivenComment{b1}},
		{"conversation C reads the other conversations rows", true, "I", "C", []storage.GivenComment{a1, a2, b1}},
	}
	for _, tt := range reads {
		t.Run(tt.name, func(t *testing.T) {
			assertGivenRows(t, tt.name, read(tt.name, tt.earlier, tt.issue, tt.convo), tt.want)
		})
	}

	edited := givenComment("A", "c1", 6, "turn", at.Add(time.Hour))
	if err := s.RecordGivenComments(ctx, "I", []storage.GivenComment{edited}); err != nil {
		t.Fatalf("RecordGivenComments(I) of an edited comment = %v, want nil", err)
	}
	assertGivenRows(t, "edited comment replaces its earlier version", read("edited comment replaces its earlier version", false, "I", "A"), []storage.GivenComment{edited, a2})
}
