package storage

import "testing"

func TestRunHistoryCapabilities_Breakdowns(t *testing.T) {
	t.Parallel()

	full := RunHistoryCapabilities{HasTurnsCompleted: true, HasReviewMetadata: true, HasRuleRouting: true, HasTokens: true, HasTokenMeasurement: true}
	withSettings := full
	withSettings.HasConfiguredSettings = true
	withChains := withSettings
	withChains.HasStageChains = true
	withTrigger := withChains
	withTrigger.HasTrigger = true
	chainsWithoutSettings := full
	chainsWithoutSettings.HasStageChains = true
	recorded := withTrigger
	recorded.HasFirstAfterHop, recorded.HasAgentBlocked, recorded.HasRecordedCost = true, true, true
	withSkills := recorded
	withSkills.HasSkills = true
	skillsWithoutCost := withTrigger
	skillsWithoutCost.HasSkills = true
	tests := []struct {
		name        string
		caps        RunHistoryCapabilities
		wantModel   bool
		wantChain   bool
		wantTrigger bool
		wantSkills  bool
	}{
		{"recorded cost and the skills column", withSkills, true, true, true, true},
		{"recorded cost without the skills column", recorded, true, true, true, false},
		{"skills column without recorded cost", skillsWithoutCost, true, true, true, false},
		{"full tier with settings, stage chains and trigger", withTrigger, true, true, true, false},
		{"full tier with configured settings and stage chains", withChains, true, true, false, false},
		{"full tier with configured settings", withSettings, true, false, false, false},
		{"full tier without configured settings", full, false, false, false, false},
		{"stage chains without configured settings", chainsWithoutSettings, false, false, false, false},
		{"trigger without stage chains", RunHistoryCapabilities{HasTrigger: true}, false, false, false, false},
		{"configured settings without the full tier", RunHistoryCapabilities{HasConfiguredSettings: true, HasStageChains: true, HasTrigger: true}, false, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.caps.ModelBreakdown(); got != tt.wantModel {
				t.Errorf("%+v.ModelBreakdown() = %v, want %v", tt.caps, got, tt.wantModel)
			}
			if got := tt.caps.ChainBreakdown(); got != tt.wantChain {
				t.Errorf("%+v.ChainBreakdown() = %v, want %v", tt.caps, got, tt.wantChain)
			}
			if got := tt.caps.TriggerBreakdown(); got != tt.wantTrigger {
				t.Errorf("%+v.TriggerBreakdown() = %v, want %v", tt.caps, got, tt.wantTrigger)
			}
			if got := tt.caps.SkillBreakdown(); got != tt.wantSkills {
				t.Errorf("%+v.SkillBreakdown() = %v, want %v", tt.caps, got, tt.wantSkills)
			}
		})
	}
}
