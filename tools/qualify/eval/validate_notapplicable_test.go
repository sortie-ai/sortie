package eval

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
)

func protocolHumanInputNotApplicable(sessionID string) evidence.Observation {
	return evidence.Observation{
		Grade:        evidence.GradeNotApplicable,
		Outcome:      evidence.OutcomeNotApplicable,
		Detail:       "the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose",
		SessionID:    sessionID,
		EvidencePath: evidence.SemanticEvidencePath(evidence.SurfaceProtocol),
	}
}

func TestValidateNotApplicableAdmittedOnProtocolHumanInput(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	sessionID := evidencetest.FixtureSession(evidence.SurfaceProtocol, "permission")
	if err := fixture.SetSemanticObservation(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, protocolHumanInputNotApplicable(sessionID)); err != nil {
		t.Fatalf("SetSemanticObservation(protocol human_input) error = %v, want nil", err)
	}

	path := evidencetest.WriteEvidenceFile(t, fixture.Records)
	if _, err := ValidateObservations(path, fixture.Declarations()); err != nil {
		t.Errorf("ValidateObservations(...) error = %v, want nil", err)
	}
}

func TestValidateNotApplicableRejectedOffProtocolHumanInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		surface    evidence.Surface
		capability evidence.Capability
		caseID     evidence.Case
	}{
		{"turn_disposition success on protocol", evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseSuccess},
		{"native human_input", evidence.SurfaceNativeJSON, evidence.CapabilityRetryClassification, evidence.CaseHumanInput},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
			fixture.Finalize()
			rec := fixture.FindFirst(evidencetest.MatchSemantic(tc.surface, tc.capability, tc.caseID))
			if rec == nil {
				t.Fatalf("no semantic record for %s %s %s", tc.surface, tc.capability, tc.caseID)
			}
			rec.Grade = evidence.GradeNotApplicable
			rec.Outcome = evidence.OutcomeNotApplicable

			path := evidencetest.WriteEvidenceFile(t, fixture.Records)
			_, err := ValidateObservations(path, fixture.Declarations())
			if err == nil || !strings.Contains(err.Error(), "not_applicable is invalid") {
				t.Errorf("ValidateObservations(...) error = %v, want a rejection naming not_applicable as invalid", err)
			}
		})
	}
}

func TestValidateNotApplicableRequiresASession(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	if err := fixture.SetSemanticObservation(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, protocolHumanInputNotApplicable("")); err != nil {
		t.Fatalf("SetSemanticObservation(protocol human_input) error = %v, want nil", err)
	}

	path := evidencetest.WriteEvidenceFile(t, fixture.Records)
	_, err := ValidateObservations(path, fixture.Declarations())
	if err == nil || !strings.Contains(err.Error(), "not_applicable record must carry its own session_id") {
		t.Errorf("ValidateObservations(...) error = %v, want a rejection naming the missing session_id", err)
	}
}

func TestValidateNotApplicableRequiresAnEvidencePath(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	sessionID := evidencetest.FixtureSession(evidence.SurfaceProtocol, "permission")
	if err := fixture.SetSemanticObservation(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, protocolHumanInputNotApplicable(sessionID)); err != nil {
		t.Fatalf("SetSemanticObservation(protocol human_input) error = %v, want nil", err)
	}
	rec := fixture.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseHumanInput))
	if rec == nil {
		t.Fatal("no protocol human_input semantic record")
	}
	rec.EvidencePath = nil

	path := evidencetest.WriteEvidenceFile(t, fixture.Records)
	_, err := ValidateObservations(path, fixture.Declarations())
	if err == nil || !strings.Contains(err.Error(), "evidence_path must be set") {
		t.Errorf("ValidateObservations(...) error = %v, want a rejection naming the missing evidence_path", err)
	}
}

func TestCheckOutcomeGradePairingRejectsMismatchedNotApplicable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		grade   evidence.Grade
		outcome evidence.Outcome
	}{
		{"not_applicable grade with a pass outcome", evidence.GradeNotApplicable, evidence.OutcomePass},
		{"not_applicable outcome with a usable grade", evidence.GradeUsable, evidence.OutcomeNotApplicable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := evidence.ValidRecord()
			rec.Grade = tc.grade
			rec.Outcome = tc.outcome
			if err := CheckOutcomeGradePairing(&rec); err == nil {
				t.Errorf("CheckOutcomeGradePairing(grade=%s, outcome=%s) = nil error, want a rejection", tc.grade, tc.outcome)
			}
		})
	}
}
