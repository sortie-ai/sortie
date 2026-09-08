//go:build unix

package probe

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// writeEvidenceRecords renders records as newline-delimited JSON and
// writes them to path, the durable copy Run leaves under
// Coordinates.OutputDir.
func writeEvidenceRecords(path string, records []qualification.Record) error {
	var b strings.Builder
	for _, rec := range records {
		line, err := qualification.MarshalRecord(rec)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// writeMeasurement encodes measurement exactly as the tracked
// measurement artifact is encoded and writes it to path.
func writeMeasurement(path string, measurement qualification.Measurement) error {
	data, err := json.MarshalIndent(measurement, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// notesConsistencyReporter is the subset of *testing.T
// checkNotesConsistency calls, factored out so a meta-test can drive it
// against a fake that records failures instead of reddening its own
// run.
type notesConsistencyReporter interface {
	Fatalf(format string, args ...any)
}

// checkNotesConsistency implements the notes-binding decision table
// over the tracked document's four possible states: absent, unreadable,
// agreeing, and disagreeing. A verdict that does not require a notes
// document tolerates one that does not exist yet, so a runtime being
// onboarded can reach its first measurement before its notes exist. Any
// other read failure always fails, and a readable document is always
// compared with qualification.ValidateNotes regardless of verdict, so
// notes claiming a verdict a run did not reach are rejected on the
// eligibility line. summary is carried into every failure message so
// the operator can transcribe the run's own printed summary without a
// second paid measurement.
func checkNotesConsistency(r notesConsistencyReporter, readNotes func(string) ([]byte, error), notesPath string, want qualification.NotesExpectation, summary string) {
	document, readErr := readNotes(notesPath)
	switch {
	case os.IsNotExist(readErr):
		if qualification.NotesBindingRequired(want.Verdict) {
			r.Fatalf("notes %s do not exist, but a %s verdict requires a notes document to compare against; write the document from this summary, then re-check without the gate\n%s",
				notesPath, want.Verdict, summary)
		}
		return
	case readErr != nil:
		r.Fatalf("read notes %s: %v\n%s", notesPath, readErr, summary)
		return
	}
	if mismatch := qualification.ValidateNotes(string(document), want); mismatch != nil {
		r.Fatalf("notes %s disagree with the validated run: %v\n%s", notesPath, mismatch, summary)
	}
}

// enforceNotesConsistency reads notesPath and fails t when it disagrees
// with want, the expectation derived from a validated run, per the
// decision table checkNotesConsistency implements. It never mutates
// notesPath.
func enforceNotesConsistency(t testingT, notesPath string, want qualification.NotesExpectation, summary string) {
	t.Helper()
	checkNotesConsistency(t, os.ReadFile, notesPath, want, summary)
}

// testingT is the subset of *testing.T enforceNotesConsistency needs,
// so it can be driven by a fake reporter in a meta-test.
type testingT interface {
	notesConsistencyReporter
	Helper()
}
