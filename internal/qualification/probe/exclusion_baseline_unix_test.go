//go:build unix

package probe

import (
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/qualification"
)

func owningCapability(caseID qualification.Case) qualification.Capability {
	for _, capability := range qualification.Capabilities {
		if slices.Contains(qualification.CapabilityCases[capability], caseID) {
			return capability
		}
	}
	return ""
}

func baselineGradeOf(t *testing.T, records []qualification.Record, surface qualification.Surface, capability qualification.Capability) qualification.Grade {
	t.Helper()
	for _, rec := range records {
		if rec.Scenario == qualification.ScenarioSurfaceBaseline && rec.Surface == surface && rec.Capability == capability {
			return rec.Grade
		}
	}
	t.Fatalf("no published %s baseline row for surface %s", capability, surface)
	return ""
}

// A case excluded because the surface's own channel stays silent on a
// condition that does arise keeps its obligation, so the surface cannot
// publish usable for that capability.
func TestPublishedBaselineCountsAChannelSilentExclusionAsAGap(t *testing.T) {
	t.Parallel()

	checked := 0
	for _, profilePath := range stalenessProfilePaths(t) {
		profile, err := qualification.ReadRuntimeProfileFile(profilePath)
		if err != nil {
			t.Fatalf("ReadRuntimeProfileFile(%s) error = %v, want nil", profilePath, err)
		}
		collected := fullyObservedCollected(profile, qualification.GradeUsable, qualification.GradeUsable, qualification.GradeUsable)
		fixture, err := gradedEvidence(profile, collected, time.Now().UTC())
		if err != nil {
			t.Fatalf("gradedEvidence(%s, ...) error = %v, want nil", profilePath, err)
		}

		for _, entry := range profile.NotInducibleCases {
			if qualification.NotInducibleExclusion(entry.Reason) != qualification.ExclusionSurfaceSilent {
				continue
			}
			if !slices.Contains(profile.MeasuredSurfaces(), entry.Surface) {
				continue
			}
			capability := owningCapability(entry.Case)
			checked++
			if got := baselineGradeOf(t, fixture.Records, entry.Surface, capability); got == qualification.GradeUsable {
				t.Errorf("%s: published %s baseline for surface %s = usable, want a grade carrying the %s shortfall on case %s",
					profilePath, capability, entry.Surface, entry.Reason, entry.Case)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no shipped profile names a channel-silence exclusion, so this control measured nothing")
	}
}
