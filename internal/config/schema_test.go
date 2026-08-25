package config

import (
	"slices"
	"strings"
	"testing"
)

// TestValidateFrontMatterDispatchRulesSequence verifies that dispatch.rules
// is treated as a FieldSequence: rule-level unknown keys and match-level
// unknown keys are reported as unknown_sub_key warnings.
func TestValidateFrontMatterDispatchRulesSequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        map[string]any
		wantCount  int
		wantChecks []string
		wantFields []string
	}{
		{
			name: "unknown rule key emits unknown_sub_key",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"agent":      "mock",
							"typo_field": "value",
						},
					},
				},
			},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"dispatch.rules[0].typo_field"},
		},
		{
			name: "unknown match key emits unknown_sub_key",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"agent": "mock",
							"match": map[string]any{
								"labels":    []any{"bug"},
								"not_a_key": "val",
							},
						},
					},
				},
			},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"dispatch.rules[0].match.not_a_key"},
		},
		{
			name: "valid rule keys emit no warnings",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":     "bug",
							"match":    map[string]any{"labels": []any{"bug"}},
							"agent":    "mock",
							"template": "some/path.md",
						},
					},
				},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ValidateFrontMatter(tt.raw, ServiceConfig{Agent: AgentConfig{Kind: "mock"}})

			if len(got) != tt.wantCount {
				t.Fatalf("ValidateFrontMatter() returned %d warnings, want %d\nwarnings: %+v",
					len(got), tt.wantCount, got)
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
		})
	}
}

// TestDerivePassThroughKindsFromRaw verifies that the function extracts
// agent kinds from dispatch rules and default even when BuildDispatchConfig
// would fail.
func TestDerivePassThroughKindsFromRaw(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     map[string]any
		wantLen int
		wantIn  []string
	}{
		{
			name:    "nil raw returns nil",
			raw:     nil,
			wantLen: 0,
		},
		{
			name:    "absent dispatch key returns nil",
			raw:     map[string]any{},
			wantLen: 0,
		},
		{
			name: "extracts kinds from rules and default",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{"agent": "claude-code"},
						map[string]any{"agent": "codex"},
					},
					"default": map[string]any{
						"agent": "mock",
					},
				},
			},
			wantLen: 3,
			wantIn:  []string{"claude-code", "codex", "mock"},
		},
		{
			name: "rules with bad dispatch config still extracted",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"agent": "my-agent",
							"match": map[string]any{"labels": []any{"[unclosed"}},
						},
					},
				},
			},
			wantLen: 1,
			wantIn:  []string{"my-agent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := derivePassThroughKindsFromRaw(tt.raw)

			if len(got) != tt.wantLen {
				t.Errorf("derivePassThroughKindsFromRaw() len = %d, want %d; got %v",
					len(got), tt.wantLen, got)
				return
			}
			for _, want := range tt.wantIn {
				found := slices.Contains(got, want)
				if !found {
					t.Errorf("derivePassThroughKindsFromRaw() = %v, want to contain %q", got, want)
				}
			}
		})
	}
}

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

		// --- Unknown top-level keys ---
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

		// --- Unknown sub-keys in known sections ---
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

		// --- Section-level type mismatch (scalar instead of map) ---
		{
			name: "tracker section is scalar not map",
			raw:  map[string]any{"tracker": "not-a-map"},
			// Sections iterate alphabetically: agent, hooks, polling, tracker.
			// Only "tracker" is present and scalar.
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"tracker"},
		},

		// --- Field-level type mismatches ---
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
			// timeout_ms = "30000" passes both type coercion and the positive-value check.
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

		// --- Top-level db_path ---
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

		// --- hooks.timeout_ms semantic (non-positive) ---
		{
			// -5 passes the int-type check but fails the positive-value check.
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
			// 0 passes the int-type check but fails the positive-value check.
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

		// --- agent.max_concurrent_agents_by_state semantic ---
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

		// --- agent.max_consecutive_absences schema registration ---
		{
			name:      "agent.max_consecutive_absences known key produces no warning",
			raw:       map[string]any{"agent": map[string]any{"max_consecutive_absences": 5}},
			wantCount: 0,
		},
		{
			name: "type mismatch agent.max_consecutive_absences is string abc",
			raw: map[string]any{
				"agent": map[string]any{"max_consecutive_absences": "abc"},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"agent.max_consecutive_absences"},
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

// --- unresolved_extension_var warning tests ---

// buildCfgWithExtension is a helper that constructs a ServiceConfig with an
// extension block populated after env resolution so ValidateFrontMatter can
// operate on a realistic snapshot.
func buildCfgWithExtension(t *testing.T, extKey string, extVal map[string]any) ServiceConfig {
	t.Helper()
	raw := map[string]any{extKey: extVal}
	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}
	return cfg
}

// TestValidateFrontMatter_UnresolvedExtensionVar covers the new
// unresolved_extension_var warning class: bare form, braced form, nested paths,
// slice-index paths, set variables, multi-ref fields, $$ non-flagging,
// deduplication of repeated variable names, and message stability.
func TestValidateFrontMatter_UnresolvedExtensionVar(t *testing.T) {
	t.Run("EmitsWarningOnUnset", func(t *testing.T) {
		// An empty value is treated as unset by extractUnsetExtensionVars.
		t.Setenv("SORTIE_TEST_512_MISSING_BARE", "")

		raw := map[string]any{
			"myext": map[string]any{"api_key": "$SORTIE_TEST_512_MISSING_BARE"},
		}
		cfg := buildCfgWithExtension(t, "myext", map[string]any{"api_key": "$SORTIE_TEST_512_MISSING_BARE"})

		got := ValidateFrontMatter(raw, cfg)

		found := findUnresolvedExtVarWarning(got)
		if found == nil {
			t.Fatalf("ValidateFrontMatter() no unresolved_extension_var warning; warnings: %+v", got)
		}
		if found.Field != "myext.api_key" {
			t.Errorf("warning.Field = %q, want %q", found.Field, "myext.api_key")
		}
		if !strings.Contains(found.Message, "SORTIE_TEST_512_MISSING_BARE") {
			t.Errorf("warning.Message = %q, want to contain variable name", found.Message)
		}
	})

	t.Run("NoWarningWhenVariableSet", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_512_SET_VAR", "resolved-token")

		raw := map[string]any{
			"myext": map[string]any{"api_key": "$SORTIE_TEST_512_SET_VAR"},
		}
		cfg := buildCfgWithExtension(t, "myext", map[string]any{"api_key": "$SORTIE_TEST_512_SET_VAR"})

		got := ValidateFrontMatter(raw, cfg)

		for _, w := range got {
			if w.Check == "unresolved_extension_var" {
				t.Errorf("unexpected unresolved_extension_var warning when variable is set: %+v", w)
			}
		}
	})

	t.Run("BracedForm", func(t *testing.T) {
		// An empty value is treated as unset by extractUnsetExtensionVars.
		t.Setenv("SORTIE_TEST_512_BRACED_UNSET", "")

		raw := map[string]any{
			"myext": map[string]any{"token": "${SORTIE_TEST_512_BRACED_UNSET}"},
		}
		cfg := buildCfgWithExtension(t, "myext", map[string]any{"token": "${SORTIE_TEST_512_BRACED_UNSET}"})

		got := ValidateFrontMatter(raw, cfg)

		found := findUnresolvedExtVarWarning(got)
		if found == nil {
			t.Fatalf("ValidateFrontMatter() no unresolved_extension_var warning for ${VAR}; warnings: %+v", got)
		}
		if found.Field != "myext.token" {
			t.Errorf("warning.Field = %q, want %q", found.Field, "myext.token")
		}
		// Variable name without braces must appear in the message.
		if !strings.Contains(found.Message, "SORTIE_TEST_512_BRACED_UNSET") {
			t.Errorf("warning.Message = %q, want variable name without braces", found.Message)
		}
	})

	t.Run("SliceIndexPath", func(t *testing.T) {
		// An empty value is treated as unset by extractUnsetExtensionVars.
		t.Setenv("SORTIE_TEST_512_SLICE_HOST", "")

		raw := map[string]any{
			"worker": map[string]any{
				"ssh_hosts": []any{"literal-host", "literal2", "$SORTIE_TEST_512_SLICE_HOST"},
			},
		}
		cfg := buildCfgWithExtension(t, "worker", map[string]any{
			"ssh_hosts": []any{"literal-host", "literal2", "$SORTIE_TEST_512_SLICE_HOST"},
		})

		got := ValidateFrontMatter(raw, cfg)

		found := findUnresolvedExtVarWarning(got)
		if found == nil {
			t.Fatalf("no unresolved_extension_var warning for slice element; warnings: %+v", got)
		}
		if found.Field != "worker.ssh_hosts[2]" {
			t.Errorf("warning.Field = %q, want %q", found.Field, "worker.ssh_hosts[2]")
		}
	})

	t.Run("MultipleRefsOneUnset", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_512_MULTI_HOST", "example.com")
		// An empty value is treated as unset by extractUnsetExtensionVars.
		t.Setenv("SORTIE_TEST_512_MULTI_PORT", "")

		raw := map[string]any{
			"myext": map[string]any{
				"url": "https://$SORTIE_TEST_512_MULTI_HOST:$SORTIE_TEST_512_MULTI_PORT/path",
			},
		}
		cfg := buildCfgWithExtension(t, "myext", map[string]any{
			"url": "https://$SORTIE_TEST_512_MULTI_HOST:$SORTIE_TEST_512_MULTI_PORT/path",
		})

		got := ValidateFrontMatter(raw, cfg)

		var found []*FrontMatterWarning
		for i := range got {
			if got[i].Check == "unresolved_extension_var" {
				found = append(found, new(got[i]))
			}
		}
		if len(found) != 1 {
			t.Fatalf("ValidateFrontMatter() returned %d unresolved_extension_var warnings, want exactly 1; got %+v", len(found), got)
		}
		// Message must name only the unset variable.
		if !strings.Contains(found[0].Message, "SORTIE_TEST_512_MULTI_PORT") {
			t.Errorf("warning.Message = %q, want to contain PORT variable name", found[0].Message)
		}
		if strings.Contains(found[0].Message, "SORTIE_TEST_512_MULTI_HOST") {
			t.Errorf("warning.Message = %q, must not contain the set HOST variable name", found[0].Message)
		}
		// The resolved value must not appear.
		if strings.Contains(found[0].Message, "example.com") {
			t.Errorf("warning.Message = %q, must not contain the resolved value", found[0].Message)
		}
	})

	t.Run("DedupRepeatedVar", func(t *testing.T) {
		// An empty value is treated as unset by extractUnsetExtensionVars.
		t.Setenv("SORTIE_TEST_512_DEDUP_HOST", "")

		raw := map[string]any{
			"myext": map[string]any{
				"url": "https://$SORTIE_TEST_512_DEDUP_HOST:80/$SORTIE_TEST_512_DEDUP_HOST/path",
			},
		}
		cfg := buildCfgWithExtension(t, "myext", map[string]any{
			"url": "https://$SORTIE_TEST_512_DEDUP_HOST:80/$SORTIE_TEST_512_DEDUP_HOST/path",
		})

		got := ValidateFrontMatter(raw, cfg)

		found := findUnresolvedExtVarWarning(got)
		if found == nil {
			t.Fatalf("no unresolved_extension_var warning for dedup case; warnings: %+v", got)
		}
		// The variable name must appear exactly once in the message.
		count := strings.Count(found.Message, "SORTIE_TEST_512_DEDUP_HOST")
		if count != 1 {
			t.Errorf("warning.Message = %q, variable name appears %d times, want exactly 1 (dedup contract)", found.Message, count)
		}
		// Singular form: "variable ... is unset or empty"
		if !strings.Contains(found.Message, "variable") {
			t.Errorf("warning.Message = %q, want singular 'variable' form", found.Message)
		}
	})

	t.Run("DollarDollarNotFlagged", func(t *testing.T) {
		t.Parallel()
		// os.ExpandEnv("$$") -> ""; the resolved value is empty but the
		// variable name extracted by readShellName is "" so no warning fires.
		raw := map[string]any{
			"myext": map[string]any{"val": "$$"},
		}
		cfg := buildCfgWithExtension(t, "myext", map[string]any{"val": "$$"})

		got := ValidateFrontMatter(raw, cfg)

		for _, w := range got {
			if w.Check == "unresolved_extension_var" {
				t.Errorf("unexpected unresolved_extension_var warning for $$ literal: %+v", w)
			}
		}
	})

	t.Run("MessageStability", func(t *testing.T) {
		t.Parallel()
		// Verify formatUnresolvedExtensionVarMessage produces the exact
		// fixed-shape strings the spec mandates.
		one := formatUnresolvedExtensionVarMessage([]string{"MY_VAR"})
		wantOne := `unresolved $VAR reference: variable "MY_VAR" is unset or empty`
		if one != wantOne {
			t.Errorf("1-name: got %q, want %q", one, wantOne)
		}

		two := formatUnresolvedExtensionVarMessage([]string{"VAR_A", "VAR_B"})
		wantTwo := `unresolved $VAR reference: variables "VAR_A" and "VAR_B" are unset or empty`
		if two != wantTwo {
			t.Errorf("2-name: got %q, want %q", two, wantTwo)
		}

		three := formatUnresolvedExtensionVarMessage([]string{"VAR_A", "VAR_B", "VAR_C"})
		wantThree := `unresolved $VAR reference: variables "VAR_A", "VAR_B", and "VAR_C" are unset or empty`
		if three != wantThree {
			t.Errorf("3-name: got %q, want %q", three, wantThree)
		}
	})
}

// findUnresolvedExtVarWarning returns the first FrontMatterWarning with
// Check == "unresolved_extension_var", or nil when none exists.
func findUnresolvedExtVarWarning(warnings []FrontMatterWarning) *FrontMatterWarning {
	for i := range warnings {
		if warnings[i].Check == "unresolved_extension_var" {
			return new(warnings[i])
		}
	}
	return nil
}

// TestValidateFrontMatterReactions verifies that the reactions section with
// AllowDynamicKeys=true never emits unknown_sub_key warnings for any reaction
// kind keys, while still emitting type_mismatch when the top-level value is
// not a map.
func TestValidateFrontMatterReactions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        map[string]any
		cfg        ServiceConfig
		wantCount  int
		wantChecks []string
		wantFields []string
	}{
		{
			name: "ci_failure key produces no unknown_sub_key warning",
			raw: map[string]any{
				"reactions": map[string]any{
					"ci_failure": map[string]any{
						"provider":      "github-actions",
						"max_log_lines": 100,
					},
				},
			},
			wantCount: 0,
		},
		{
			name: "arbitrary reaction kind key produces no warning",
			raw: map[string]any{
				"reactions": map[string]any{
					"review_comments": map[string]any{
						"max_retries": 3,
						"custom_flag": "anything",
					},
				},
			},
			wantCount: 0,
		},
		{
			name: "multiple reaction kinds produce no warnings",
			raw: map[string]any{
				"reactions": map[string]any{
					"ci_failure":      map[string]any{"provider": "github-actions"},
					"review_comments": map[string]any{"max_retries": 3},
					"new_kind":        map[string]any{"arbitrary_key": "value"},
				},
			},
			wantCount: 0,
		},
		{
			name:       "reactions section as scalar emits type_mismatch",
			raw:        map[string]any{"reactions": "not-a-map"},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"reactions"},
		},
		{
			name:      "reactions section absent produces no warning",
			raw:       map[string]any{},
			wantCount: 0,
		},
		{
			name: "tracker adapter passthrough unaffected by reactions AllowDynamicKeys",
			raw: map[string]any{
				"tracker": map[string]any{
					"kind": "jira",
					"jira": map[string]any{"foo": "bar"},
				},
				"reactions": map[string]any{
					"ci_failure": map[string]any{"provider": "github-actions"},
				},
			},
			cfg:       ServiceConfig{Tracker: TrackerConfig{Kind: "jira"}},
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
		})
	}
}
