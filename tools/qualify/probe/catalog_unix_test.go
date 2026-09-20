//go:build unix

package probe

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

func TestBoundStream(t *testing.T) {
	t.Parallel()

	t.Run("an empty stream is full with a zero byte count", func(t *testing.T) {
		t.Parallel()

		bounded, size, retention := boundStream("")
		if bounded != "" || size != 0 || retention != evidence.StreamRetentionFull {
			t.Errorf("boundStream(\"\") = %q, %d, %s, want \"\", 0, %s", bounded, size, retention, evidence.StreamRetentionFull)
		}
	})

	t.Run("a stream at or below the bound is retained whole", func(t *testing.T) {
		t.Parallel()

		s := strings.Repeat("a", streamRetentionBound)
		bounded, size, retention := boundStream(s)
		if bounded != s {
			t.Error("boundStream() at exactly the bound rewrote the stream, want it unchanged")
		}
		if size != streamRetentionBound {
			t.Errorf("boundStream() size = %d, want %d", size, streamRetentionBound)
		}
		if retention != evidence.StreamRetentionFull {
			t.Errorf("boundStream() retention = %s, want %s", retention, evidence.StreamRetentionFull)
		}
	})

	t.Run("a stream past the bound is truncated head and tail with the pre-bound size recorded", func(t *testing.T) {
		t.Parallel()

		head := strings.Repeat("h", streamRetentionBound/2)
		middle := strings.Repeat("m", 4096)
		tail := strings.Repeat("t", streamRetentionBound/2)
		s := head + middle + tail

		bounded, size, retention := boundStream(s)
		if retention != evidence.StreamRetentionTruncated {
			t.Errorf("boundStream() retention = %s, want %s", retention, evidence.StreamRetentionTruncated)
		}
		if int64(len(s)) != size {
			t.Errorf("boundStream() reported size = %d, want the pre-bound length %d", size, len(s))
		}
		if len(bounded) != streamRetentionBound {
			t.Errorf("boundStream() retained %d bytes, want exactly %d", len(bounded), streamRetentionBound)
		}
		if !strings.HasPrefix(bounded, head) {
			t.Error("boundStream() did not retain the original head")
		}
		if !strings.HasSuffix(bounded, tail) {
			t.Error("boundStream() did not retain the original tail")
		}
		if strings.Contains(bounded, middle) {
			t.Error("boundStream() retained the discarded middle, want it dropped")
		}
	})
}

func TestBoundStreamCapture(t *testing.T) {
	t.Parallel()

	capture := boundStreamCapture(strings.Repeat("x", streamRetentionBound+1))
	if capture.Retention != evidence.StreamRetentionTruncated {
		t.Errorf("boundStreamCapture() Retention = %s, want %s", capture.Retention, evidence.StreamRetentionTruncated)
	}
	if capture.StdoutBytes != streamRetentionBound+1 {
		t.Errorf("boundStreamCapture() StdoutBytes = %d, want %d", capture.StdoutBytes, streamRetentionBound+1)
	}
	if capture.Stderr != "" {
		t.Errorf("boundStreamCapture() Stderr = %q, want empty: native output is combined before this package sees it", capture.Stderr)
	}
}

func TestObservationJournalAppend(t *testing.T) {
	t.Parallel()

	t.Run("a nil journal or one with no path writes nothing and does not panic", func(t *testing.T) {
		t.Parallel()

		var nilJournal *observationJournal
		nilJournal.append("protocol", "success", transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass}))

		empty := &observationJournal{}
		empty.append("protocol", "success", transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass}))
	})

	t.Run("a recognizer entry carries its launch and streams for re-derivation", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "observations.jsonl")
		j := &observationJournal{path: path}

		launch := evidence.LaunchRecord{Argv: []string{"runtime"}, Outcome: evidence.LaunchOutcomeCompleted}
		streams := evidence.StreamCapture{Stdout: "output", StdoutBytes: 6, Retention: evidence.StreamRetentionFull}
		j.append("native_json", "success", recognizerGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "matched"}, launch, streams))

		if err := j.err(); err != nil {
			t.Fatalf("observationJournal.err() = %v, want nil", err)
		}

		entries := readJournalEntries(t, path)
		if len(entries) != 1 {
			t.Fatalf("journal carries %d entries, want 1", len(entries))
		}
		entry := entries[0]
		if entry.Derivation != evidence.DerivationRecognizer {
			t.Errorf("entry.Derivation = %s, want %s", entry.Derivation, evidence.DerivationRecognizer)
		}
		if entry.Launch == nil || entry.Streams == nil {
			t.Fatal("a recognizer entry must carry launch and streams")
		}
		if entry.Launch.Outcome != evidence.LaunchOutcomeCompleted {
			t.Errorf("entry.Launch.Outcome = %s, want %s", entry.Launch.Outcome, evidence.LaunchOutcomeCompleted)
		}
		if entry.Streams.Stdout != "output" {
			t.Errorf("entry.Streams.Stdout = %q, want %q", entry.Streams.Stdout, "output")
		}
	})

	t.Run("a transport entry carries no launch or streams", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "observations.jsonl")
		j := &observationJournal{path: path}
		j.append("protocol", "success", transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass}))

		entries := readJournalEntries(t, path)
		if len(entries) != 1 {
			t.Fatalf("journal carries %d entries, want 1", len(entries))
		}
		if entries[0].Launch != nil || entries[0].Streams != nil {
			t.Error("a transport entry carries launch/streams, want neither")
		}
	})

	t.Run("multiple appends accumulate in call order", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "observations.jsonl")
		j := &observationJournal{path: path}
		for _, caseID := range []string{"success", "runtime_failure", "cancellation"} {
			j.append("protocol", caseID, transportGraded(evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass}))
		}

		entries := readJournalEntries(t, path)
		if len(entries) != 3 {
			t.Fatalf("journal carries %d entries, want 3", len(entries))
		}
		want := []string{"success", "runtime_failure", "cancellation"}
		for i, entry := range entries {
			if entry.Case != want[i] {
				t.Errorf("entry %d Case = %q, want %q", i, entry.Case, want[i])
			}
		}
	})
}

func readJournalEntries(t *testing.T, path string) []evidence.JournalEntry {
	t.Helper()

	file, err := os.Open(path) //nolint:gosec // this test's own temp directory
	if err != nil {
		t.Fatalf("open journal %s: %v", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only handle in a test

	var entries []evidence.JournalEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry evidence.JournalEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("decode journal line %q: %v", scanner.Text(), err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan journal: %v", err)
	}
	return entries
}
