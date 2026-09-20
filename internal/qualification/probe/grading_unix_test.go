//go:build unix

package probe

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/qualification"
)

var inducedGrades = []qualification.Grade{qualification.GradeUsable, qualification.GradeGap, qualification.GradeNotObserved}

func outcomeForSweptGrade(grade qualification.Grade) qualification.Outcome {
	if grade == qualification.GradeNotObserved {
		return qualification.OutcomeFixtureInductionFailed
	}
	return qualification.OutcomePass
}

func fullyObservedCollected(profile qualification.RuntimeProfile, toolGrade, permissionGrade, continuationGrade qualification.Grade) collectedObservations {
	measured := profile.MeasuredSurfaces()

	declared := map[[2]string]bool{}
	for _, d := range profile.Declarations {
		declared[[2]string{string(d.Capability), string(d.Case)}] = true
	}
	notInducible := map[[2]string]bool{}
	for _, entry := range profile.NotInducibleCases {
		notInducible[[2]string{string(entry.Surface), string(entry.Case)}] = true
	}

	collected := collectedObservations{
		semantic: map[qualification.Surface]map[qualification.Case]qualification.Observation{},
	}

	permissionSessionID := "sess-protocol-permission"

	for _, surface := range measured {
		byCase := map[qualification.Case]qualification.Observation{}
		for _, capability := range []qualification.Capability{qualification.CapabilityTurnDisposition, qualification.CapabilityRetryClassification} {
			for _, caseID := range qualification.CapabilityCases[capability] {
				if slices.Contains(qualification.CatalogNotInducibleCases, caseID) || notInducible[[2]string{string(surface), string(caseID)}] {
					continue
				}
				// The refusal disposition and its non_retryable_refusal retry
				// peer derive from one physical run, so both share one session
				// id per surface; protocol human_input reuses the permission
				// attempt's session.
				sessionKey := string(caseID)
				if _, hasPeer := qualification.DeclaredGapPeers[caseID]; hasPeer {
					sessionKey = "refusal-pair"
				}
				sessionID := fmt.Sprintf("sess-%s-%s", surface, sessionKey)
				if surface == qualification.SurfaceProtocol && caseID == qualification.CaseHumanInput {
					sessionID = permissionSessionID
				}
				if declared[[2]string{string(capability), string(caseID)}] {
					byCase[caseID] = qualification.Observation{Grade: qualification.GradeNotObserved, Outcome: qualification.OutcomeFixtureInductionFailed, Detail: "declared", SessionID: sessionID}
					continue
				}
				byCase[caseID] = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "migrated fixture observation", SessionID: sessionID, EvidencePath: "/turn/stop_reason"}
			}
		}
		collected.semantic[surface] = byCase
	}

	collected.toolServer = qualification.Observation{Grade: toolGrade, Outcome: outcomeForSweptGrade(toolGrade), Detail: "tool server induction: " + string(toolGrade), SessionID: "sess-protocol-mcp"}
	collected.permission = qualification.Observation{Grade: permissionGrade, Outcome: outcomeForSweptGrade(permissionGrade), Detail: "permission induction: " + string(permissionGrade), SessionID: permissionSessionID}
	collected.policy = qualification.Observation{Grade: qualification.GradeUsable, Outcome: qualification.OutcomePass, Detail: "policy precondition", SessionID: "sess-protocol-policy"}

	collected.continuationGrade = continuationGrade
	collected.continuationDetail = "continuation induction: " + string(continuationGrade)

	return collected
}

func gradeOfClass(t *testing.T, records []qualification.Record, class qualification.RowClass) qualification.Grade {
	t.Helper()
	for i := range records {
		got, err := qualification.ClassifyRecord(&records[i])
		if err != nil {
			t.Fatalf("ClassifyRecord(%+v) error = %v, want nil", records[i], err)
		}
		if got == class {
			return records[i].Grade
		}
	}
	return ""
}

func continuationBaselineGrade(records []qualification.Record) qualification.Grade {
	for i := range records {
		rec := &records[i]
		if rec.Scenario == qualification.ScenarioSurfaceBaseline && rec.Surface == qualification.SurfaceProtocol && rec.Capability == qualification.CapabilitySessionContinuation {
			return rec.Grade
		}
	}
	return ""
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

							collected := fullyObservedCollected(profile, toolGrade, permissionGrade, continuationGrade)
							fixture, err := gradedEvidence(profile, collected, time.Now().UTC())
							if err != nil {
								t.Fatalf("gradedEvidence(...) error = %v, want nil", err)
							}

							path := qualification.WriteEvidenceFile(t, fixture.Records)
							if _, err := qualification.ValidateObservationsWithDeclarations(path, profile); err != nil {
								t.Fatalf("ValidateObservationsWithDeclarations(...) error = %v, want nil", err)
							}

							if got := gradeOfClass(t, fixture.Records, qualification.RowMCPDelivery); got != toolGrade {
								t.Errorf("tool server delivery grade = %s, want %s", got, toolGrade)
							}
							if got := gradeOfClass(t, fixture.Records, qualification.RowPermission); got != permissionGrade {
								t.Errorf("permission handling grade = %s, want %s", got, permissionGrade)
							}
							if got := continuationBaselineGrade(fixture.Records); got != continuationGrade {
								t.Errorf("protocol continuation baseline grade = %s, want %s", got, continuationGrade)
							}
						})
					}
				}
			}
		})
	}
}
