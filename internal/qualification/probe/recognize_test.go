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
