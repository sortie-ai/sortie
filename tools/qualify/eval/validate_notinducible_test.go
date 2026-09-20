package eval

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func TestValidatorPerSurfaceNotInducibleScope(t *testing.T) {
	t.Parallel()

	declarations := profile.RuntimeProfile{
		NotInducibleCases: []profile.SurfaceNotInducible{
			{Surface: evidence.SurfaceNativeJSON, Case: evidence.CaseHumanInput, Reason: evidence.NotInducibleChannelTooSmall},
		},
	}

	t.Run("a not_inducible grade on exactly the named surface is satisfied", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.NotInducibleChannelTooSmall)
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		if _, err := ValidateObservations(path, declarations); err != nil {
			t.Errorf("ValidateObservations() error = %v, want nil: a not_inducible grade on exactly the declared surface satisfies the scoped rule", err)
		}
	})

	t.Run("an extra surface also carrying not_inducible is rejected", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.NotInducibleDetail)
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeStreamJSON, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.NotInducibleDetail)
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		_, err := ValidateObservations(path, declarations)
		if err == nil {
			t.Fatal("ValidateObservations() = nil error, want rejection of an unnamed surface also carrying not_inducible")
		}
		if !strings.Contains(err.Error(), "unexpected not-inducible grade") {
			t.Errorf("ValidateObservations() error = %v, want it to name the unexpected surface", err)
		}
	})

	t.Run("the named surface not carrying not_inducible is rejected", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.Finalize()
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeStreamJSON, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.NotInducibleDetail)
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		_, err := ValidateObservations(path, declarations)
		if err == nil {
			t.Fatal("ValidateObservations() = nil error, want rejection: the declared surface itself carries no not_inducible grade")
		}
		if !strings.Contains(err.Error(), "not-inducible grade is missing on surfaces") {
			t.Errorf("ValidateObservations() error = %v, want it to name the missing surface", err)
		}
	})

	t.Run("a catalog-wide case still requires not_inducible on every measured surface", func(t *testing.T) {
		t.Parallel()

		fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
		fixture.SetSemanticNotInducible(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome, evidence.NotInducibleDetail)
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome, evidence.NotInducibleDetail)
		fixture.SetSemanticNotInducible(evidence.SurfaceNativeStreamJSON, evidence.CapabilityRetryClassification, evidence.CaseUnknownOutcome, evidence.NotInducibleDetail)
		fixture.Finalize()
		path := evidencetest.WriteEvidenceFile(t, fixture.Records)
		if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err != nil {
			t.Errorf("ValidateObservations() error = %v, want nil: a catalog-wide case covering every measured surface is satisfied with no not_inducible_cases entry at all", err)
		}
	})
}
