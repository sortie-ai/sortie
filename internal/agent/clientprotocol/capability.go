package clientprotocol

import "strings"

// capabilityState is the state of one entry in a session's capability
// record. It holds exactly two values: a capability this transport
// trusts a runtime to honor, and one known or observed not to be
// delivered.
type capabilityState string

const (
	capabilityProtocol capabilityState = "protocol"
	capabilityGap      capabilityState = "gap"
)

// Operator labels for the capability record's four entries, reported
// in this fixed order by the once-per-session gap notice.
const (
	capabilityLabelToolServers         = "tool servers"
	capabilityLabelTokenCounts         = "token counts"
	capabilityLabelSessionContinuation = "session continuation"
	capabilityLabelAgentVersion        = "agent version"
)

// Compile-time fragments the once-per-session gap notice is assembled
// from. Nothing about a specific runtime, configuration, or error is
// ever interpolated into it.
const (
	capabilityGapNoticeStem      = "this session started with a declared capability gap in: "
	capabilityGapNoticeSeparator = ", "
)

// capabilityRecord is a session's own record of which of this
// transport's capabilities its runtime actually delivers. Every entry
// starts at protocol or gap, resolved from structural knowledge and
// the initialize handshake, and a lowering to gap holds for the rest
// of the session: an entry never rises back to protocol once lowered.
type capabilityRecord struct {
	toolServers         capabilityState
	tokenCounts         capabilityState
	sessionContinuation capabilityState
	agentVersion        capabilityState
}

// newCapabilityRecord builds a session's capability record with its
// stage-one states, resolved from structural knowledge available
// before the handshake. remote reports whether the session's launch
// target runs the agent on a remote host.
//
// sessionContinuation starts at gap because session continuation is
// not implemented yet; a local launch with no generated tool-server
// configuration at all still leaves toolServers at protocol, because
// nothing was withheld when nothing was offered.
func newCapabilityRecord(remote bool) *capabilityRecord {
	toolServers := capabilityProtocol
	if remote {
		toolServers = capabilityGap
	}
	return &capabilityRecord{
		toolServers:         toolServers,
		tokenCounts:         capabilityGap,
		sessionContinuation: capabilityGap,
		agentVersion:        capabilityProtocol,
	}
}

// capabilityEntry pairs one capability record field with the operator
// label it is reported under.
type capabilityEntry struct {
	label string
	state capabilityState
}

// entries returns the record's four entries with their operator
// labels, in the fixed order the once-per-session notice reports them.
func (r capabilityRecord) entries() [4]capabilityEntry {
	return [4]capabilityEntry{
		{label: capabilityLabelToolServers, state: r.toolServers},
		{label: capabilityLabelTokenCounts, state: r.tokenCounts},
		{label: capabilityLabelSessionContinuation, state: r.sessionContinuation},
		{label: capabilityLabelAgentVersion, state: r.agentVersion},
	}
}

// gapNotice returns the once-per-session notice text listing every
// entry of r in the gap state, in the record's own field order, and
// reports whether there is at least one such entry. The text is
// assembled only from compile-time constant fragments.
func (r capabilityRecord) gapNotice() (string, bool) {
	var labels []string
	for _, entry := range r.entries() {
		if entry.state == capabilityGap {
			labels = append(labels, entry.label)
		}
	}
	if len(labels) == 0 {
		return "", false
	}
	return capabilityGapNoticeStem + strings.Join(labels, capabilityGapNoticeSeparator), true
}

// advertisesSessionContinuation reports whether caps advertises support
// for continuing a prior session through session/load or
// session/resume.
func advertisesSessionContinuation(caps agentCapabilities) bool {
	if caps.LoadSession != nil && *caps.LoadSession {
		return true
	}
	return caps.SessionCapabilities != nil && caps.SessionCapabilities.Resume != nil
}

// lower moves *entry to the gap state and reports whether it actually
// changed. Lowering an entry already at gap is idempotent: an entry
// never rises back to protocol within a session.
func lower(entry *capabilityState) bool {
	if *entry == capabilityGap {
		return false
	}
	*entry = capabilityGap
	return true
}
