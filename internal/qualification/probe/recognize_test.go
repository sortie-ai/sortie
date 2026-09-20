//go:build unix

package probe

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

func recognizerShapedProfile(recognizer qualification.Recognizer) qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		Recognizers: map[qualification.Surface]qualification.Recognizer{
			qualification.SurfaceNativeJSON: recognizer,
		},
	}
}

func TestNativeTerminal(t *testing.T) {
	t.Parallel()

	firstValueProfile := recognizerShapedProfile(qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "first_value"},
		SuccessMember: "response",
	})

	t.Run("a launch failure carries no terminal signal", func(t *testing.T) {
		t.Parallel()

		terminal, transportLoss, found := nativeTerminal(firstValueProfile, qualification.SurfaceNativeJSON, "", errNativeLaunchFailed)
		if found || transportLoss || terminal != (qualification.Terminal{}) {
			t.Errorf("nativeTerminal() = %+v, %v, %v, want zero Terminal, false, false for a launch failure", terminal, transportLoss, found)
		}
	})

	t.Run("a bounded exit with no recognized terminal is transport loss", func(t *testing.T) {
		t.Parallel()

		terminal, transportLoss, found := nativeTerminal(firstValueProfile, qualification.SurfaceNativeJSON, "", errNativeBoundExceeded)
		if found || !transportLoss || terminal != (qualification.Terminal{}) {
			t.Errorf("nativeTerminal() = %+v, %v, %v, want zero Terminal, true, false for a bounded exit with no recognized terminal", terminal, transportLoss, found)
		}
	})

	t.Run("a surface the profile carries no recognizer for recognizes nothing", func(t *testing.T) {
		t.Parallel()

		profile := qualification.RuntimeProfile{Recognizers: map[qualification.Surface]qualification.Recognizer{}}
		terminal, transportLoss, found := nativeTerminal(profile, qualification.SurfaceNativeJSON, `{"response":{}}`, nil)
		if found || transportLoss || terminal != (qualification.Terminal{}) {
			t.Errorf("nativeTerminal() = %+v, %v, %v, want zero Terminal, false, false when the profile carries no recognizer for the surface", terminal, transportLoss, found)
		}
	})

	t.Run("a non-zero exit with a recognized terminal is graded from that terminal, not transport loss", func(t *testing.T) {
		t.Parallel()

		nonZeroExit := errors.New("exit status 1")
		terminal, transportLoss, found := nativeTerminal(firstValueProfile, qualification.SurfaceNativeJSON, `{"response":{}}`, nonZeroExit)
		if !found || transportLoss || terminal != (qualification.Terminal{EndTurn: true}) {
			t.Errorf("nativeTerminal() = %+v, %v, %v, want {EndTurn:true}, false, true: this is gemini's own documented error contract, a recognized terminal from a non-zero exit", terminal, transportLoss, found)
		}
	})

	t.Run("a non-zero exit with nothing recognizable is still transport loss", func(t *testing.T) {
		t.Parallel()

		nonZeroExit := errors.New("exit status 1")
		terminal, transportLoss, found := nativeTerminal(firstValueProfile, qualification.SurfaceNativeJSON, "not json at all", nonZeroExit)
		if found || !transportLoss || terminal != (qualification.Terminal{}) {
			t.Errorf("nativeTerminal() = %+v, %v, %v, want zero Terminal, true, false: a non-zero exit with no recognized terminal is transport loss, the sibling to the recognized-terminal case above", terminal, transportLoss, found)
		}
	})
}

func TestNativeTerminalDispatchIsProfileDriven(t *testing.T) {
	t.Parallel()

	const output = `{"result":{"text":"hi"}}`

	responseShapedProfile := recognizerShapedProfile(qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "first_value"},
		SuccessMember: "response",
	})
	kiroShapedProfile := recognizerShapedProfile(qualification.Recognizer{
		Locator:       qualification.TerminalLocator{Mode: "first_value"},
		SuccessMember: "result",
	})

	responseTerminal, _, responseFound := nativeTerminal(responseShapedProfile, qualification.SurfaceNativeJSON, output, nil)
	if responseFound {
		t.Fatalf("nativeTerminal(responseShapedProfile, %q) = %+v, found=%v, want found=false: this profile's recognizer names success_member %q, absent from the output", output, responseTerminal, responseFound, "response")
	}

	kiroTerminal, _, kiroFound := nativeTerminal(kiroShapedProfile, qualification.SurfaceNativeJSON, output, nil)
	if !kiroFound || kiroTerminal != (qualification.Terminal{EndTurn: true}) {
		t.Fatalf("nativeTerminal(kiroShapedProfile, %q) = %+v, found=%v, want {EndTurn:true}, found=true: this profile's recognizer names success_member %q, present in the output", output, kiroTerminal, kiroFound, "result")
	}
}

func TestNativeFailureObservation(t *testing.T) {
	t.Parallel()

	t.Run("transport loss overrides the outcome to runtime failure whatever the caller passed", func(t *testing.T) {
		t.Parallel()

		obs := nativeFailureObservation("sess-1", true, qualification.OutcomePass, "caller's own detail")
		want := qualification.Observation{
			Grade:     qualification.GradeNotObserved,
			Outcome:   qualification.OutcomeRuntimeFailed,
			Detail:    "the launch ended in a bounded exit or timeout, producing no recognized terminal",
			SessionID: "sess-1",
		}
		if obs != want {
			t.Errorf("nativeFailureObservation(%q, true, %q, %q) = %+v, want %+v", "sess-1", qualification.OutcomePass, "caller's own detail", obs, want)
		}
	})

	t.Run("no transport loss passes the caller's own outcome and detail through unchanged", func(t *testing.T) {
		t.Parallel()

		obs := nativeFailureObservation("sess-2", false, qualification.OutcomeFixtureInductionFailed, "caller's own detail")
		want := qualification.Observation{
			Grade:     qualification.GradeNotObserved,
			Outcome:   qualification.OutcomeFixtureInductionFailed,
			Detail:    "caller's own detail",
			SessionID: "sess-2",
		}
		if obs != want {
			t.Errorf("nativeFailureObservation(%q, false, %q, %q) = %+v, want %+v", "sess-2", qualification.OutcomeFixtureInductionFailed, "caller's own detail", obs, want)
		}
	})
}

func writeAll(t *testing.T, w *lineBoundedWriter, writes []string) {
	t.Helper()
	for _, chunk := range writes {
		n, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q) error = %v, want nil", chunk, err)
		}
		if n != len(chunk) {
			t.Fatalf("Write(%q) = %d, want %d", chunk, n, len(chunk))
		}
	}
}

func TestLineBoundedWriter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		limit       int
		writes      []string
		flush       bool
		wantString  string
		wantDropped int
		wantPeak    int
	}{
		{
			name:       "a single write carrying two complete lines retains both",
			limit:      100,
			writes:     []string{"line1\nline2\n"},
			wantString: "line1\nline2\n",
			wantPeak:   len("line1\n"),
		},
		{
			name:       "a line split across two writes reassembles on the second",
			limit:      100,
			writes:     []string{"li", "ne1\n"},
			wantString: "line1\n",
			wantPeak:   len("line1\n"),
		},
		{
			name:        "a line found to exceed limit within one write is dropped before ever reaching the buffer",
			limit:       5,
			writes:      []string{"abcdefgh\n"},
			wantString:  "",
			wantDropped: 1,
			wantPeak:    0,
		},
		{
			name:        "a dropped oversized line does not affect a later line in the same write",
			limit:       5,
			writes:      []string{"abcdefgh\nok\n"},
			wantString:  "ok\n",
			wantDropped: 1,
			wantPeak:    len("ok\n"),
		},
		{
			name:        "a write exceeding limit with no newline yet discards through the write that supplies the newline",
			limit:       5,
			writes:      []string{"abcdefgh", "ijk\nok\n"},
			wantString:  "ok\n",
			wantDropped: 1,
			wantPeak:    len("ok\n"),
		},
		{
			name:        "discarding spans more than one write while none of them carries a newline",
			limit:       5,
			writes:      []string{"abcdefgh", "more-no-newline-either", "ijk\nok\n"},
			wantString:  "ok\n",
			wantDropped: 1,
			wantPeak:    len("ok\n"),
		},
		{
			name:       "a trailing partial line is retained only after Flush",
			limit:      100,
			writes:     []string{"partial-no-newline"},
			flush:      true,
			wantString: "partial-no-newline",
			wantPeak:   len("partial-no-newline"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := &lineBoundedWriter{limit: tt.limit}
			writeAll(t, w, tt.writes)
			if tt.flush {
				w.Flush()
			}

			if got := w.String(); got != tt.wantString {
				t.Errorf("String() = %q, want %q", got, tt.wantString)
			}
			if w.dropped != tt.wantDropped {
				t.Errorf("dropped = %d, want %d", w.dropped, tt.wantDropped)
			}
			if w.peak != tt.wantPeak {
				t.Errorf("peak = %d, want %d", w.peak, tt.wantPeak)
			}
		})
	}
}

func TestLineBoundedWriterPeak(t *testing.T) {
	t.Parallel()

	t.Run("peak reaches the full reassembled line length", func(t *testing.T) {
		t.Parallel()

		w := &lineBoundedWriter{limit: 100}
		writeAll(t, w, []string{"abc", "defgh\n"})
		if w.peak != len("abcdefgh\n") {
			t.Errorf("peak = %d, want %d", w.peak, len("abcdefgh\n"))
		}
	})

	t.Run("a write dropped before buffering leaves peak at zero", func(t *testing.T) {
		t.Parallel()

		w := &lineBoundedWriter{limit: 5}
		writeAll(t, w, []string{"abcdefghij"})
		if w.peak != 0 {
			t.Errorf("peak = %d, want 0: the oversized write must never reach buffer", w.peak)
		}
	})
}
