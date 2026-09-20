package probe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

func trackedMeasurementBytes(t *testing.T) map[string][]byte {
	t.Helper()

	root, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	documents := map[string][]byte{}
	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Fatalf("%s: decode: %v", profilePath, err)
		}
		path := filepath.Join(root, profile.MeasurementPath)
		data, err := os.ReadFile(path) //nolint:gosec // a repository-relative path resolved from the tracked profile
		if err != nil {
			t.Fatalf("read the tracked measurement %s: %v", path, err)
		}
		documents[profile.MeasurementPath] = data
	}
	return documents
}

// withoutExpectationMember returns document with one expectation member
// removed, the shape a document takes when a run publishes an answer by saying
// nothing about it.
func withoutExpectationMember(t *testing.T, document []byte, member string) []byte {
	t.Helper()

	var top map[string]json.RawMessage
	if err := json.Unmarshal(document, &top); err != nil {
		t.Fatalf("decode the measurement document: %v", err)
	}
	var expectation map[string]json.RawMessage
	if err := json.Unmarshal(top["expectation"], &expectation); err != nil {
		t.Fatalf("decode the measurement expectation: %v", err)
	}
	if _, stated := expectation[member]; !stated {
		t.Fatalf("the tracked expectation states no %q member, so it publishes one answer for both questions", member)
	}
	delete(expectation, member)
	reduced, err := json.Marshal(expectation)
	if err != nil {
		t.Fatalf("re-encode the measurement expectation: %v", err)
	}
	top["expectation"] = reduced
	data, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("re-encode the measurement document: %v", err)
	}
	return data
}

func TestTrackedMeasurementStatesTheProductAnswer(t *testing.T) {
	t.Parallel()

	for path, document := range trackedMeasurementBytes(t) {
		if _, err := qualification.DecodeMeasurement(document); err != nil {
			t.Errorf("DecodeMeasurement(%s) error = %v, want nil", path, err)
			continue
		}
		reduced := withoutExpectationMember(t, document, "Conformance")
		if _, err := qualification.DecodeMeasurement(reduced); err == nil {
			t.Errorf("DecodeMeasurement(%s without its product answer) = nil error, want a refusal", path)
		}
	}
}

func TestPublishedMeasurementKeepsTheTwoAnswersApart(t *testing.T) {
	t.Parallel()

	conclusions := Conclusions{
		Verdict:     qualification.VerdictQualified,
		Conformance: qualification.VerdictNotQualified,
	}
	expectation := ExpectationFrom(conclusions)
	if expectation.Conformance == nil {
		t.Fatal("ExpectationFrom(...).Conformance = nil, want the product answer the run reached")
	}
	if *expectation.Conformance != qualification.VerdictNotQualified {
		t.Errorf("ExpectationFrom(...).Conformance = %s, want %s", *expectation.Conformance, qualification.VerdictNotQualified)
	}
	if expectation.Verdict != qualification.VerdictQualified {
		t.Errorf("ExpectationFrom(...).Verdict = %s, want %s", expectation.Verdict, qualification.VerdictQualified)
	}
}
