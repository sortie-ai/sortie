package blockers

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// fakeTrackerAdapter is a minimal domain.TrackerAdapter stub. Only the
// methods this package's tests need behave; the rest satisfy the
// interface with zero values.
type fakeTrackerAdapter struct{}

var _ domain.TrackerAdapter = (*fakeTrackerAdapter)(nil)

func (fakeTrackerAdapter) FetchCandidateIssues(context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (fakeTrackerAdapter) FetchIssueByID(context.Context, string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (fakeTrackerAdapter) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (fakeTrackerAdapter) FetchIssueStatesByIDs(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (fakeTrackerAdapter) FetchIssueStatesByIdentifiers(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (fakeTrackerAdapter) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	return nil, nil
}
func (fakeTrackerAdapter) TransitionIssue(context.Context, string, string) error { return nil }
func (fakeTrackerAdapter) CommentIssue(context.Context, string, string) error    { return nil }
func (fakeTrackerAdapter) AddLabel(context.Context, string, string) error        { return nil }

// fakeBlockerReaderAdapter embeds fakeTrackerAdapter and additionally
// implements domain.BlockerReader, recording every call and returning
// a scripted result.
type fakeBlockerReaderAdapter struct {
	fakeTrackerAdapter
	calls   []string
	blocked []domain.BlockerRef
	err     error
}

var _ domain.BlockerReader = (*fakeBlockerReaderAdapter)(nil)

func (f *fakeBlockerReaderAdapter) FetchIssueBlockers(_ context.Context, issueID string) ([]domain.BlockerRef, error) {
	f.calls = append(f.calls, issueID)
	if f.err != nil {
		return nil, f.err
	}
	return f.blocked, nil
}

func unresolvedIssue() domain.Issue {
	return domain.Issue{ID: "1", Identifier: "1", BlockersUnresolved: true}
}

func resolvedIssue(id string) domain.Issue {
	return domain.Issue{ID: id, Identifier: id, BlockedBy: []domain.BlockerRef{}}
}

func TestNewResolver_NilAdapterYieldsNilResolver(t *testing.T) {
	t.Parallel()

	r := NewResolver(nil, registry.BlockersPerIssue, nil)
	if r != nil {
		t.Errorf("NewResolver(nil, ...) = %v, want nil", r)
	}
}

func TestResolver_NilReceiver(t *testing.T) {
	t.Parallel()

	var r *Resolver

	if got := r.NeedsRead(unresolvedIssue()); got {
		t.Errorf("(*Resolver)(nil).NeedsRead() = %t, want false", got)
	}

	issue := unresolvedIssue()
	got, err := r.Resolve(context.Background(), issue)
	if err != nil {
		t.Errorf("(*Resolver)(nil).Resolve() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, issue) {
		t.Errorf("(*Resolver)(nil).Resolve() = %+v, want the issue unchanged %+v", got, issue)
	}
}

func TestResolver_NeedsRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source registry.BlockerSource
		issue  domain.Issue
		want   bool
	}{
		{
			name:   "candidates source never needs a read",
			source: registry.BlockersFromCandidates,
			issue:  unresolvedIssue(),
			want:   false,
		},
		{
			name:   "unsupported source never needs a read",
			source: registry.BlockersUnsupported,
			issue:  unresolvedIssue(),
			want:   false,
		},
		{
			name:   "empty source never needs a read",
			source: "",
			issue:  unresolvedIssue(),
			want:   false,
		},
		{
			name:   "per_issue source with resolved issue needs no read",
			source: registry.BlockersPerIssue,
			issue:  resolvedIssue("1"),
			want:   false,
		},
		{
			name:   "per_issue source with unresolved issue needs a read",
			source: registry.BlockersPerIssue,
			issue:  unresolvedIssue(),
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewResolver(&fakeBlockerReaderAdapter{}, tt.source, nil)
			if got := r.NeedsRead(tt.issue); got != tt.want {
				t.Errorf("NeedsRead(source=%q, issue) = %t, want %t", tt.source, got, tt.want)
			}
		})
	}
}

func TestResolver_Resolve(t *testing.T) {
	t.Parallel()

	sentinelErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}

	tests := []struct {
		name           string
		source         registry.BlockerSource
		reader         *fakeBlockerReaderAdapter // nil means the adapter carries no reader
		issue          domain.Issue
		wantCalls      int
		wantErr        error
		wantUnresolved bool
		wantBlockedLen int
	}{
		{
			name:           "candidates source returns the issue unchanged, no call",
			source:         registry.BlockersFromCandidates,
			reader:         &fakeBlockerReaderAdapter{},
			issue:          unresolvedIssue(),
			wantCalls:      0,
			wantUnresolved: true,
		},
		{
			name:           "already-resolved per_issue issue costs no call",
			source:         registry.BlockersPerIssue,
			reader:         &fakeBlockerReaderAdapter{},
			issue:          resolvedIssue("1"),
			wantCalls:      0,
			wantUnresolved: false,
		},
		{
			name:           "per_issue read succeeds and clears the flag",
			source:         registry.BlockersPerIssue,
			reader:         &fakeBlockerReaderAdapter{blocked: []domain.BlockerRef{{ID: "b1", Identifier: "b1"}}},
			issue:          unresolvedIssue(),
			wantCalls:      1,
			wantUnresolved: false,
			wantBlockedLen: 1,
		},
		{
			name:           "per_issue read fails and sets the flag",
			source:         registry.BlockersPerIssue,
			reader:         &fakeBlockerReaderAdapter{err: sentinelErr},
			issue:          unresolvedIssue(),
			wantCalls:      1,
			wantErr:        sentinelErr,
			wantUnresolved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := NewResolver(tt.reader, tt.source, nil)
			got, err := r.Resolve(context.Background(), tt.issue)

			if len(tt.reader.calls) != tt.wantCalls {
				t.Errorf("reader calls = %d, want %d", len(tt.reader.calls), tt.wantCalls)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("Resolve() error = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("Resolve() unexpected error: %v", err)
			}
			if got.BlockersUnresolved != tt.wantUnresolved {
				t.Errorf("Resolve().BlockersUnresolved = %t, want %t", got.BlockersUnresolved, tt.wantUnresolved)
			}
			if len(got.BlockedBy) != tt.wantBlockedLen {
				t.Errorf("Resolve().BlockedBy len = %d, want %d", len(got.BlockedBy), tt.wantBlockedLen)
			}
		})
	}
}

// TestResolver_MissingReader pins that a per_issue adapter that does
// not implement domain.BlockerReader degrades to
// domain.ErrNoBlockerReader rather than panicking or silently
// resolving to no blockers.
func TestResolver_MissingReader(t *testing.T) {
	t.Parallel()

	r := NewResolver(fakeTrackerAdapter{}, registry.BlockersPerIssue, nil)

	got, err := r.Resolve(context.Background(), unresolvedIssue())
	if !errors.Is(err, domain.ErrNoBlockerReader) {
		t.Errorf("Resolve() error = %v, want %v", err, domain.ErrNoBlockerReader)
	}
	if !got.BlockersUnresolved {
		t.Error("Resolve().BlockersUnresolved = false, want true when no reader is available")
	}
}

// TestResolver_CancelledContext pins that a cancelled context reaches
// the reader as an ordinary read failure: the resolver does not
// special-case it, and the issue is left unresolved.
func TestResolver_CancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reader := &fakeBlockerReaderAdapter{err: context.Canceled}
	r := NewResolver(reader, registry.BlockersPerIssue, nil)

	got, err := r.Resolve(ctx, unresolvedIssue())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Resolve() error = %v, want context.Canceled", err)
	}
	if !got.BlockersUnresolved {
		t.Error("Resolve().BlockersUnresolved = false, want true after a cancelled-context failure")
	}
	if len(reader.calls) != 1 {
		t.Errorf("reader calls = %d, want 1", len(reader.calls))
	}
}

// TestResolver_NeedsReadResolveAgreement pins that NeedsRead and
// Resolve decide from the same two inputs (declared source, issue
// flag): whenever NeedsRead reports false, Resolve must make no call
// and must return the issue unchanged.
func TestResolver_NeedsReadResolveAgreement(t *testing.T) {
	t.Parallel()

	issues := []domain.Issue{
		unresolvedIssue(),
		resolvedIssue("2"),
	}
	sources := []registry.BlockerSource{
		registry.BlockersFromCandidates,
		registry.BlockersUnsupported,
		registry.BlockersPerIssue,
		"",
	}

	for _, source := range sources {
		for _, issue := range issues {
			reader := &fakeBlockerReaderAdapter{blocked: []domain.BlockerRef{{ID: "x", Identifier: "x"}}}
			r := NewResolver(reader, source, nil)

			needsRead := r.NeedsRead(issue)
			got, err := r.Resolve(context.Background(), issue)

			if !needsRead {
				if len(reader.calls) != 0 {
					t.Errorf("source=%q issue=%q: NeedsRead=false but Resolve made %d calls, want 0", source, issue.ID, len(reader.calls))
				}
				if !reflect.DeepEqual(got, issue) {
					t.Errorf("source=%q issue=%q: Resolve() = %+v, want the issue unchanged %+v", source, issue.ID, got, issue)
				}
				if err != nil {
					t.Errorf("source=%q issue=%q: Resolve() error = %v, want nil", source, issue.ID, err)
				}
			}
		}
	}
}

// TestResolver_ResolveDoesNotMutateArgument pins that Resolve returns a
// copy: the caller's original issue value is untouched even when the
// read succeeds and changes BlockedBy and BlockersUnresolved.
func TestResolver_ResolveDoesNotMutateArgument(t *testing.T) {
	t.Parallel()

	original := unresolvedIssue()
	argument := original

	reader := &fakeBlockerReaderAdapter{blocked: []domain.BlockerRef{{ID: "b1", Identifier: "b1"}}}
	r := NewResolver(reader, registry.BlockersPerIssue, nil)

	if _, err := r.Resolve(context.Background(), argument); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if argument.BlockersUnresolved != original.BlockersUnresolved {
		t.Error("Resolve mutated the caller's issue value in place")
	}
}
