package probe

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

func loadSummaryGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "summary", name))
	if err != nil {
		t.Fatalf("os.ReadFile(%s): %v", name, err)
	}
	return string(data)
}

func threeSurfaceProfile() qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			qualification.SurfaceProtocol:         {},
			qualification.SurfaceNativeJSON:       {},
			qualification.SurfaceNativeStreamJSON: {},
		},
	}
}

func bothNativeSurfacesAbsentProfile() qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			qualification.SurfaceProtocol: {},
		},
		AbsentSurfaces: []qualification.AbsentSurface{
			{Surface: qualification.SurfaceNativeJSON, Reason: qualification.SurfaceNotOffered},
			{Surface: qualification.SurfaceNativeStreamJSON, Reason: qualification.SurfaceNotOffered},
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
		profile qualification.RuntimeProfile
		absent  []qualification.AbsentSurface
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

			fixture := qualification.NewFixture(qualification.FixtureQualified, tt.absent...)
			fixture.Finalize()

			verdict := qualification.ComputeEligibility(fixture.Records, tt.profile)
			conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, tt.profile)
			if err != nil {
				t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
			}

			got := FormatSummary(conclusions)
			want := loadSummaryGolden(t, tt.golden)
			if got != want {
				t.Errorf("FormatSummary(...) diverges from %s at line %d\ngot:\n%s\nwant:\n%s", tt.golden, firstDiffLine(got, want), got, want)
			}
		})
	}
}

func TestConclusionsFromRecordsErrors(t *testing.T) {
	t.Parallel()

	t.Run("a record outside the closed table fails classification", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		fixture.Finalize()
		fixture.Records[0].Scenario = "not_a_real_scenario"

		_, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("ConclusionsFromRecords(...) = _, nil, want an error for a record outside the closed table")
		}
	})

	t.Run("fewer capability grades than the profile requires fails", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(qualification.MatchBaseline(qualification.SurfaceNativeStreamJSON, qualification.CapabilityTurnDisposition))

		_, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("ConclusionsFromRecords(...) = _, nil, want an error for a missing capability grade")
		}
	})

	t.Run("fewer semantic verdicts than the profile requires fails", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(qualification.MatchSemantic(qualification.SurfaceProtocol, qualification.CapabilityTurnDisposition, qualification.CaseSuccess))

		_, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("ConclusionsFromRecords(...) = _, nil, want an error for a missing semantic verdict")
		}
	})

	t.Run("fewer continuation outcomes than the profile requires fails", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(qualification.MatchContinuation(qualification.SurfaceProtocol, qualification.InputContinuationRecall))

		_, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("ConclusionsFromRecords(...) = _, nil, want an error for a missing continuation outcome")
		}
	})

	t.Run("no workspace security record fails", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		fixture.Finalize()
		fixture.RemoveAll(func(rec *qualification.Record) bool {
			return rec.Capability == qualification.CapabilityWorkspaceSecurity
		})

		_, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, threeSurfaceProfile())
		if err == nil {
			t.Fatal("ConclusionsFromRecords(...) = _, nil, want an error for no workspace security record")
		}
	})
}

func TestExpectationFrom(t *testing.T) {
	t.Parallel()

	conclusions := Conclusions{
		Verdict: qualification.VerdictNotQualified,
		Grades: []summaryGrade{
			{
				Surface:    qualification.SurfaceProtocol,
				Capability: qualification.CapabilityTurnDisposition,
				Grade:      qualification.GradeUsable,
				Label:      "Observed:",
			},
		},
		Excluded:   []string{"turn_disposition runtime_refusal: declared outcome_never_produced"},
		Unobserved: []string{"protocol retry_classification human_input: not_observed"},
		Workspace:  "Observed: ok",
	}

	got := ExpectationFrom(conclusions)
	want := qualification.NotesExpectation{
		Verdict: qualification.VerdictNotQualified,
		Grades: []qualification.NotesGrade{
			{
				Surface:    qualification.SurfaceProtocol,
				Capability: qualification.CapabilityTurnDisposition,
				Grade:      qualification.GradeUsable,
				Label:      "Observed:",
			},
		},
		Excluded:   []string{"turn_disposition runtime_refusal: declared outcome_never_produced"},
		Unobserved: []string{"protocol retry_classification human_input: not_observed"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpectationFrom(%+v) = %+v, want %+v", conclusions, got, want)
	}
}

func TestConclusionsFromRecordsExclusionAndBlockingBranches(t *testing.T) {
	t.Parallel()

	t.Run("a not-observed semantic case is unobserved and leaves its row unmeasured", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureUnmeasured)
		fixture.Finalize()
		profile := threeSurfaceProfile()

		verdict := qualification.ComputeEligibility(fixture.Records, profile)
		if verdict != qualification.VerdictUnmeasured {
			t.Fatalf("ComputeEligibility(...) = %s, want %s", verdict, qualification.VerdictUnmeasured)
		}

		conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, profile)
		if err != nil {
			t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
		}

		wantUnobserved := "protocol turn_disposition runtime_refusal: not_observed"
		if !slices.Contains(conclusions.Unobserved, wantUnobserved) {
			t.Errorf("Unobserved = %v, want it to contain %q", conclusions.Unobserved, wantUnobserved)
		}
		if len(conclusions.UnmeasuredRows) == 0 {
			t.Error("UnmeasuredRows is empty, want at least one row for the unobserved case")
		}

		if summary := FormatSummary(conclusions); !strings.Contains(summary, wantUnobserved) {
			t.Errorf("FormatSummary(...) does not contain %q:\n%s", wantUnobserved, summary)
		}
	})

	t.Run("a declared-gap semantic case is excluded", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureDeclaredGap)
		fixture.Finalize()
		profile := threeSurfaceProfile()

		verdict := qualification.ComputeEligibility(fixture.Records, profile)
		conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, profile)
		if err != nil {
			t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
		}

		wantExcluded := "turn_disposition runtime_refusal: declared " + qualification.DeclaredGapNeverProduced
		if !slices.Contains(conclusions.Excluded, wantExcluded) {
			t.Errorf("Excluded = %v, want it to contain %q", conclusions.Excluded, wantExcluded)
		}

		if summary := FormatSummary(conclusions); !strings.Contains(summary, wantExcluded) {
			t.Errorf("FormatSummary(...) does not contain %q:\n%s", wantExcluded, summary)
		}
	})

	t.Run("a not-inducible semantic case is excluded", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureQualified)
		fixture.Finalize()
		fixture.SetSemanticNotInducible(qualification.SurfaceProtocol, qualification.CapabilityRetryClassification, qualification.CaseUnknownOutcome, qualification.NotInducibleDetail)

		profile := threeSurfaceProfile()
		conclusions, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, profile)
		if err != nil {
			t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
		}

		wantExcluded := "retry_classification unknown_outcome: no deterministic inducer, so neither the condition nor the surface's account of it was established"
		if !slices.Contains(conclusions.Excluded, wantExcluded) {
			t.Errorf("Excluded = %v, want it to contain %q", conclusions.Excluded, wantExcluded)
		}
	})

	t.Run("a conflated protocol disposition below its native reference is blocking", func(t *testing.T) {
		t.Parallel()

		fixture := qualification.NewFixture(qualification.FixtureNotQualified)
		fixture.Finalize()
		profile := threeSurfaceProfile()

		verdict := qualification.ComputeEligibility(fixture.Records, profile)
		if verdict != qualification.VerdictNotQualified {
			t.Fatalf("ComputeEligibility(...) = %s, want %s", verdict, qualification.VerdictNotQualified)
		}

		conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, profile)
		if err != nil {
			t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
		}
		if len(conclusions.Blocking) == 0 {
			t.Fatal("Blocking is empty, want at least one blocking row for the conflated protocol disposition")
		}

		if summary := FormatSummary(conclusions); !strings.Contains(summary, conclusions.Blocking[0]) {
			t.Errorf("FormatSummary(...) does not contain the blocking row %q:\n%s", conclusions.Blocking[0], summary)
		}
	})
}
