package scmcore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
	"github.com/sortie-ai/sortie/internal/waittest"
)

type accessStep struct {
	comment     domain.Comment
	lookupOK    bool
	lookupErr   error
	wantOK      bool
	wantLookups int
}

func TestWriteAccessCacheCanWrite(t *testing.T) {
	t.Parallel()

	errLookup := errors.New("lookup failed")
	alice1 := domain.Comment{ID: "c1", AuthorID: "alice"}
	alice2 := domain.Comment{ID: "c2", AuthorID: "alice"}

	tests := []struct {
		name  string
		steps []accessStep
	}{
		{"granted author's next comment sends no lookup", []accessStep{
			{comment: alice1, lookupOK: true, wantOK: true, wantLookups: 1},
			{comment: alice2, wantOK: true},
		}},
		{"refused author's next comment sends a lookup", []accessStep{
			{comment: alice1, wantLookups: 1},
			{comment: alice2, wantLookups: 1},
		}},
		{"same refused comment again sends no lookup", []accessStep{
			{comment: alice1, wantLookups: 1},
			{comment: alice1},
		}},
		{"failed lookup returns its error and the same comment looks up again", []accessStep{
			{comment: alice1, lookupErr: errLookup, wantLookups: 1},
			{comment: alice1, lookupOK: true, wantOK: true, wantLookups: 1},
		}},
		{"empty author is refused without a lookup", []accessStep{
			{comment: domain.Comment{ID: "c1"}, lookupOK: true},
		}},
		{"comment refused before its author was granted reads as granted", []accessStep{
			{comment: alice1, wantLookups: 1},
			{comment: alice2, lookupOK: true, wantOK: true, wantLookups: 1},
			{comment: alice1, wantOK: true},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := waittest.Context(t)
			var step accessStep
			var lookups int
			cache := scmcore.NewWriteAccessCache(func(_ context.Context, authorID string) (bool, error) {
				lookups++
				if authorID != step.comment.AuthorID {
					t.Errorf("lookup(%q), want author %q", authorID, step.comment.AuthorID)
				}
				return step.lookupOK, step.lookupErr
			})

			for i, s := range tt.steps {
				step, lookups = s, 0

				got, err := cache.CanWrite(ctx, s.comment)

				if !errors.Is(err, s.lookupErr) {
					t.Fatalf("step %d: CanWrite(%+v) error = %v, want %v", i, s.comment, err, s.lookupErr)
				}
				if got != s.wantOK {
					t.Errorf("step %d: CanWrite(%+v) = %v, want %v", i, s.comment, got, s.wantOK)
				}
				if lookups != s.wantLookups {
					t.Errorf("step %d: CanWrite(%+v) sent %d lookups, want %d", i, s.comment, lookups, s.wantLookups)
				}
			}
		})
	}
}
