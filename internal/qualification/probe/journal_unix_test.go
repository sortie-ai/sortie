//go:build unix

package probe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

func readJournal(t *testing.T, path string) []journalEntry {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // path is this test's own temporary directory
	if err != nil {
		t.Fatalf("read the journal %q: %v", path, err)
	}
	var entries []journalEntry
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var entry journalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode journal line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestObservationsArePersistedAsTheyAreObtained(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "observations.jsonl")
	fixture := &sharedFixture{journal: &observationJournal{path: path}}
	byCase := map[qualification.Case]qualification.Observation{}

	record(fixture, byCase, qualification.SurfaceProtocol, qualification.CaseSuccess,
		qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "the turn completed", SessionID: "sess-1"})

	// Read back before any later phase: the journal's point is that a collection
	// which never reaches its own composition still leaves what it paid for.
	entries := readJournal(t, path)
	if len(entries) != 1 {
		t.Fatalf("the journal carries %d entries, want 1 as soon as the first observation was taken", len(entries))
	}
	got := entries[0]
	if got.Surface != string(qualification.SurfaceProtocol) || got.Case != string(qualification.CaseSuccess) {
		t.Errorf("the journal entry names %s/%s, want protocol/success", got.Surface, got.Case)
	}
	if got.Grade != string(qualification.GradeUsable) || got.Outcome != string(qualification.OutcomePass) {
		t.Errorf("the journal entry reports %s/%s, want the observation's own reading", got.Grade, got.Outcome)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("the journal entry session id = %q, want the one the observation carried", got.SessionID)
	}
	if got.RecordedAt == "" {
		t.Error("the journal entry carries no time, want the moment the observation was taken")
	}

	record(fixture, byCase, qualification.SurfaceProtocol, qualification.CaseCancellation,
		qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed})
	if entries := readJournal(t, path); len(entries) != 2 {
		t.Errorf("the journal carries %d entries after a second observation, want 2: each is appended, never rewritten", len(entries))
	}
	if _, ok := byCase[qualification.CaseCancellation]; !ok {
		t.Error("record(...) did not store the observation it journaled")
	}
}

func TestObservationJournalReportsItsOwnFailure(t *testing.T) {
	t.Parallel()

	unwritable := filepath.Join(t.TempDir(), "missing-directory", "observations.jsonl")
	journal := &observationJournal{path: unwritable}

	journal.append("protocol", "success", qualification.Observation{Grade: qualification.GradeUsable})

	if journal.err() == nil {
		t.Error("the journal reports no error after an append that could not be written, want the first failure kept")
	}
}
