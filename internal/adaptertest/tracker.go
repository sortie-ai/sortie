// Package adaptertest provides shared conformance assertions for tracker
// and source-control adapter test suites. Each assertion pins one clause
// of the [domain.TrackerAdapter] or [domain.SCMAdapter] contract that
// otherwise lives only in prose, so a per-adapter suite can call it
// instead of re-deriving the check. No assertion constructs an adapter,
// starts a server, or reads a fixture; the calling suite owns those.
package adaptertest

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// AssertIssueNormalized pins the normalization clause every tracker
// adapter's candidate-issue output must satisfy: a stable ID and a
// human-readable Identifier are both set, and Labels is a non-nil slice
// whose entries are already lowercased.
func AssertIssueNormalized(t *testing.T, issue domain.Issue) {
	t.Helper()

	if issue.ID == "" {
		t.Error("AssertIssueNormalized: issue.ID is empty, want a stable tracker-internal identifier")
	}
	if issue.Identifier == "" {
		t.Error("AssertIssueNormalized: issue.Identifier is empty, want a human-readable ticket key")
	}
	if issue.Labels == nil {
		t.Error("AssertIssueNormalized: issue.Labels is nil, want a non-nil slice even when the issue carries no labels")
	}
	for _, label := range issue.Labels {
		if lower := strings.ToLower(label); lower != label {
			t.Errorf("AssertIssueNormalized: label %q is not lowercased, want %q", label, lower)
		}
	}
}

// AssertCommentsAscending pins the ordering clause a fetched comment list
// must satisfy: entries are sorted oldest first by CreatedAt.
func AssertCommentsAscending(t *testing.T, comments []domain.Comment) {
	t.Helper()

	for i := 1; i < len(comments); i++ {
		if comments[i-1].CreatedAt > comments[i].CreatedAt {
			t.Errorf("AssertCommentsAscending: comment[%d].CreatedAt %q sorts after comment[%d].CreatedAt %q, want ascending order",
				i-1, comments[i-1].CreatedAt, i, comments[i].CreatedAt)
		}
	}
}

// AssertEmptyNonNil pins the clause that an operation returning no
// results MUST return a non-nil empty slice rather than nil, so a
// template or caller iterating the result never distinguishes "no
// results" from "not fetched" by a nil check. op names the operation
// under test for the failure message.
func AssertEmptyNonNil[T any](t *testing.T, got []T, op string) {
	t.Helper()

	if got == nil {
		t.Errorf("AssertEmptyNonNil: %s returned a nil slice, want a non-nil empty slice", op)
	}
	if len(got) != 0 {
		t.Errorf("AssertEmptyNonNil: %s returned %d elements, want zero", op, len(got))
	}
}

// AssertStateMapOmitsMissing pins the clause that a state-batch operation
// omits an unresolvable id from its returned map rather than including it
// with a placeholder value: every key in got must have been present in
// requested, and every value must be non-empty.
func AssertStateMapOmitsMissing(t *testing.T, requested []string, got map[string]string) {
	t.Helper()

	for id, state := range got {
		if !slices.Contains(requested, id) {
			t.Errorf("AssertStateMapOmitsMissing: got map carries key %q, which was not in requested", id)
		}
		if state == "" {
			t.Errorf("AssertStateMapOmitsMissing: got map carries an empty state for key %q, want the key omitted instead", id)
		}
	}
}

// AssertTrackerErrorKind pins the clause that a tracker adapter failure
// unwraps to a [*domain.TrackerError] of the expected normalized kind.
func AssertTrackerErrorKind(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()

	var trackerErr *domain.TrackerError
	if !errors.As(err, &trackerErr) {
		t.Errorf("AssertTrackerErrorKind: error %v does not unwrap to *domain.TrackerError, want kind %q", err, want)
		return
	}
	if trackerErr.Kind != want {
		t.Errorf("AssertTrackerErrorKind: kind = %q, want %q", trackerErr.Kind, want)
	}
}

// AssertNoRequestOnEmptyInput pins the clause that a batch operation
// given an empty input issues no request to the tracker. calls is the
// number of requests the caller's fake recorded; op names the operation
// under test for the failure message.
func AssertNoRequestOnEmptyInput(t *testing.T, calls int, op string) {
	t.Helper()

	if calls != 0 {
		t.Errorf("AssertNoRequestOnEmptyInput: %s issued %d request(s) for empty input, want zero", op, calls)
	}
}

// AssertLabelAddIsAdditive pins the clause that AddLabel only adds: every
// label present before the call is still present after, and added is
// present after.
func AssertLabelAddIsAdditive(t *testing.T, before, after []string, added string) {
	t.Helper()

	for _, label := range before {
		if !slices.Contains(after, label) {
			t.Errorf("AssertLabelAddIsAdditive: pre-existing label %q is missing after AddLabel", label)
		}
	}
	if !slices.ContainsFunc(after, func(label string) bool { return strings.EqualFold(label, added) }) {
		t.Errorf("AssertLabelAddIsAdditive: added label %q is missing after AddLabel", added)
	}
}

// AssertCandidateBlockerSource pins the clause that a candidate issue's
// blocker fields agree with the blocker source its adapter declared.
// wantBlockers is the number of blockers the caller's fixture defines
// for that issue.
//
// For a per-issue source the caller must pass a candidate whose own
// payload does not prove the issue has no dependencies, because that is
// the only candidate whose unresolved marking the assertion can check;
// the assertion fails when such a candidate is not marked. A caller
// whose fixture carries no such candidate is asserting nothing, so the
// assertion says that in its failure message rather than passing.
func AssertCandidateBlockerSource(t *testing.T, source registry.BlockerSource, issue domain.Issue, wantBlockers int) {
	t.Helper()

	switch source {
	case registry.BlockersFromCandidates, registry.BlockersUnsupported:
		if issue.BlockersUnresolved {
			t.Errorf("AssertCandidateBlockerSource: issue.BlockersUnresolved is true for blocker source %q, want false", source)
		}
		if len(issue.BlockedBy) != wantBlockers {
			t.Errorf("AssertCandidateBlockerSource: len(issue.BlockedBy) = %d, want %d", len(issue.BlockedBy), wantBlockers)
		}
	case registry.BlockersPerIssue:
		if wantBlockers == 0 && !issue.BlockersUnresolved {
			if len(issue.BlockedBy) != 0 {
				t.Errorf("AssertCandidateBlockerSource: len(issue.BlockedBy) = %d, want 0 for a cheaply-answered no-dependency candidate", len(issue.BlockedBy))
			}
			return
		}
		if !issue.BlockersUnresolved {
			t.Error("AssertCandidateBlockerSource: issue.BlockersUnresolved is false for a per_issue candidate the caller's fixture did not prove dependency-free; the fixture asserts nothing about the unresolved-marking clause")
		}
	default:
		t.Errorf("AssertCandidateBlockerSource: unrecognized blocker source %q", source)
	}
}

// AssertBlockerRefsNormalized pins the clause that a resolved blocker
// list is a non-nil slice whose entries carry a stable ID and a
// human-readable Identifier. A blocker State may be empty, which the
// dispatch gate reads as non-terminal.
func AssertBlockerRefsNormalized(t *testing.T, blockers []domain.BlockerRef) {
	t.Helper()

	if blockers == nil {
		t.Error("AssertBlockerRefsNormalized: blockers is nil, want a non-nil slice even when the issue carries no blockers")
	}
	for i, b := range blockers {
		if b.ID == "" {
			t.Errorf("AssertBlockerRefsNormalized: blockers[%d].ID is empty, want a stable tracker-internal identifier", i)
		}
		if b.Identifier == "" {
			t.Errorf("AssertBlockerRefsNormalized: blockers[%d].Identifier is empty, want a human-readable key", i)
		}
	}
}

// AssertBlockerIdentifiersMatchIssue pins the clause that a blocker ref
// carries the same identifier shape the adapter gives an issue: an
// unqualified Identifier, and a DisplayID that is either empty on every
// ref or qualified on every ref, matching whether issue.DisplayID is
// itself non-empty. issue is any issue the same adapter normalized.
func AssertBlockerIdentifiersMatchIssue(t *testing.T, issue domain.Issue, blockers []domain.BlockerRef) {
	t.Helper()

	issueQualifies := issue.DisplayID != ""
	for i, b := range blockers {
		if b.Identifier != "" && strings.ContainsAny(b.Identifier, "/#") {
			t.Errorf("AssertBlockerIdentifiersMatchIssue: blockers[%d].Identifier %q looks qualified, want the bare tracker key", i, b.Identifier)
		}
		refQualifies := b.DisplayID != ""
		if refQualifies != issueQualifies {
			t.Errorf("AssertBlockerIdentifiersMatchIssue: blockers[%d].DisplayID qualified = %v, want %v to match issue.DisplayID", i, refQualifies, issueQualifies)
		}
	}
}
