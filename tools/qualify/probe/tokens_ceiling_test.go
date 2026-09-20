//go:build unix

package probe

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func tokenCeilingRecognizer(tokenPaths ...profile.TokenPath) profile.Recognizer {
	return profile.Recognizer{
		Locator:       profile.TerminalLocator{Mode: "first_value"},
		SuccessMember: "ok",
		SessionIDPath: []string{"session_id"},
		TokenPaths:    tokenPaths,
	}
}

var tokenCeilingSpendPath = profile.TokenPath{Path: []string{"usage", "total"}, Kind: "spend"}

func TestNativeTokenInventoryUnreadOutputIsNotAProvenZero(t *testing.T) {
	t.Parallel()

	sessionID, paths, inventory := nativeTokenInventory(tokenRecognizerProfile(tokenCeilingRecognizer()), tokenTestSurface, []string{"not a terminal"})
	if sessionID != "" || paths != nil {
		t.Errorf("nativeTokenInventory(...) = (%q, %v), want the empty sentinel", sessionID, paths)
	}
	if inventory.obs.Grade != evidence.GradeNotObserved {
		t.Errorf("nativeTokenInventory(...) inventory = %+v, want not_observed: no terminal was recognized, so no source count is known", inventory.obs)
	}
	if inventory.obs.Outcome == evidence.OutcomePass {
		t.Errorf("nativeTokenInventory(...) inventory outcome = %s, want an induction failure rather than a pass", inventory.obs.Outcome)
	}
}

func TestNativeTokenInventoryInventoriesEveryWildcardSource(t *testing.T) {
	t.Parallel()

	perModel := profile.TokenPath{Path: []string{"stats", "models", "*", "tokens", "prompt"}, Kind: "spend"}
	output := `{"ok":true,"session_id":"real-session","stats":{"models":{"model-a":{"tokens":{"prompt":11}},"model-b":{"tokens":{"prompt":22}}}}}`

	sessionID, paths, inventory := nativeTokenInventory(tokenRecognizerProfile(tokenCeilingRecognizer(perModel)), tokenTestSurface, []string{output})
	if inventory.obs.Grade == evidence.GradeGap && len(paths) == 0 {
		t.Fatalf("nativeTokenInventory(...) = (%q, %v, %+v), want the two per-model sources rather than a zero-source gap", sessionID, paths, inventory.obs)
	}
	if sessionID != "real-session" {
		t.Errorf("nativeTokenInventory(...) sessionID = %q, want %q", sessionID, "real-session")
	}
	got := map[string]bool{}
	for _, path := range paths {
		got[path.EvidencePath] = true
	}
	for _, want := range []string{"/stats/models/model-a/tokens/prompt", "/stats/models/model-b/tokens/prompt"} {
		if !got[want] {
			t.Errorf("nativeTokenInventory(...) paths = %+v, want one carrying %s", paths, want)
		}
	}
}

func TestNativeTokenInventoryRejectsANonSummableFigure(t *testing.T) {
	t.Parallel()

	output := `{"ok":true,"session_id":"real-session","usage":{"total":-5}}`

	_, paths, inventory := nativeTokenInventory(tokenRecognizerProfile(tokenCeilingRecognizer(tokenCeilingSpendPath)), tokenTestSurface, []string{output})
	if len(paths) > 0 {
		t.Errorf("nativeTokenInventory(...) paths = %+v, want no spend source for a figure no budget can sum", paths)
	}
	if inventory.obs.Outcome == evidence.OutcomePass {
		t.Errorf("nativeTokenInventory(...) inventory = %+v, want an unsupported-form failure rather than a pass", inventory.obs)
	}
}

func TestNativeTokenInventoryNamesTheUnverifiedCeilingStop(t *testing.T) {
	t.Parallel()

	output := `{"ok":true,"session_id":"real-session","usage":{"total":5}}`

	_, paths, inventory := nativeTokenInventory(tokenRecognizerProfile(tokenCeilingRecognizer(tokenCeilingSpendPath)), tokenTestSurface, []string{output})
	if len(paths) != 1 {
		t.Fatalf("nativeTokenInventory(...) paths = %+v, want the one resolved spend source", paths)
	}
	if !strings.Contains(inventory.obs.Detail, "max_tokens") {
		t.Errorf("nativeTokenInventory(...) inventory detail = %q, want it to name the max_tokens stop this measurer never induced", inventory.obs.Detail)
	}
}

func TestProtocolTokenInventoryNamesTheUnverifiedCeilingStop(t *testing.T) {
	t.Parallel()

	_, paths, inventory, _ := protocolTokenInventory(&sharedFixture{usage: &usageTracker{}, wireTraceDir: completeExtensionTrace(t)})
	if len(paths) == 0 {
		t.Fatalf("protocolTokenInventory(...) paths = %+v, want the counters the capture carried", paths)
	}
	if !strings.Contains(inventory.obs.Detail, "max_tokens") {
		t.Errorf("protocolTokenInventory(...) inventory detail = %q, want it to name the max_tokens stop this measurer never induced", inventory.obs.Detail)
	}
}

func TestNativeTokenInventoryDoesNotInventASessionID(t *testing.T) {
	t.Parallel()

	unattributed := profile.Recognizer{
		Locator:       profile.TerminalLocator{Mode: "first_value"},
		SuccessMember: "ok",
		TokenPaths:    []profile.TokenPath{tokenCeilingSpendPath},
	}
	output := `{"ok":true,"usage":{"total":5}}`

	sessionID, paths, inventory := nativeTokenInventory(tokenRecognizerProfile(unattributed), tokenTestSurface, []string{output})
	if sessionID != "" {
		t.Errorf("nativeTokenInventory(...) sessionID = %q, want empty: no observation supplied one", sessionID)
	}
	if len(paths) > 0 {
		t.Errorf("nativeTokenInventory(...) paths = %+v, want no attributed source", paths)
	}
	if inventory.obs.Outcome == evidence.OutcomePass {
		t.Errorf("nativeTokenInventory(...) inventory = %+v, want an induction failure rather than a pass", inventory.obs)
	}
}

// Deduplicating the paths must not discard the recognized observation count,
// which is separate evidence for an accumulating budget.
func TestNativeTokenInventoryKeepsTheRecognizedObservationCount(t *testing.T) {
	t.Parallel()

	first := `{"ok":true,"session_id":"real-session","usage":{"total":5}}`
	second := `{"ok":true,"session_id":"real-session","usage":{"total":9}}`

	_, _, one := nativeTokenInventory(tokenRecognizerProfile(tokenCeilingRecognizer(tokenCeilingSpendPath)), tokenTestSurface, []string{first})
	_, _, two := nativeTokenInventory(tokenRecognizerProfile(tokenCeilingRecognizer(tokenCeilingSpendPath)), tokenTestSurface, []string{first, second})
	if one.obs.Detail == two.obs.Detail {
		t.Errorf("nativeTokenInventory(...) reported the same detail %q for one and for two recognized observations", one.obs.Detail)
	}
}
