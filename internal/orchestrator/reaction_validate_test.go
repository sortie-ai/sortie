package orchestrator

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// --- Test helpers ---

// assertNoMessageContains fails the test if any diag's Message contains substr.
func assertNoMessageContains(t *testing.T, diags []registry.ValidationDiag, substr string) {
	t.Helper()
	for i, d := range diags {
		if strings.Contains(d.Message, substr) {
			t.Errorf("diag[%d].Message = %q, must not contain %q", i, d.Message, substr)
		}
	}
}

// --- Tests ---

func TestValidateReactionConfigs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cfg        config.ServiceConfig
		wantChecks []string
	}{
		{
			name: "review_comments poll_interval_ms below minimum",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {Provider: "gitea", Extra: map[string]any{"poll_interval_ms": 1000}},
				},
			},
			wantChecks: []string{"reactions.review_comments"},
		},
		{
			name: "review_comments debounce_ms negative",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {Provider: "gitea", Extra: map[string]any{"debounce_ms": -1}},
				},
			},
			wantChecks: []string{"reactions.review_comments"},
		},
		{
			name: "review_comments max_continuation_turns zero",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {Provider: "gitea", Extra: map[string]any{"max_continuation_turns": 0}},
				},
			},
			wantChecks: []string{"reactions.review_comments"},
		},
		{
			name: "bot_review bare string bot_usernames",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea", Extra: map[string]any{"bot_usernames": "alice"}},
				},
			},
			wantChecks: []string{"reactions.bot_review"},
		},
		{
			name: "bot_review non-string element in bot_usernames list",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea", Extra: map[string]any{"bot_usernames": []any{"alice", 42}}},
				},
			},
			wantChecks: []string{"reactions.bot_review"},
		},
		{
			name: "auto_merge strategy out of enum",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"auto_merge": {Provider: "gitea", Extra: map[string]any{"strategy": "rebase-merge"}},
				},
			},
			wantChecks: []string{"reactions.auto_merge"},
		},
		{
			name: "auto_merge strategy non-string",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"auto_merge": {Provider: "gitea", Extra: map[string]any{"strategy": 123}},
				},
			},
			wantChecks: []string{"reactions.auto_merge"},
		},
		{
			name: "merge_conflicts poll_interval_ms below minimum",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"merge_conflicts": {Provider: "gitea", Extra: map[string]any{"poll_interval_ms": 1000}},
				},
			},
			wantChecks: []string{"reactions.merge_conflicts"},
		},
		{
			name: "review_comments watch_window_ms negative surfaces a diagnostic",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {Provider: "gitea", Extra: map[string]any{"watch_window_ms": -1}},
				},
			},
			wantChecks: []string{"reactions.review_comments"},
		},
		{
			name: "bot_review watch_window_ms above ceiling surfaces a diagnostic",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea", Extra: map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS) + 1}},
				},
			},
			wantChecks: []string{"reactions.bot_review"},
		},
		{
			name: "auto_merge watch_window_ms non-numeric surfaces a diagnostic",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"auto_merge": {Provider: "gitea", Extra: map[string]any{"watch_window_ms": "soon"}},
				},
			},
			wantChecks: []string{"reactions.auto_merge"},
		},
		{
			name: "merge_conflicts watch_window_ms zero produces no diagnostic",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"merge_conflicts": {Provider: "gitea", Extra: map[string]any{"watch_window_ms": 0}},
				},
			},
			wantChecks: nil,
		},
		{
			// buildReactionsConfig hard-errors on a malformed escalation
			// before ValidateReactionConfigs ever runs, so this arm is
			// reachable ONLY through this direct call, never through
			// runValidate. See TestValidateShadowedEscalation in cmd/sortie
			// for the end-to-end proof that the config layer fires first.
			name: "bot_review shadowed escalation reachable only by direct call",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea", Escalation: "bogus"},
				},
			},
			wantChecks: []string{"reactions.bot_review"},
		},
		{
			name: "auto_merge shadowed escalation reachable only by direct call",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"auto_merge": {Provider: "gitea", Escalation: "bogus"},
				},
			},
			wantChecks: []string{"reactions.auto_merge"},
		},
		{
			name: "both reactions invalid accumulate in order",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea", Extra: map[string]any{"bot_usernames": "alice"}},
					"auto_merge": {Provider: "gitea", Extra: map[string]any{"strategy": "bad"}},
				},
			},
			wantChecks: []string{"reactions.bot_review", "reactions.auto_merge"},
		},
		{
			name: "clean bot_review and auto_merge produce no diags",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea", Extra: map[string]any{"bot_usernames": []any{"alice", "bob"}}},
					"auto_merge": {Provider: "gitea", Extra: map[string]any{"strategy": "squash"}},
				},
			},
			wantChecks: nil,
		},
		{
			name: "clean review_comments and merge_conflicts produce no diags",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {
						Provider: "gitea",
						Extra: map[string]any{
							"poll_interval_ms":       30000,
							"debounce_ms":            0,
							"max_continuation_turns": 1,
						},
					},
					"merge_conflicts": {Provider: "gitea", Extra: map[string]any{"poll_interval_ms": 30000}},
				},
			},
			wantChecks: nil,
		},
		{
			name: "review_comments and merge_conflicts invalid accumulate in order",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {Provider: "gitea", Extra: map[string]any{"debounce_ms": -1}},
					"merge_conflicts": {Provider: "gitea", Extra: map[string]any{"poll_interval_ms": 1000}},
				},
			},
			wantChecks: []string{"reactions.review_comments", "reactions.merge_conflicts"},
		},
		{
			name:       "absent reactions map",
			cfg:        config.ServiceConfig{},
			wantChecks: nil,
		},
		{
			name: "inactive reactions with empty provider are skipped",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {Provider: "", Extra: map[string]any{"poll_interval_ms": 1000}},
					"bot_review":      {Provider: "", Extra: map[string]any{"bot_usernames": "not-a-list"}},
					"auto_merge":      {Provider: "", Extra: map[string]any{"strategy": "bogus"}},
					"merge_conflicts": {Provider: "", Extra: map[string]any{"poll_interval_ms": 1000}},
				},
			},
			wantChecks: nil,
		},
		{
			name: "nil Extra tolerated with defaults applied",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"review_comments": {Provider: "gitea"},
					"bot_review":      {Provider: "gitea"},
					"auto_merge":      {Provider: "gitea"},
					"merge_conflicts": {Provider: "gitea"},
				},
			},
			wantChecks: nil,
		},
		{
			name: "unknown reaction fields and kinds are ignored",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"merge_conflicts":  {Provider: "gitea", Extra: map[string]any{"debounce_ms": "unused"}},
					"label_commands":   {Provider: "gitea", Extra: map[string]any{"anything": "goes"}},
					"unknown_reaction": {Provider: "gitea", Extra: map[string]any{"anything": "goes"}},
				},
			},
			wantChecks: nil,
		},
		{
			name: "merge_completion invalid target_state produces a diagnostic",
			cfg: config.ServiceConfig{
				Tracker: config.TrackerConfig{
					HandoffState:   "in-review",
					TerminalStates: []string{"done"},
				},
				Reactions: map[string]config.ReactionConfig{
					"merge_completion": {Provider: "gitea", Extra: map[string]any{"target_state": "in-review"}},
				},
			},
			wantChecks: []string{"reactions.merge_completion"},
		},
		{
			name: "merge_completion valid block produces no diagnostic",
			cfg: config.ServiceConfig{
				Tracker: config.TrackerConfig{
					HandoffState:   "in-review",
					TerminalStates: []string{"done"},
				},
				Reactions: map[string]config.ReactionConfig{
					"merge_completion": {Provider: "gitea", Extra: map[string]any{"target_state": "done"}},
				},
			},
			wantChecks: nil,
		},
		{
			name: "no merge_completion block produces no diagnostic of that check",
			cfg: config.ServiceConfig{
				Reactions: map[string]config.ReactionConfig{
					"bot_review": {Provider: "gitea", Extra: map[string]any{"bot_usernames": []any{"alice"}}},
				},
			},
			wantChecks: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ValidateReactionConfigs(tt.cfg, registry.TrackerMeta{})

			if len(got) != len(tt.wantChecks) {
				t.Fatalf("ValidateReactionConfigs(cfg) = %v, want %d diag(s) with checks %v", got, len(tt.wantChecks), tt.wantChecks)
			}
			for i, wantCheck := range tt.wantChecks {
				if got[i].Check != wantCheck {
					t.Errorf("ValidateReactionConfigs(cfg) diag[%d].Check = %q, want %q", i, got[i].Check, wantCheck)
				}
				if got[i].Severity != "error" {
					t.Errorf("ValidateReactionConfigs(cfg) diag[%d].Severity = %q, want %q", i, got[i].Severity, "error")
				}
			}
		})
	}

	t.Run("messages never leak a secret value", func(t *testing.T) {
		t.Parallel()

		const secret = "sortie-secret-token-9f3ac1" //nolint:gosec // test fixture value, not a real credential

		cfg := config.ServiceConfig{
			Tracker: config.TrackerConfig{APIKey: secret},
			Reactions: map[string]config.ReactionConfig{
				"bot_review": {Provider: "gitea", Extra: map[string]any{"bot_usernames": "not-a-list"}},
				"auto_merge": {Provider: "gitea", Extra: map[string]any{"strategy": "rebase-merge"}},
			},
		}

		got := ValidateReactionConfigs(cfg, registry.TrackerMeta{})

		if len(got) != 2 {
			t.Fatalf("ValidateReactionConfigs(cfg) = %v, want 2 diags", got)
		}
		assertNoMessageContains(t, got, secret)
	})
}
