//go:build unix

package probe

import "github.com/sortie-ai/sortie/internal/qualification"

// inducedRow is one collector-driven observation grade and its detail,
// passed to gradedEvidence for one of the three rows Run's own
// inducers grade.
type inducedRow struct {
	grade  qualification.Grade
	detail string
}

// observation renders the row as the Observation its setter consumes,
// naming the protocol session the not-observed fixture already carries
// for sessionName so the row keeps its identifier.
func (r inducedRow) observation(sessionName string) qualification.Observation {
	return qualification.Observation{
		Grade:     r.grade,
		Outcome:   qualification.BaselineVerdictFor(r.grade),
		Detail:    r.detail,
		SessionID: qualification.FixtureSession(qualification.SurfaceProtocol, sessionName),
	}
}

// gradedEvidence builds the evidence one live collection publishes: a
// not-observed fixture for profile's own declared and absent surfaces,
// with every declaration applied, and exactly the three
// collector-driven rows rewritten to the grade and detail each
// inducer observed: tool server delivery, permission handling, and
// protocol session continuation. It launches nothing and reads no
// environment.
func gradedEvidence(profile qualification.RuntimeProfile, toolServer, permission, continuation inducedRow) (*qualification.Fixture, error) {
	fixture := qualification.NewFixture(qualification.FixtureNotObserved, profile.AbsentSurfaces...)
	for _, declaration := range profile.Declarations {
		fixture.SetSemanticDeclaredGap(declaration.Capability, declaration.Case, declaration.Reason)
	}
	if err := fixture.SetToolServerDelivery(toolServer.observation("mcp")); err != nil {
		return nil, err
	}
	if err := fixture.SetPermissionHandling(permission.observation("permission")); err != nil {
		return nil, err
	}
	fixture.SetSessionContinuation(qualification.SurfaceProtocol, continuation.grade, continuation.detail)
	fixture.Finalize()
	return fixture, nil
}
