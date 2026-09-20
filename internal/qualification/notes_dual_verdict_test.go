package qualification

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishedNotes validates the bytes read back from disk, not the in-memory
// string, so the control grades what a reader sees.
func publishedNotes(t *testing.T, document string, want NotesExpectation) error {
	t.Helper()

	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write the notes document: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // this test's own temporary directory
	if err != nil {
		t.Fatalf("read back the notes document: %v", err)
	}
	return ValidateNotes(string(data), want)
}

func notesStating(want NotesExpectation, conformance Verdict) string {
	return strings.Replace(compliantNotesDocument(want),
		NotesEligibilityPrefix+string(want.Verdict)+"\n",
		NotesEligibilityPrefix+string(want.Verdict)+"\n"+NotesConformancePrefix+string(conformance)+"\n", 1)
}

func bothAnswers() NotesExpectation {
	want := compliantNotesExpectation()
	conformance := VerdictNotQualified
	want.Conformance = &conformance
	return want
}

func TestNotesMustStateTheProductAnswer(t *testing.T) {
	t.Parallel()

	want := bothAnswers()
	err := publishedNotes(t, compliantNotesDocument(want), want)
	if err == nil {
		t.Fatal("ValidateNotes() = nil, want a rejection: the document states no product-conformance answer")
	}
	if !strings.Contains(err.Error(), NotesConformancePrefix) {
		t.Errorf("ValidateNotes() error = %v, want it to name the missing line", err)
	}
}

func TestNotesProductAnswerIsBoundToTheRun(t *testing.T) {
	t.Parallel()

	want := bothAnswers()
	if err := publishedNotes(t, notesStating(want, VerdictQualified), want); err == nil {
		t.Error("ValidateNotes() = nil, want a rejection: the document claims a product answer the run did not reach")
	}
}

func TestNotesAcceptBothAnswersWhenTheyAgree(t *testing.T) {
	t.Parallel()

	want := bothAnswers()
	if err := publishedNotes(t, notesStating(want, VerdictNotQualified), want); err != nil {
		t.Errorf("ValidateNotes() error = %v, want nil for a document stating both answers as the run produced them", err)
	}
}

func TestNotesCannotStateAProductAnswerNoRunProduced(t *testing.T) {
	t.Parallel()

	want := compliantNotesExpectation()
	if want.Conformance != nil {
		t.Fatal("compliantNotesExpectation() states a product answer, want none for this control")
	}
	if err := publishedNotes(t, notesStating(want, VerdictQualified), want); err == nil {
		t.Error("ValidateNotes() = nil, want a rejection: the run behind this expectation answered only the transport question")
	}
}

func TestNotesRejectASecondProductAnswerLine(t *testing.T) {
	t.Parallel()

	want := bothAnswers()
	document := notesStating(want, VerdictNotQualified)
	if err := publishedNotes(t, document+NotesConformancePrefix+string(VerdictNotQualified)+"\n", want); err == nil {
		t.Error("ValidateNotes() = nil, want a rejection: the document states the product answer twice")
	}
}
