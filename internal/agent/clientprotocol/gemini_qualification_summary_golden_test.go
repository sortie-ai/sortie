package clientprotocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// loadGoldenSummary reads one golden summary fixture from testdata.
func loadGoldenSummary(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gemini_qualification_summary", name))
	if err != nil {
		t.Fatalf("loadGoldenSummary(%q): %v", name, err)
	}
	return string(data)
}

// requireGoldenSummary fails t unless got equals the named golden
// fixture.
func requireGoldenSummary(t *testing.T, name string, got string) {
	t.Helper()
	want := loadGoldenSummary(t, name)
	if got != want {
		t.Errorf("formatGeminiQualificationSummary() does not match testdata/gemini_qualification_summary/%s\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// absentSurfaceFixtureRecords builds the qualified fixture's records
// with both structured native surfaces declared absent, reflecting
// what a live collector, which never probes a declared-absent surface
// at all, actually produces.
func absentSurfaceFixtureRecords(t *testing.T) *qualification.Fixture {
	t.Helper()

	fixture := qualification.NewFixture(qualification.FixtureQualified,
		qualification.AbsentSurface{Surface: qualification.SurfaceNativeJSON, Reason: qualification.SurfaceNotOffered},
		qualification.AbsentSurface{Surface: qualification.SurfaceNativeStreamJSON, Reason: qualification.SurfaceNotOffered},
	)
	fixture.Finalize()
	return fixture
}

// TestGeminiQualificationSummaryGolden binds the rendered summary to a
// golden file over two record sets: one with all four surfaces
// measured, one with both structured native surfaces declared absent.
// The second confirms the declared-absent section sits directly under
// the rationale line, names both surfaces, their reasons, and their
// corroboration, and states that every comparison row stands on the
// protocol surface alone; both confirm the rationale line itself is
// identical, since VerdictRationale cannot know a reference was absent.
func TestGeminiQualificationSummaryGolden(t *testing.T) {
	t.Parallel()

	fourSurfaceFixture := qualification.NewFixture(qualification.FixtureQualified)
	fourSurfaceFixture.Finalize()
	fourSurfaceVerdict := qualification.ComputeEligibility(fourSurfaceFixture.Records, fourSurfaceFixture.Declarations())
	fourSurfaceConclusions, err := geminiSummaryConclusionsFromRecords(fourSurfaceFixture.Records, fourSurfaceVerdict, fourSurfaceFixture.Declarations())
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}
	fourSurfaceSummary := formatGeminiQualificationSummary(fourSurfaceConclusions)
	requireGoldenSummary(t, "four_surfaces.golden", fourSurfaceSummary)

	absentFixture := absentSurfaceFixtureRecords(t)
	absentVerdict := qualification.ComputeEligibility(absentFixture.Records, absentFixture.Declarations())
	absentConclusions, err := geminiSummaryConclusionsFromRecords(absentFixture.Records, absentVerdict, absentFixture.Declarations())
	if err != nil {
		t.Fatalf("geminiSummaryConclusionsFromRecords() error = %v", err)
	}
	absentSummary := formatGeminiQualificationSummary(absentConclusions)
	requireGoldenSummary(t, "both_native_surfaces_absent.golden", absentSummary)

	fourSurfaceLines := strings.Split(fourSurfaceSummary, "\n")
	absentLines := strings.Split(absentSummary, "\n")
	if len(fourSurfaceLines) < 2 || len(absentLines) < 2 {
		t.Fatal("summary carries too few lines to check the rationale placement")
	}

	rationaleLine := fourSurfaceLines[1]
	if rationaleLine != qualification.VerdictRationale(fourSurfaceVerdict) {
		t.Fatalf("four-surface rationale line = %q, want %q", rationaleLine, qualification.VerdictRationale(fourSurfaceVerdict))
	}
	if absentLines[1] != rationaleLine {
		t.Errorf("absent-surface rationale line = %q, want it identical to the four-surface run's %q: VerdictRationale cannot know a reference was absent", absentLines[1], rationaleLine)
	}
	if absentLines[2] != "Declared-absent surfaces:" {
		t.Errorf("line directly under the rationale = %q, want the declared-absent section header", absentLines[2])
	}

	for _, want := range []string{
		"native_json: declared absent (surface_not_offered); corroborated",
		"native_stream_json: declared absent (surface_not_offered); corroborated",
		"no structured native surface was measured: every comparison row stands on the protocol surface alone",
	} {
		if !strings.Contains(absentSummary, want) {
			t.Errorf("absent-surface summary is missing the expected line %q", want)
		}
	}
	if strings.Contains(fourSurfaceSummary, "declared absent") {
		t.Error("the four-surface summary names a declared-absent surface, want none")
	}
}
