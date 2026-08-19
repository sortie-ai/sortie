package main

import (
	"testing"
)

func TestScmProviderConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		kinds                 []scmReactionKind
		wantActiveKinds       []string
		wantDistinctProviders []string
		wantConflict          bool
	}{
		{
			name:                  "zero active kinds",
			kinds:                 []scmReactionKind{},
			wantActiveKinds:       nil,
			wantDistinctProviders: nil,
			wantConflict:          false,
		},
		{
			name: "one active kind",
			kinds: []scmReactionKind{
				{name: "review_comments", active: true, provider: "github"},
			},
			wantActiveKinds:       []string{"review_comments"},
			wantDistinctProviders: []string{"github"},
			wantConflict:          false,
		},
		{
			name: "two active kinds same provider",
			kinds: []scmReactionKind{
				{name: "review_comments", active: true, provider: "github"},
				{name: "auto_merge", active: true, provider: "github"},
			},
			wantActiveKinds:       []string{"review_comments", "auto_merge"},
			wantDistinctProviders: []string{"github"},
			wantConflict:          false,
		},
		{
			name: "three active kinds same provider",
			kinds: []scmReactionKind{
				{name: "review_comments", active: true, provider: "github"},
				{name: "auto_merge", active: true, provider: "github"},
				{name: "bot_review", active: true, provider: "github"},
			},
			wantActiveKinds:       []string{"review_comments", "auto_merge", "bot_review"},
			wantDistinctProviders: []string{"github"},
			wantConflict:          false,
		},
		{
			name: "two active kinds differing providers",
			kinds: []scmReactionKind{
				{name: "review_comments", active: true, provider: "github"},
				{name: "auto_merge", active: true, provider: "gitlab"},
			},
			wantActiveKinds:       []string{"review_comments", "auto_merge"},
			wantDistinctProviders: []string{"github", "gitlab"},
			wantConflict:          true,
		},
		{
			name: "three active kinds differing providers",
			kinds: []scmReactionKind{
				{name: "review_comments", active: true, provider: "github"},
				{name: "auto_merge", active: true, provider: "gitlab"},
				{name: "bot_review", active: true, provider: "bitbucket"},
			},
			wantActiveKinds:       []string{"review_comments", "auto_merge", "bot_review"},
			wantDistinctProviders: []string{"github", "gitlab", "bitbucket"},
			wantConflict:          true,
		},
		{
			name: "two active kinds one inactive with different provider",
			kinds: []scmReactionKind{
				{name: "review_comments", active: true, provider: "github"},
				{name: "auto_merge", active: false, provider: "gitlab"},
				{name: "bot_review", active: true, provider: "github"},
			},
			wantActiveKinds:       []string{"review_comments", "bot_review"},
			wantDistinctProviders: []string{"github"},
			wantConflict:          false,
		},
		{
			name: "active kinds returned in input order on conflict",
			kinds: []scmReactionKind{
				{name: "bot_review", active: true, provider: "github"},
				{name: "review_comments", active: true, provider: "gitlab"},
			},
			wantActiveKinds:       []string{"bot_review", "review_comments"},
			wantDistinctProviders: []string{"github", "gitlab"},
			wantConflict:          true,
		},
		{
			name: "ci_failure active alongside a different provider",
			kinds: []scmReactionKind{
				{name: "review_comments", active: true, provider: "github"},
				{name: "ci_failure", active: true, provider: "gitlab"},
			},
			wantActiveKinds:       []string{"review_comments", "ci_failure"},
			wantDistinctProviders: []string{"github", "gitlab"},
			wantConflict:          true,
		},
		{
			name: "ci_failure active alone",
			kinds: []scmReactionKind{
				{name: "ci_failure", active: true, provider: "github"},
			},
			wantActiveKinds:       []string{"ci_failure"},
			wantDistinctProviders: []string{"github"},
			wantConflict:          false,
		},
		{
			name: "all inactive kinds",
			kinds: []scmReactionKind{
				{name: "review_comments", active: false, provider: "github"},
				{name: "auto_merge", active: false, provider: "gitlab"},
				{name: "bot_review", active: false, provider: "bitbucket"},
			},
			wantActiveKinds:       nil,
			wantDistinctProviders: nil,
			wantConflict:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotKinds, gotProviders := scmProviderConflict(tt.kinds)

			// Verify conflict detection: more than one distinct provider == conflict.
			gotConflict := len(gotProviders) > 1
			if gotConflict != tt.wantConflict {
				t.Errorf("scmProviderConflict(%v) conflict = %v, want %v (providers: %v)",
					tt.kinds, gotConflict, tt.wantConflict, gotProviders)
			}

			// Verify active kinds list length and content.
			if len(gotKinds) != len(tt.wantActiveKinds) {
				t.Errorf("scmProviderConflict(%v) activeKinds = %v, want %v",
					tt.kinds, gotKinds, tt.wantActiveKinds)
			} else {
				for i, want := range tt.wantActiveKinds {
					if gotKinds[i] != want {
						t.Errorf("scmProviderConflict activeKinds[%d] = %q, want %q",
							i, gotKinds[i], want)
					}
				}
			}

			// Verify distinct providers list length and content.
			if len(gotProviders) != len(tt.wantDistinctProviders) {
				t.Errorf("scmProviderConflict(%v) distinctProviders = %v, want %v",
					tt.kinds, gotProviders, tt.wantDistinctProviders)
			} else {
				for i, want := range tt.wantDistinctProviders {
					if gotProviders[i] != want {
						t.Errorf("scmProviderConflict distinctProviders[%d] = %q, want %q",
							i, gotProviders[i], want)
					}
				}
			}
		})
	}
}
