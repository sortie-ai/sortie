package eval

import (
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func recognizerShapedProfile(recognizer profile.Recognizer) profile.RuntimeProfile {
	return profile.RuntimeProfile{
		Recognizers: map[evidence.Surface]profile.Recognizer{
			evidence.SurfaceNativeJSON: recognizer,
		},
	}
}

func TestNativeTerminalFrom(t *testing.T) {
	t.Parallel()

	firstValueProfile := recognizerShapedProfile(profile.Recognizer{
		Locator:       profile.TerminalLocator{Mode: "first_value"},
		SuccessMember: "response",
	})

	t.Run("a launch failure carries no terminal signal", func(t *testing.T) {
		t.Parallel()

		terminal, transportLoss, found := nativeTerminalFrom(firstValueProfile, evidence.SurfaceNativeJSON, "", evidence.LaunchOutcomeLaunchFailed)
		if found || transportLoss || terminal != (profile.Terminal{}) {
			t.Errorf("nativeTerminalFrom() = %+v, %v, %v, want zero Terminal, false, false for a launch failure", terminal, transportLoss, found)
		}
	})

	t.Run("a bounded exit with no recognized terminal is transport loss", func(t *testing.T) {
		t.Parallel()

		terminal, transportLoss, found := nativeTerminalFrom(firstValueProfile, evidence.SurfaceNativeJSON, "", evidence.LaunchOutcomeRunFailed)
		if found || !transportLoss || terminal != (profile.Terminal{}) {
			t.Errorf("nativeTerminalFrom() = %+v, %v, %v, want zero Terminal, true, false for a bounded exit with no recognized terminal", terminal, transportLoss, found)
		}
	})

	t.Run("a surface the profile carries no recognizer for recognizes nothing", func(t *testing.T) {
		t.Parallel()

		p := profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{}}
		terminal, transportLoss, found := nativeTerminalFrom(p, evidence.SurfaceNativeJSON, `{"response":{}}`, evidence.LaunchOutcomeCompleted)
		if found || transportLoss || terminal != (profile.Terminal{}) {
			t.Errorf("nativeTerminalFrom() = %+v, %v, %v, want zero Terminal, false, false when the profile carries no recognizer for the surface", terminal, transportLoss, found)
		}
	})

	t.Run("a run failure with a recognized terminal is graded from that terminal, not transport loss", func(t *testing.T) {
		t.Parallel()

		terminal, transportLoss, found := nativeTerminalFrom(firstValueProfile, evidence.SurfaceNativeJSON, `{"response":{}}`, evidence.LaunchOutcomeRunFailed)
		if !found || transportLoss || terminal != (profile.Terminal{EndTurn: true}) {
			t.Errorf("nativeTerminalFrom() = %+v, %v, %v, want {EndTurn:true}, false, true: this is gemini's own documented error contract, a recognized terminal from a non-zero exit", terminal, transportLoss, found)
		}
	})

	t.Run("a run failure with nothing recognizable is still transport loss", func(t *testing.T) {
		t.Parallel()

		terminal, transportLoss, found := nativeTerminalFrom(firstValueProfile, evidence.SurfaceNativeJSON, "not json at all", evidence.LaunchOutcomeRunFailed)
		if found || !transportLoss || terminal != (profile.Terminal{}) {
			t.Errorf("nativeTerminalFrom() = %+v, %v, %v, want zero Terminal, true, false: a run failure with no recognized terminal is transport loss, the sibling to the recognized-terminal case above", terminal, transportLoss, found)
		}
	})
}

func TestNativeTerminalFromDispatchIsProfileDriven(t *testing.T) {
	t.Parallel()

	const output = `{"result":{"text":"hi"}}`

	responseShapedProfile := recognizerShapedProfile(profile.Recognizer{
		Locator:       profile.TerminalLocator{Mode: "first_value"},
		SuccessMember: "response",
	})
	kiroShapedProfile := recognizerShapedProfile(profile.Recognizer{
		Locator:       profile.TerminalLocator{Mode: "first_value"},
		SuccessMember: "result",
	})

	responseTerminal, _, responseFound := nativeTerminalFrom(responseShapedProfile, evidence.SurfaceNativeJSON, output, evidence.LaunchOutcomeCompleted)
	if responseFound {
		t.Fatalf("nativeTerminalFrom(responseShapedProfile, %q) = %+v, found=%v, want found=false: this profile's recognizer names success_member %q, absent from the output", output, responseTerminal, responseFound, "response")
	}

	kiroTerminal, _, kiroFound := nativeTerminalFrom(kiroShapedProfile, evidence.SurfaceNativeJSON, output, evidence.LaunchOutcomeCompleted)
	if !kiroFound || kiroTerminal != (profile.Terminal{EndTurn: true}) {
		t.Fatalf("nativeTerminalFrom(kiroShapedProfile, %q) = %+v, found=%v, want {EndTurn:true}, found=true: this profile's recognizer names success_member %q, present in the output", output, kiroTerminal, kiroFound, "result")
	}
}

func TestResolvedSessionID(t *testing.T) {
	t.Parallel()

	output := `{"type":"result","status":"end_turn","session_id":"sess-native"}`
	withSessionIDPath := profile.Recognizer{
		Locator:       profile.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
		SessionIDPath: []string{"session_id"},
	}
	noSessionIDPath := profile.Recognizer{
		Locator: profile.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
	}

	tests := []struct {
		name   string
		p      profile.RuntimeProfile
		l      evidence.LaunchRecord
		output string
		want   string
	}{
		{
			name:   "no recognizer for the surface reports empty",
			p:      profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{}},
			output: output,
			want:   "",
		},
		{
			name:   "a recognizer stating no session_id_path reports empty",
			p:      profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{evidence.SurfaceNativeJSON: noSessionIDPath}},
			output: output,
			want:   "",
		},
		{
			name:   "a resolvable session_id_path reports its value",
			p:      profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{evidence.SurfaceNativeJSON: withSessionIDPath}},
			output: output,
			want:   "sess-native",
		},
		{
			name:   "a seeded session id occurring literally in output falls back",
			p:      profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{evidence.SurfaceNativeJSON: noSessionIDPath}},
			l:      evidence.LaunchRecord{SeededSessionID: "sess-seeded"},
			output: `{"type":"result","status":"end_turn","echo":"sess-seeded"}`,
			want:   "sess-seeded",
		},
		{
			name:   "a seeded session id absent from output is never reported",
			p:      profile.RuntimeProfile{Recognizers: map[evidence.Surface]profile.Recognizer{evidence.SurfaceNativeJSON: noSessionIDPath}},
			l:      evidence.LaunchRecord{SeededSessionID: "sess-seeded"},
			output: output,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := resolvedSessionID(tt.p, evidence.SurfaceNativeJSON, tt.l, tt.output); got != tt.want {
				t.Errorf("resolvedSessionID(..., %q, %q) = %q, want %q", evidence.SurfaceNativeJSON, tt.output, got, tt.want)
			}
		})
	}
}

func TestNativeFailureObservation(t *testing.T) {
	t.Parallel()

	t.Run("transport loss overrides the outcome to runtime failure whatever the caller passed", func(t *testing.T) {
		t.Parallel()

		obs := nativeFailureObservation("sess-1", true, evidence.OutcomePass, "caller's own detail")
		want := evidence.Observation{
			Grade:     evidence.GradeNotObserved,
			Outcome:   evidence.OutcomeRuntimeFailed,
			Detail:    "the launch ended in a bounded exit or timeout, producing no recognized terminal",
			SessionID: "sess-1",
		}
		if obs != want {
			t.Errorf("nativeFailureObservation(%q, true, %q, %q) = %+v, want %+v", "sess-1", evidence.OutcomePass, "caller's own detail", obs, want)
		}
	})

	t.Run("no transport loss passes the caller's own outcome and detail through unchanged", func(t *testing.T) {
		t.Parallel()

		obs := nativeFailureObservation("sess-2", false, evidence.OutcomeFixtureInductionFailed, "caller's own detail")
		want := evidence.Observation{
			Grade:     evidence.GradeNotObserved,
			Outcome:   evidence.OutcomeFixtureInductionFailed,
			Detail:    "caller's own detail",
			SessionID: "sess-2",
		}
		if obs != want {
			t.Errorf("nativeFailureObservation(%q, false, %q, %q) = %+v, want %+v", "sess-2", evidence.OutcomeFixtureInductionFailed, "caller's own detail", obs, want)
		}
	})
}

func TestAnswerDeclinesRecall(t *testing.T) {
	t.Parallel()

	const controlNonce = "REVIEW_NONCE"
	const controlDeclinedAnswer = `I appreciate you testing my consistency, but I need to be direct: I don't retain information across distinct user messages in a way that allows me to replay nonces or other values on demand as a verification mechanism.

Each message I receive is processed independently. While I can see the conversation history within a single session, I'm not designed to act as a secure storage or recall system for cryptographic nonces or similar sensitive identifiers.

If you need to verify session continuity or test my behavior, I'm happy to help with that in a different way. What are you actually trying to accomplish?`

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
			answer: "I’m not going to repeat that value.",
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
