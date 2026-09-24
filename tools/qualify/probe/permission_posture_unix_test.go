//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

func TestGradePermissionLaunchSeparatesPermissionFromHumanInput(t *testing.T) {
	t.Parallel()

	permission, policy, humanInput := gradePermissionLaunch(permissionLaunchSignals{
		continuableNotice: true,
		sessionID:         "sess-1",
	})

	if permission.Grade != evidence.GradeUsable || permission.Outcome != evidence.OutcomePass {
		t.Errorf("permission = %s/%s, want usable/pass", permission.Grade, permission.Outcome)
	}
	if policy.Grade != evidence.GradeUsable {
		t.Errorf("policy grade = %s, want usable", policy.Grade)
	}
	if humanInput.Grade != evidence.GradeNotApplicable || humanInput.Outcome != evidence.OutcomeNotApplicable {
		t.Errorf("human input = %s/%s, want not_applicable/not_applicable", humanInput.Grade, humanInput.Outcome)
	}
	if humanInput.EvidencePath == "" {
		t.Error("a graded human-input observation carries no evidence path")
	}
	if humanInput.SessionID != "sess-1" {
		t.Errorf("human input session id = %q, want %q", humanInput.SessionID, "sess-1")
	}
}

func TestGradePermissionLaunchNoRefusingOption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		endedRequiringInput   bool
		wantPermissionGrade   evidence.Grade
		wantHumanInputGrade   evidence.Grade
		wantHumanInputOutcome evidence.Outcome
	}{
		{
			name:                  "attempt ended",
			endedRequiringInput:   true,
			wantPermissionGrade:   evidence.GradeUsable,
			wantHumanInputGrade:   evidence.GradeUsable,
			wantHumanInputOutcome: evidence.OutcomePass,
		},
		{
			name:                  "attempt stayed open",
			endedRequiringInput:   false,
			wantPermissionGrade:   evidence.GradeGap,
			wantHumanInputGrade:   evidence.GradeGap,
			wantHumanInputOutcome: evidence.OutcomePass,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			permission, _, humanInput := gradePermissionLaunch(permissionLaunchSignals{
				noOptionNotice:      true,
				endedRequiringInput: tc.endedRequiringInput,
				sessionID:           "sess-2",
			})
			if permission.Grade != tc.wantPermissionGrade {
				t.Errorf("permission grade = %s, want %s", permission.Grade, tc.wantPermissionGrade)
			}
			if humanInput.Grade != tc.wantHumanInputGrade || humanInput.Outcome != tc.wantHumanInputOutcome {
				t.Errorf("human input = %s/%s, want %s/%s", humanInput.Grade, humanInput.Outcome, tc.wantHumanInputGrade, tc.wantHumanInputOutcome)
			}
			if humanInput.EvidencePath == "" {
				t.Error("a graded human-input observation carries no evidence path")
			}
		})
	}
}

func TestGradePermissionLaunchHumanInputEnding(t *testing.T) {
	t.Parallel()

	_, _, humanInput := gradePermissionLaunch(permissionLaunchSignals{
		turnFailed:          true,
		endedRequiringInput: true,
		sessionID:           "sess-3",
	})
	if humanInput.Grade != evidence.GradeUsable || humanInput.Outcome != evidence.OutcomePass {
		t.Fatalf("human input = %s/%s, want usable/pass", humanInput.Grade, humanInput.Outcome)
	}
	if humanInput.EvidencePath == "" {
		t.Error("a graded human-input observation carries no evidence path")
	}
}

func TestGradePermissionLaunchAllRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		continuableNotice     bool
		noOptionNotice        bool
		turnFailed            bool
		endedRequiringInput   bool
		wantPermissionGrade   evidence.Grade
		wantPermissionOutcome evidence.Outcome
		wantPolicyGrade       evidence.Grade
		wantPolicyOutcome     evidence.Outcome
		wantHumanInputGrade   evidence.Grade
		wantHumanInputOutcome evidence.Outcome
		wantHumanInputDetail  string
		wantEvidencePath      bool
	}{
		{
			name:                  "no notice and no failure",
			wantPermissionGrade:   evidence.GradeNotObserved,
			wantPermissionOutcome: evidence.OutcomeFixtureInductionFailed,
			wantPolicyGrade:       evidence.GradeNotObserved,
			wantPolicyOutcome:     evidence.OutcomePrerequisiteFailed,
			wantHumanInputGrade:   evidence.GradeNotObserved,
			wantHumanInputOutcome: evidence.OutcomeFixtureInductionFailed,
			wantHumanInputDetail:  "no request of either class was raised under this posture",
		},
		{
			name:                  "no notice, turn failed without requiring input",
			turnFailed:            true,
			wantPermissionGrade:   evidence.GradeNotObserved,
			wantPermissionOutcome: evidence.OutcomeRuntimeFailed,
			wantPolicyGrade:       evidence.GradeNotObserved,
			wantPolicyOutcome:     evidence.OutcomePrerequisiteFailed,
			wantHumanInputGrade:   evidence.GradeNotObserved,
			wantHumanInputOutcome: evidence.OutcomeRuntimeFailed,
			wantHumanInputDetail:  "the turn did not complete, so no human-input case was induced",
		},
		{
			name:                  "no notice, turn failed requiring input",
			turnFailed:            true,
			endedRequiringInput:   true,
			wantPermissionGrade:   evidence.GradeNotObserved,
			wantPermissionOutcome: evidence.OutcomeRuntimeFailed,
			wantPolicyGrade:       evidence.GradeNotObserved,
			wantPolicyOutcome:     evidence.OutcomePrerequisiteFailed,
			wantHumanInputGrade:   evidence.GradeUsable,
			wantHumanInputOutcome: evidence.OutcomePass,
			wantHumanInputDetail:  "the turn ended requiring human input with no permission request to attribute it to",
			wantEvidencePath:      true,
		},
		{
			name:                  "no-option notice, turn did not fail",
			noOptionNotice:        true,
			wantPermissionGrade:   evidence.GradeGap,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeGap,
			wantHumanInputOutcome: evidence.OutcomePass,
			wantHumanInputDetail:  "the request offered no refusing option and the turn went on without ending requiring human input",
			wantEvidencePath:      true,
		},
		{
			name:                  "no-option notice, turn failed without requiring input",
			noOptionNotice:        true,
			turnFailed:            true,
			wantPermissionGrade:   evidence.GradeGap,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeNotObserved,
			wantHumanInputOutcome: evidence.OutcomeRuntimeFailed,
			wantHumanInputDetail:  "the turn did not complete, so no human-input case was induced",
		},
		{
			name:                  "no-option notice, turn failed requiring input",
			noOptionNotice:        true,
			turnFailed:            true,
			endedRequiringInput:   true,
			wantPermissionGrade:   evidence.GradeUsable,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeUsable,
			wantHumanInputOutcome: evidence.OutcomePass,
			wantHumanInputDetail:  "the request offered no refusing option and the attempt ended requiring human input",
			wantEvidencePath:      true,
		},
		{
			name:                  "continuable notice, turn did not fail",
			continuableNotice:     true,
			wantPermissionGrade:   evidence.GradeUsable,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeNotApplicable,
			wantHumanInputOutcome: evidence.OutcomeNotApplicable,
			wantHumanInputDetail:  "the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose",
			wantEvidencePath:      true,
		},
		{
			name:                  "continuable notice, turn failed without requiring input",
			continuableNotice:     true,
			turnFailed:            true,
			wantPermissionGrade:   evidence.GradeUsable,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeNotObserved,
			wantHumanInputOutcome: evidence.OutcomeRuntimeFailed,
			wantHumanInputDetail:  "the turn did not complete, so no human-input case was induced",
		},
		{
			name:                  "continuable notice, turn failed requiring input",
			continuableNotice:     true,
			turnFailed:            true,
			endedRequiringInput:   true,
			wantPermissionGrade:   evidence.GradeGap,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeUsable,
			wantHumanInputOutcome: evidence.OutcomePass,
			wantHumanInputDetail:  "the turn ended requiring human input with no permission request to attribute it to",
			wantEvidencePath:      true,
		},
		{
			name:                  "both notices, turn did not fail",
			continuableNotice:     true,
			noOptionNotice:        true,
			wantPermissionGrade:   evidence.GradeGap,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeGap,
			wantHumanInputOutcome: evidence.OutcomePass,
			wantHumanInputDetail:  "the request offered no refusing option and the turn went on without ending requiring human input",
			wantEvidencePath:      true,
		},
		{
			name:                  "both notices, turn failed without requiring input",
			continuableNotice:     true,
			noOptionNotice:        true,
			turnFailed:            true,
			wantPermissionGrade:   evidence.GradeGap,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeNotObserved,
			wantHumanInputOutcome: evidence.OutcomeRuntimeFailed,
			wantHumanInputDetail:  "the turn did not complete, so no human-input case was induced",
		},
		{
			name:                  "both notices, turn failed requiring input",
			continuableNotice:     true,
			noOptionNotice:        true,
			turnFailed:            true,
			endedRequiringInput:   true,
			wantPermissionGrade:   evidence.GradeUsable,
			wantPermissionOutcome: evidence.OutcomePass,
			wantPolicyGrade:       evidence.GradeUsable,
			wantPolicyOutcome:     evidence.OutcomePass,
			wantHumanInputGrade:   evidence.GradeUsable,
			wantHumanInputOutcome: evidence.OutcomePass,
			wantHumanInputDetail:  "the request offered no refusing option and the attempt ended requiring human input",
			wantEvidencePath:      true,
		},
	}

	seenRows := map[string]bool{}
	for _, tc := range tests {
		seenRows[tc.wantHumanInputDetail] = true
	}
	if len(seenRows) != 6 {
		t.Fatalf("the table's human-input details cover %d distinct rows, want 6", len(seenRows))
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const sessionID = "sess-table"
			permission, policy, humanInput := gradePermissionLaunch(permissionLaunchSignals{
				continuableNotice:   tc.continuableNotice,
				noOptionNotice:      tc.noOptionNotice,
				turnFailed:          tc.turnFailed,
				endedRequiringInput: tc.endedRequiringInput,
				sessionID:           sessionID,
			})

			if permission.Grade != tc.wantPermissionGrade || permission.Outcome != tc.wantPermissionOutcome {
				t.Errorf("permission = %s/%s, want %s/%s", permission.Grade, permission.Outcome, tc.wantPermissionGrade, tc.wantPermissionOutcome)
			}
			if policy.Grade != tc.wantPolicyGrade || policy.Outcome != tc.wantPolicyOutcome {
				t.Errorf("policy = %s/%s, want %s/%s", policy.Grade, policy.Outcome, tc.wantPolicyGrade, tc.wantPolicyOutcome)
			}
			if humanInput.Grade != tc.wantHumanInputGrade || humanInput.Outcome != tc.wantHumanInputOutcome {
				t.Errorf("human input = %s/%s, want %s/%s", humanInput.Grade, humanInput.Outcome, tc.wantHumanInputGrade, tc.wantHumanInputOutcome)
			}
			if humanInput.Detail != tc.wantHumanInputDetail {
				t.Errorf("human input detail = %q, want %q", humanInput.Detail, tc.wantHumanInputDetail)
			}
			if hasPath := humanInput.EvidencePath != ""; hasPath != tc.wantEvidencePath {
				t.Errorf("human input evidence path present = %v, want %v", hasPath, tc.wantEvidencePath)
			}
			if humanInput.SessionID != sessionID {
				t.Errorf("human input session id = %q, want %q", humanInput.SessionID, sessionID)
			}
		})
	}
}
