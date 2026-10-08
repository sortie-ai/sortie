package issuekit_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

type scriptedOps struct {
	reads   [][]issuekit.IssueLabel
	readErr error
	write   issuekit.LabelWrite
	err     error

	calls   []string
	removed []issuekit.IssueLabel
}

func (s *scriptedOps) ops() issuekit.IssueLabelOps {
	return issuekit.IssueLabelOps{
		Read: func(context.Context) ([]issuekit.IssueLabel, error) {
			s.calls = append(s.calls, "read")
			if len(s.reads) == 0 {
				return nil, s.readErr
			}
			next := s.reads[0]
			s.reads = s.reads[1:]
			return next, nil
		},
		Add: func(context.Context, string) (issuekit.LabelWrite, error) {
			s.calls = append(s.calls, "write")
			return s.write, s.err
		},
		Remove: func(_ context.Context, labels []issuekit.IssueLabel) (issuekit.LabelWrite, error) {
			s.calls = append(s.calls, "write")
			s.removed = labels
			return s.write, s.err
		},
	}
}

func named(names ...string) []issuekit.IssueLabel {
	labels := make([]issuekit.IssueLabel, len(names))
	for i, name := range names {
		labels[i] = issuekit.IssueLabel{Name: name, ID: "id-" + name}
	}
	return labels
}

func kindErr(kind domain.TrackerErrorKind) error {
	return &domain.TrackerError{Kind: kind, Message: "scripted"}
}

func TestLabelDrivers(t *testing.T) {
	t.Parallel()

	present, gone := named("Stage-Plan", "bug"), named("bug")
	reported := func(names ...string) issuekit.LabelWrite { return issuekit.LabelWrite{Reported: true, After: names} }

	tests := []struct {
		name        string
		remove      bool
		label       string
		ops         scriptedOps
		wantKind    domain.TrackerErrorKind
		wantCalls   []string
		wantRemoved []issuekit.IssueLabel
	}{
		{name: "remove blank", remove: true, label: " \t", wantKind: domain.ErrTrackerPayload},
		{name: "add blank", label: "", wantKind: domain.ErrTrackerPayload},
		{name: "remove absent writes nothing", remove: true, label: "stage-plan", ops: scriptedOps{reads: [][]issuekit.IssueLabel{gone}}, wantCalls: []string{"read"}},
		{
			name:   "remove sends every spelling in input order",
			remove: true,
			label:  "stage-plan",
			ops: scriptedOps{
				reads: [][]issuekit.IssueLabel{{{Name: "Stage-Plan", ID: "1"}, {Name: "bug", ID: "2"}, {Name: "stage-plan", ID: "3"}}},
				write: reported("bug"),
			},
			wantCalls:   []string{"read", "write"},
			wantRemoved: []issuekit.IssueLabel{{Name: "Stage-Plan", ID: "1"}, {Name: "stage-plan", ID: "3"}},
		},
		{name: "remove unreported is settled by the read", remove: true, label: "stage-plan", ops: scriptedOps{reads: [][]issuekit.IssueLabel{present, gone}}, wantCalls: []string{"read", "write", "read"}},
		{name: "remove accepted but still carried", remove: true, label: "stage-plan", ops: scriptedOps{reads: [][]issuekit.IssueLabel{present, present}}, wantKind: domain.ErrTrackerPayload, wantCalls: []string{"read", "write", "read"}},
		{name: "remove rejection with label gone", remove: true, label: "stage-plan", ops: scriptedOps{reads: [][]issuekit.IssueLabel{present, gone}, err: kindErr(domain.ErrTrackerNotFound)}, wantCalls: []string{"read", "write", "read"}},
		{name: "remove rejection with label kept is returned", remove: true, label: "stage-plan", ops: scriptedOps{reads: [][]issuekit.IssueLabel{present, present}, err: kindErr(domain.ErrTrackerNotFound)}, wantKind: domain.ErrTrackerNotFound, wantCalls: []string{"read", "write", "read"}},
		{name: "remove auth error returns at once", remove: true, label: "stage-plan", ops: scriptedOps{reads: [][]issuekit.IssueLabel{present}, err: kindErr(domain.ErrTrackerAuth)}, wantKind: domain.ErrTrackerAuth, wantCalls: []string{"read", "write"}},
		{name: "add reported carrying needs no read", label: "stage-plan", ops: scriptedOps{write: reported("bug", "Stage-Plan")}, wantCalls: []string{"write"}},
		{name: "add not carried after the read", label: "stage-plan", ops: scriptedOps{reads: [][]issuekit.IssueLabel{gone}, write: reported("bug")}, wantKind: domain.ErrTrackerPayload, wantCalls: []string{"write", "read"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ops := tt.ops

			var err error
			if tt.remove {
				err = issuekit.RemoveIssueLabel(t.Context(), tt.label, ops.ops())
			} else {
				err = issuekit.AddIssueLabel(t.Context(), tt.label, ops.ops())
			}

			var te *domain.TrackerError
			switch {
			case tt.wantKind != "":
				if !errors.As(err, &te) || te.Kind != tt.wantKind {
					t.Fatalf("driver(%q) error = %v, want kind %q", tt.label, err, tt.wantKind)
				}
			case err != nil:
				t.Fatalf("driver(%q) error = %v, want nil", tt.label, err)
			}
			if !slices.Equal(ops.calls, tt.wantCalls) {
				t.Errorf("driver(%q) ops calls = %v, want %v", tt.label, ops.calls, tt.wantCalls)
			}
			if tt.wantRemoved != nil && !slices.Equal(ops.removed, tt.wantRemoved) {
				t.Errorf("driver(%q) removed = %v, want %v", tt.label, ops.removed, tt.wantRemoved)
			}
		})
	}
}

func TestLabelMatching(t *testing.T) {
	t.Parallel()

	if got, want := issuekit.LabelVariants([]string{"Stage-Plan", "bug", "stage-plan"}, "STAGE-PLAN"), []string{"Stage-Plan", "stage-plan"}; !slices.Equal(got, want) {
		t.Errorf("LabelVariants = %v, want %v in input order", got, want)
	}

	stored := []string{"Stage-Plan", "İstanbul", "K", "straße"}
	requested := []string{"stage-plan", "istanbul", "k", "strasse", "straße", " stage-plan"}
	for _, s := range stored {
		for _, label := range requested {
			want := config.StageLabelsEqual(issuekit.NormalizeLabels([]string{s})[0], label)
			if got := issuekit.SameLabel(s, label); got != want {
				t.Errorf("SameLabel(%q, %q) = %t, want %t", s, label, got, want)
			}
		}
	}
}
