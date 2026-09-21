package clientprotocol

import "strings"

// capabilityState is the state of one capability record entry: trusted to the
// runtime via protocol, supplied by this transport, or known not to be
// delivered.
type capabilityState string

const (
	capabilityProtocol capabilityState = "protocol"
	capabilityOurs     capabilityState = "ours"
	capabilityGap      capabilityState = "gap"
)

const (
	capabilityLabelToolServers         = "tool servers"
	capabilityLabelTokenCounts         = "token counts"
	capabilityLabelSessionContinuation = "session continuation"
	capabilityLabelAgentVersion        = "agent version"
)

// The gap notice is assembled only from these compile-time fragments; nothing
// about a runtime, configuration, or error is interpolated into it.
const (
	capabilityGapNoticeStem      = "this session started with a declared capability gap in: "
	capabilityGapNoticeSeparator = ", "
)

// capabilityRecord is a session's record of which of this transport's
// capabilities its runtime actually delivers. An entry never rises back to
// protocol once lowered to gap.
type capabilityRecord struct {
	toolServers         capabilityState
	tokenCounts         capabilityState
	sessionContinuation capabilityState
	agentVersion        capabilityState
}

// newCapabilityRecord builds a session's capability record with its stage-one
// states. remote reports whether the launch target is remote; measured reports
// whether a measurement source claimed the launch.
//
// tokenCounts never reaches protocol because the wire carries no spend counter.
// toolServers starts at protocol for a local launch, since nothing is withheld
// when nothing was offered; the handshake and a later undelivered call are what
// lower it and sessionContinuation.
func newCapabilityRecord(remote, measured bool) *capabilityRecord {
	toolServers := capabilityProtocol
	if remote {
		toolServers = capabilityGap
	}
	tokenCounts := capabilityGap
	if measured {
		tokenCounts = capabilityOurs
	}
	return &capabilityRecord{
		toolServers:         toolServers,
		tokenCounts:         tokenCounts,
		sessionContinuation: capabilityProtocol,
		agentVersion:        capabilityProtocol,
	}
}

type capabilityEntry struct {
	label string
	state capabilityState
}

// entries returns the record's four entries with their operator labels, in the
// fixed order the once-per-session notice reports them.
func (r capabilityRecord) entries() [4]capabilityEntry {
	return [4]capabilityEntry{
		{label: capabilityLabelToolServers, state: r.toolServers},
		{label: capabilityLabelTokenCounts, state: r.tokenCounts},
		{label: capabilityLabelSessionContinuation, state: r.sessionContinuation},
		{label: capabilityLabelAgentVersion, state: r.agentVersion},
	}
}

// gapNotice returns the once-per-session notice listing every gap entry of r in
// field order, and whether there is at least one.
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

// advertisesSessionContinuation reports whether caps advertises session/load or
// session/resume. It defers to [chooseContinuationMethod] so the handshake's
// lowering decision and resolveSession's routing decision cannot disagree.
func advertisesSessionContinuation(caps agentCapabilities) bool {
	return chooseContinuationMethod(caps) != continuationNone
}

// advertisesSessionClose reports whether caps advertises session/close.
func advertisesSessionClose(caps agentCapabilities) bool {
	return caps.SessionCapabilities != nil && caps.SessionCapabilities.Close != nil
}

func advertisesSessionDelete(caps agentCapabilities) bool {
	return caps.SessionCapabilities != nil && caps.SessionCapabilities.Delete != nil
}

// lower moves *entry to the gap state and reports whether it changed. An entry
// never rises back to protocol within a session, so lowering one already at gap
// is idempotent.
func lower(entry *capabilityState) bool {
	if *entry == capabilityGap {
		return false
	}
	*entry = capabilityGap
	return true
}
