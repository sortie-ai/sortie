package qualification_test

import (
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
	"github.com/sortie-ai/sortie/internal/qualification/probe"
)

func notObservedProfile(absent []qualification.AbsentSurface) qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			qualification.SurfaceProtocol:         {},
			qualification.SurfaceNativeJSON:       {},
			qualification.SurfaceNativeStreamJSON: {},
		},
		AbsentSurfaces: absent,
	}
}

func TestFixtureNotObservedVariant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		absent []qualification.AbsentSurface
	}{
		{name: "no absent surface"},
		{
			name: "native_json absent",
			absent: []qualification.AbsentSurface{
				{Surface: qualification.SurfaceNativeJSON, Reason: qualification.SurfaceNotOffered},
			},
		},
		{
			name: "native_stream_json absent",
			absent: []qualification.AbsentSurface{
				{Surface: qualification.SurfaceNativeStreamJSON, Reason: qualification.SurfaceNotOffered},
			},
		},
		{
			name: "both native surfaces absent",
			absent: []qualification.AbsentSurface{
				{Surface: qualification.SurfaceNativeJSON, Reason: qualification.SurfaceNotOffered},
				{Surface: qualification.SurfaceNativeStreamJSON, Reason: qualification.SurfaceNotOffered},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := qualification.NewFixture(qualification.FixtureNotObserved, tt.absent...)
			fixture.Finalize()

			for i, rec := range fixture.Records {
				if rec.Grade != qualification.GradeNotObserved {
					t.Errorf("record %d Grade = %s, want %s", i, rec.Grade, qualification.GradeNotObserved)
				}
				if rec.Outcome != qualification.OutcomeNotObserved {
					t.Errorf("record %d Outcome = %s, want %s", i, rec.Outcome, qualification.OutcomeNotObserved)
				}
			}

			declarations := fixture.Declarations()
			if len(declarations.Declarations) != 0 {
				t.Errorf("Declarations().Declarations = %v, want none", declarations.Declarations)
			}
			if !slices.Equal(declarations.AbsentSurfaces, tt.absent) {
				t.Errorf("Declarations().AbsentSurfaces = %v, want %v", declarations.AbsentSurfaces, tt.absent)
			}

			path := qualification.WriteEvidenceFile(t, fixture.Records)
			verdict, err := qualification.ValidateObservationsWithDeclarations(path, declarations)
			if err != nil {
				t.Fatalf("ValidateObservationsWithDeclarations(...) error = %v, want nil", err)
			}
			if verdict != qualification.VerdictUnmeasured {
				t.Errorf("ValidateObservationsWithDeclarations(...) = %s, want %s", verdict, qualification.VerdictUnmeasured)
			}

			profile := notObservedProfile(tt.absent)
			conclusions, err := probe.ConclusionsFromRecords(fixture.Records, verdict, profile)
			if err != nil {
				t.Fatalf("ConclusionsFromRecords(...) error = %v, want nil", err)
			}
			expectation := probe.ExpectationFrom(conclusions)

			measured := profile.MeasuredSurfaces()
			wantGrades := len(measured)*len(qualification.ComparisonCapabilities) + 2
			if len(expectation.Grades) != wantGrades {
				t.Fatalf("Grades = %d rows, want %d (%d baseline capabilities per measured surface plus tool server delivery and permission handling)",
					len(expectation.Grades), wantGrades, len(qualification.ComparisonCapabilities))
			}

			wantLabel := qualification.StatusLabel(qualification.GradeNotObserved)
			for _, grade := range expectation.Grades {
				if grade.Grade != qualification.GradeNotObserved {
					t.Errorf("Grades row %s %s = %s, want %s", grade.Surface, grade.Capability, grade.Grade, qualification.GradeNotObserved)
				}
				if grade.Label != wantLabel {
					t.Errorf("Grades row %s %s label = %q, want %q", grade.Surface, grade.Capability, grade.Label, wantLabel)
				}
			}
			if len(expectation.Excluded) != 0 {
				t.Errorf("Excluded = %v, want none", expectation.Excluded)
			}

			wantUnobserved := len(measured) * (len(qualification.CapabilityCases[qualification.CapabilityTurnDisposition]) + len(qualification.CapabilityCases[qualification.CapabilityRetryClassification]))
			if len(expectation.Unobserved) != wantUnobserved {
				t.Errorf("Unobserved = %d entries, want %d (one per semantic case per measured surface)", len(expectation.Unobserved), wantUnobserved)
			}
		})
	}
}
