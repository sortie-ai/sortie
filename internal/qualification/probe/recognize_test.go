//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// recognizerShapedProfile returns a minimal RuntimeProfile carrying
// recognizer for SurfaceNativeJSON only, sufficient for nativeTerminal
// dispatch without any other profile member.
func recognizerShapedProfile(recognizer qualification.Recognizer) qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		Recognizers: map[qualification.Surface]qualification.Recognizer{
			qualification.SurfaceNativeJSON: recognizer,
		},
	}
}

// TestNativeTerminal covers nativeTerminal's three dispatch branches:
// a launch failure carries no terminal signal, a bounded exit or
// timeout with no recognized terminal is transport loss, and a surface
// the profile carries no recognizer for recognizes nothing.
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
		if !found || !transportLoss || terminal != (qualification.Terminal{}) {
			t.Errorf("nativeTerminal() = %+v, %v, %v, want zero Terminal, true, true for a bounded exit with no recognized terminal", terminal, transportLoss, found)
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
}

// TestNativeTerminalDispatchIsProfileDriven proves the dispatch reads
// its recognition rule from the profile argument rather than from a
// hardcoded field name. Two profiles carry differently-shaped
// native_json recognizers over the exact same raw output: one naming
// the success member a tracked structured-native profile uses today,
// and a second, kiro-shaped, control naming a different one. A driver
// that special-cased one runtime's field name would recognize the
// kiro-shaped output identically to the other one, or fail to
// recognize either, collapsing this test's two branches into one; the
// data-driven dispatch this package implements must not.
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

// writeAll issues one lineBoundedWriter.Write call per element of
// writes, in order, failing t if any call reports an error or a short
// write.
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

// TestLineBoundedWriter covers the line-retention decision table: a
// complete line within limit, a line reassembled across Write calls,
// a line found to exceed limit within one Write, and a write that
// itself exceeds limit before any newline arrives and so must discard
// through a later Write until the next newline.
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

// TestLineBoundedWriterPeak proves peak records the buffered line's
// high-water mark, reached mid-write across a line split over two
// Write calls, and left untouched by a write whose payload is dropped
// before ever reaching the pending buffer.
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
