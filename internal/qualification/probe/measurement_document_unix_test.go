//go:build unix

package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

func composedMeasurement(t *testing.T, requestedModel, provenanceDigest string) qualification.Measurement {
	t.Helper()

	conclusions := Conclusions{
		Verdict:     qualification.VerdictQualified,
		Conformance: qualification.VerdictQualified,
	}
	measurement, err := composeMeasurement(firstProfile(t), conclusions, requestedModel, provenanceDigest)
	if err != nil {
		t.Fatalf("composeMeasurement(...) error = %v, want nil", err)
	}
	return measurement
}

func TestComposedMeasurementStatesTheCurrentSchema(t *testing.T) {
	t.Parallel()

	measurement := composedMeasurement(t, "a-model", "a-provenance-digest")
	if measurement.SchemaVersion != 4 {
		t.Errorf("composeMeasurement(...).SchemaVersion = %d, want 4", measurement.SchemaVersion)
	}
}

func TestComposedMeasurementCarriesTheProvenanceDigest(t *testing.T) {
	t.Parallel()

	const digest = "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"
	measurement := composedMeasurement(t, "a-model", digest)
	if measurement.ProvenanceDigest == nil {
		t.Fatal("composeMeasurement(...).ProvenanceDigest = nil, want the digest of the provenance artifact")
	}
	if *measurement.ProvenanceDigest != digest {
		t.Errorf("composeMeasurement(...).ProvenanceDigest = %q, want %q", *measurement.ProvenanceDigest, digest)
	}
}

func TestComposedMeasurementRefusesAnUnstatedProvenanceDigest(t *testing.T) {
	t.Parallel()

	profile := firstProfile(t)
	if _, err := composeMeasurement(profile, Conclusions{}, "a-model", ""); err == nil {
		t.Error("composeMeasurement(..., \"\") = nil error, want a refusal: the provenance artifact must be written before the measurement that digests it")
	}
}

// Defaulting the served model to the requested coordinate would republish the
// request as an observation, the defect the two separate members prevent.
func TestComposedMeasurementLeavesTheServedModelUnstated(t *testing.T) {
	t.Parallel()

	measurement := composedMeasurement(t, "a-requested-model", "a-provenance-digest")
	if measurement.RequestedModel != "a-requested-model" {
		t.Errorf("composeMeasurement(...).RequestedModel = %q, want the requested coordinate", measurement.RequestedModel)
	}
	if measurement.ObservedModel != nil {
		t.Errorf("composeMeasurement(...).ObservedModel = %q, want null: no reading in this collection reports the model a runtime served", *measurement.ObservedModel)
	}
}

func TestProvenanceDigestIsTheHashOfTheWrittenArtifact(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "provenance.json")
	if err := writeProvenance(path, collectionProvenance{SchemaVersion: 1, RequestedModel: "a-model"}); err != nil {
		t.Fatalf("writeProvenance(%q) error = %v, want nil", path, err)
	}
	digest, err := fileDigest(path)
	if err != nil {
		t.Fatalf("fileDigest(%q) error = %v, want nil", path, err)
	}
	content, err := os.ReadFile(path) //nolint:gosec // this test's own temporary directory
	if err != nil {
		t.Fatalf("read back %q: %v", path, err)
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Errorf("fileDigest(%q) = %q, want %q", path, digest, want)
	}
}

func TestComposedMeasurementRefusesAnUnstatedProductAnswer(t *testing.T) {
	t.Parallel()

	_, err := composeMeasurement(firstProfile(t), Conclusions{Verdict: qualification.VerdictQualified}, "a-model", "a-provenance-digest")
	if err == nil {
		t.Error("composeMeasurement(...) = nil error, want a refusal: the conclusions carry no product-conformance answer")
	}
}

func TestPublishedMeasurementCarriesTheProductAnswer(t *testing.T) {
	t.Parallel()

	conclusions := Conclusions{
		Verdict:     qualification.VerdictQualified,
		Conformance: qualification.VerdictNotQualified,
	}
	measurement, err := composeMeasurement(firstProfile(t), conclusions, "a-model", "a-provenance-digest")
	if err != nil {
		t.Fatalf("composeMeasurement(...) error = %v, want nil", err)
	}
	path := filepath.Join(t.TempDir(), "measurement.json")
	if err := writeMeasurement(path, measurement); err != nil {
		t.Fatalf("writeMeasurement(%q) error = %v, want nil", path, err)
	}
	published, err := qualification.ReadMeasurementFile(path)
	if err != nil {
		t.Fatalf("ReadMeasurementFile(%q) error = %v, want nil", path, err)
	}
	if published.Expectation.Conformance == nil {
		t.Fatal("the published measurement states no product answer, want the one the run reached")
	}
	if *published.Expectation.Conformance != qualification.VerdictNotQualified {
		t.Errorf("published product answer = %s, want %s", *published.Expectation.Conformance, qualification.VerdictNotQualified)
	}
	if published.Expectation.Verdict != qualification.VerdictQualified {
		t.Errorf("published transport answer = %s, want %s", published.Expectation.Verdict, qualification.VerdictQualified)
	}
}
