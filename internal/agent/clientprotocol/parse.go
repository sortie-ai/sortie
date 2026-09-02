package clientprotocol

import "encoding/json"

// sessionUpdateKind identifies which of the eleven pinned session/update
// variants a decoded notification carried.
type sessionUpdateKind int

const (
	updateUnknown sessionUpdateKind = iota
	updateAgentMessageChunk
	updateAgentThoughtChunk
	updateUserMessageChunk
	updateToolCall
	updateToolCallUpdate
	updatePlan
	updateAvailableCommands
	updateCurrentMode
	updateConfigOption
	updateSessionInfo
	updateUsage
)

// sessionUpdateEvent is the internal, per-variant decoding of one
// session/update notification's payload. Exactly the fields the
// recognized kind needs are populated; the rest hold their zero value.
type sessionUpdateEvent struct {
	kind       sessionUpdateKind
	rawVariant string

	chunk          contentChunk
	toolCallBegin  toolCall
	toolCallUpdate toolCallUpdate
	usage          usageUpdate
}

// parseSessionUpdate decodes the discriminator of raw, the undecoded
// "update" payload of one session/update notification, and then decodes
// only the fields the recognized variant needs. An unrecognized
// discriminator value, or a discriminator that does not decode at all,
// reports found=false rather than an error: the caller records the "any
// other value" row of the normalization table rather than failing the
// message, because the pinned schema itself expects a variant published
// after the pin to arrive undescribed.
func parseSessionUpdate(raw json.RawMessage) (event sessionUpdateEvent, found bool) {
	var probe struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return sessionUpdateEvent{}, false
	}

	event.rawVariant = probe.SessionUpdate

	switch probe.SessionUpdate {
	case sessionUpdateAgentMessageChunk:
		event.kind = updateAgentMessageChunk
		_ = json.Unmarshal(raw, &event.chunk)
	case sessionUpdateAgentThoughtChunk:
		event.kind = updateAgentThoughtChunk
		_ = json.Unmarshal(raw, &event.chunk)
	case sessionUpdateUserMessageChunk:
		event.kind = updateUserMessageChunk
		_ = json.Unmarshal(raw, &event.chunk)
	case sessionUpdateToolCall:
		event.kind = updateToolCall
		_ = json.Unmarshal(raw, &event.toolCallBegin)
	case sessionUpdateToolCallUpdate:
		event.kind = updateToolCallUpdate
		_ = json.Unmarshal(raw, &event.toolCallUpdate)
	case sessionUpdatePlan:
		event.kind = updatePlan
	case sessionUpdateAvailableCommandsUpdate:
		event.kind = updateAvailableCommands
	case sessionUpdateCurrentModeUpdate:
		event.kind = updateCurrentMode
	case sessionUpdateConfigOptionUpdate:
		event.kind = updateConfigOption
	case sessionUpdateSessionInfoUpdate:
		event.kind = updateSessionInfo
	case sessionUpdateUsageUpdate:
		event.kind = updateUsage
		_ = json.Unmarshal(raw, &event.usage)
	default:
		event.kind = updateUnknown
		return event, false
	}

	return event, true
}
