package issuekit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/sortie-ai/sortie/internal/domain"
)

// IssueLabel is one label an issue carries, as the tracker stores it.
type IssueLabel struct {
	// Name is the spelling the tracker stores.
	Name string

	// ID is the handle the tracker removes the label by; empty where it
	// removes by name.
	ID string
}

// LabelWrite is what a label write learned about the issue's labels.
type LabelWrite struct {
	// After holds the label names on the issue after the write. It is
	// meaningful only when Reported is true.
	After []string

	// Reported is true when the write's response carried the issue's
	// label set.
	Reported bool
}

// IssueLabelOps is the set of tracker calls [AddIssueLabel] and
// [RemoveIssueLabel] drive for one issue. A Remove that sends one request
// per label sends all of them: a not-found or payload rejection of one
// does not stop the rest and is returned after the last, and any other
// error returns at once.
type IssueLabelOps struct {
	// Read returns every label the issue carries.
	Read func(ctx context.Context) ([]IssueLabel, error)

	// Add adds one label to the issue.
	Add func(ctx context.Context, label string) (LabelWrite, error)

	// Remove removes the given labels from the issue.
	Remove func(ctx context.Context, labels []IssueLabel) (LabelWrite, error)
}

// SameLabel reports whether the stored label names label: the two compare
// equal ignoring letter case, the comparison stage selection applies to a
// normalized label set. Neither argument is trimmed.
func SameLabel(stored, label string) bool {
	return strings.EqualFold(strings.ToLower(stored), label)
}

// LabelVariants returns, in input order, every entry of stored that names
// label under [SameLabel]. It returns nil when none does.
func LabelVariants(stored []string, label string) []string {
	var variants []string
	for _, name := range stored {
		if SameLabel(name, label) {
			variants = append(variants, name)
		}
	}
	return variants
}

// RemoveIssueLabel removes every label of the issue that names label and
// returns nil only when the issue's label set after the write carries no
// such label. An issue that carries none receives no write.
//
// A not-found or payload rejection of the write is settled by a confirming
// read: the removal stands if the label is gone and the rejection is
// returned otherwise. A blank label returns [domain.ErrTrackerPayload]
// before any call to ops. The function holds no state and is safe for
// concurrent use.
func RemoveIssueLabel(ctx context.Context, label string, ops IssueLabelOps) error {
	if blank(label) {
		return blankLabelError(label)
	}

	current, err := ops.Read(ctx)
	if err != nil {
		return err
	}
	matched := matchingLabels(current, label)
	if len(matched) == 0 {
		return nil
	}

	write, writeErr := ops.Remove(ctx, matched)
	if writeErr != nil && !rejection(writeErr) {
		return writeErr
	}
	if writeErr == nil && write.Reported && !carries(write.After, label) {
		return nil
	}

	after, err := ops.Read(ctx)
	if err != nil {
		return err
	}
	if !carries(labelNames(after), label) {
		return nil
	}
	if writeErr != nil {
		return writeErr
	}
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf("tracker accepted removal of label %q and the issue still carries it", label),
	}
}

// AddIssueLabel adds label to the issue and returns nil only when the
// issue's label set after the write carries it.
//
// A write whose response reports a label set that carries the label is
// trusted; otherwise a confirming read decides and a missing label is
// returned as [domain.ErrTrackerPayload]. A blank label returns the same
// kind before any call to ops. The function holds no state and is safe for
// concurrent use.
func AddIssueLabel(ctx context.Context, label string, ops IssueLabelOps) error {
	if blank(label) {
		return blankLabelError(label)
	}

	write, err := ops.Add(ctx, label)
	if err != nil {
		return err
	}
	if write.Reported && carries(write.After, label) {
		return nil
	}

	after, err := ops.Read(ctx)
	if err != nil {
		return err
	}
	if carries(labelNames(after), label) {
		return nil
	}
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf("tracker accepted label %q and the issue does not carry it", label),
	}
}

func blank(label string) bool {
	return strings.TrimFunc(label, unicode.IsSpace) == ""
}

func blankLabelError(label string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf("label %q is blank", label),
	}
}

func carries(names []string, label string) bool {
	for _, name := range names {
		if SameLabel(name, label) {
			return true
		}
	}
	return false
}

func rejection(err error) bool {
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		return false
	}
	return te.Kind == domain.ErrTrackerNotFound || te.Kind == domain.ErrTrackerPayload
}

func matchingLabels(labels []IssueLabel, label string) []IssueLabel {
	var matched []IssueLabel
	for _, l := range labels {
		if SameLabel(l.Name, label) {
			matched = append(matched, l)
		}
	}
	return matched
}

func labelNames(labels []IssueLabel) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}
