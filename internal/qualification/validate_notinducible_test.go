package qualification

import (
	"strings"
	"testing"
)

func TestValidatorPerSurfaceNotInducibleScope(T *testing.T) {
	T.Parallel()

	declarations := RuntimeProfile{
		NotInducibleCases: []SurfaceNotInducible{
			{Surface: SurfaceNativeJSON, Case: CaseHumanInput, Reason: NotInducibleChannelTooSmall},
		},
	}

	T.Run("a not_inducible grade on exactly the named surface is satisfied", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareNotInducible(fixture, SurfaceNativeJSON, CapabilityRetryClassification, CaseHumanInput)
		fixture.FindFirst(MatchSemantic(SurfaceNativeJSON, CapabilityRetryClassification, CaseHumanInput)).Detail = NotInducibleChannelTooSmall
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservationsWithDeclarations(path, declarations); err != nil {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want nil: a not_inducible grade on exactly the declared surface satisfies the scoped rule", err)
		}
	})

	T.Run("an extra surface also carrying not_inducible is rejected", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareNotInducible(fixture, SurfaceNativeJSON, CapabilityRetryClassification, CaseHumanInput)
		declareNotInducible(fixture, SurfaceNativeStreamJSON, CapabilityRetryClassification, CaseHumanInput)
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservationsWithDeclarations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection of an unnamed surface also carrying not_inducible")
		}
		if !strings.Contains(err.Error(), "unexpected not-inducible grade") {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want it to name the unexpected surface", err)
		}
	})

	T.Run("the named surface not carrying not_inducible is rejected", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.Finalize()
		declareNotInducible(fixture, SurfaceNativeStreamJSON, CapabilityRetryClassification, CaseHumanInput)
		path := WriteEvidenceFile(T, fixture.Records)
		_, err := ValidateObservationsWithDeclarations(path, declarations)
		if err == nil {
			T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection: the declared surface itself carries no not_inducible grade")
		}
		if !strings.Contains(err.Error(), "not-inducible grade is missing on surfaces") {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want it to name the missing surface", err)
		}
	})

	T.Run("a catalog-wide case still requires not_inducible on every measured surface", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		declareNotInducible(fixture, SurfaceProtocol, CapabilityRetryClassification, CaseUnknownOutcome)
		declareNotInducible(fixture, SurfaceNativeJSON, CapabilityRetryClassification, CaseUnknownOutcome)
		declareNotInducible(fixture, SurfaceNativeStreamJSON, CapabilityRetryClassification, CaseUnknownOutcome)
		fixture.Finalize()
		path := WriteEvidenceFile(T, fixture.Records)
		if _, err := ValidateObservationsWithDeclarations(path, RuntimeProfile{}); err != nil {
			T.Errorf("ValidateObservationsWithDeclarations() error = %v, want nil: a catalog-wide case covering every measured surface is satisfied with no not_inducible_cases entry at all", err)
		}
	})
}
