//go:build unix

package probe

import (
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func owningCapability(caseID evidence.Case) evidence.Capability {
	for _, capability := range evidence.Capabilities {
		if slices.Contains(evidence.CapabilityCases[capability], caseID) {
			return capability
		}
	}
	return ""
}

func baselineGradeOf(t *testing.T, records []evidence.Record, surface evidence.Surface, capability evidence.Capability) evidence.Grade {
	t.Helper()
	for _, rec := range records {
		if rec.Scenario == evidence.ScenarioSurfaceBaseline && rec.Surface == surface && rec.Capability == capability {
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
		p, err := profile.Load(mustRepositoryRoot(t), profilePath)
		if err != nil {
			t.Fatalf("profile.Load(%s) error = %v, want nil", profilePath, err)
		}
		collected := fullyObservedCollected(p, evidence.GradeUsable, evidence.GradeUsable, evidence.GradeUsable)
		fixture, err := gradedEvidence(p, collected, time.Now().UTC())
		if err != nil {
			t.Fatalf("gradedEvidence(%s, ...) error = %v, want nil", profilePath, err)
		}

		for _, entry := range p.NotInducibleCases {
			if evidence.NotInducibleExclusion(entry.Reason) != evidence.ExclusionSurfaceSilent {
				continue
			}
			if !slices.Contains(p.DeclaredMeasuredSurfaces(), entry.Surface) {
				continue
			}
			capability := owningCapability(entry.Case)
			checked++
			if got := baselineGradeOf(t, fixture.Records, entry.Surface, capability); got == evidence.GradeUsable {
				t.Errorf("%s: published %s baseline for surface %s = usable, want a grade carrying the %s shortfall on case %s",
					profilePath, capability, entry.Surface, entry.Reason, entry.Case)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no shipped profile names a channel-silence exclusion, so this control measured nothing")
	}
}
