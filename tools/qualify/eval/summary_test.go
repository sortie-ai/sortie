package eval

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func loadSummaryGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "summary", name))
	if err != nil {
		t.Fatalf("os.ReadFile(%s): %v", name, err)
	}
	return string(data)
}

func threeSurfaceProfile() profile.RuntimeProfile {
	return profile.RuntimeProfile{
		EntryPoints: map[evidence.Surface]profile.EntryPoint{
			evidence.SurfaceProtocol:         {},
			evidence.SurfaceNativeJSON:       {},
			evidence.SurfaceNativeStreamJSON: {},
		},
	}
}

func bothNativeSurfacesAbsentProfile() profile.RuntimeProfile {
	return profile.RuntimeProfile{
		EntryPoints: map[evidence.Surface]profile.EntryPoint{
			evidence.SurfaceProtocol: {},
		},
		AbsentSurfaces: []profile.AbsentSurface{
			{Surface: evidence.SurfaceNativeJSON, Reason: evidence.SurfaceNotOffered},
			{Surface: evidence.SurfaceNativeStreamJSON, Reason: evidence.SurfaceNotOffered},
		},
	}
}

func firstDiffLine(got, want string) int {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			return i + 1
		}
	}
	return 0
}

func TestFormatSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		profile profile.RuntimeProfile
		absent  []profile.AbsentSurface
		golden  string
	}{
		{
			name:    "three surfaces measured",
			profile: threeSurfaceProfile(),
			golden:  "three_surfaces.golden",
		},
		{
			name:    "both native surfaces declared absent",
			profile: bothNativeSurfacesAbsentProfile(),
			absent:  bothNativeSurfacesAbsentProfile().AbsentSurfaces,
			golden:  "both_native_surfaces_absent.golden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureQualified, tt.absent...)
			fixture.Finalize()

			verdict := ComputeEligibility(fixture.Records, tt.profile)
			conclusions, err := conclusionsFromRecords(fixture.Records, verdict, tt.profile)
			if err != nil {
				t.Fatalf("conclusionsFromRecords(...) = _, %v, want nil", err)
			}

			got := formatSummary(conclusions, false)
			want := loadSummaryGolden(t, tt.golden)
			if got != want {
				t.Errorf("formatSummary(...) diverges from %s at line %d\ngot:\n%s\nwant:\n%s", tt.golden, firstDiffLine(got, want), got, want)
			}
		})
	}
}

func TestConclusionsFromRecordsErrors(t *testing.T) {
	t.Parallel()

	t.Run("a record outside the closed table fails classification", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.Records[0].Scenario = "not_a_real_scenario"

		_, err := conclusionsFromRecords(fixture.Records, evidence.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("conclusionsFromRecords(...) = _, nil, want an error for a record outside the closed table")
		}
	})

	t.Run("fewer capability grades than the profile requires fails", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(evidence.MatchBaseline(evidence.SurfaceNativeStreamJSON, evidence.CapabilityTurnDisposition))

		_, err := conclusionsFromRecords(fixture.Records, evidence.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("conclusionsFromRecords(...) = _, nil, want an error for a missing capability grade")
		}
	})

	t.Run("fewer semantic verdicts than the profile requires fails", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseSuccess))

		_, err := conclusionsFromRecords(fixture.Records, evidence.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("conclusionsFromRecords(...) = _, nil, want an error for a missing semantic verdict")
		}
	})

	t.Run("fewer continuation outcomes than the profile requires fails", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))

		_, err := conclusionsFromRecords(fixture.Records, evidence.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("conclusionsFromRecords(...) = _, nil, want an error for a missing continuation outcome")
		}
	})

	t.Run("no workspace security record fails", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(func(rec *evidence.Record) bool {
			return rec.Capability == evidence.CapabilityWorkspaceSecurity
		})

		_, err := conclusionsFromRecords(fixture.Records, evidence.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("conclusionsFromRecords(...) = _, nil, want an error for no workspace security record")
		}
	})
}

func TestExpectationFrom(t *testing.T) {
	t.Parallel()

	conclusions := Conclusions{
		Verdict: evidence.VerdictNotQualified,
		Grades: []summaryGrade{
			{
				Surface:    evidence.SurfaceProtocol,
				Capability: evidence.CapabilityTurnDisposition,
				Grade:      evidence.GradeUsable,
				Label:      "Observed:",
			},
		},
		Excluded:   []string{"turn_disposition runtime_refusal: declared outcome_never_produced"},
		Unobserved: []string{"protocol retry_classification human_input: not_observed"},
		Workspace:  "Observed: ok",
	}

	got := expectationFrom(conclusions)
	want := evidence.NotesExpectation{
		Verdict: evidence.VerdictNotQualified,
		Grades: []evidence.NotesGrade{
			{
				Surface:    evidence.SurfaceProtocol,
				Capability: evidence.CapabilityTurnDisposition,
				Grade:      evidence.GradeUsable,
				Label:      "Observed:",
			},
		},
		Excluded:   []string{"turn_disposition runtime_refusal: declared outcome_never_produced"},
		Unobserved: []string{"protocol retry_classification human_input: not_observed"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("expectationFrom(%+v) = %+v, want %+v", conclusions, got, want)
	}
}

func TestConclusionsFromRecordsExclusionAndBlockingBranches(t *testing.T) {
	t.Parallel()

	t.Run("a not-observed semantic case is unobserved and leaves its row unmeasured", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureUnmeasured)
		fixture.Finalize()
		p := threeSurfaceProfile()

		verdict := ComputeEligibility(fixture.Records, p)
		if verdict != evidence.VerdictUnmeasured {
			t.Fatalf("ComputeEligibility(...) = %s, want %s", verdict, evidence.VerdictUnmeasured)
		}

		conclusions, err := conclusionsFromRecords(fixture.Records, verdict, p)
		if err != nil {
			t.Fatalf("conclusionsFromRecords(...) = _, %v, want nil", err)
		}

		wantUnobserved := "protocol turn_disposition runtime_refusal: not_observed"
		if !slices.Contains(conclusions.Unobserved, wantUnobserved) {
			t.Errorf("Unobserved = %v, want it to contain %q", conclusions.Unobserved, wantUnobserved)
		}
		if len(conclusions.UnmeasuredRows) == 0 {
			t.Error("UnmeasuredRows is empty, want at least one row for the unobserved case")
		}

		if summary := formatSummary(conclusions, false); !strings.Contains(summary, wantUnobserved) {
			t.Errorf("formatSummary(...) does not contain %q:\n%s", wantUnobserved, summary)
		}
	})

	t.Run("a declared-gap semantic case is excluded", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureDeclaredGap)
		fixture.Finalize()
		p := threeSurfaceProfile()

		verdict := ComputeEligibility(fixture.Records, p)
		conclusions, err := conclusionsFromRecords(fixture.Records, verdict, p)
		if err != nil {
			t.Fatalf("conclusionsFromRecords(...) = _, %v, want nil", err)
		}

		wantExcluded := "turn_disposition runtime_refusal: declared " + evidence.DeclaredGapNeverProduced
		if !slices.Contains(conclusions.Excluded, wantExcluded) {
			t.Errorf("Excluded = %v, want it to contain %q", conclusions.Excluded, wantExcluded)
		}

		if summary := formatSummary(conclusions, false); !strings.Contains(summary, wantExcluded) {
			t.Errorf("formatSummary(...) does not contain %q:\n%s", wantExcluded, summary)
		}
	})

	t.Run("a not-inducible semantic case is excluded", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.SetSemanticNotInducible(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome, evidence.NotInducibleDetail)

		p := threeSurfaceProfile()
		conclusions, err := conclusionsFromRecords(fixture.Records, evidence.VerdictQualified, p)
		if err != nil {
			t.Fatalf("conclusionsFromRecords(...) = _, %v, want nil", err)
		}

		wantExcluded := "retry_classification unknown_outcome: no deterministic inducer, so neither the condition nor the surface's account of it was established"
		if !slices.Contains(conclusions.Excluded, wantExcluded) {
			t.Errorf("Excluded = %v, want it to contain %q", conclusions.Excluded, wantExcluded)
		}
	})

	t.Run("a conflated protocol disposition below its native reference is blocking", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureNotQualified)
		fixture.Finalize()
		p := threeSurfaceProfile()

		verdict := ComputeEligibility(fixture.Records, p)
		if verdict != evidence.VerdictNotQualified {
			t.Fatalf("ComputeEligibility(...) = %s, want %s", verdict, evidence.VerdictNotQualified)
		}

		conclusions, err := conclusionsFromRecords(fixture.Records, verdict, p)
		if err != nil {
			t.Fatalf("conclusionsFromRecords(...) = _, %v, want nil", err)
		}
		if len(conclusions.Blocking) == 0 {
			t.Fatal("Blocking is empty, want at least one blocking row for the conflated protocol disposition")
		}

		if summary := formatSummary(conclusions, false); !strings.Contains(summary, conclusions.Blocking[0]) {
			t.Errorf("formatSummary(...) does not contain the blocking row %q:\n%s", conclusions.Blocking[0], summary)
		}
	})
}
