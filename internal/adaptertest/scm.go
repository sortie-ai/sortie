package adaptertest

import (
	"errors"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// AssertSCMErrorKind pins the clause that a source-control adapter
// failure unwraps to a [*domain.SCMError] of the expected normalized
// kind.
func AssertSCMErrorKind(t *testing.T, err error, want domain.SCMErrorKind) {
	t.Helper()

	var scmErr *domain.SCMError
	if !errors.As(err, &scmErr) {
		t.Errorf("AssertSCMErrorKind: error %v does not unwrap to *domain.SCMError, want kind %q", err, want)
		return
	}
	if scmErr.Kind != want {
		t.Errorf("AssertSCMErrorKind: kind = %q, want %q", scmErr.Kind, want)
	}
}

// AssertAlreadyMergedMarker pins the clause that a merge which raced an
// external merge is reported as an [domain.ErrSCMConflict] whose Message
// carries the substring "already merged", case-insensitively, so the
// auto-merge reconcile pass can dispatch the outcome as success.
func AssertAlreadyMergedMarker(t *testing.T, err error) {
	t.Helper()

	var scmErr *domain.SCMError
	if !errors.As(err, &scmErr) {
		t.Errorf("AssertAlreadyMergedMarker: error %v does not unwrap to *domain.SCMError", err)
		return
	}
	if scmErr.Kind != domain.ErrSCMConflict {
		t.Errorf("AssertAlreadyMergedMarker: kind = %q, want %q", scmErr.Kind, domain.ErrSCMConflict)
	}
	if !strings.Contains(strings.ToLower(scmErr.Message), "already merged") {
		t.Errorf("AssertAlreadyMergedMarker: message %q does not carry the already-merged marker", scmErr.Message)
	}
}

// AssertBranchAbsentDisposition pins DeleteBranch's disposition for a
// branch that no longer exists: a [*domain.SCMError] with Kind
// [domain.ErrSCMNotFound], which the caller treats as a successful
// no-op rather than a nil return.
func AssertBranchAbsentDisposition(t *testing.T, err error) {
	t.Helper()
	AssertSCMErrorKind(t, err, domain.ErrSCMNotFound)
}

// AssertLabelAbsentDisposition pins RemoveLabel's disposition for a
// label that is already absent: implementations map the not-found result
// to a nil return, unlike DeleteBranch's disposition for an absent
// branch.
func AssertLabelAbsentDisposition(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Errorf("AssertLabelAbsentDisposition: err = %v, want nil for an already-absent label", err)
	}
}

// AssertCIAggregateMatchesCore pins the clause that a CI provider's
// aggregate and failing count are derived from the same rule
// [scmcore.AggregateCIStatus] and [scmcore.FailingCount] implement, by
// recomputing the expected verdict from result.CheckRuns and comparing
// it against the supplied result.
func AssertCIAggregateMatchesCore(t *testing.T, result domain.CIResult) {
	t.Helper()

	wantStatus := scmcore.AggregateCIStatus(result.CheckRuns)
	if result.Status != wantStatus {
		t.Errorf("AssertCIAggregateMatchesCore: Status = %q, want %q", result.Status, wantStatus)
	}

	wantFailing := scmcore.FailingCount(result.CheckRuns)
	if result.FailingCount != wantFailing {
		t.Errorf("AssertCIAggregateMatchesCore: FailingCount = %d, want %d", result.FailingCount, wantFailing)
	}
}

// AssertLabelEventsOrdered pins the clause that ListLabelEvents returns
// the label-event journal oldest first: ordering entries by (At, ID) as
// strings matches their journal order.
func AssertLabelEventsOrdered(t *testing.T, events []domain.LabelEvent) {
	t.Helper()

	for i := 1; i < len(events); i++ {
		prev, cur := events[i-1], events[i]
		if prev.At.After(cur.At) {
			t.Errorf("AssertLabelEventsOrdered: event[%d].At %v is after event[%d].At %v, want ascending order",
				i-1, prev.At, i, cur.At)
			continue
		}
		if prev.At.Equal(cur.At) && prev.ID > cur.ID {
			t.Errorf("AssertLabelEventsOrdered: event[%d].ID %q sorts after event[%d].ID %q at equal timestamps, want ascending order",
				i-1, prev.ID, i, cur.ID)
		}
	}
}
