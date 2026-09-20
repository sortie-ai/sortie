//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

var inducedGrades = []qualification.Grade{qualification.GradeUsable, qualification.GradeGap, qualification.GradeNotObserved}

// wantInducedGrade returns the grade a gradedEvidence output's row for
// surface and capability must carry: the corresponding inducer's own
// grade for one of the three rows Run's inducers drive, and
// not_observed for every other row.
func wantInducedGrade(surface qualification.Surface, capability qualification.Capability, toolGrade, permissionGrade, continuationGrade qualification.Grade) qualification.Grade {
	if surface != qualification.SurfaceProtocol {
		return qualification.GradeNotObserved
	}
	switch capability {
	case qualification.CapabilityToolServerDelivery:
		return toolGrade
	case qualification.CapabilityPermissionHandling:
		return permissionGrade
	case qualification.CapabilitySessionContinuation:
		return continuationGrade
	default:
		return qualification.GradeNotObserved
	}
}

func TestGradedEvidenceValidatesAgainstEveryProfile(t *testing.T) {
	t.Parallel()

	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Fatalf("ReadRuntimeProfileFile(%s) error = %v, want nil", profilePath, err)
		}

		t.Run(profilePath, func(t *testing.T) {
			t.Parallel()

			for _, toolGrade := range inducedGrades {
				for _, permissionGrade := range inducedGrades {
					for _, continuationGrade := range inducedGrades {
						name := string(toolGrade) + "_" + string(permissionGrade) + "_" + string(continuationGrade)
						t.Run(name, func(t *testing.T) {
							t.Parallel()

							fixture, err := gradedEvidence(profile,
								inducedRow{grade: toolGrade, detail: "tool server induction: " + string(toolGrade)},
								inducedRow{grade: permissionGrade, detail: "permission induction: " + string(permissionGrade)},
								inducedRow{grade: continuationGrade, detail: "continuation induction: " + string(continuationGrade)},
							)
							if err != nil {
								t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
							}

							path := qualification.WriteEvidenceFile(t, fixture.Records)
							verdict, err := qualification.ValidateObservationsWithDeclarations(path, profile)
							if err != nil {
								t.Fatalf("ValidateObservationsWithDeclarations(...) error = %v, want nil", err)
							}

							conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, profile)
							if err != nil {
								t.Fatalf("ConclusionsFromRecords(...) error = %v, want nil", err)
							}
							expectation := ExpectationFrom(conclusions)

							measured := profile.MeasuredSurfaces()
							wantGrades := len(measured)*len(qualification.ComparisonCapabilities) + 2
							if len(expectation.Grades) != wantGrades {
								t.Fatalf("Grades = %d rows, want %d (%d baseline capabilities per measured surface plus tool server delivery and permission handling)",
									len(expectation.Grades), wantGrades, len(qualification.ComparisonCapabilities))
							}

							for _, grade := range expectation.Grades {
								want := wantInducedGrade(grade.Surface, grade.Capability, toolGrade, permissionGrade, continuationGrade)
								if grade.Grade != want {
									t.Errorf("Grades row %s %s = %s, want %s", grade.Surface, grade.Capability, grade.Grade, want)
								}
								wantLabel := qualification.StatusLabel(want)
								if grade.Label != wantLabel {
									t.Errorf("Grades row %s %s label = %q, want %q", grade.Surface, grade.Capability, grade.Label, wantLabel)
								}
							}
						})
					}
				}
			}
		})
	}
}

// gradedEvidenceRewrittenRow reports whether rec is one of the five
// protocol records gradedEvidence's three inducer calls rewrite: tool
// server delivery, permission handling, and the continuation baseline,
// recall, and seed.
func gradedEvidenceRewrittenRow(t *testing.T, rec *qualification.Record) bool {
	t.Helper()

	if rec.Surface != qualification.SurfaceProtocol {
		return false
	}
	class, err := qualification.ClassifyRecord(rec)
	if err != nil {
		t.Fatalf("ClassifyRecord(%+v) error = %v, want nil", *rec, err)
	}
	switch class {
	case qualification.RowMCPDelivery, qualification.RowPermission, qualification.RowContinuationRecall, qualification.RowContinuationSeed:
		return true
	case qualification.RowBaseline:
		return rec.Capability == qualification.CapabilitySessionContinuation
	default:
		return false
	}
}

// TestGradedEvidenceOnlyRewritesTheThreeProtocolRows confirms that in
// a gradedEvidence output, every record whose grade is neither
// not_observed nor declared_gap is one of the five protocol records
// its three inducer calls rewrite.
func TestGradedEvidenceOnlyRewritesTheThreeProtocolRows(t *testing.T) {
	t.Parallel()

	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Fatalf("ReadRuntimeProfileFile(%s) error = %v, want nil", profilePath, err)
		}

		t.Run(profilePath, func(t *testing.T) {
			t.Parallel()

			fixture, err := gradedEvidence(profile,
				inducedRow{grade: qualification.GradeUsable, detail: "tool server induction"},
				inducedRow{grade: qualification.GradeGap, detail: "permission induction"},
				inducedRow{grade: qualification.GradeUsable, detail: "continuation induction"},
			)
			if err != nil {
				t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
			}

			for i := range fixture.Records {
				rec := &fixture.Records[i]
				if rec.Grade == qualification.GradeNotObserved || rec.Grade == qualification.GradeDeclaredGap {
					continue
				}
				if !gradedEvidenceRewrittenRow(t, rec) {
					t.Errorf("record %d carries grade %s on %s %s, want one of the five protocol rows gradedEvidence rewrites", rec.Sequence, rec.Grade, rec.Surface, rec.Capability)
				}
			}
		})
	}
}
