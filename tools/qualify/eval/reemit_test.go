package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// stageTrackedCapture copies one runtime's tracked capture directory into a
// scratch directory Reemit may rewrite, and returns it with the loaded
// profile.
func stageTrackedCapture(t *testing.T, runtimeID string) (string, profile.RuntimeProfile) {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	p, err := profile.Load(root, filepath.Join("..", "profiles", runtimeID+".json"))
	if err != nil {
		t.Fatalf("Load(...) error = %v, want nil", err)
	}

	src := filepath.Join("..", "probe", "testdata", runtimeID)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error = %v", src, err)
	}
	dst := t.TempDir()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(src, entry.Name())) //nolint:gosec // a tracked testdata path
		if readErr != nil {
			t.Fatalf("read %s: %v", entry.Name(), readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(dst, entry.Name()), content, 0o600); writeErr != nil {
			t.Fatalf("stage %s: %v", entry.Name(), writeErr)
		}
	}
	return dst, p
}

func readMeasurementRaw(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "measurement.json")) //nolint:gosec // this test's own staged capture directory
	if err != nil {
		t.Fatalf("read measurement.json: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode measurement.json: %v", err)
	}
	return doc
}

func TestReemitTrackedCorpusIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, runtimeID := range []string{"gemini-cli", "kiro-cli"} {
		t.Run(runtimeID, func(t *testing.T) {
			t.Parallel()

			dir, p := stageTrackedCapture(t, runtimeID)
			before := readMeasurementRaw(t, dir)

			if err := Reemit(dir, p); err != nil {
				t.Fatalf("Reemit(%q, ...) error = %v, want nil", dir, err)
			}
			after := readMeasurementRaw(t, dir)

			if string(before["expectation"]) != string(after["expectation"]) {
				t.Errorf("Reemit(...) moved the expectation member:\nbefore: %s\nafter:  %s", before["expectation"], after["expectation"])
			}
			if string(before["measured_at"]) != string(after["measured_at"]) {
				t.Errorf("Reemit(...) measured_at = %s, want it carried over unchanged from %s", after["measured_at"], before["measured_at"])
			}
			if string(before["requested_model"]) != string(after["requested_model"]) {
				t.Errorf("Reemit(...) requested_model = %s, want it carried over unchanged from %s", after["requested_model"], before["requested_model"])
			}

			var evaluatorVersionAfter int
			if err := json.Unmarshal(after["evaluator_version"], &evaluatorVersionAfter); err != nil {
				t.Fatalf("decode evaluator_version: %v", err)
			}
			if evaluatorVersionAfter != evaluatorVersion {
				t.Errorf("Reemit(...) evaluator_version = %d, want the current constant %d", evaluatorVersionAfter, evaluatorVersion)
			}
		})
	}
}

func TestReemitCarriesEvidenceOverByteForByteWithNoJournal(t *testing.T) {
	t.Parallel()

	dir, p := stageTrackedCapture(t, "gemini-cli")
	before, err := os.ReadFile(filepath.Join(dir, "evidence.jsonl")) //nolint:gosec // this test's own staged capture directory
	if err != nil {
		t.Fatalf("read evidence.jsonl: %v", err)
	}

	if err := Reemit(dir, p); err != nil {
		t.Fatalf("Reemit(%q, ...) error = %v, want nil", dir, err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "evidence.jsonl")) //nolint:gosec // this test's own staged capture directory
	if err != nil {
		t.Fatalf("read evidence.jsonl after Reemit: %v", err)
	}
	if string(before) != string(after) {
		t.Error("Reemit(...) rewrote evidence.jsonl though no observations.jsonl was present, want it carried over byte for byte")
	}
}

func TestReemitRecomputesProfileAndEvidenceDigests(t *testing.T) {
	t.Parallel()

	dir, p := stageTrackedCapture(t, "gemini-cli")
	if err := Reemit(dir, p); err != nil {
		t.Fatalf("Reemit(%q, ...) error = %v, want nil", dir, err)
	}

	provenanceData, err := os.ReadFile(filepath.Join(dir, "provenance.json")) //nolint:gosec // this test's own staged capture directory
	if err != nil {
		t.Fatalf("read provenance.json: %v", err)
	}
	var provenance provenanceDocument
	if err := json.Unmarshal(provenanceData, &provenance); err != nil {
		t.Fatalf("decode provenance.json: %v", err)
	}
	if provenance.ProfileDigest != p.Digest() {
		t.Errorf("provenance.json profile_digest = %s, want %s", provenance.ProfileDigest, p.Digest())
	}
	wantEvidenceDigest, err := stalenessFileSHA256(filepath.Join(dir, "evidence.jsonl"))
	if err != nil {
		t.Fatalf("stalenessFileSHA256(evidence.jsonl) error = %v", err)
	}
	if provenance.EvidenceDigest != wantEvidenceDigest {
		t.Errorf("provenance.json evidence_digest = %s, want %s", provenance.EvidenceDigest, wantEvidenceDigest)
	}
}

func TestReemitRefusesUnderAnUnchangedEvaluatorVersion(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	evidencePath := evidencetest.WriteEvidenceFile(t, fixture.Records)
	dir := filepath.Dir(evidencePath)

	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write provenance.json: %v", err)
	}
	measurement := map[string]any{
		"measured_at":       "2026-01-01",
		"requested_model":   "fixture-model",
		"observed_model":    nil,
		"evaluator_version": evaluatorVersion,
	}
	measurementData, err := json.Marshal(measurement)
	if err != nil {
		t.Fatalf("json.Marshal(measurement) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "measurement.json"), measurementData, 0o600); err != nil {
		t.Fatalf("write measurement.json: %v", err)
	}

	entry := evidence.JournalEntry{
		Surface:    "native_json",
		Case:       "success",
		Grade:      "usable",
		Outcome:    "pass",
		Detail:     "the recognized terminal reported end of turn",
		RecordedAt: "2026-01-01T00:00:00Z",
		Derivation: evidence.DerivationRecognizer,
		Launch: &evidence.LaunchRecord{
			Argv:    []string{"runtime"},
			Outcome: evidence.LaunchOutcomeCompleted,
		},
		Streams: &evidence.StreamCapture{
			Stdout:      `{"response":{"text":"no recognizable terminal here"}}`,
			StdoutBytes: 48,
			Retention:   evidence.StreamRetentionFull,
		},
	}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("json.Marshal(entry) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observations.jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatalf("write observations.jsonl: %v", err)
	}

	err = Reemit(dir, measuredThreeSurfaceProfile())
	if err == nil {
		t.Fatal("Reemit(...) = nil, want a refusal: the journal is internally self-inconsistent under an unchanged evaluator version")
	}
	if !strings.Contains(err.Error(), "unchanged evaluator version") {
		t.Errorf("Reemit(...) error = %v, want it to name the unchanged evaluator version", err)
	}
}

// The staged evaluator_version stays at its unset zero value, distinct from
// the current evaluatorVersion constant, so the refusal can only be
// explained by the no-journal arm, not the unchanged-evaluator-version one.
func TestReemitRefusesADerivedContentChangeWithNoJournalPresent(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	evidencePath := evidencetest.WriteEvidenceFile(t, fixture.Records)
	dir := filepath.Dir(evidencePath)

	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write provenance.json: %v", err)
	}

	p := measuredThreeSurfaceProfile()
	result, err := Run(Input{CaptureDir: dir, Profile: p})
	if err != nil {
		t.Fatalf("Run(...) error = %v, want nil", err)
	}
	expectation := expectationFrom(result.Conclusions)
	stale := evidence.VerdictNotQualified
	if expectation.Verdict == stale {
		stale = evidence.VerdictQualified
	}
	expectation.Verdict = stale

	measurement := map[string]any{
		"measured_at":     "2026-01-01",
		"requested_model": "fixture-model",
		"observed_model":  nil,
		"expectation":     expectation,
	}
	measurementData, err := json.Marshal(measurement)
	if err != nil {
		t.Fatalf("json.Marshal(measurement) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "measurement.json"), measurementData, 0o600); err != nil {
		t.Fatalf("write measurement.json: %v", err)
	}

	// No observations.jsonl is staged.

	err = Reemit(dir, p)
	if err == nil {
		t.Fatal("Reemit(...) = nil, want a refusal: the expectation would move with no observations.jsonl present to justify it")
	}
	if !strings.Contains(err.Error(), journalFileName) {
		t.Errorf("Reemit(...) error = %v, want it to name %s", err, journalFileName)
	}
}

// TestQualifyReemit skips cleanly, never fails, when its env vars are
// unset: this is a maintainer action, never something CI drives unattended.
func TestQualifyReemit(t *testing.T) {
	dir := os.Getenv(captureDirEnvVar)
	profilePath := os.Getenv(qualifyReemitProfileEnvVar)
	if dir == "" || profilePath == "" {
		t.Skipf("skipping: set %s and %s to reemit one capture directory", captureDirEnvVar, qualifyReemitProfileEnvVar)
	}

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	p, err := profile.Load(root, profilePath)
	if err != nil {
		t.Fatalf("Load(%q, %q) error = %v, want nil", root, profilePath, err)
	}
	if err := Reemit(dir, p); err != nil {
		t.Fatalf("Reemit(%q, ...) error = %v", dir, err)
	}
}

// qualifyReemitProfileEnvVar names the runtime profile TestQualifyReemit
// loads, mirroring tools/qualify/probe's own SORTIE_CLIENTPROTOCOL_QUALIFICATION_PROFILE
// coordinate. eval must not import probe, so the literal is restated here
// rather than shared.
const qualifyReemitProfileEnvVar = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_PROFILE"
