//go:build unix

package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func firstProfile(t *testing.T) profile.RuntimeProfile {
	t.Helper()
	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("profile.CheckoutRoot() error = %v, want nil", err)
	}
	paths := stalenessProfilePaths(t)
	p, err := profile.Load(root, paths[0])
	if err != nil {
		t.Fatalf("profile.Load(%s) error = %v, want nil", paths[0], err)
	}
	return p
}

func composedMeasurement(t *testing.T, requestedModel, provenanceDigest string) evidence.Measurement {
	t.Helper()

	conclusions := Conclusions{
		Verdict:     evidence.VerdictQualified,
		Conformance: evidence.VerdictQualified,
	}
	result := Result{Conformance: conclusions.Conformance, Conclusions: conclusions, EvaluatorVersion: evaluatorVersion, JournalAvailable: true}
	_, measurement, err := Render(result, firstProfile(t), "2026-01-01T00:00:00Z", requestedModel, provenanceDigest)
	if err != nil {
		t.Fatalf("Render(...) error = %v, want nil", err)
	}
	return measurement
}

func TestComposedMeasurementStatesTheCurrentSchema(t *testing.T) {
	t.Parallel()

	measurement := composedMeasurement(t, "a-model", "a-provenance-digest")
	if measurement.SchemaVersion != evidence.MeasurementSchemaVersion {
		t.Errorf("Render(...).SchemaVersion = %d, want %d", measurement.SchemaVersion, evidence.MeasurementSchemaVersion)
	}
}

func TestComposedMeasurementCarriesTheProvenanceDigest(t *testing.T) {
	t.Parallel()

	const digest = "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"
	measurement := composedMeasurement(t, "a-model", digest)
	if measurement.ProvenanceDigest == nil {
		t.Fatal("Render(...).ProvenanceDigest = nil, want the digest of the provenance artifact")
	}
	if *measurement.ProvenanceDigest != digest {
		t.Errorf("Render(...).ProvenanceDigest = %q, want %q", *measurement.ProvenanceDigest, digest)
	}
}

func TestComposedMeasurementRefusesAnUnstatedProvenanceDigest(t *testing.T) {
	t.Parallel()

	p := firstProfile(t)
	result := Result{Conformance: evidence.VerdictQualified, Conclusions: Conclusions{Verdict: evidence.VerdictQualified, Conformance: evidence.VerdictQualified}, EvaluatorVersion: evaluatorVersion}
	if _, _, err := Render(result, p, "2026-01-01T00:00:00Z", "a-model", ""); err == nil {
		t.Error("Render(..., \"\") = nil error, want a refusal: the provenance artifact must be written before the measurement that digests it")
	}
}

// Defaulting the served model to the requested coordinate would republish the
// request as an observation, the defect the two separate members prevent.
func TestComposedMeasurementLeavesTheServedModelUnstated(t *testing.T) {
	t.Parallel()

	measurement := composedMeasurement(t, "a-requested-model", "a-provenance-digest")
	if measurement.RequestedModel != "a-requested-model" {
		t.Errorf("Render(...).RequestedModel = %q, want the requested coordinate", measurement.RequestedModel)
	}
	if measurement.ObservedModel != nil {
		t.Errorf("Render(...).ObservedModel = %q, want null: no reading in this collection reports the model a runtime served", *measurement.ObservedModel)
	}
}

func TestProvenanceDigestIsTheHashOfTheWrittenArtifact(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "provenance.json")
	content := []byte(`{"schema_version":1,"requested_model":"a-model"}` + "\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write the provenance fixture: %v", err)
	}
	digest, err := fileDigest(path)
	if err != nil {
		t.Fatalf("fileDigest(%q) error = %v, want nil", path, err)
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Errorf("fileDigest(%q) = %q, want %q", path, digest, want)
	}
}

func TestComposedMeasurementRefusesAnUnstatedProductAnswer(t *testing.T) {
	t.Parallel()

	result := Result{Conclusions: Conclusions{Verdict: evidence.VerdictQualified}, EvaluatorVersion: evaluatorVersion}
	_, _, err := Render(result, firstProfile(t), "2026-01-01T00:00:00Z", "a-model", "a-provenance-digest")
	if err == nil {
		t.Error("Render(...) = nil error, want a refusal: the result carries no product-conformance answer")
	}
}

func TestPublishedMeasurementCarriesTheProductAnswer(t *testing.T) {
	t.Parallel()

	conclusions := Conclusions{
		Verdict:     evidence.VerdictQualified,
		Conformance: evidence.VerdictNotQualified,
	}
	result := Result{Conformance: conclusions.Conformance, Conclusions: conclusions, EvaluatorVersion: evaluatorVersion, JournalAvailable: true}
	_, measurement, err := Render(result, firstProfile(t), "2026-01-01T00:00:00Z", "a-model", "a-provenance-digest")
	if err != nil {
		t.Fatalf("Render(...) error = %v, want nil", err)
	}
	path := filepath.Join(t.TempDir(), "measurement.json")
	if err := writeMeasurementFixture(path, measurement); err != nil {
		t.Fatalf("write the measurement fixture: %v", err)
	}
	published, err := evidence.ReadMeasurementFile(path)
	if err != nil {
		t.Fatalf("ReadMeasurementFile(%q) error = %v, want nil", path, err)
	}
	if published.Expectation.Conformance == nil {
		t.Fatal("the published measurement states no product answer, want the one the run reached")
	}
	if *published.Expectation.Conformance != evidence.VerdictNotQualified {
		t.Errorf("published product answer = %s, want %s", *published.Expectation.Conformance, evidence.VerdictNotQualified)
	}
	if published.Expectation.Verdict != evidence.VerdictQualified {
		t.Errorf("published transport answer = %s, want %s", published.Expectation.Verdict, evidence.VerdictQualified)
	}
}

// fileDigest hashes path's content, mirroring the collector's own digest
// function so a test fixture can be verified against it without importing
// the probe package, which this package must not depend on.
func fileDigest(path string) (string, error) {
	content, err := os.ReadFile(path) //nolint:gosec // path is this test's own temporary directory
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func writeMeasurementFixture(path string, measurement evidence.Measurement) error {
	data, err := json.MarshalIndent(measurement, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
