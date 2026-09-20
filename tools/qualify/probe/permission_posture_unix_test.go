//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

// A continuable refusal is the wanted posture, so it grades permission usable
// and leaves human-input unmeasured: this launch never asked a person.
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
	if humanInput.Grade == evidence.GradeGap {
		t.Errorf("human input graded gap on a continuable permission refusal; the refusal is the wanted posture, not a human-input failure")
	}
	if humanInput.Grade != evidence.GradeNotObserved || humanInput.Outcome != evidence.OutcomeFixtureInductionFailed {
		t.Errorf("human input = %s/%s, want not_observed/fixture_induction_failed", humanInput.Grade, humanInput.Outcome)
	}
	if humanInput.EvidencePath != "" {
		t.Errorf("unmeasured human input carries evidence path %q, want none", humanInput.EvidencePath)
	}
}

// When the request offers nothing to refuse with, ending the attempt is the
// pass, not a shortfall.
func TestGradePermissionLaunchNoRefusingOption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		endedRequiringInput bool
		wantGrade           evidence.Grade
	}{
		{"attempt ended", true, evidence.GradeUsable},
		{"attempt stayed open", false, evidence.GradeGap},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			permission, _, humanInput := gradePermissionLaunch(permissionLaunchSignals{
				noOptionNotice:      true,
				endedRequiringInput: tc.endedRequiringInput,
				sessionID:           "sess-2",
			})
			if permission.Grade != tc.wantGrade {
				t.Errorf("permission grade = %s, want %s", permission.Grade, tc.wantGrade)
			}
			if humanInput.Grade == evidence.GradeUsable {
				t.Errorf("human input graded usable off a permission request; sound permission handling does not measure human-input handling")
			}
		})
	}
}

// The one shape that measures human input: an attempt that ended requiring
// input with no permission request to attribute the ending to.
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
