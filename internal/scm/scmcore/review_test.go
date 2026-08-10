package scmcore_test

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func TestIsBotAuthor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		login       string
		platformBot bool
		allowlist   []string
		want        bool
	}{
		{
			name:        "platform bot marker set",
			login:       "golangci-lint[bot]",
			platformBot: true,
			want:        true,
		},
		{
			name:        "not a platform bot and not in allowlist",
			login:       "alice",
			platformBot: false,
			want:        false,
		},
		{
			name:        "not a platform bot but in allowlist exact match",
			login:       "houndci-bot",
			platformBot: false,
			allowlist:   []string{"houndci-bot"},
			want:        true,
		},
		{
			name:        "not a platform bot but in allowlist case-insensitive",
			login:       "houndci-bot",
			platformBot: false,
			allowlist:   []string{"HOUNDCI-BOT"},
			want:        true,
		},
		{
			name:        "not a platform bot and not in a populated allowlist",
			login:       "bob",
			platformBot: false,
			allowlist:   []string{"houndci-bot", "golangci-lint[bot]"},
			want:        false,
		},
		{
			name:        "empty allowlist and not a platform bot",
			login:       "carol",
			platformBot: false,
			allowlist:   []string{},
			want:        false,
		},
		{
			name:        "nil allowlist and not a platform bot",
			login:       "carol",
			platformBot: false,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := scmcore.IsBotAuthor(tt.login, tt.platformBot, tt.allowlist)
			if got != tt.want {
				t.Errorf("IsBotAuthor(%q, %v, %v) = %v, want %v",
					tt.login, tt.platformBot, tt.allowlist, got, tt.want)
			}
		})
	}
}

func TestSortableEventID_LexicalMatchesNumericAcrossDigitBoundary(t *testing.T) {
	t.Parallel()

	// Raw decimal ids misorder lexically across a digit-width boundary
	// ("9" sorts after "10"); the normalized form must preserve numeric
	// order so the reconcile's (At, id) mark comparison stays chronological
	// for events that share a timestamp.
	for _, pair := range []struct{ lo, hi int64 }{{9, 10}, {99, 100}, {999999999, 1000000000}} {
		if scmcore.SortableEventID(pair.lo) >= scmcore.SortableEventID(pair.hi) {
			t.Errorf("SortableEventID(%d)=%q not lexically before SortableEventID(%d)=%q",
				pair.lo, scmcore.SortableEventID(pair.lo), pair.hi, scmcore.SortableEventID(pair.hi))
		}
	}
}
