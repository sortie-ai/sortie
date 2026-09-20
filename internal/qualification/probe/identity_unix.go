//go:build unix

package probe

import "github.com/sortie-ai/sortie/internal/qualification"

// pinnedProtocolVersionMirror mirrors clientprotocol's unexported
// negotiated protocol version; a change to the adapter's pinned version
// must be followed here.
const pinnedProtocolVersionMirror = 1

// induceRuntimeIdentity reads the log spy for the adapter's "agent
// implementation" handshake records, returning what each session
// reported. The adapter logs one record per session, so a collection
// that opened sessions against several builds reports several readings.
// A spy with no usable record grades not_observed with runtime_failed.
func induceRuntimeIdentity(fixture *sharedFixture) (qualification.Observation, map[string]qualification.SessionIdentity) {
	identities := map[string]qualification.SessionIdentity{}
	for _, entry := range fixture.logSpy.Entries() {
		if entry.Msg != "agent implementation" {
			continue
		}
		sessionID := entry.Attrs["session_id"]
		name := entry.Attrs["name"]
		version := entry.Attrs["version"]
		if sessionID == "" || name == "" || version == "" {
			continue
		}
		if _, named := identities[sessionID]; named {
			continue
		}
		identities[sessionID] = qualification.SessionIdentity{Name: name, Version: version}
	}
	if len(identities) == 0 {
		return qualification.Observation{
			Grade:   qualification.GradeNotObserved,
			Outcome: qualification.OutcomeRuntimeFailed,
			Detail:  "no handshake record reported a session's own agent name and version",
		}, nil
	}
	return qualification.Observation{
		Grade:   qualification.GradeUsable,
		Outcome: qualification.OutcomePass,
		Detail:  "the handshake reported the agent's own name and version",
	}, identities
}
