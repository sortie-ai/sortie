package probe

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// TestQualificationProfile is the operator's entry point into the live profile:
// the one gated test that drives a paid collection. Run reports its own failures
// through t, so this adds only the checks Run cannot make about its result.
//
// The verdict is logged, not asserted: a not_qualified or unmeasured outcome is
// the measurement this run exists to produce, and failing on it would make the
// instrument report the answer it was built to find.
//
// Run launches real processes and spends model quota, so this test MUST NOT
// call t.Parallel().
func TestQualificationProfile(t *testing.T) {
	coords, ok := Gated(t)
	if !ok {
		return
	}

	result := Run(t, coords)

	vocabulary := []qualification.Verdict{
		qualification.VerdictQualified,
		qualification.VerdictNotQualified,
		qualification.VerdictUnmeasured,
	}
	if !slices.Contains(vocabulary, result.Verdict) {
		t.Errorf("Run() verdict = %q, want one of %v", result.Verdict, vocabulary)
	}

	artifacts := []struct {
		name string
		path string
	}{
		{"evidence", result.EvidencePath},
		{"summary", result.SummaryPath},
		{"measurement", result.MeasurementPath},
		{"provenance", result.ProvenancePath},
	}
	for _, artifact := range artifacts {
		info, err := os.Stat(artifact.path)
		if err != nil {
			t.Errorf("stat the %s artifact Run reported: %v", artifact.name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("the %s artifact at %s is empty, want the run's product", artifact.name, artifact.path)
		}
	}

	t.Logf("qualification verdict %s; artifacts under %s", result.Verdict, filepath.Dir(result.EvidencePath))
}
