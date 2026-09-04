// This file implements a pure, presence-and-value shape check over one
// recorded Agent Client Protocol capture. The existing schema pass
// validates whatever the runtime happened to send against the pinned
// wire schema; it says nothing about what the runtime failed to send.
// recordedShapeViolations closes that gap: it correlates the two
// captured directions of one session and reports every rule a
// completed turn is expected to satisfy that the capture does not.
package clientprotocol

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// captureExpectation names what a capture must contain. Each value is a
// superset of the one above it.
type captureExpectation int

const (
	// expectHandshakeOnly requires only an initialize response and a
	// session-establishing response.
	expectHandshakeOnly captureExpectation = iota

	// expectCompletedTurn adds a completed session/prompt response and
	// at least one text agent_message_chunk.
	expectCompletedTurn

	// expectToolForcingTurn adds the tool-call rule, for the one prompt
	// that forces a tool call.
	expectToolForcingTurn
)

// shapeObservation is what one capture was measured to carry.
type shapeObservation struct {
	agentName    string // InitializeResponse.agentInfo.name
	agentVersion string // InitializeResponse.agentInfo.version
	protocolVer  string // InitializeResponse.protocolVersion, rendered

	sessionUpdates map[string]int // sessionUpdate discriminator value -> count
	stopReasons    []string       // PromptResponse.stopReason, in wire order
	toolCallPairs  int            // tool_call ids later carried by a terminal tool_call_update
	nonTextChunks  int            // agent_message_chunk whose content.type is not text
	establishedBy  []string       // every session/new, session/load, and session/resume answered, in wire order
}

// liveEnvelope decodes the classifying members of one JSON-RPC line
// captured from the agent direction, without committing to a params or
// result shape: a notification carries a method and params, a response
// carries an id, a result, and no method.
type liveEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// hasError reports whether e carries a JSON-RPC error member.
func (e liveEnvelope) hasError() bool {
	return len(e.Error) != 0 && string(e.Error) != "null"
}

// recordedShapeViolations is the whole contract, as a pure function over
// one capture. It performs no I/O and touches no testing.T, so the
// ungated controls drive it directly.
func recordedShapeViolations(clientLines, agentLines [][]byte, events []domain.AgentEvent, expect captureExpectation) (violations []string, observed shapeObservation) {
	observed.sessionUpdates = make(map[string]int)

	// The client direction supplies the JSON-RPC id to method map,
	// which is the only way to know which definition an agent response
	// with no method of its own is answering.
	methodByID := make(map[string]string)
	for _, line := range clientLines {
		var header wireHeader
		if err := json.Unmarshal(line, &header); err != nil {
			continue
		}
		if header.Method != "" && len(header.ID) != 0 {
			methodByID[jsonRPCIDKey(header.ID)] = header.Method
		}
	}

	var initializeResponses int
	var sessionEstablished bool
	var sawTextChunk bool
	toolCallSeen := make(map[string]bool)
	toolCallPairCounted := make(map[string]bool)

	for _, line := range agentLines {
		var envelope liveEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}
		if envelope.hasError() {
			continue
		}

		switch envelope.Method {
		case methodSessionUpdate:
			var notification sessionNotification
			if err := json.Unmarshal(envelope.Params, &notification); err != nil {
				continue
			}
			variant := notification.Update.SessionUpdate
			observed.sessionUpdates[variant]++

			switch variant {
			case sessionUpdateAgentMessageChunk:
				var chunk contentChunk
				if err := json.Unmarshal(notification.Update.Remainder, &chunk); err != nil {
					continue
				}
				if chunk.Content.Type != contentBlockText {
					observed.nonTextChunks++
					continue
				}
				var text textContent
				if err := json.Unmarshal(chunk.Content.Remainder, &text); err == nil && text.Text != "" {
					sawTextChunk = true
				}
			case sessionUpdateToolCall:
				var call toolCall
				if err := json.Unmarshal(notification.Update.Remainder, &call); err != nil {
					continue
				}
				if call.ToolCallID == "" {
					violations = append(violations, "S-8: a tool_call update carries an empty toolCallId")
					continue
				}
				toolCallSeen[string(call.ToolCallID)] = true
			case sessionUpdateToolCallUpdate:
				var update toolCallUpdate
				if err := json.Unmarshal(notification.Update.Remainder, &update); err != nil {
					continue
				}
				if update.ToolCallID == "" {
					violations = append(violations, "S-8: a tool_call_update carries an empty toolCallId")
					continue
				}
				id := string(update.ToolCallID)
				isTerminalStatus := update.Status != nil && (*update.Status == toolCallStatusCompleted || *update.Status == toolCallStatusFailed)
				if isTerminalStatus && toolCallSeen[id] && !toolCallPairCounted[id] {
					observed.toolCallPairs++
					toolCallPairCounted[id] = true
				}
			}

		case "":
			switch methodByID[jsonRPCIDKey(envelope.ID)] {
			case methodInitialize:
				initializeResponses++
				var resp initializeResponse
				if err := json.Unmarshal(envelope.Result, &resp); err == nil {
					if resp.AgentInfo != nil {
						observed.agentName = resp.AgentInfo.Name
						observed.agentVersion = resp.AgentInfo.Version
					}
					observed.protocolVer = fmt.Sprintf("%d", int64(resp.ProtocolVersion))
				}
			case methodSessionNew:
				observed.establishedBy = append(observed.establishedBy, methodSessionNew)
				var resp newSessionResponse
				if err := json.Unmarshal(envelope.Result, &resp); err == nil && resp.SessionID != "" {
					sessionEstablished = true
				}
			case methodSessionLoad:
				observed.establishedBy = append(observed.establishedBy, methodSessionLoad)
				sessionEstablished = true
			case methodSessionResume:
				observed.establishedBy = append(observed.establishedBy, methodSessionResume)
				sessionEstablished = true
			case methodSessionPrompt:
				var resp promptResponse
				if err := json.Unmarshal(envelope.Result, &resp); err == nil {
					observed.stopReasons = append(observed.stopReasons, string(resp.StopReason))
				}
			}
		}
	}

	if initializeResponses != 1 {
		violations = append(violations, fmt.Sprintf("S-1: expected exactly one initialize response, observed %d", initializeResponses))
	}
	if observed.agentName == "" || observed.agentVersion == "" {
		violations = append(violations, "S-2: the initialize response's agentInfo carries an empty name or version")
	}
	if !sessionEstablished {
		violations = append(violations, "S-3: no session-establishing response was observed")
	}

	if expect >= expectCompletedTurn {
		if len(observed.stopReasons) == 0 {
			violations = append(violations, "S-4: no session/prompt response was observed")
		}
		for _, reason := range observed.stopReasons {
			if reason != string(stopReasonEndTurn) {
				violations = append(violations, fmt.Sprintf("S-5: observed stopReason %q, want %q", reason, stopReasonEndTurn))
			}
		}
		if observed.sessionUpdates[sessionUpdateAgentMessageChunk] == 0 {
			violations = append(violations, "S-6: no session/update carried the agent_message_chunk discriminator")
		}
		if !sawTextChunk {
			violations = append(violations, "S-7: no agent_message_chunk carried a non-empty text content block")
		}
	}

	if observed.toolCallPairs > 0 {
		var sawToolResult bool
		for _, e := range events {
			if e.Type == domain.EventToolResult {
				sawToolResult = true
				break
			}
		}
		if !sawToolResult {
			violations = append(violations, "S-9: a tool_call/tool_call_update pair was observed but no domain.EventToolResult was recorded")
		}
	}

	if expect == expectToolForcingTurn && observed.toolCallPairs == 0 {
		violations = append(violations, "S-10: no tool_call was paired with a terminal tool_call_update (completed or failed)")
	}

	return violations, observed
}

// --- Ungated negative controls ---
//
// TestRecordedShapeViolations proves recordedShapeViolations can both
// pass a clean capture and fail on the mutations the spec names. It
// runs with no gate variable set: every control is an in-test edit to
// one decoded, hand-written base capture, never a live recording.

// shapeFixtureLine is one decoded JSON-RPC line from a live_shape
// fixture, kept as a generic map so a control can remove or rewrite
// exactly the member it targets before re-encoding the line.
type shapeFixtureLine = map[string]any

// shapeCapture is the client and agent direction of one capture, held
// as decoded lines so a control can mutate a clone before encoding it
// back into the [][]byte shape recordedShapeViolations accepts.
type shapeCapture struct {
	client []shapeFixtureLine
	agent  []shapeFixtureLine
}

// loadShapeFixtureLines reads and decodes every line of path.
func loadShapeFixtureLines(t *testing.T, path string) []shapeFixtureLine {
	t.Helper()
	raw := capturedJSONLines(t, path)
	lines := make([]shapeFixtureLine, len(raw))
	for i, line := range raw {
		if err := json.Unmarshal(line, &lines[i]); err != nil {
			t.Fatalf("decode fixture line %d of %s: %v", i, path, err)
		}
	}
	return lines
}

// cloneShapeFixtureLines deep-copies lines through a marshal/unmarshal
// round trip, so a control can mutate the result without the mutation
// reaching the shared base capture other subtests read concurrently.
func cloneShapeFixtureLines(t *testing.T, lines []shapeFixtureLine) []shapeFixtureLine {
	t.Helper()
	out := make([]shapeFixtureLine, len(lines))
	for i, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("clone fixture line %d: marshal: %v", i, err)
		}
		if err := json.Unmarshal(b, &out[i]); err != nil {
			t.Fatalf("clone fixture line %d: unmarshal: %v", i, err)
		}
	}
	return out
}

// encodeShapeFixtureLines re-encodes lines into the [][]byte shape
// recordedShapeViolations accepts.
func encodeShapeFixtureLines(t *testing.T, lines []shapeFixtureLine) [][]byte {
	t.Helper()
	out := make([][]byte, len(lines))
	for i, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("encode fixture line %d: %v", i, err)
		}
		out[i] = b
	}
	return out
}

// loadBaseShapeCapture loads the hand-written base capture fixture
// pair once per test. Every control clones it before mutating.
func loadBaseShapeCapture(t *testing.T) shapeCapture {
	t.Helper()
	return shapeCapture{
		client: loadShapeFixtureLines(t, "testdata/live_shape/base_capture.client.jsonl"),
		agent:  loadShapeFixtureLines(t, "testdata/live_shape/base_capture.agent.jsonl"),
	}
}

// clone returns a deep copy of c, safe for a control to mutate.
func (c shapeCapture) clone(t *testing.T) shapeCapture {
	t.Helper()
	return shapeCapture{
		client: cloneShapeFixtureLines(t, c.client),
		agent:  cloneShapeFixtureLines(t, c.agent),
	}
}

// violations encodes c and drives recordedShapeViolations over it.
func (c shapeCapture) violations(t *testing.T, expect captureExpectation, events []domain.AgentEvent) ([]string, shapeObservation) {
	t.Helper()
	return recordedShapeViolations(encodeShapeFixtureLines(t, c.client), encodeShapeFixtureLines(t, c.agent), events, expect)
}

// isMethod reports whether line names method.
func isMethod(method string) func(shapeFixtureLine) bool {
	return func(line shapeFixtureLine) bool {
		m, _ := line["method"].(string)
		return m == method
	}
}

// isResponseToID reports whether line is a response (no method member)
// answering id.
func isResponseToID(id int) func(shapeFixtureLine) bool {
	return func(line shapeFixtureLine) bool {
		if _, hasMethod := line["method"]; hasMethod {
			return false
		}
		n, ok := line["id"].(float64)
		return ok && int(n) == id
	}
}

// sessionUpdateOf reports the update object of a session/update line,
// or ok=false when line is not one.
func sessionUpdateOf(line shapeFixtureLine) (update map[string]any, ok bool) {
	if m, _ := line["method"].(string); m != methodSessionUpdate {
		return nil, false
	}
	params, ok := line["params"].(map[string]any)
	if !ok {
		return nil, false
	}
	update, ok = params["update"].(map[string]any)
	return update, ok
}

// isSessionUpdateVariant reports whether line is a session/update
// notification whose sessionUpdate discriminator equals variant.
func isSessionUpdateVariant(variant string) func(shapeFixtureLine) bool {
	return func(line shapeFixtureLine) bool {
		update, ok := sessionUpdateOf(line)
		if !ok {
			return false
		}
		v, _ := update["sessionUpdate"].(string)
		return v == variant
	}
}

// forEachSessionUpdate calls fn with the decoded update object of
// every session/update line in lines whose discriminator equals
// variant, so a control can rewrite it in place.
func forEachSessionUpdate(lines []shapeFixtureLine, variant string, fn func(update map[string]any)) {
	for _, line := range lines {
		update, ok := sessionUpdateOf(line)
		if !ok {
			continue
		}
		if v, _ := update["sessionUpdate"].(string); v != variant {
			continue
		}
		fn(update)
	}
}

// removeLines returns a copy of lines excluding every line drop
// reports true for.
func removeLines(lines []shapeFixtureLine, drop func(shapeFixtureLine) bool) []shapeFixtureLine {
	out := make([]shapeFixtureLine, 0, len(lines))
	for _, line := range lines {
		if drop(line) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// requestLine returns the request line in lines (a line carrying a
// method member) answering id, failing t when none exists.
func requestLine(t *testing.T, lines []shapeFixtureLine, id int) shapeFixtureLine {
	t.Helper()
	for _, line := range lines {
		if _, hasMethod := line["method"]; !hasMethod {
			continue
		}
		if n, ok := line["id"].(float64); ok && int(n) == id {
			return line
		}
	}
	t.Fatalf("no request line with id %d", id)
	return nil
}

// shapeResponseResult returns the decoded result object of the response
// line in lines answering id, failing t when none exists or the
// result member is not an object.
func shapeResponseResult(t *testing.T, lines []shapeFixtureLine, id int) map[string]any {
	t.Helper()
	for _, line := range lines {
		if _, hasMethod := line["method"]; hasMethod {
			continue
		}
		n, ok := line["id"].(float64)
		if !ok || int(n) != id {
			continue
		}
		result, ok := line["result"].(map[string]any)
		if !ok {
			t.Fatalf("response line id %d has no result object", id)
		}
		return result
	}
	t.Fatalf("no response line with id %d", id)
	return nil
}

// nonTextContentBlock is a synthetic content block whose type is not
// text, for the S-7 controls.
func nonTextContentBlock() map[string]any {
	return map[string]any{"type": contentBlockImage, "data": "ZmFrZQ==", "mimeType": "image/png"}
}

// mutateInitAgentInfoRemoved removes the initialize response's
// agentInfo member entirely, targeting S-2.
func mutateInitAgentInfoRemoved(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	delete(shapeResponseResult(t, mutated.agent, 1), "agentInfo")
	return mutated
}

// mutateInitAgentInfoVersionEmpty sets the initialize response's
// agentInfo.version to the empty string, targeting S-2.
func mutateInitAgentInfoVersionEmpty(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	agentInfo, ok := shapeResponseResult(t, mutated.agent, 1)["agentInfo"].(map[string]any)
	if !ok {
		t.Fatal("base capture's initialize response has no agentInfo object")
	}
	agentInfo["version"] = ""
	return mutated
}

// mutateStopReasonChanged rewrites the session/prompt response's
// stopReason to reason, targeting S-5.
func mutateStopReasonChanged(t *testing.T, c shapeCapture, reason string) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	shapeResponseResult(t, mutated.agent, 3)["stopReason"] = reason
	return mutated
}

// mutateEveryAgentMessageChunkRemoved drops every agent_message_chunk
// session/update notification, targeting S-6.
func mutateEveryAgentMessageChunkRemoved(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	mutated.agent = removeLines(mutated.agent, isSessionUpdateVariant(sessionUpdateAgentMessageChunk))
	return mutated
}

// mutateEveryToolCallRemoved drops every tool_call session/update
// notification, leaving its tool_call_update counterpart in place so
// the pairing that closes S-10 cannot form, targeting S-10.
func mutateEveryToolCallRemoved(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	mutated.agent = removeLines(mutated.agent, isSessionUpdateVariant(sessionUpdateToolCall))
	return mutated
}

// mutateAllAgentMessageChunksToNonText replaces the content block of
// every agent_message_chunk notification with a non-text one,
// targeting the S-7-violated control.
func mutateAllAgentMessageChunksToNonText(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	forEachSessionUpdate(mutated.agent, sessionUpdateAgentMessageChunk, func(update map[string]any) {
		update["content"] = nonTextContentBlock()
	})
	return mutated
}

// mutateOneAgentMessageChunkToNonText replaces the content block of
// the first agent_message_chunk notification only, leaving at least
// one text chunk, targeting the S-7-satisfied control.
func mutateOneAgentMessageChunkToNonText(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	var replaced bool
	forEachSessionUpdate(mutated.agent, sessionUpdateAgentMessageChunk, func(update map[string]any) {
		if replaced {
			return
		}
		update["content"] = nonTextContentBlock()
		replaced = true
	})
	if !replaced {
		t.Fatal("base capture carries no agent_message_chunk update to replace")
	}
	return mutated
}

// mutateSessionEstablishedByLoad retargets the session/new request to
// session/load, so the matching response resolves through the
// session/load branch of S-3 instead of session/new.
func mutateSessionEstablishedByLoad(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	requestLine(t, mutated.client, 2)["method"] = methodSessionLoad
	return mutated
}

// addSecondSessionNewResponse appends a second session-establishing
// request and response pair naming id and sessionID, so
// observed.establishedBy records two entries.
func addSecondSessionNewResponse(c shapeCapture, id int, sessionID string) shapeCapture {
	c.client = append(c.client, shapeFixtureLine{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  methodSessionNew,
		"params":  map[string]any{"cwd": "/workspace", "mcpServers": []any{}},
	})
	c.agent = append(c.agent, shapeFixtureLine{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]any{"sessionId": sessionID},
	})
	return c
}

// handshakeOnlyCapture drops the session/prompt request and response
// and every session/update notification, leaving only the initialize
// and session-establishing exchanges.
func handshakeOnlyCapture(t *testing.T, c shapeCapture) shapeCapture {
	t.Helper()
	mutated := c.clone(t)
	mutated.client = removeLines(mutated.client, isMethod(methodSessionPrompt))
	mutated.agent = removeLines(mutated.agent, isMethod(methodSessionUpdate))
	mutated.agent = removeLines(mutated.agent, isResponseToID(3))
	return mutated
}

// assertNoViolations fails t unless violations is empty.
func assertNoViolations(t *testing.T, violations []string) {
	t.Helper()
	if len(violations) != 0 {
		t.Errorf("recordedShapeViolations() violations = %v, want none", violations)
	}
}

// assertViolationsExactly fails t unless violations has exactly one
// entry per wantPrefixes, each starting with the corresponding
// prefix, order-independent. A mutation that empties every observed
// agent_message_chunk, for example, legitimately breaks both S-6 and
// S-7 at once, per the rule the two checks apply independently; this
// asserts the whole set a control produces, not just one member of
// it, so an unrelated extra violation still fails the test.
func assertViolationsExactly(t *testing.T, violations []string, wantPrefixes ...string) {
	t.Helper()
	remaining := slices.Clone(wantPrefixes)
	for _, v := range violations {
		idx := slices.IndexFunc(remaining, func(p string) bool { return strings.HasPrefix(v, p) })
		if idx == -1 {
			t.Errorf("recordedShapeViolations() unexpected violation %q, want one of prefixes %v", v, wantPrefixes)
			continue
		}
		remaining = slices.Delete(remaining, idx, idx+1)
	}
	if len(remaining) != 0 {
		t.Errorf("recordedShapeViolations() violations = %v, missing violations with prefixes %v", violations, remaining)
	}
}

// TestRecordedShapeViolations drives recordedShapeViolations over the
// hand-written base capture and a set of in-test mutations of it, with
// no gate variable set: this is the only place a runtime that stops
// sending a wire message the adapter consumes can be caught before a
// release ships.
func TestRecordedShapeViolations(t *testing.T) {
	t.Parallel()

	base := loadBaseShapeCapture(t)
	toolResultEvents := []domain.AgentEvent{{Type: domain.EventToolResult}}

	t.Run("clean_capture", func(t *testing.T) {
		t.Parallel()
		violations, _ := base.violations(t, expectToolForcingTurn, toolResultEvents)
		assertNoViolations(t, violations)
	})

	t.Run("S-3/session_load_only", func(t *testing.T) {
		t.Parallel()
		capture := mutateSessionEstablishedByLoad(t, base)
		violations, _ := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertNoViolations(t, violations)
	})

	t.Run("S-3/session_load_then_session_new", func(t *testing.T) {
		t.Parallel()
		capture := addSecondSessionNewResponse(mutateSessionEstablishedByLoad(t, base), 4, "sess-0002")
		_, observed := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		want := []string{methodSessionLoad, methodSessionNew}
		if !slices.Equal(observed.establishedBy, want) {
			t.Errorf("recordedShapeViolations() observed.establishedBy = %v, want %v", observed.establishedBy, want)
		}
	})

	t.Run("handshake_only/no_violation_under_handshake_only", func(t *testing.T) {
		t.Parallel()
		capture := handshakeOnlyCapture(t, base)
		violations, _ := capture.violations(t, expectHandshakeOnly, nil)
		assertNoViolations(t, violations)
	})

	t.Run("handshake_only/S-4_violation_under_completed_turn", func(t *testing.T) {
		t.Parallel()
		capture := handshakeOnlyCapture(t, base)
		violations, _ := capture.violations(t, expectCompletedTurn, nil)
		assertViolationsExactly(t, violations, "S-4:", "S-6:", "S-7:")
	})

	t.Run("S-2/agentInfo_removed", func(t *testing.T) {
		t.Parallel()
		capture := mutateInitAgentInfoRemoved(t, base)
		violations, _ := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertViolationsExactly(t, violations, "S-2:")
	})

	t.Run("S-2/agentInfo_version_empty", func(t *testing.T) {
		t.Parallel()
		capture := mutateInitAgentInfoVersionEmpty(t, base)
		violations, _ := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertViolationsExactly(t, violations, "S-2:")
	})

	t.Run("S-5/stop_reason_changed", func(t *testing.T) {
		t.Parallel()
		capture := mutateStopReasonChanged(t, base, string(stopReasonMaxTurnRequests))
		violations, _ := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertViolationsExactly(t, violations, "S-5:")
	})

	t.Run("S-6/agent_message_chunk_removed", func(t *testing.T) {
		t.Parallel()
		capture := mutateEveryAgentMessageChunkRemoved(t, base)
		violations, observed := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertViolationsExactly(t, violations, "S-6:", "S-7:")
		if observed.sessionUpdates[sessionUpdateAgentMessageChunk] != 0 {
			t.Errorf("observed.sessionUpdates[%q] = %d, want 0", sessionUpdateAgentMessageChunk, observed.sessionUpdates[sessionUpdateAgentMessageChunk])
		}
	})

	t.Run("S-7/all_chunks_non_text", func(t *testing.T) {
		t.Parallel()
		capture := mutateAllAgentMessageChunksToNonText(t, base)
		violations, observed := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertViolationsExactly(t, violations, "S-7:")
		if observed.nonTextChunks == 0 {
			t.Errorf("observed.nonTextChunks = %d, want greater than 0", observed.nonTextChunks)
		}
	})

	t.Run("S-7/one_chunk_non_text", func(t *testing.T) {
		t.Parallel()
		capture := mutateOneAgentMessageChunkToNonText(t, base)
		violations, observed := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertNoViolations(t, violations)
		if observed.nonTextChunks != 1 {
			t.Errorf("observed.nonTextChunks = %d, want 1", observed.nonTextChunks)
		}
	})

	t.Run("S-9/tool_result_event_missing", func(t *testing.T) {
		t.Parallel()
		violations, observed := base.violations(t, expectToolForcingTurn, nil)
		assertViolationsExactly(t, violations, "S-9:")
		if observed.toolCallPairs == 0 {
			t.Errorf("observed.toolCallPairs = %d, want greater than 0", observed.toolCallPairs)
		}
	})

	t.Run("S-10/tool_call_removed", func(t *testing.T) {
		t.Parallel()
		capture := mutateEveryToolCallRemoved(t, base)
		violations, observed := capture.violations(t, expectToolForcingTurn, toolResultEvents)
		assertViolationsExactly(t, violations, "S-10:")
		if observed.toolCallPairs != 0 {
			t.Errorf("observed.toolCallPairs = %d, want 0", observed.toolCallPairs)
		}
	})
}
