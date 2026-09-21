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

// writeLines writes raw JSONL bodies to a temp file so a control can supply
// malformed lines, and returns its path.
func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "qualification")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create evidence directory: %v", err)
	}
	path := filepath.Join(dir, "evidence.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write evidence file: %v", err)
	}
	return path
}

func marshalRecordFields(t *testing.T, rec evidence.Record) map[string]json.RawMessage {
	t.Helper()
	line, err := evidence.MarshalRecord(rec)
	if err != nil {
		t.Fatalf("MarshalRecord() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatalf("unmarshal line %s: %v", line, err)
	}
	return fields
}

// RequireObservationVerdict fails t unless ValidateObservations(path, zero
// profile) reports wantVerdict.
func RequireObservationVerdict(t *testing.T, path string, wantVerdict evidence.Verdict) {
	t.Helper()
	v, err := ValidateObservations(path, profile.RuntimeProfile{})
	if err != nil {
		t.Errorf("ValidateObservations(%s) error = %v, want nil", path, err)
		return
	}
	if v != wantVerdict {
		t.Errorf("ValidateObservations(%s) = %q, want %q", path, v, wantVerdict)
	}
}

// RequireFinalVerdict fails t unless ValidateEvidence(path, zero profile)
// reports wantVerdict.
func RequireFinalVerdict(t *testing.T, path string, wantVerdict evidence.Verdict) {
	t.Helper()
	v, err := ValidateEvidence(path, profile.RuntimeProfile{})
	if err != nil {
		t.Errorf("ValidateEvidence(%s) error = %v, want nil", path, err)
		return
	}
	if v != wantVerdict {
		t.Errorf("ValidateEvidence(%s) = %q, want %q", path, v, wantVerdict)
	}
}

// publishedReport writes records, validates them against p, and returns the
// eligibility report a fresh read of the published bytes derives.
func publishedReport(t *testing.T, records []evidence.Record, p profile.RuntimeProfile) EligibilityReport {
	t.Helper()
	path := evidencetest.WriteEvidenceFile(t, records)
	published, err := evidence.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("read back the published evidence: %v", err)
	}
	if _, err := ValidateObservations(path, p); err != nil {
		t.Fatalf("validate the published evidence: %v", err)
	}
	return ExplainEligibility(published, p)
}
