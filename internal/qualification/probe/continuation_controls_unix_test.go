//go:build unix

package probe

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

const (
	controlNonce           = "REVIEW_NONCE"
	controlSeedSessionID   = "real-session"
	controlFreshSessionID  = "fresh-session"
	controlSeedNativeOutpt = `{"ok":true,"session_id":"real-session"}`
)

// controlDeclinedAnswer is the answer a live kiro-cli run gave the recall
// prompt over the client protocol, with both turns in one session journal: the
// model declined to repeat the value rather than reporting it held none.
const controlDeclinedAnswer = `I appreciate you testing my consistency, but I need to be direct: I don't retain information across distinct user messages in a way that allows me to replay nonces or other values on demand as a verification mechanism.

Each message I receive is processed independently. While I can see the conversation history within a single session, I'm not designed to act as a secure storage or recall system for cryptographic nonces or similar sensitive identifiers.

If you need to verify session continuity or test my behavior, I'm happy to help with that in a different way. What are you actually trying to accomplish?`

const continuationNativeScenario = "continuation-native"

type continuationNativeParams struct {
	SeedOutput   string
	RecallOutput string
}

func runContinuationNative(args []string, params continuationNativeParams) int {
	joined := strings.Join(args, "\x00")
	var output string
	switch {
	case strings.Contains(joined, "SEED_MARKER"):
		output = params.SeedOutput
	case strings.Contains(joined, "RECALL_MARKER"):
		output = params.RecallOutput
	default:
		return 1
	}
	if _, err := fmt.Fprint(os.Stdout, output); err != nil {
		return 2
	}
	return 0
}

func init() {
	probeScenarios[continuationNativeScenario] = agenttest.Typed(runContinuationNative)
}

const tokenTestSurface = qualification.SurfaceNativeJSON

func tokenRecognizerProfile(recognizer qualification.Recognizer) qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		ProbePrompts: map[string]string{
			"continuation_seed":   "SEED_MARKER",
			"continuation_recall": "RECALL_MARKER",
		},
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			tokenTestSurface: {Args: []string{"--prompt", "{prompt}"}},
		},
		Recognizers: map[qualification.Surface]qualification.Recognizer{
			tokenTestSurface: recognizer,
		},
	}
}

// continuationStreamScenario writes one canned answer per launch with stdout
// and stderr under the test's control, so a control can put the nonce on a
// stream the runtime never answers on.
const continuationStreamScenario = "continuation-native-streams"

type continuationStreamParams struct {
	ReceiptDir   string
	SeedStdout   string
	RecallStdout string
	RecallStderr string
}

func runContinuationStreams(args []string, params continuationStreamParams) int {
	joined := strings.Join(args, "\x00")
	var name, stdout, stderr string
	switch {
	case strings.Contains(joined, "SEED_MARKER"):
		name, stdout = "seed", params.SeedStdout
	case strings.Contains(joined, "RECALL_MARKER"):
		name, stdout, stderr = "recall", params.RecallStdout, params.RecallStderr
	default:
		return 1
	}
	if params.ReceiptDir != "" {
		if err := os.WriteFile(filepath.Join(params.ReceiptDir, name+".pid"), fmt.Appendf(nil, "%d", os.Getpid()), 0o600); err != nil {
			return 2
		}
	}
	if _, err := fmt.Fprint(os.Stdout, stdout); err != nil {
		return 2
	}
	if _, err := fmt.Fprint(os.Stderr, stderr); err != nil {
		return 2
	}
	return 0
}

func init() {
	probeScenarios[continuationStreamScenario] = agenttest.Typed(runContinuationStreams)
	probeScenarios[continuationACPScenario] = agenttest.Typed(runContinuationACPAgent)
}

// continuationRecallProfile is the minimal native continuation profile:
// resume_args but no seed_args, so the seed's identifier can come only from the
// seed launch's own output.
func continuationRecallProfile() qualification.RuntimeProfile {
	profile := tokenRecognizerProfile(qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "first_value"},
		ErrorMembers:  []string{"error"},
		SuccessMember: "ok",
		SessionIDPath: []string{"session_id"},
	})
	entry := profile.EntryPoints[tokenTestSurface]
	entry.ResumeArgs = []string{"--resume", "{session_id}"}
	profile.EntryPoints[tokenTestSurface] = entry
	return profile
}

func assertRecallObservation(t *testing.T, recall qualification.Observation, wantGrade qualification.Grade, wantOutcome qualification.Outcome, wantDetail, wantSessionID string) {
	t.Helper()

	if uuidV4Pattern.MatchString(recall.SessionID) {
		t.Errorf("recall session id = %q, want an identifier the run observed rather than a generated one", recall.SessionID)
	}
	if recall.Grade != wantGrade || recall.Outcome != wantOutcome || recall.Detail != wantDetail {
		t.Errorf("recall = {%s %s %s}, want {%s %s %s}", recall.Grade, recall.Outcome, recall.Detail, wantGrade, wantOutcome, wantDetail)
	}
	if recall.SessionID != wantSessionID {
		t.Errorf("recall session id = %q, want %q", recall.SessionID, wantSessionID)
	}
}

func TestInduceNativeContinuationRecallEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		recallOutput  string
		wantGrade     qualification.Grade
		wantOutcome   qualification.Outcome
		wantDetail    string
		wantSessionID string
	}{
		{
			name:          "an answer carrying the nonce confirms recall in the seed's own session",
			recallOutput:  `{"ok":true,"session_id":"real-session","response":"REVIEW_NONCE"}`,
			wantGrade:     qualification.GradeUsable,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallConfirmedSameSession,
			wantSessionID: controlSeedSessionID,
		},
		{
			name:          "a nonce replayed outside the answer confirms nothing",
			recallOutput:  "replayed old user message: REVIEW_NONCE\n" + `{"ok":true,"session_id":"real-session","response":"I do not remember"}`,
			wantGrade:     qualification.GradeGap,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallSameSessionWithoutRecall,
			wantSessionID: controlSeedSessionID,
		},
		{
			name:          "the seed's own session answering without the nonce is that session without recall",
			recallOutput:  `{"ok":true,"session_id":"real-session","response":"I do not remember"}`,
			wantGrade:     qualification.GradeGap,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallSameSessionWithoutRecall,
			wantSessionID: controlSeedSessionID,
		},
		{
			name:          "an answer declining the request reports nothing about the seed's own session",
			recallOutput:  `{"ok":true,"session_id":"real-session","response":"I won't repeat that value."}`,
			wantGrade:     qualification.GradeNotObserved,
			wantOutcome:   qualification.OutcomeFixtureInductionFailed,
			wantDetail:    qualification.RecallDeclined,
			wantSessionID: controlSeedSessionID,
		},
		{
			name:          "a different session identifier is the fresh fallback it reports",
			recallOutput:  `{"ok":true,"session_id":"fresh-session","response":"I do not remember"}`,
			wantGrade:     qualification.GradeGap,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallFreshFallback,
			wantSessionID: controlFreshSessionID,
		},
		{
			name:          "an error terminal after the seed loaded observes no recall at all",
			recallOutput:  `{"error":"the model call failed","session_id":"real-session"}`,
			wantGrade:     qualification.GradeNotObserved,
			wantOutcome:   qualification.OutcomeRuntimeFailed,
			wantDetail:    qualification.RecallUnobservedActual,
			wantSessionID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script := agenttest.FakeRuntime(t, t.TempDir(), "native", continuationNativeScenario, continuationNativeParams{
				SeedOutput:   controlSeedNativeOutpt,
				RecallOutput: tt.recallOutput,
			})
			fixture := semanticFixture(t)
			fixture.nonce = controlNonce

			seed, recall := induceNativeContinuation(t, Coordinates{CommandPath: script, Profile: continuationRecallProfile()}, fixture, tokenTestSurface)

			if seed.Grade != qualification.GradeUsable || seed.SessionID != controlSeedSessionID {
				t.Errorf("seed = %+v, want usable in the session %q its own output reported", seed, controlSeedSessionID)
			}
			assertRecallObservation(t, recall, tt.wantGrade, tt.wantOutcome, tt.wantDetail, tt.wantSessionID)
		})
	}
}

func TestInduceNativeContinuationReadsAStreamedSessionIdentifier(t *testing.T) {
	t.Parallel()

	profile := tokenRecognizerProfile(qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
		ErrorMembers:  []string{"error"},
		StatusMember:  "status",
		StatusEndTurn: []string{"end_turn"},
		SessionIDPath: []string{"session_id"},
	})
	entry := profile.EntryPoints[tokenTestSurface]
	entry.ResumeArgs = []string{"--resume", "{session_id}"}
	profile.EntryPoints[tokenTestSurface] = entry

	// The session identifier arrives in the launch's first event and the
	// terminal never repeats it, the shape a streaming surface reports.
	script := agenttest.FakeRuntime(t, t.TempDir(), "native", continuationNativeScenario, continuationNativeParams{
		SeedOutput:   `{"type":"init","session_id":"stream-session"}` + "\n" + `{"type":"result","status":"end_turn"}`,
		RecallOutput: `{"type":"init","session_id":"stream-session"}` + "\n" + `{"type":"result","status":"end_turn","response":"REVIEW_NONCE"}`,
	})
	fixture := semanticFixture(t)
	fixture.nonce = controlNonce

	seed, recall := induceNativeContinuation(t, Coordinates{CommandPath: script, Profile: profile}, fixture, tokenTestSurface)

	if seed.SessionID != "stream-session" {
		t.Errorf("seed session id = %q, want the identifier %q the launch announced outside its terminal", seed.SessionID, "stream-session")
	}
	assertRecallObservation(t, recall, qualification.GradeUsable, qualification.OutcomePass, qualification.RecallConfirmedSameSession, "stream-session")
}

// continuationStreamingProfile is the minimal streaming native profile.
// Its seed prompt carries the nonce, so a launch echoing that prompt back
// reproduces the one channel a nonce reaches a recall through without the
// runtime remembering anything.
func continuationStreamingProfile() qualification.RuntimeProfile {
	profile := tokenRecognizerProfile(qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
		ErrorMembers:  []string{"error"},
		StatusMember:  "status",
		StatusEndTurn: []string{"end_turn"},
		SessionIDPath: []string{"session_id"},
	})
	entry := profile.EntryPoints[tokenTestSurface]
	entry.ResumeArgs = []string{"--resume", "{session_id}"}
	profile.EntryPoints[tokenTestSurface] = entry
	profile.ProbePrompts[promptKeyContinuationSeed] = controlStreamSeedPrompt
	return profile
}

// Neither the init announcement nor the terminal carries the turn's text.
const (
	controlStreamSeedPrompt = "SEED_MARKER remember {nonce}"
	controlStreamResult     = `{"type":"result","timestamp":"2026-09-19T00:00:02.000Z","status":"end_turn","stats":{"total_tokens":12}}`
)

func streamInitRecord(t *testing.T, sessionID string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"type":       "init",
		"timestamp":  "2026-09-19T00:00:00.000Z",
		"session_id": sessionID,
		"model":      "probe-model",
	})
	if err != nil {
		t.Fatalf("encode the init record: %v", err)
	}
	return string(encoded)
}

func streamMessageRecord(t *testing.T, role, text string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"type":      "message",
		"timestamp": "2026-09-19T00:00:01.000Z",
		"role":      role,
		"content":   text,
		"delta":     role == "assistant",
	})
	if err != nil {
		t.Fatalf("encode the %s message record: %v", role, err)
	}
	return string(encoded)
}

func streamingLaunchOutput(t *testing.T, sessionID, prompt, replayed string, deltas ...string) string {
	t.Helper()
	records := []string{streamInitRecord(t, sessionID), streamMessageRecord(t, "user", prompt)}
	if replayed != "" {
		records = append(records, streamMessageRecord(t, "user", replayed))
	}
	for _, delta := range deltas {
		records = append(records, streamMessageRecord(t, "assistant", delta))
	}
	return strings.Join(append(records, controlStreamResult), "\n")
}

func TestInduceNativeContinuationReadsAStreamedAnswer(t *testing.T) {
	t.Parallel()

	seededPrompt := strings.ReplaceAll(controlStreamSeedPrompt, "{nonce}", controlNonce)

	tests := []struct {
		name        string
		sessionID   string
		replayed    string
		deltas      []string
		wantGrade   qualification.Grade
		wantOutcome qualification.Outcome
		wantDetail  string
	}{
		{
			name:        "an answer streamed in deltas confirms recall in the seed's own session",
			deltas:      []string{"REVIEW_", "NONCE"},
			wantGrade:   qualification.GradeUsable,
			wantOutcome: qualification.OutcomePass,
			wantDetail:  qualification.RecallConfirmedSameSession,
		},
		{
			name:        "a streamed answer without the nonce is that session without recall",
			deltas:      []string{"I do not ", "remember"},
			wantGrade:   qualification.GradeGap,
			wantOutcome: qualification.OutcomePass,
			wantDetail:  qualification.RecallSameSessionWithoutRecall,
		},
		{
			name:        "a decline streamed across chunks reports nothing about that session",
			deltas:      []string{"I am not de", "signed to repeat that value."},
			wantGrade:   qualification.GradeNotObserved,
			wantOutcome: qualification.OutcomeFixtureInductionFailed,
			wantDetail:  qualification.RecallDeclined,
		},
		{
			name:        "a nonce the launch only replays from the seed's own prompt confirms nothing",
			replayed:    seededPrompt,
			deltas:      []string{"I do not ", "remember"},
			wantGrade:   qualification.GradeGap,
			wantOutcome: qualification.OutcomePass,
			wantDetail:  qualification.RecallSameSessionWithoutRecall,
		},
		{
			name:        "a nonce carried only by the session identifier is not an answer",
			sessionID:   controlSeedSessionID + "-" + controlNonce,
			deltas:      []string{"I do not ", "remember"},
			wantGrade:   qualification.GradeGap,
			wantOutcome: qualification.OutcomePass,
			wantDetail:  qualification.RecallSameSessionWithoutRecall,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sessionID := cmp.Or(tt.sessionID, controlSeedSessionID)
			script := agenttest.FakeRuntime(t, t.TempDir(), "native", continuationNativeScenario, continuationNativeParams{
				SeedOutput:   streamingLaunchOutput(t, sessionID, seededPrompt, "", "STORED"),
				RecallOutput: streamingLaunchOutput(t, sessionID, "RECALL_MARKER", tt.replayed, tt.deltas...),
			})
			fixture := semanticFixture(t)
			fixture.nonce = controlNonce

			seed, recall := induceNativeContinuation(t, Coordinates{CommandPath: script, Profile: continuationStreamingProfile()}, fixture, tokenTestSurface)

			if seed.Grade != qualification.GradeUsable || seed.SessionID != sessionID {
				t.Errorf("seed = %+v, want usable in the session %q the launch announced", seed, sessionID)
			}
			assertRecallObservation(t, recall, tt.wantGrade, tt.wantOutcome, tt.wantDetail, sessionID)
		})
	}
}

func TestInduceNativeContinuationReadsOnlyTheAnsweringStream(t *testing.T) {
	t.Parallel()

	receipts := t.TempDir()
	script := agenttest.FakeRuntime(t, t.TempDir(), "native", continuationStreamScenario, continuationStreamParams{
		ReceiptDir:   receipts,
		SeedStdout:   controlSeedNativeOutpt,
		RecallStdout: `{"ok":true,"session_id":"real-session","response":"I do not remember"}`,
		RecallStderr: "debug: prior turn stored the nonce REVIEW_NONCE\n",
	})
	fixture := semanticFixture(t)
	fixture.nonce = controlNonce

	_, recall := induceNativeContinuation(t, Coordinates{CommandPath: script, Profile: continuationRecallProfile()}, fixture, tokenTestSurface)

	t.Run("a nonce on standard error does not confirm recall", func(t *testing.T) {
		assertRecallObservation(t, recall, qualification.GradeGap, qualification.OutcomePass, qualification.RecallSameSessionWithoutRecall, controlSeedSessionID)
	})

	t.Run("the seed and the recall are separate processes", func(t *testing.T) {
		seedPID := readReceipt(t, receipts, "seed.pid")
		recallPID := readReceipt(t, receipts, "recall.pid")
		if seedPID == recallPID {
			t.Errorf("seed pid = recall pid = %q, want the recall to resume from a process that never held the seed turn in memory", seedPID)
		}
	})
}

func TestInduceProtocolContinuationAccountsForItsRecallSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := agenttest.FakeRuntime(t, dir, "acp-agent", continuationACPScenario, continuationACPParams{
		SessionID:    controlSeedSessionID,
		LoadOutcome:  acpLoadReplaysUser,
		ReplayText:   "remember " + controlNonce,
		AnswerChunks: []string{controlNonce},
	})
	fixture := semanticFixture(t)
	fixture.nonce = controlNonce

	induceProtocolContinuation(t, Coordinates{CommandPath: script, Profile: continuationACPProfile()}, fixture)

	cleanup := induceProcessCleanup(fixture)
	if cleanup.Grade != qualification.GradeUsable || !strings.Contains(cleanup.Detail, "stopped_sessions=1") {
		t.Errorf("induceProcessCleanup(...) = %+v, want usable over the one recall session the continuation induction left open: a session stopped by a teardown of its own is never the cleanup reading's subject", cleanup)
	}
}

const continuationACPScenario = "continuation-acp-agent"

type acpLoadOutcome string

const (
	acpLoadReplaysUser  acpLoadOutcome = "replay_user_message"
	acpLoadReplaysAgent acpLoadOutcome = "replay_agent_message"
	acpLoadFails        acpLoadOutcome = "error"
)

type continuationACPParams struct {
	SessionID         string
	FallbackSessionID string
	LoadOutcome       acpLoadOutcome
	ReplayText        string
	AnswerChunks      []string
	PromptFails       bool
}

// runContinuationACPAgent answers the calls one continuation induction makes.
// Seed and recall launches are separate processes, told apart by whether a
// session/load arrived before session/new.
func runContinuationACPAgent(_ []string, params continuationACPParams) int {
	out := bufio.NewWriter(os.Stdout)
	sessionID := params.SessionID
	loadSeen := false

	respond := func(id json.RawMessage, payload string) int {
		if _, err := fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,%s}`+"\n", id, payload); err != nil {
			return 2
		}
		if err := out.Flush(); err != nil {
			return 2
		}
		return 0
	}
	notifyChunk := func(variant, text string) int {
		chunk, err := json.Marshal(map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": variant,
				"content":       map[string]any{"type": "text", "text": text},
			},
		})
		if err != nil {
			return 2
		}
		if _, err := fmt.Fprintf(out, `{"jsonrpc":"2.0","method":"session/update","params":%s}`+"\n", chunk); err != nil {
			return 2
		}
		if err := out.Flush(); err != nil {
			return 2
		}
		return 0
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var header struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
			continue
		}
		if header.Method == "" || len(header.ID) == 0 {
			continue
		}

		var code int
		switch header.Method {
		case "initialize":
			code = respond(header.ID, `"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`)
		case "session/new":
			if loadSeen && params.FallbackSessionID != "" {
				sessionID = params.FallbackSessionID
			}
			code = respond(header.ID, fmt.Sprintf(`"result":{"sessionId":%q}`, sessionID))
		case "session/load":
			loadSeen = true
			var load struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal(header.Params, &load); err == nil && load.SessionID != "" {
				sessionID = load.SessionID
			}
			if params.LoadOutcome == acpLoadFails {
				code = respond(header.ID, `"error":{"code":-32603,"message":"this session cannot be loaded"}`)
				break
			}
			variant := "user_message_chunk"
			if params.LoadOutcome == acpLoadReplaysAgent {
				variant = "agent_message_chunk"
			}
			if code = notifyChunk(variant, params.ReplayText); code != 0 {
				return code
			}
			code = respond(header.ID, `"result":{}`)
		case "session/prompt":
			recalling := strings.Contains(string(header.Params), "RECALL_MARKER")
			if recalling && params.PromptFails {
				code = respond(header.ID, `"error":{"code":-32603,"message":"the model call failed"}`)
				break
			}
			chunks := []string{"STORED"}
			if recalling {
				chunks = params.AnswerChunks
			}
			for _, chunk := range chunks {
				if code = notifyChunk("agent_message_chunk", chunk); code != 0 {
					return code
				}
			}
			code = respond(header.ID, `"result":{"stopReason":"end_turn"}`)
		default:
			code = respond(header.ID, `"error":{"code":-32601,"message":"method not found"}`)
		}
		if code != 0 {
			return code
		}
	}
	return 0
}

func continuationACPProfile() qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		ProbePrompts: map[string]string{
			promptKeyContinuationSeed:   "SEED_MARKER remember {nonce}",
			promptKeyContinuationRecall: "RECALL_MARKER report the nonce",
		},
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			qualification.SurfaceProtocol: {Args: []string{"--acp"}},
		},
	}
}

func TestInduceProtocolContinuationRecallEvidence(t *testing.T) {
	t.Parallel()

	binaries := t.TempDir()

	tests := []struct {
		name          string
		params        continuationACPParams
		wantGrade     qualification.Grade
		wantOutcome   qualification.Outcome
		wantDetail    string
		wantSessionID string
	}{
		{
			name: "an answer carrying the nonce confirms recall in the loaded session",
			params: continuationACPParams{
				SessionID:    controlSeedSessionID,
				LoadOutcome:  acpLoadReplaysUser,
				ReplayText:   "remember " + controlNonce,
				AnswerChunks: []string{controlNonce},
			},
			wantGrade:     qualification.GradeUsable,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallConfirmedSameSession,
			wantSessionID: controlSeedSessionID,
		},
		{
			name: "an answer streamed in chunks confirms recall in the loaded session",
			params: continuationACPParams{
				SessionID:    controlSeedSessionID,
				LoadOutcome:  acpLoadReplaysUser,
				ReplayText:   "remember " + controlNonce,
				AnswerChunks: []string{"REVIEW_", "NONCE"},
			},
			wantGrade:     qualification.GradeUsable,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallConfirmedSameSession,
			wantSessionID: controlSeedSessionID,
		},
		{
			name: "a nonce carried only by the load's own replay confirms nothing",
			params: continuationACPParams{
				SessionID:    controlSeedSessionID,
				LoadOutcome:  acpLoadReplaysAgent,
				ReplayText:   "stored the nonce " + controlNonce,
				AnswerChunks: []string{"I do not remember"},
			},
			wantGrade:     qualification.GradeGap,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallSameSessionWithoutRecall,
			wantSessionID: controlSeedSessionID,
		},
		{
			name: "the loaded session answering without the nonce is that session without recall",
			params: continuationACPParams{
				SessionID:    controlSeedSessionID,
				LoadOutcome:  acpLoadReplaysUser,
				ReplayText:   "remember " + controlNonce,
				AnswerChunks: []string{"I do not remember"},
			},
			wantGrade:     qualification.GradeGap,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallSameSessionWithoutRecall,
			wantSessionID: controlSeedSessionID,
		},
		{
			name: "an answer declining the request reports nothing about the loaded session",
			params: continuationACPParams{
				SessionID:    controlSeedSessionID,
				LoadOutcome:  acpLoadReplaysUser,
				ReplayText:   "remember " + controlNonce,
				AnswerChunks: []string{controlDeclinedAnswer},
			},
			wantGrade:     qualification.GradeNotObserved,
			wantOutcome:   qualification.OutcomeFixtureInductionFailed,
			wantDetail:    qualification.RecallDeclined,
			wantSessionID: controlSeedSessionID,
		},
		{
			name: "a decline streamed across chunks reports nothing about the loaded session",
			params: continuationACPParams{
				SessionID:    controlSeedSessionID,
				LoadOutcome:  acpLoadReplaysUser,
				ReplayText:   "remember " + controlNonce,
				AnswerChunks: []string{"I am not de", "signed to repeat that value."},
			},
			wantGrade:     qualification.GradeNotObserved,
			wantOutcome:   qualification.OutcomeFixtureInductionFailed,
			wantDetail:    qualification.RecallDeclined,
			wantSessionID: controlSeedSessionID,
		},
		{
			name: "a turn that failed after a confirmed load observes no recall",
			params: continuationACPParams{
				SessionID:   controlSeedSessionID,
				LoadOutcome: acpLoadReplaysUser,
				ReplayText:  "remember " + controlNonce,
				PromptFails: true,
			},
			wantGrade:   qualification.GradeNotObserved,
			wantOutcome: qualification.OutcomeRuntimeFailed,
			wantDetail:  qualification.RecallUnobservedActual,
		},
		{
			name: "a load the runtime refused falls back to a session it reports itself",
			params: continuationACPParams{
				SessionID:         controlSeedSessionID,
				FallbackSessionID: controlFreshSessionID,
				LoadOutcome:       acpLoadFails,
				AnswerChunks:      []string{"I do not remember"},
			},
			wantGrade:     qualification.GradeGap,
			wantOutcome:   qualification.OutcomePass,
			wantDetail:    qualification.RecallFreshFallback,
			wantSessionID: controlFreshSessionID,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A directory named without the subtest's own spaces: the
			// adapter splits its command on whitespace.
			dir := filepath.Join(binaries, fmt.Sprintf("case-%d", i))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("create the fake agent's own directory: %v", err)
			}
			script := agenttest.FakeRuntime(t, dir, "acp-agent", continuationACPScenario, tt.params)

			fixture := semanticFixture(t)
			fixture.nonce = controlNonce

			seed, recall := induceProtocolContinuation(t, Coordinates{CommandPath: script, Profile: continuationACPProfile()}, fixture)

			if seed.Grade != qualification.GradeUsable || seed.SessionID != controlSeedSessionID {
				t.Errorf("seed = %+v, want usable in the session %q the runtime reported", seed, controlSeedSessionID)
			}
			assertRecallObservation(t, recall, tt.wantGrade, tt.wantOutcome, tt.wantDetail, tt.wantSessionID)
		})
	}
}

func TestAnswerDeclinesRecall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		answer string
		want   bool
	}{
		{
			name:   "the answer a live runtime declined the recall prompt with",
			answer: controlDeclinedAnswer,
			want:   true,
		},
		{
			name:   "a decline written with a typographic apostrophe",
			answer: "I\u2019m not going to repeat that value.",
			want:   true,
		},
		{
			name:   "an answer reporting no memory of the seed turn",
			answer: "I do not remember",
			want:   false,
		},
		{
			name:   "an answer reporting that the history never arrived",
			answer: "I am not designed to see anything from a previous turn, and I have none here.",
			want:   false,
		},
		{
			name:   "the nonce itself",
			answer: controlNonce,
			want:   false,
		},
		{
			name:   "no answer at all",
			answer: "",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := answerDeclinesRecall(tt.answer); got != tt.want {
				t.Errorf("answerDeclinesRecall(%q) = %t, want %t", tt.answer, got, tt.want)
			}
		})
	}
}
