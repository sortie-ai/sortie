//go:build unix

package probe

import "github.com/sortie-ai/sortie/tools/qualify/evidence"

// pinnedProtocolVersionMirror mirrors clientprotocol's unexported negotiated
// protocol version; a change to the adapter's pinned version must be
// followed here.
const pinnedProtocolVersionMirror = 1

// induceRuntimeIdentity reads the log spy's "agent implementation"
// handshake records, one per session, and grades not_observed with
// runtime_failed when none is usable.
func induceRuntimeIdentity(fixture *sharedFixture) (evidence.Observation, map[string]evidence.SessionIdentity) {
	identities := map[string]evidence.SessionIdentity{}
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
		identities[sessionID] = evidence.SessionIdentity{Name: name, Version: version}
	}
	if len(identities) == 0 {
		return evidence.Observation{
			Grade:   evidence.GradeNotObserved,
			Outcome: evidence.OutcomeRuntimeFailed,
			Detail:  "no handshake record reported a session's own agent name and version",
		}, nil
	}
	return evidence.Observation{
		Grade:   evidence.GradeUsable,
		Outcome: evidence.OutcomePass,
		Detail:  "the handshake reported the agent's own name and version",
	}, identities
}
