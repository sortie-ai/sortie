package persistence

import (
	"context"
	"testing"
)

func mustOpenStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	migrateOrFatal(t, s)
	t.Cleanup(func() { closeStore(t, s) })
	return s
}

func TestUpsertReactionFingerprint_NewRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertReactionFingerprint(ctx, "ISS-1", "ci", "sha-abc"); err != nil {
		t.Fatalf("UpsertReactionFingerprint: %v", err)
	}

	fp, dispatched, err := s.GetReactionFingerprint(ctx, "ISS-1", "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	if fp != "sha-abc" {
		t.Errorf("GetReactionFingerprint() fingerprint = %q, want %q", fp, "sha-abc")
	}
	if dispatched {
		t.Error("GetReactionFingerprint() dispatched = true, want false for new row")
	}
}

func TestUpsertReactionFingerprint_SameFingerprintPreservesDispatched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertReactionFingerprint(ctx, "ISS-2", "ci", "sha-abc"); err != nil {
		t.Fatalf("UpsertReactionFingerprint (initial): %v", err)
	}
	if err := s.MarkReactionDispatched(ctx, "ISS-2", "ci"); err != nil {
		t.Fatalf("MarkReactionDispatched: %v", err)
	}

	// Upsert same fingerprint again — dispatched must remain 1.
	if err := s.UpsertReactionFingerprint(ctx, "ISS-2", "ci", "sha-abc"); err != nil {
		t.Fatalf("UpsertReactionFingerprint (repeat): %v", err)
	}

	_, dispatched, err := s.GetReactionFingerprint(ctx, "ISS-2", "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	if !dispatched {
		t.Error("GetReactionFingerprint() dispatched = false, want true (same fingerprint must not reset)")
	}
}

func TestUpsertReactionFingerprint_ChangedFingerprintResetsDispatched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertReactionFingerprint(ctx, "ISS-3", "ci", "sha-old"); err != nil {
		t.Fatalf("UpsertReactionFingerprint (old): %v", err)
	}
	if err := s.MarkReactionDispatched(ctx, "ISS-3", "ci"); err != nil {
		t.Fatalf("MarkReactionDispatched: %v", err)
	}

	// Upsert a different fingerprint — dispatched must be reset to 0.
	if err := s.UpsertReactionFingerprint(ctx, "ISS-3", "ci", "sha-new"); err != nil {
		t.Fatalf("UpsertReactionFingerprint (new): %v", err)
	}

	fp, dispatched, err := s.GetReactionFingerprint(ctx, "ISS-3", "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	if fp != "sha-new" {
		t.Errorf("GetReactionFingerprint() fingerprint = %q, want %q", fp, "sha-new")
	}
	if dispatched {
		t.Error("GetReactionFingerprint() dispatched = true, want false after fingerprint change")
	}
}

func TestGetReactionFingerprint_Absent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	fp, dispatched, err := s.GetReactionFingerprint(ctx, "ISS-NOTHERE", "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	if fp != "" {
		t.Errorf("GetReactionFingerprint() fingerprint = %q, want %q", fp, "")
	}
	if dispatched {
		t.Error("GetReactionFingerprint() dispatched = true, want false for absent row")
	}
}

func TestGetReactionFingerprint_AfterMarkDispatched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertReactionFingerprint(ctx, "ISS-4", "ci", "sha-dispatched"); err != nil {
		t.Fatalf("UpsertReactionFingerprint: %v", err)
	}
	if err := s.MarkReactionDispatched(ctx, "ISS-4", "ci"); err != nil {
		t.Fatalf("MarkReactionDispatched: %v", err)
	}

	fp, dispatched, err := s.GetReactionFingerprint(ctx, "ISS-4", "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	if fp != "sha-dispatched" {
		t.Errorf("GetReactionFingerprint() fingerprint = %q, want %q", fp, "sha-dispatched")
	}
	if !dispatched {
		t.Error("GetReactionFingerprint() dispatched = false, want true after MarkReactionDispatched")
	}
}

func TestMarkReactionDispatched_AbsentRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	// MarkReactionDispatched on a non-existent row must be a no-op (not an error).
	if err := s.MarkReactionDispatched(ctx, "ISS-NOTHERE", "ci"); err != nil {
		t.Fatalf("MarkReactionDispatched on absent row returned error: %v", err)
	}
}

func TestDeleteReactionFingerprint_RemovesRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	if err := s.UpsertReactionFingerprint(ctx, "ISS-5", "ci", "sha-delete"); err != nil {
		t.Fatalf("UpsertReactionFingerprint: %v", err)
	}

	if err := s.DeleteReactionFingerprint(ctx, "ISS-5", "ci"); err != nil {
		t.Fatalf("DeleteReactionFingerprint: %v", err)
	}

	fp, dispatched, err := s.GetReactionFingerprint(ctx, "ISS-5", "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint after delete: %v", err)
	}
	if fp != "" || dispatched {
		t.Errorf("GetReactionFingerprint() = (%q, %v), want (%q, false) after delete", fp, dispatched, "")
	}
}

func TestDeleteReactionFingerprintsByIssue_RemovesAllKinds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	const issueID = "ISS-6"

	kinds := []string{"ci", "review_comments", "custom_kind"}
	for _, kind := range kinds {
		if err := s.UpsertReactionFingerprint(ctx, issueID, kind, "fp-"+kind); err != nil {
			t.Fatalf("UpsertReactionFingerprint(%q): %v", kind, err)
		}
	}

	// Also insert a row for a different issue to confirm it is not deleted.
	if err := s.UpsertReactionFingerprint(ctx, "ISS-OTHER", "ci", "sha-other"); err != nil {
		t.Fatalf("UpsertReactionFingerprint(ISS-OTHER): %v", err)
	}

	if err := s.DeleteReactionFingerprintsByIssue(ctx, issueID); err != nil {
		t.Fatalf("DeleteReactionFingerprintsByIssue: %v", err)
	}

	for _, kind := range kinds {
		fp, dispatched, err := s.GetReactionFingerprint(ctx, issueID, kind)
		if err != nil {
			t.Fatalf("GetReactionFingerprint(%q) after bulk delete: %v", kind, err)
		}
		if fp != "" || dispatched {
			t.Errorf("GetReactionFingerprint(%q) = (%q, %v), want (%q, false) after bulk delete", kind, fp, dispatched, "")
		}
	}

	// Unrelated issue row must be untouched.
	fp, _, err := s.GetReactionFingerprint(ctx, "ISS-OTHER", "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint(ISS-OTHER) after bulk delete: %v", err)
	}
	if fp != "sha-other" {
		t.Errorf("GetReactionFingerprint(ISS-OTHER) = %q, want %q", fp, "sha-other")
	}
}

func TestUpsertReactionFingerprint_MultipleKindsIndependent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := mustOpenStore(t)

	const issueID = "ISS-7"

	if err := s.UpsertReactionFingerprint(ctx, issueID, "ci", "sha-ci"); err != nil {
		t.Fatalf("UpsertReactionFingerprint(ci): %v", err)
	}
	if err := s.UpsertReactionFingerprint(ctx, issueID, "review_comments", "sha-review"); err != nil {
		t.Fatalf("UpsertReactionFingerprint(review_comments): %v", err)
	}
	if err := s.MarkReactionDispatched(ctx, issueID, "ci"); err != nil {
		t.Fatalf("MarkReactionDispatched(ci): %v", err)
	}

	_, ciDispatched, err := s.GetReactionFingerprint(ctx, issueID, "ci")
	if err != nil {
		t.Fatalf("GetReactionFingerprint(ci): %v", err)
	}
	_, reviewDispatched, err := s.GetReactionFingerprint(ctx, issueID, "review_comments")
	if err != nil {
		t.Fatalf("GetReactionFingerprint(review_comments): %v", err)
	}

	if !ciDispatched {
		t.Error("GetReactionFingerprint(ci) dispatched = false, want true")
	}
	if reviewDispatched {
		t.Error("GetReactionFingerprint(review_comments) dispatched = true, want false (independent of ci)")
	}
}
