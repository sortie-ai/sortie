//go:build unix

package clientprotocol

import (
	"strings"
	"testing"
)

// TestToolDeliveryUncallableNoticeExcludesGeminiRefusalStems confirms
// the uncallable-tool notice never carries either stem a Gemini
// permission-refusal notice carries, so an operator reading the two
// notices side by side never mistakes one for the other. This lives
// behind the unix build tag because geminiPermissionNoticeStem and
// geminiPermissionUnansweredStem are declared in a unix-only file;
// permission_test.go carries no build constraint and must stay
// buildable on every GOOS.
func TestToolDeliveryUncallableNoticeExcludesGeminiRefusalStems(t *testing.T) {
	t.Parallel()

	if strings.Contains(toolDeliveryUncallableNotice, geminiPermissionNoticeStem) {
		t.Errorf("toolDeliveryUncallableNotice = %q, must not contain %q", toolDeliveryUncallableNotice, geminiPermissionNoticeStem)
	}
	if strings.Contains(toolDeliveryUncallableNotice, geminiPermissionUnansweredStem) {
		t.Errorf("toolDeliveryUncallableNotice = %q, must not contain %q", toolDeliveryUncallableNotice, geminiPermissionUnansweredStem)
	}
}
