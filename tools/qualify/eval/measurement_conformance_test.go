package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func trackedMeasurementBytes(t *testing.T) map[string][]byte {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	documents := map[string][]byte{}
	for _, profilePath := range stalenessProfilePaths(t) {
		p, err := profile.Load(root, profilePath)
		if err != nil {
			t.Fatalf("%s: decode: %v", profilePath, err)
		}
		path := filepath.Join(root, p.MeasurementPath)
		data, err := os.ReadFile(path) //nolint:gosec // a repository-relative path resolved from the tracked profile
		if err != nil {
			t.Fatalf("read the tracked measurement %s: %v", path, err)
		}
		documents[p.MeasurementPath] = data
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
		if _, err := evidence.DecodeMeasurement(document); err != nil {
			t.Errorf("DecodeMeasurement(%s) error = %v, want nil", path, err)
			continue
		}
		reduced := withoutExpectationMember(t, document, "Conformance")
		if _, err := evidence.DecodeMeasurement(reduced); err == nil {
			t.Errorf("DecodeMeasurement(%s without its product answer) = nil error, want a refusal", path)
		}
	}
}

func TestPublishedMeasurementKeepsTheTwoAnswersApart(t *testing.T) {
	t.Parallel()

	conclusions := Conclusions{
		Verdict:     evidence.VerdictQualified,
		Conformance: evidence.VerdictNotQualified,
	}
	expectation := expectationFrom(conclusions)
	if expectation.Conformance == nil {
		t.Fatal("expectationFrom(...).Conformance = nil, want the product answer the run reached")
	}
	if *expectation.Conformance != evidence.VerdictNotQualified {
		t.Errorf("expectationFrom(...).Conformance = %s, want %s", *expectation.Conformance, evidence.VerdictNotQualified)
	}
	if expectation.Verdict != evidence.VerdictQualified {
		t.Errorf("expectationFrom(...).Verdict = %s, want %s", expectation.Verdict, evidence.VerdictQualified)
	}
}
