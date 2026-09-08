//go:build unix

package clientprotocol

import (
	"strings"
	"testing"
)

// permissionRefusalNoticeStem is the fixed fragment a permission-answer
// notification carries when the operator's own policy refused a
// request.
const permissionRefusalNoticeStem = "refused a permission request"

// permissionRefusalUnansweredStem is the fixed fragment a
// permission-answer notification carries when an unattended run cannot
// grant the request at all.
const permissionRefusalUnansweredStem = "needs a permission this unattended run cannot grant"

// TestToolDeliveryUncallableNoticeExcludesPermissionRefusalStems
// confirms the uncallable-tool notice never carries either stem a
// permission-refusal notice carries, so an operator reading the two
// notices side by side never mistakes one for the other. This lives
// behind the unix build tag to mirror where its two stem constants were
// pinned; permission_test.go carries no build constraint and must stay
// buildable on every GOOS.
func TestToolDeliveryUncallableNoticeExcludesPermissionRefusalStems(t *testing.T) {
	t.Parallel()

	if strings.Contains(toolDeliveryUncallableNotice, permissionRefusalNoticeStem) {
		t.Errorf("toolDeliveryUncallableNotice = %q, must not contain %q", toolDeliveryUncallableNotice, permissionRefusalNoticeStem)
	}
	if strings.Contains(toolDeliveryUncallableNotice, permissionRefusalUnansweredStem) {
		t.Errorf("toolDeliveryUncallableNotice = %q, must not contain %q", toolDeliveryUncallableNotice, permissionRefusalUnansweredStem)
	}
}
