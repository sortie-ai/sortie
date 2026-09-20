//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// A generated identifier in the session member is indistinguishable in
// the published artifact from one a runtime reported, attributing a run
// to a session that never existed.
func TestEndToEndRowOfALaunchThatNeverStartedNamesNoSession(t *testing.T) {
	t.Parallel()

	coords := Coordinates{Profile: qualification.RuntimeProfile{}}
	record := induceEndToEnd(t, coords, &sharedFixture{tracker: &groupTracker{}}, "", "")

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	if err := fixture.SetEndToEnd(endToEndObservation(record)); err != nil {
		t.Fatalf("SetEndToEnd(observation of a launch that never started) error = %v, want nil", err)
	}
	fixture.Finalize()

	path := qualification.WriteEvidenceFile(t, fixture.Records)
	if _, err := qualification.ValidateObservations(path); err != nil {
		t.Fatalf("ValidateObservations(%s) = %v, want nil", path, err)
	}

	published, err := qualification.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("ReadEvidenceFile(%s) = _, %v, want nil", path, err)
	}
	for _, rec := range published {
		if rec.Scenario != qualification.ScenarioEndToEnd {
			continue
		}
		if rec.SessionID != nil {
			t.Errorf("published end-to-end row session_id = %q, want null: no session was ever started", *rec.SessionID)
		}
		return
	}
	t.Fatal("the published evidence carries no end-to-end row")
}
