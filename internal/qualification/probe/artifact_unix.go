//go:build unix

package probe

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// writeEvidenceRecords writes records as newline-delimited JSON to path.
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

// writeUnrecognizedTerminals writes terminals as newline-delimited JSON
// to path. It is a no-op when terminals is empty, leaving no misleading
// empty file behind.
func writeUnrecognizedTerminals(path string, terminals []map[string]any) error {
	if len(terminals) == 0 {
		return nil
	}
	var b strings.Builder
	for _, terminal := range terminals {
		line, err := json.Marshal(terminal)
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
// checkNotesConsistency uses, so a meta-test can drive it against a
// fake that records failures instead of failing its own run.
type notesConsistencyReporter interface {
	Fatalf(format string, args ...any)
}

// checkNotesConsistency compares the tracked notes document against
// want. A verdict that does not require notes tolerates an absent
// document, so a runtime being onboarded can reach its first
// measurement before its notes exist; any other read failure fails.
// summary is carried into every failure so the operator can transcribe
// the run's printed summary without a second paid measurement.
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
// with want. It never mutates notesPath.
func enforceNotesConsistency(t testingT, notesPath string, want qualification.NotesExpectation, summary string) {
	t.Helper()
	checkNotesConsistency(t, os.ReadFile, notesPath, want, summary)
}

// testingT is the subset of *testing.T enforceNotesConsistency needs,
// so a fake reporter can drive it in a meta-test.
type testingT interface {
	notesConsistencyReporter
	Helper()
}
