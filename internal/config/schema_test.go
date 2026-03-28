package config

import (
	"strings"
	"testing"
)

// TestValidateFrontMatter validates the advisory static analysis in
// ValidateFrontMatter. Each case exercises a single warning category in
// isolation so that ordering assumptions remain trivially verifiable.
func TestValidateFrontMatter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         map[string]any
		cfg         ServiceConfig
		wantCount   int
		wantChecks  []string // expected Check values in order
		wantFields  []string // expected Field values in order
		wantMsgSubs []string // substring to find in warnings[i].Message (optional)
	}{
		// --- Nil / empty maps ---
		{
			name:      "nil raw returns no warnings",
			raw:       nil,
			wantCount: 0,
		},
		{
			name:      "empty raw returns no warnings",
			raw:       map[string]any{},
			wantCount: 0,
		},

		// --- Phase 1: Unknown top-level keys ---
		{
			name:       "unknown top-level key trackers",
			raw:        map[string]any{"trackers": map[string]any{"kind": "file"}},
			wantCount:  1,
			wantChecks: []string{"unknown_key"},
			wantFields: []string{"trackers"},
		},
		{
			// Alphabetical order: "pooling" before "trackers".
			name: "multiple unknown top-level keys sorted alphabetically",
			raw: map[string]any{
				"trackers": map[string]any{},
				"pooling":  map[string]any{},
			},
			wantCount:  2,
			wantChecks: []string{"unknown_key", "unknown_key"},
			wantFields: []string{"pooling", "trackers"},
		},
		{
			name:      "known extension key server produces no warning",
			raw:       map[string]any{"server": map[string]any{"port": 8080}},
			wantCount: 0,
		},
		{
			name:      "known extension key worker produces no warning",
			raw:       map[string]any{"worker": map[string]any{"ssh_hosts": []any{}}},
			wantCount: 0,
		},
		{
			name:      "known extension key logging produces no warning",
			raw:       map[string]any{"logging": map[string]any{"level": "debug"}},
			wantCount: 0,
		},
		{
			// cfg.Tracker.Kind = "jira" registers "jira" as a recognized key.
			name: "dynamic extension key matches tracker kind",
			raw: map[string]any{
				"tracker": map[string]any{"kind": "jira"},
				"jira":    map[string]any{"url": "https://jira.example.com"},
			},
			cfg:       ServiceConfig{Tracker: TrackerConfig{Kind: "jira"}},
			wantCount: 0,
		},
		{
			// cfg.Agent.Kind = "claude-code" registers "claude-code" as a recognized key.
			name: "dynamic extension key matches agent kind",
			raw: map[string]any{
				"agent":       map[string]any{"kind": "claude-code"},
				"claude-code": map[string]any{"model": "claude-3"},
			},
			cfg:       ServiceConfig{Agent: AgentConfig{Kind: "claude-code"}},
			wantCount: 0,
		},
		{
			// cfg.Tracker.Kind = "file", so "jira" is not a recognized extension key.
			name: "dynamic extension key does not match tracker kind",
			raw: map[string]any{
				"tracker": map[string]any{"kind": "file"},
				"jira":    map[string]any{"url": "https://jira.example.com"},
			},
			cfg:        ServiceConfig{Tracker: TrackerConfig{Kind: "file"}},
			wantCount:  1,
			wantChecks: []string{"unknown_key"},
			wantFields: []string{"jira"},
		},
		{
			// env-override-created section: tracker with known keys only.
			name:      "env-override-created tracker section no false positives",
			raw:       map[string]any{"tracker": map[string]any{"kind": "file"}},
			wantCount: 0,
		},

		// --- Phase 2: Unknown sub-keys in known sections ---
		{
			name: "unknown tracker sub-key",
			raw: map[string]any{
				"tracker": map[string]any{"kind": "file", "typo_endpoint": "https://example.com"},
			},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"tracker.typo_endpoint"},
		},
		{
			// tracker.kind = "jira", so "jira" sub-object is exempt.
			name: "adapter passthrough sub-key exempt when kind matches",
			raw: map[string]any{
				"tracker": map[string]any{
					"kind": "jira",
					"jira": map[string]any{"foo": "bar"},
				},
			},
			cfg:       ServiceConfig{Tracker: TrackerConfig{Kind: "jira"}},
			wantCount: 0,
		},
		{
			// tracker.kind = "file", so "jira" sub-object is NOT exempt.
			name: "adapter passthrough sub-key not exempt when kind differs",
			raw: map[string]any{
				"tracker": map[string]any{
					"kind": "file",
					"jira": map[string]any{"foo": "bar"},
				},
			},
			cfg:        ServiceConfig{Tracker: TrackerConfig{Kind: "file"}},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"tracker.jira"},
		},
		{
			name: "unknown nested sub-key in tracker.comments",
			raw: map[string]any{
				"tracker": map[string]any{
					"comments": map[string]any{"typo_field": true},
				},
			},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"tracker.comments.typo_field"},
		},
		{
			name: "unknown hooks sub-key",
			raw: map[string]any{
				"hooks": map[string]any{
					"after_create": "echo done",
					"typo_hook":    "echo extra",
				},
			},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"hooks.typo_hook"},
		},
		{
			// agent.kind = "mock", so "mock" sub-object is exempt.
			name: "agent adapter passthrough sub-key exempt",
			raw: map[string]any{
				"agent": map[string]any{"kind": "mock", "mock": map[string]any{}},
			},
			cfg:       ServiceConfig{Agent: AgentConfig{Kind: "mock"}},
			wantCount: 0,
		},
		{
			// "typo_field" is not known, and "typo_field" != adapterKind "mock".
			name: "unknown agent sub-key not exempt",
			raw: map[string]any{
				"agent": map[string]any{"kind": "mock", "typo_field": "value"},
			},
			cfg:        ServiceConfig{Agent: AgentConfig{Kind: "mock"}},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"agent.typo_field"},
		},

		// --- Phase 3: Section-level type mismatch (scalar instead of map) ---
		{
			name: "tracker section is scalar not map",
			raw:  map[string]any{"tracker": "not-a-map"},
			// Sections iterate alphabetically: agent, hooks, polling, tracker.
			// Only "tracker" is present and scalar.
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"tracker"},
		},

		// --- Phase 3: Field-level type mismatches ---
		{
			name: "type mismatch tracker.kind is integer",
			raw: map[string]any{
				"tracker": map[string]any{"kind": 123},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"tracker.kind"},
		},
		{
			name: "type mismatch tracker.active_states is string not list",
			raw: map[string]any{
				"tracker": map[string]any{"active_states": "Open"},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"tracker.active_states"},
		},
		{
			// Elements [1] and [2] are non-string; [0] "Open" is valid.
			name: "non-string elements in tracker.active_states",
			raw: map[string]any{
				"tracker": map[string]any{
					"active_states": []any{"Open", 123, true},
				},
			},
			wantCount:  2,
			wantChecks: []string{"type_mismatch", "type_mismatch"},
			wantFields: []string{"tracker.active_states[1]", "tracker.active_states[2]"},
		},
		{
			name: "type mismatch tracker.comments.on_dispatch is string not bool",
			raw: map[string]any{
				"tracker": map[string]any{
					"comments": map[string]any{"on_dispatch": "yes"},
				},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"tracker.comments.on_dispatch"},
		},
		{
			name: "type mismatch polling.interval_ms is non-numeric string",
			raw: map[string]any{
				"polling": map[string]any{"interval_ms": "not-a-number"},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"polling.interval_ms"},
		},
		{
			// "30000" is a coercible string — treated as valid integer.
			name:      "polling.interval_ms coercible string produces no warning",
			raw:       map[string]any{"polling": map[string]any{"interval_ms": "30000"}},
			wantCount: 0,
		},
		{
			name: "type mismatch hooks.timeout_ms is non-numeric string",
			raw: map[string]any{
				"hooks": map[string]any{"timeout_ms": "not-a-number"},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"hooks.timeout_ms"},
		},
		{
			// timeout_ms = "30000" passes both Phase 3 (coercible) and Phase 3b (>0).
			name:      "hooks.timeout_ms coercible string 30000 produces no warning",
			raw:       map[string]any{"hooks": map[string]any{"timeout_ms": "30000"}},
			wantCount: 0,
		},
		{
			// timeout_ms float64(30000) is a valid JSON number.
			name:      "hooks.timeout_ms float64 30000 produces no warning",
			raw:       map[string]any{"hooks": map[string]any{"timeout_ms": float64(30000)}},
			wantCount: 0,
		},
		{
			// stall_timeout_ms = 0 is a valid sentinel meaning "disable stall check".
			name:      "agent.stall_timeout_ms zero is valid sentinel",
			raw:       map[string]any{"agent": map[string]any{"stall_timeout_ms": 0}},
			wantCount: 0,
		},
		{
			name: "type mismatch agent.stall_timeout_ms is string abc",
			raw: map[string]any{
				"agent": map[string]any{"stall_timeout_ms": "abc"},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"agent.stall_timeout_ms"},
		},

		// --- Phase 3: Top-level db_path ---
		{
			name: "type mismatch db_path is integer",
			raw:  map[string]any{"db_path": 123},
			// db_path is a known top-level key (no unknown_key warning), but must be a string.
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"db_path"},
		},
		{
			name:      "db_path as string produces no warning",
			raw:       map[string]any{"db_path": "some/path.db"},
			wantCount: 0,
		},

		// --- Phase 3b: hooks.timeout_ms semantic (non-positive) ---
		{
			// -5 passes Phase 3 (valid int type) but fails Phase 3b (≤ 0).
			name: "hooks.timeout_ms negative value",
			raw: map[string]any{
				"hooks": map[string]any{"timeout_ms": -5},
			},
			wantCount:   1,
			wantChecks:  []string{"type_mismatch"},
			wantFields:  []string{"hooks.timeout_ms"},
			wantMsgSubs: []string{"non-positive"},
		},
		{
			// 0 passes Phase 3 (valid int type) but fails Phase 3b (≤ 0).
			name: "hooks.timeout_ms zero value",
			raw: map[string]any{
				"hooks": map[string]any{"timeout_ms": 0},
			},
			wantCount:   1,
			wantChecks:  []string{"type_mismatch"},
			wantFields:  []string{"hooks.timeout_ms"},
			wantMsgSubs: []string{"non-positive"},
		},
		{
			name:      "hooks.timeout_ms positive value produces no warning",
			raw:       map[string]any{"hooks": map[string]any{"timeout_ms": 1}},
			wantCount: 0,
		},

		// --- Phase 3b: agent.max_concurrent_agents_by_state semantic ---
		{
			name: "agent.max_concurrent_agents_by_state non-numeric value",
			raw: map[string]any{
				"agent": map[string]any{
					"max_concurrent_agents_by_state": map[string]any{
						"In Progress": "abc",
					},
				},
			},
			wantCount:   1,
			wantChecks:  []string{"type_mismatch"},
			wantFields:  []string{"agent.max_concurrent_agents_by_state.In Progress"},
			wantMsgSubs: []string{"non-numeric"},
		},
		{
			name: "agent.max_concurrent_agents_by_state non-positive value",
			raw: map[string]any{
				"agent": map[string]any{
					"max_concurrent_agents_by_state": map[string]any{
						"In Progress": -1,
					},
				},
			},
			wantCount:   1,
			wantChecks:  []string{"type_mismatch"},
			wantFields:  []string{"agent.max_concurrent_agents_by_state.In Progress"},
			wantMsgSubs: []string{"non-positive"},
		},
		{
			name: "agent.max_concurrent_agents_by_state positive value produces no warning",
			raw: map[string]any{
				"agent": map[string]any{
					"max_concurrent_agents_by_state": map[string]any{
						"In Progress": 2,
					},
				},
			},
			wantCount: 0,
		},

		// --- Full valid config: no warnings ---
		{
			name: "fully valid config with all known keys produces no warnings",
			raw: map[string]any{
				"tracker": map[string]any{
					"kind":            "file",
					"active_states":   []any{"To Do", "In Progress"},
					"terminal_states": []any{"Done"},
					"comments": map[string]any{
						"on_dispatch":   true,
						"on_completion": true,
						"on_failure":    false,
					},
				},
				"polling": map[string]any{
					"interval_ms": 30000,
				},
				"workspace": map[string]any{
					"root": "/tmp/ws",
				},
				"hooks": map[string]any{
					"after_create": "echo created",
					"timeout_ms":   60000,
				},
				"agent": map[string]any{
					"kind":                  "mock",
					"max_concurrent_agents": 5,
					"max_turns":             10,
				},
				"db_path": "/data/db.sqlite",
			},
			cfg: ServiceConfig{
				Tracker: TrackerConfig{Kind: "file"},
				Agent:   AgentConfig{Kind: "mock"},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ValidateFrontMatter(tt.raw, tt.cfg)

			if len(got) != tt.wantCount {
				t.Fatalf("ValidateFrontMatter() returned %d warnings, want %d\nwarnings: %+v", len(got), tt.wantCount, got)
			}
			for i, wantCheck := range tt.wantChecks {
				if got[i].Check != wantCheck {
					t.Errorf("warnings[%d].Check = %q, want %q", i, got[i].Check, wantCheck)
				}
			}
			for i, wantField := range tt.wantFields {
				if got[i].Field != wantField {
					t.Errorf("warnings[%d].Field = %q, want %q", i, got[i].Field, wantField)
				}
			}
			for i, wantSub := range tt.wantMsgSubs {
				if !strings.Contains(got[i].Message, wantSub) {
					t.Errorf("warnings[%d].Message = %q, want to contain %q", i, got[i].Message, wantSub)
				}
			}
		})
	}
}
