package probe

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

func excludedLineFor(t *testing.T, reason string) string {
	t.Helper()

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	fixture.Finalize()
	fixture.SetSemanticNotInducible(qualification.SurfaceProtocol, qualification.CapabilityRetryClassification, qualification.CaseUnknownOutcome, reason)

	profile := threeSurfaceProfile()
	conclusions, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, profile)
	if err != nil {
		t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
	}
	prefix := "retry_classification unknown_outcome: "
	for _, entry := range conclusions.Excluded {
		if line, found := strings.CutPrefix(entry, prefix); found {
			return line
		}
	}
	t.Fatalf("Excluded = %v, want an entry beginning %q", conclusions.Excluded, prefix)
	return ""
}

func TestSummaryDistinguishesNotInducibleReasons(t *testing.T) {
	t.Parallel()

	notInduced := excludedLineFor(t, qualification.NotInducibleChannelTooSmall)
	surfaceSilent := excludedLineFor(t, qualification.NotInducibleOutputSilentOnFailure)
	unstated := excludedLineFor(t, qualification.NotInducibleDetail)

	lines := []string{notInduced, surfaceSilent, unstated}
	if len(slices.Compact(slices.Clone(lines))) != len(lines) {
		t.Errorf("the three not-inducible reasons render as %v, want one distinct line each", lines)
	}

	if !strings.Contains(notInduced, qualification.NotInducibleChannelTooSmall) {
		t.Errorf("the not-induced line = %q, want it to name its own reason", notInduced)
	}
	if !strings.Contains(notInduced, "unmeasured") {
		t.Errorf("the not-induced line = %q, want it to say the case stays unmeasured on this surface", notInduced)
	}

	if !strings.Contains(surfaceSilent, qualification.NotInducibleOutputSilentOnFailure) {
		t.Errorf("the surface-silent line = %q, want it to name its own reason", surfaceSilent)
	}
	if strings.Contains(surfaceSilent, "unmeasured") {
		t.Errorf("the surface-silent line = %q, want it to report observed behaviour: the condition arose and the surface reported nothing", surfaceSilent)
	}
	if !strings.Contains(surfaceSilent, "obligation") {
		t.Errorf("the surface-silent line = %q, want it to say the case keeps its obligation", surfaceSilent)
	}
}

func TestSummaryKeepsApplicabilityApartFromInduction(t *testing.T) {
	t.Parallel()

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	fixture.Finalize()
	fixture.SetSemanticDeclaredGap(qualification.CapabilityRetryClassification, qualification.CaseRuntimeRefusal, qualification.DeclaredGapNeverProduced)
	fixture.SetSemanticDeclaredGap(qualification.CapabilityRetryClassification, qualification.CaseNonRetryableRefusal, qualification.DeclaredGapNeverProduced)
	fixture.SetSemanticNotInducible(qualification.SurfaceProtocol, qualification.CapabilityRetryClassification, qualification.CaseUnknownOutcome, qualification.NotInducibleChannelTooSmall)

	conclusions, err := ConclusionsFromRecords(fixture.Records, qualification.VerdictQualified, threeSurfaceProfile())
	if err != nil {
		t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
	}

	var declared, induction string
	for _, entry := range conclusions.Excluded {
		switch {
		case strings.HasPrefix(entry, "turn_disposition runtime_refusal: "):
			declared = entry
		case strings.HasPrefix(entry, "retry_classification unknown_outcome: "):
			induction = entry
		}
	}
	if declared == "" || induction == "" {
		t.Fatalf("Excluded = %v, want both a declared-gap line and a not-inducible line", conclusions.Excluded)
	}
	if !strings.Contains(declared, qualification.DeclaredGapNeverProduced) {
		t.Errorf("the declared-gap line = %q, want it to name the runtime's own declared reason", declared)
	}
	if strings.Contains(declared, "induc") {
		t.Errorf("the declared-gap line = %q, want it to state applicability rather than anything about induction", declared)
	}
}
