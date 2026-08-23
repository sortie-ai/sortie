package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewServiceConfig(t *testing.T) {
	t.Run("Defaults/EmptyMap", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertIntEqual(t, "Polling.IntervalMS", 30000, cfg.Polling.IntervalMS)
		if !strings.HasSuffix(cfg.Workspace.Root, "sortie_workspaces") {
			t.Errorf("Workspace.Root = %q, want suffix sortie_workspaces", cfg.Workspace.Root)
		}
		assertIntEqual(t, "Hooks.TimeoutMS", 60000, cfg.Hooks.TimeoutMS)
		assertStringEqual(t, "Agent.Kind", "claude-code", cfg.Agent.Kind)
		assertIntEqual(t, "Agent.TurnTimeoutMS", 3600000, cfg.Agent.TurnTimeoutMS)
		assertIntEqual(t, "Agent.ReadTimeoutMS", 5000, cfg.Agent.ReadTimeoutMS)
		assertIntEqual(t, "Agent.StallTimeoutMS", 300000, cfg.Agent.StallTimeoutMS)
		assertIntEqual(t, "Agent.MaxConcurrentAgents", 10, cfg.Agent.MaxConcurrentAgents)
		assertIntEqual(t, "Agent.MaxTurns", 20, cfg.Agent.MaxTurns)
		assertIntEqual(t, "Agent.MaxRetryBackoffMS", 300000, cfg.Agent.MaxRetryBackoffMS)
		if cfg.Agent.MaxConcurrentByState == nil {
			t.Error("Agent.MaxConcurrentByState is nil, want empty map")
		}
		if len(cfg.Agent.MaxConcurrentByState) != 0 {
			t.Errorf("Agent.MaxConcurrentByState has %d entries, want 0", len(cfg.Agent.MaxConcurrentByState))
		}
		if cfg.extensions == nil {
			t.Error("Extensions is nil, want empty map")
		}
	})

	t.Run("Defaults/NilMap", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Polling.IntervalMS", 30000, cfg.Polling.IntervalMS)
		assertStringEqual(t, "Agent.Kind", "claude-code", cfg.Agent.Kind)
	})

	t.Run("FullRoundTrip", func(t *testing.T) {
		t.Setenv("TEST_API_KEY", "tok_abc")

		raw := map[string]any{
			"tracker": map[string]any{
				"kind":            "jira",
				"endpoint":        "https://jira.example.com",
				"api_key":         "$TEST_API_KEY",
				"project":         "PROJ",
				"active_states":   []any{"To Do", "In Progress"},
				"terminal_states": []any{"Done"},
			},
			"polling": map[string]any{
				"interval_ms": 15000,
			},
			"workspace": map[string]any{
				"root": "/tmp/test_workspaces",
			},
			"hooks": map[string]any{
				"after_create":  "echo created",
				"before_run":    "echo before",
				"after_run":     "echo after",
				"before_remove": "echo removing",
				"timeout_ms":    30000,
			},
			"agent": map[string]any{
				"kind":                           "codex",
				"command":                        "codex --run",
				"turn_timeout_ms":                1800000,
				"read_timeout_ms":                3000,
				"stall_timeout_ms":               120000,
				"max_concurrent_agents":          5,
				"max_turns":                      10,
				"max_retry_backoff_ms":           600000,
				"max_concurrent_agents_by_state": map[string]any{"In Progress": 3, "Review": 1},
			},
			"db_path": "/data/sortie.db",
		}

		cfg, err := NewServiceConfig(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertStringEqual(t, "Tracker.Kind", "jira", cfg.Tracker.Kind)
		assertStringEqual(t, "Tracker.Endpoint", "https://jira.example.com", cfg.Tracker.Endpoint)
		assertStringEqual(t, "Tracker.APIKey", "tok_abc", cfg.Tracker.APIKey)
		assertStringEqual(t, "Tracker.Project", "PROJ", cfg.Tracker.Project)
		assertStringSliceEqual(t, "Tracker.ActiveStates", []string{"To Do", "In Progress"}, cfg.Tracker.ActiveStates)
		assertStringSliceEqual(t, "Tracker.TerminalStates", []string{"Done"}, cfg.Tracker.TerminalStates)

		assertIntEqual(t, "Polling.IntervalMS", 15000, cfg.Polling.IntervalMS)
		assertStringEqual(t, "Workspace.Root", "/tmp/test_workspaces", cfg.Workspace.Root)

		assertStringEqual(t, "Hooks.AfterCreate", "echo created", cfg.Hooks.AfterCreate)
		assertStringEqual(t, "Hooks.BeforeRun", "echo before", cfg.Hooks.BeforeRun)
		assertStringEqual(t, "Hooks.AfterRun", "echo after", cfg.Hooks.AfterRun)
		assertStringEqual(t, "Hooks.BeforeRemove", "echo removing", cfg.Hooks.BeforeRemove)
		assertIntEqual(t, "Hooks.TimeoutMS", 30000, cfg.Hooks.TimeoutMS)

		assertStringEqual(t, "Agent.Kind", "codex", cfg.Agent.Kind)
		assertStringEqual(t, "Agent.Command", "codex --run", cfg.Agent.Command)
		assertIntEqual(t, "Agent.TurnTimeoutMS", 1800000, cfg.Agent.TurnTimeoutMS)
		assertIntEqual(t, "Agent.ReadTimeoutMS", 3000, cfg.Agent.ReadTimeoutMS)
		assertIntEqual(t, "Agent.StallTimeoutMS", 120000, cfg.Agent.StallTimeoutMS)
		assertIntEqual(t, "Agent.MaxConcurrentAgents", 5, cfg.Agent.MaxConcurrentAgents)
		assertIntEqual(t, "Agent.MaxTurns", 10, cfg.Agent.MaxTurns)
		assertIntEqual(t, "Agent.MaxRetryBackoffMS", 600000, cfg.Agent.MaxRetryBackoffMS)
		assertIntEqual(t, "ByState[in progress]", 3, cfg.Agent.MaxConcurrentByState["in progress"])
		assertIntEqual(t, "ByState[review]", 1, cfg.Agent.MaxConcurrentByState["review"])

		assertStringEqual(t, "DBPath", "/data/sortie.db", cfg.DBPath)
	})

	t.Run("EnvResolution/DollarVar", func(t *testing.T) {
		t.Setenv("MY_TOKEN", "secret123")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"api_key": "$MY_TOKEN"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.APIKey", "secret123", cfg.Tracker.APIKey)
	})

	t.Run("EnvResolution/BraceSyntax", func(t *testing.T) {
		t.Setenv("MY_TOKEN", "secret123")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"api_key": "${MY_TOKEN}"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.APIKey", "secret123", cfg.Tracker.APIKey)
	})

	t.Run("EnvResolution/Embedded", func(t *testing.T) {
		t.Setenv("MY_TOKEN", "secret123")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"api_key": "Bearer $MY_TOKEN"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.APIKey", "Bearer secret123", cfg.Tracker.APIKey)
	})

	t.Run("EnvResolution/EndpointWholeVar", func(t *testing.T) {
		t.Setenv("JIRA_URL", "https://jira.example.com/rest/api/3")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"endpoint": "$JIRA_URL"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.Endpoint", "https://jira.example.com/rest/api/3", cfg.Tracker.Endpoint)
	})

	t.Run("EnvResolution/EndpointPreservesInlineVar", func(t *testing.T) {
		t.Setenv("JIRA_HOST", "jira.example.com")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"endpoint": "https://$JIRA_HOST/rest/api/3"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Inline $VAR in URIs must NOT be expanded.
		assertStringEqual(t, "Tracker.Endpoint", "https://$JIRA_HOST/rest/api/3", cfg.Tracker.Endpoint)
	})

	t.Run("EnvResolution/UnsetVar", func(t *testing.T) {
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"api_key": "$UNSET_VAR_XYZ_SORTIE_TEST"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.APIKey", "", cfg.Tracker.APIKey)
	})

	t.Run("PathExpansion/Tilde", func(t *testing.T) {
		t.Parallel()
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot determine home directory")
		}
		cfg, err := NewServiceConfig(map[string]any{
			"workspace": map[string]any{"root": "~/workspaces"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(home, "workspaces")
		assertStringEqual(t, "Workspace.Root", want, cfg.Workspace.Root)
	})

	t.Run("PathExpansion/EnvVar", func(t *testing.T) {
		t.Setenv("WORK_DIR", "/tmp/my_workspaces")
		cfg, err := NewServiceConfig(map[string]any{
			"workspace": map[string]any{"root": "$WORK_DIR"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Workspace.Root", "/tmp/my_workspaces", cfg.Workspace.Root)
	})

	t.Run("PathExpansion/TildeWithEnvVar", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot determine home directory")
		}
		t.Setenv("SORTIE_TEST_ENV", "staging")
		cfg, err := NewServiceConfig(map[string]any{
			"workspace": map[string]any{"root": "~/workspaces/$SORTIE_TEST_ENV"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(home, "workspaces", "staging")
		assertStringEqual(t, "Workspace.Root", want, cfg.Workspace.Root)
	})

	t.Run("Coercion/StringToInt", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"polling": map[string]any{"interval_ms": "5000"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Polling.IntervalMS", 5000, cfg.Polling.IntervalMS)
	})

	t.Run("Coercion/Float64ToInt", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_concurrent_agents": float64(5)},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxConcurrentAgents", 5, cfg.Agent.MaxConcurrentAgents)
	})

	t.Run("Coercion/InvalidString", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"polling": map[string]any{"interval_ms": "notanumber"},
		})
		assertConfigErrorField(t, err, "polling.interval_ms")
	})

	t.Run("Coercion/FractionalFloat64Rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"polling": map[string]any{"interval_ms": float64(0.9)},
		})
		assertConfigErrorField(t, err, "polling.interval_ms")
	})

	t.Run("ByStateMap/Normalization", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{
				"max_concurrent_agents_by_state": map[string]any{
					"In Progress": 3,
					"REVIEW":      2,
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "ByState[in progress]", 3, cfg.Agent.MaxConcurrentByState["in progress"])
		assertIntEqual(t, "ByState[review]", 2, cfg.Agent.MaxConcurrentByState["review"])
		if len(cfg.Agent.MaxConcurrentByState) != 2 {
			t.Errorf("expected 2 entries, got %d", len(cfg.Agent.MaxConcurrentByState))
		}
	})

	t.Run("ByStateMap/IgnoresNonPositive", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{
				"max_concurrent_agents_by_state": map[string]any{
					"In Progress": 0,
					"review":      -1,
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.Agent.MaxConcurrentByState) != 0 {
			t.Errorf("expected empty map, got %v", cfg.Agent.MaxConcurrentByState)
		}
	})

	t.Run("ByStateMap/IgnoresNonNumeric", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{
				"max_concurrent_agents_by_state": map[string]any{
					"In Progress": "abc",
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.Agent.MaxConcurrentByState) != 0 {
			t.Errorf("expected empty map, got %v", cfg.Agent.MaxConcurrentByState)
		}
	})

	t.Run("HooksTimeout/Zero", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"hooks": map[string]any{"timeout_ms": 0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Hooks.TimeoutMS", 60000, cfg.Hooks.TimeoutMS)
	})

	t.Run("HooksTimeout/Negative", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"hooks": map[string]any{"timeout_ms": -100},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Hooks.TimeoutMS", 60000, cfg.Hooks.TimeoutMS)
	})

	t.Run("Extensions/Collected", func(t *testing.T) {
		t.Parallel()
		raw := map[string]any{
			"server": map[string]any{"port": 8080},
			"worker": map[string]any{"ssh_hosts": []any{"host1"}},
		}
		cfg, err := NewServiceConfig(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		serverExt, ok := cfg.extensions["server"]
		if !ok {
			t.Fatal("Extensions missing 'server'")
		}
		serverMap, ok := serverExt.(map[string]any)
		if !ok {
			t.Fatalf("Extensions['server'] is %T, want map[string]any", serverExt)
		}
		if serverMap["port"] != 8080 {
			t.Errorf("server.port = %v, want 8080", serverMap["port"])
		}
		if _, ok := cfg.extensions["worker"]; !ok {
			t.Error("Extensions missing 'worker'")
		}
	})

	t.Run("AgentCommand/PreservedAsIs", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"command": "claude --flag=$VAR"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Agent.Command", "claude --flag=$VAR", cfg.Agent.Command)
	})

	t.Run("States/Extracted", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"active_states": []any{"To Do", "In Progress"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringSliceEqual(t, "Tracker.ActiveStates", []string{"To Do", "In Progress"}, cfg.Tracker.ActiveStates)
	})

	t.Run("StallTimeout/ZeroIsValid", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"stall_timeout_ms": 0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.StallTimeoutMS", 0, cfg.Agent.StallTimeoutMS)
	})

	t.Run("StallTimeout/AbsentGetsDefault", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"kind": "claude-code"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.StallTimeoutMS", 300000, cfg.Agent.StallTimeoutMS)
	})

	// --- DBPath subtests ---

	t.Run("DBPath/Absent", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig({}) unexpected error: %v", err)
		}
		assertStringEqual(t, "DBPath", "", cfg.DBPath)
	})

	t.Run("DBPath/ExplicitEmptyString", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": "",
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=\"\") unexpected error: %v", err)
		}
		assertStringEqual(t, "DBPath", "", cfg.DBPath)
	})

	t.Run("DBPath/AbsolutePath", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": "/data/sortie.db",
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=/data/sortie.db) unexpected error: %v", err)
		}
		assertStringEqual(t, "DBPath", "/data/sortie.db", cfg.DBPath)
	})

	t.Run("DBPath/RelativePath", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": "custom.db",
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=custom.db) unexpected error: %v", err)
		}
		// Config layer stores relative paths as-is; caller resolves.
		assertStringEqual(t, "DBPath", "custom.db", cfg.DBPath)
	})

	t.Run("DBPath/TildeExpansion", func(t *testing.T) {
		t.Parallel()
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot determine home directory")
		}
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": "~/sortie.db",
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=~/sortie.db) unexpected error: %v", err)
		}
		want := filepath.Join(home, "sortie.db")
		assertStringEqual(t, "DBPath", want, cfg.DBPath)
	})

	t.Run("DBPath/EnvVar", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_DB_PATH", "/tmp/test.db")
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": "$SORTIE_TEST_DB_PATH",
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=$SORTIE_TEST_DB_PATH) unexpected error: %v", err)
		}
		assertStringEqual(t, "DBPath", "/tmp/test.db", cfg.DBPath)
	})

	t.Run("DBPath/UnsetEnvVar", func(t *testing.T) {
		// An explicit db_path whose env var resolves to empty must
		// produce a ConfigError — silent fallback to the default
		// path would surprise the operator.
		_, err := NewServiceConfig(map[string]any{
			"db_path": "$SORTIE_UNSET_VAR_XYZ",
		})
		assertConfigErrorField(t, err, "db_path")
	})

	t.Run("DBPath/NonStringRejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"db_path": 42,
		})
		assertConfigErrorField(t, err, "db_path")
	})

	t.Run("DBPath/NotInExtensions", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": "/data/sortie.db",
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=/data/sortie.db) unexpected error: %v", err)
		}
		if _, ok := cfg.extensions["db_path"]; ok {
			t.Error("db_path should not appear in Extensions")
		}
	})

	t.Run("SectionAsNonMap", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": "not-a-map",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.Kind", "", cfg.Tracker.Kind)
		assertStringEqual(t, "Tracker.Endpoint", "", cfg.Tracker.Endpoint)
		assertStringEqual(t, "Tracker.APIKey", "", cfg.Tracker.APIKey)
	})

	// --- HandoffState subtests ---

	t.Run("HandoffState/Absent", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"kind": "jira",
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.HandoffState", "", cfg.Tracker.HandoffState)
	})

	t.Run("HandoffState/ValidValue", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state":   "Human Review",
				"active_states":   []any{"To Do", "In Progress"},
				"terminal_states": []any{"Done"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.HandoffState", "Human Review", cfg.Tracker.HandoffState)
	})

	t.Run("HandoffState/EmptyString", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state": "",
			},
		})
		assertConfigErrorField(t, err, "tracker.handoff_state")
	})

	t.Run("HandoffState/EnvVar", func(t *testing.T) {
		t.Setenv("TEST_HANDOFF", "Human Review")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state":   "$TEST_HANDOFF",
				"active_states":   []any{"To Do", "In Progress"},
				"terminal_states": []any{"Done"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.HandoffState", "Human Review", cfg.Tracker.HandoffState)
	})

	t.Run("HandoffState/UnsetEnvVar", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state": "$SORTIE_UNSET_VAR_XYZ",
			},
		})
		assertConfigErrorField(t, err, "tracker.handoff_state")
	})

	t.Run("HandoffState/CollidesWithActive", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state": "In Progress",
				"active_states": []any{"In Progress"},
			},
		})
		assertConfigErrorField(t, err, "tracker.handoff_state")
	})

	t.Run("HandoffState/CollidesWithActiveCaseInsensitive", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state": "in progress",
				"active_states": []any{"In Progress"},
			},
		})
		assertConfigErrorField(t, err, "tracker.handoff_state")
	})

	t.Run("HandoffState/CollidesWithTerminal", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state":   "Done",
				"terminal_states": []any{"Done"},
			},
		})
		assertConfigErrorField(t, err, "tracker.handoff_state")
	})

	t.Run("HandoffState/CollidesWithTerminalCaseInsensitive", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"handoff_state":   "done",
				"terminal_states": []any{"Done"},
			},
		})
		assertConfigErrorField(t, err, "tracker.handoff_state")
	})

	t.Run("HandoffState/ExplicitEmptyExistingField", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"kind":          "jira",
				"handoff_state": "",
			},
		})
		assertConfigErrorField(t, err, "tracker.handoff_state")
	})

	// --- HandoffEvidence subtests ---

	t.Run("HandoffEvidence/DefaultsToObserved", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := cfg.Tracker.HandoffEvidence; got != HandoffEvidenceObserved {
			t.Errorf("Tracker.HandoffEvidence = %q, want %q", got, HandoffEvidenceObserved)
		}
	})

	for _, policy := range []HandoffEvidencePolicy{
		HandoffEvidenceObserved,
		HandoffEvidenceStrict,
		HandoffEvidenceOff,
	} {
		t.Run("HandoffEvidence/Valid/"+string(policy), func(t *testing.T) {
			t.Parallel()
			cfg, err := NewServiceConfig(map[string]any{
				"tracker": map[string]any{"handoff_evidence": string(policy)},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.Tracker.HandoffEvidence; got != policy {
				t.Errorf("Tracker.HandoffEvidence = %q, want %q", got, policy)
			}
		})
	}

	for _, tt := range []struct {
		name  string
		value any
	}{
		{name: "empty", value: ""},
		{name: "unknown", value: "required"},
		{name: "wrong_case", value: "Observed"},
		{name: "non_string", value: true},
	} {
		t.Run("HandoffEvidence/Invalid/"+tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewServiceConfig(map[string]any{
				"tracker": map[string]any{"handoff_evidence": tt.value},
			})
			assertConfigErrorField(t, err, "tracker.handoff_evidence")
		})
	}

	t.Run("MaxSessions/DefaultIsZero", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxSessions", 0, cfg.Agent.MaxSessions)
	})

	t.Run("MaxSessions/ExplicitZero", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_sessions": 0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxSessions", 0, cfg.Agent.MaxSessions)
	})

	t.Run("MaxSessions/PositiveInteger", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_sessions": 5},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxSessions", 5, cfg.Agent.MaxSessions)
	})

	t.Run("MaxSessions/StringCoercion", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_sessions": "5"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxSessions", 5, cfg.Agent.MaxSessions)
	})

	t.Run("MaxSessions/NegativeRejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_sessions": -1},
		})
		assertConfigErrorField(t, err, "agent.max_sessions")
	})

	t.Run("MaxTokens/DefaultIsZero", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens", 0, cfg.Agent.MaxTokens)
	})

	t.Run("MaxTokens/ExplicitZero", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": 0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens", 0, cfg.Agent.MaxTokens)
	})

	t.Run("MaxTokens/PositiveInteger", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": 500000},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens", 500000, cfg.Agent.MaxTokens)
	})

	t.Run("MaxTokens/StringCoercion", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": "500000"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens", 500000, cfg.Agent.MaxTokens)
	})

	t.Run("MaxTokens/NegativeRejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": -1},
		})
		assertConfigErrorField(t, err, "agent.max_tokens")
	})

	// --- InProgressState subtests ---

	t.Run("InProgressState/Absent", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"kind": "jira"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.InProgressState", "", cfg.Tracker.InProgressState)
	})

	t.Run("InProgressState/Valid", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": "In Progress",
				"active_states":     []any{"In Progress", "In Review"},
				"terminal_states":   []any{"Done"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.InProgressState", "In Progress", cfg.Tracker.InProgressState)
	})

	t.Run("InProgressState/EmptyString", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": "",
			},
		})
		assertConfigErrorField(t, err, "tracker.in_progress_state")
	})

	t.Run("InProgressState/NonString", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": 42,
			},
		})
		assertConfigErrorField(t, err, "tracker.in_progress_state")
	})

	t.Run("InProgressState/EnvVarResolved", func(t *testing.T) {
		t.Setenv("TEST_IP_STATE", "Working")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": "$TEST_IP_STATE",
				"active_states":     []any{"Working"},
				"terminal_states":   []any{"Done"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertStringEqual(t, "Tracker.InProgressState", "Working", cfg.Tracker.InProgressState)
	})

	t.Run("InProgressState/EnvVarEmpty", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": "$SORTIE_UNSET_VAR_XYZ",
			},
		})
		assertConfigErrorField(t, err, "tracker.in_progress_state")
	})

	t.Run("InProgressState/CollidesWithTerminal", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": "Done",
				"terminal_states":   []any{"Done"},
				"active_states":     []any{"In Progress"},
			},
		})
		assertConfigErrorField(t, err, "tracker.in_progress_state")
	})

	t.Run("InProgressState/CollidesWithTerminalCaseInsensitive", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": "done",
				"terminal_states":   []any{"Done"},
				"active_states":     []any{"In Progress"},
			},
		})
		assertConfigErrorField(t, err, "tracker.in_progress_state")
	})

	t.Run("InProgressState/NotInActiveStates", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"in_progress_state": "Blocked",
				"active_states":     []any{"In Progress"},
				"terminal_states":   []any{"Done"},
			},
		})
		assertConfigErrorField(t, err, "tracker.in_progress_state")
	})

	// --- Reactions subtests ---

	t.Run("Reactions/Absent", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Reactions == nil {
			t.Error("Reactions is nil, want non-nil empty map")
		}
		if len(cfg.Reactions) != 0 {
			t.Errorf("Reactions length = %d, want 0", len(cfg.Reactions))
		}
	})

	t.Run("Reactions/FutureKindParsed", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"review_comments": map[string]any{
					"max_retries":      3,
					"escalation":       "comment",
					"escalation_label": "ci-escalated",
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc, ok := cfg.Reactions["review_comments"]
		if !ok {
			t.Fatal("Reactions missing key \"review_comments\"")
		}
		assertIntEqual(t, "Reactions[review_comments].MaxRetries", 3, rc.MaxRetries)
		assertStringEqual(t, "Reactions[review_comments].Escalation", "comment", rc.Escalation)
		assertStringEqual(t, "Reactions[review_comments].EscalationLabel", "ci-escalated", rc.EscalationLabel)
	})

	t.Run("Reactions/DefaultsApplied", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci": map[string]any{},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := cfg.Reactions["ci"]
		assertIntEqual(t, "Reactions[ci].MaxRetries", 2, rc.MaxRetries)
		assertStringEqual(t, "Reactions[ci].Escalation", "label", rc.Escalation)
		assertStringEqual(t, "Reactions[ci].EscalationLabel", "needs-human", rc.EscalationLabel)
	})

	t.Run("Reactions/MergeConflictsDefaultMaxRetriesOne", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"merge_conflicts": map[string]any{},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := cfg.Reactions["merge_conflicts"]
		assertIntEqual(t, "Reactions[merge_conflicts].MaxRetries", 1, rc.MaxRetries)
		assertStringEqual(t, "Reactions[merge_conflicts].Escalation", "label", rc.Escalation)
		assertStringEqual(t, "Reactions[merge_conflicts].EscalationLabel", "needs-human", rc.EscalationLabel)
	})

	t.Run("Reactions/MergeConflictsExplicitMaxRetriesOverrides", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"merge_conflicts": map[string]any{
					"max_retries": 3,
					"escalation":  "comment",
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := cfg.Reactions["merge_conflicts"]
		assertIntEqual(t, "Reactions[merge_conflicts].MaxRetries", 3, rc.MaxRetries)
		assertStringEqual(t, "Reactions[merge_conflicts].Escalation", "comment", rc.Escalation)
	})

	t.Run("Reactions/MergeConflictsDefaultLeavesOtherKindsUnchanged", func(t *testing.T) {
		t.Parallel()
		// The per-kind default-of-1 switch must apply ONLY to merge_conflicts.
		// Every sibling kind keeps its current effective default of 2.
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"merge_conflicts": map[string]any{},
				"ci_failure":      map[string]any{"provider": "github"},
				"review_comments": map[string]any{},
				"auto_merge":      map[string]any{"provider": "github"},
				"bot_review":      map[string]any{"provider": "github"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		assertIntEqual(t, "Reactions[merge_conflicts].MaxRetries", 1, cfg.Reactions["merge_conflicts"].MaxRetries)
		assertIntEqual(t, "Reactions[review_comments].MaxRetries", 2, cfg.Reactions["review_comments"].MaxRetries)
		assertIntEqual(t, "Reactions[auto_merge].MaxRetries", 2, cfg.Reactions["auto_merge"].MaxRetries)
		assertIntEqual(t, "Reactions[bot_review].MaxRetries", 2, cfg.Reactions["bot_review"].MaxRetries)
	})

	t.Run("Reactions/UnknownKeyStoredInExtra", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci": map[string]any{
					"max_retries": 1,
					"custom_flag": "value42",
				},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rc := cfg.Reactions["ci"]
		if rc.Extra == nil {
			t.Fatal("Reactions[ci].Extra is nil, want map with unknown key")
		}
		if rc.Extra["custom_flag"] != "value42" {
			t.Errorf("Reactions[ci].Extra[\"custom_flag\"] = %v, want %q", rc.Extra["custom_flag"], "value42")
		}
	})

	t.Run("Reactions/InvalidMaxRetriesNegative", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci": map[string]any{
					"max_retries": -1,
				},
			},
		})
		assertConfigErrorField(t, err, "reactions.ci.max_retries")
	})

	t.Run("Reactions/InvalidEscalationValue", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci": map[string]any{
					"escalation": "email",
				},
			},
		})
		assertConfigErrorField(t, err, "reactions.ci.escalation")
	})

	t.Run("Reactions/InvalidKeyFormatUppercase", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"CI_Feedback": map[string]any{},
			},
		})
		assertConfigErrorField(t, err, "reactions.CI_Feedback")
	})

	t.Run("Reactions/ProviderNonStringRejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider": 123,
				},
			},
		})
		assertConfigErrorField(t, err, "reactions.ci_failure.provider")
	})

	t.Run("Reactions/EscalationNonStringRejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci": map[string]any{
					"escalation": true,
				},
			},
		})
		assertConfigErrorField(t, err, "reactions.ci.escalation")
	})

	t.Run("Reactions/EscalationLabelNonStringRejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci": map[string]any{
					"escalation_label": 42,
				},
			},
		})
		assertConfigErrorField(t, err, "reactions.ci.escalation_label")
	})
}

// --- buildLabelCommandsConfig tests ---

func TestBuildLabelCommandsConfig_Defaults(t *testing.T) {
	t.Parallel()

	t.Run("absent block is a zero-value config, no error", func(t *testing.T) {
		t.Parallel()
		got, err := buildLabelCommandsConfig(nil)
		if err != nil {
			t.Fatalf("buildLabelCommandsConfig(nil): unexpected error: %v", err)
		}
		if got != (LabelCommandsConfig{}) {
			t.Errorf("buildLabelCommandsConfig(nil) = %+v, want zero value", got)
		}
	})

	t.Run("provider only fills in every default", func(t *testing.T) {
		t.Parallel()
		got, err := buildLabelCommandsConfig(map[string]any{
			"provider": "github",
		})
		if err != nil {
			t.Fatalf("buildLabelCommandsConfig: unexpected error: %v", err)
		}
		want := LabelCommandsConfig{
			Provider:       "github",
			ReviewLabel:    "sortie:review",
			FixLabel:       "sortie:fix",
			PollIntervalMS: 60000,
		}
		if got != want {
			t.Errorf("buildLabelCommandsConfig(provider only) = %+v, want %+v", got, want)
		}
	})
}

func TestBuildLabelCommandsConfig_EmptyProviderIgnoresFields(t *testing.T) {
	t.Parallel()

	// With no active provider the block is inert, so a below-floor poll
	// interval and a type-invalid label field are neither clamped nor
	// rejected: the whole block is ignored and yields a zero-value config.
	got, err := buildLabelCommandsConfig(map[string]any{
		"provider":         "",
		"poll_interval_ms": 5,
		"review_label":     123,
	})
	if err != nil {
		t.Fatalf("buildLabelCommandsConfig(empty provider): unexpected error: %v", err)
	}
	if got != (LabelCommandsConfig{}) {
		t.Errorf("buildLabelCommandsConfig(empty provider) = %+v, want zero value", got)
	}
}

func TestBuildLabelCommandsConfig_ReviewLabelDisabled(t *testing.T) {
	t.Parallel()

	got, err := buildLabelCommandsConfig(map[string]any{
		"provider":     "github",
		"review_label": "",
		"fix_label":    "sortie:fix",
	})
	if err != nil {
		t.Fatalf("buildLabelCommandsConfig: unexpected error: %v", err)
	}
	if got.ReviewLabel != "" {
		t.Errorf("ReviewLabel = %q, want empty (explicit disable)", got.ReviewLabel)
	}
	if got.FixLabel != "sortie:fix" {
		t.Errorf("FixLabel = %q, want %q", got.FixLabel, "sortie:fix")
	}
}

func TestBuildLabelCommandsConfig_FixLabelParsedNotWired(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    map[string]any
		want string
	}{
		{
			name: "absent defaults to sortie:fix",
			m:    map[string]any{"provider": "github"},
			want: "sortie:fix",
		},
		{
			name: "explicit custom value parsed verbatim",
			m:    map[string]any{"provider": "github", "fix_label": "custom:fix"},
			want: "custom:fix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildLabelCommandsConfig(tt.m)
			if err != nil {
				t.Fatalf("buildLabelCommandsConfig(%+v): unexpected error: %v", tt.m, err)
			}
			if got.FixLabel != tt.want {
				t.Errorf("FixLabel = %q, want %q", got.FixLabel, tt.want)
			}
		})
	}
}

func TestBuildLabelCommandsConfig_PollIntervalFloorClamp(t *testing.T) {
	t.Parallel()

	t.Run("below floor clamps to 30000", func(t *testing.T) {
		t.Parallel()
		got, err := buildLabelCommandsConfig(map[string]any{
			"provider":         "github",
			"poll_interval_ms": 5000,
		})
		if err != nil {
			t.Fatalf("buildLabelCommandsConfig: unexpected error: %v", err)
		}
		if got.PollIntervalMS != 30000 {
			t.Errorf("PollIntervalMS = %d, want 30000 (clamped)", got.PollIntervalMS)
		}
	})

	t.Run("at floor is unchanged", func(t *testing.T) {
		t.Parallel()
		got, err := buildLabelCommandsConfig(map[string]any{
			"provider":         "github",
			"poll_interval_ms": 30000,
		})
		if err != nil {
			t.Fatalf("buildLabelCommandsConfig: unexpected error: %v", err)
		}
		if got.PollIntervalMS != 30000 {
			t.Errorf("PollIntervalMS = %d, want 30000", got.PollIntervalMS)
		}
	})

	t.Run("above floor is unchanged", func(t *testing.T) {
		t.Parallel()
		got, err := buildLabelCommandsConfig(map[string]any{
			"provider":         "github",
			"poll_interval_ms": 90000,
		})
		if err != nil {
			t.Fatalf("buildLabelCommandsConfig: unexpected error: %v", err)
		}
		if got.PollIntervalMS != 90000 {
			t.Errorf("PollIntervalMS = %d, want 90000", got.PollIntervalMS)
		}
	})

	t.Run("non-integer value is a ConfigError", func(t *testing.T) {
		t.Parallel()
		_, err := buildLabelCommandsConfig(map[string]any{
			"provider":         "github",
			"poll_interval_ms": "not-a-number",
		})
		assertConfigErrorField(t, err, "reactions.label_commands.poll_interval_ms")
	})
}

func TestBuildLabelCommandsConfig_BothLabelsEmptyErrors(t *testing.T) {
	t.Parallel()

	_, err := buildLabelCommandsConfig(map[string]any{
		"provider":     "github",
		"review_label": "",
		"fix_label":    "",
	})
	assertConfigErrorField(t, err, "reactions.label_commands")
}

func TestNewServiceConfig_LabelCommandsExcludedFromReactionsMap(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(map[string]any{
		"reactions": map[string]any{
			"label_commands": map[string]any{
				"provider":     "github",
				"review_label": "sortie:review",
			},
		},
	})
	if err != nil {
		t.Fatalf("NewServiceConfig: unexpected error: %v", err)
	}

	if cfg.LabelCommands.Provider != "github" {
		t.Errorf("LabelCommands.Provider = %q, want %q", cfg.LabelCommands.Provider, "github")
	}
	if cfg.LabelCommands.ReviewLabel != "sortie:review" {
		t.Errorf("LabelCommands.ReviewLabel = %q, want %q", cfg.LabelCommands.ReviewLabel, "sortie:review")
	}
	if _, ok := cfg.Reactions["label_commands"]; ok {
		t.Error(`Reactions["label_commands"] present, want absent (parses through its own dedicated path)`)
	}
}

// --- test helpers ---

func assertConfigErrorField(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *ConfigError with field %q, got nil", wantField)
	}
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want *ConfigError", err)
	}
	if ce.Field != wantField {
		t.Errorf("ConfigError.Field = %q, want %q", ce.Field, wantField)
	}
}

func assertStringEqual(t *testing.T, name, want, got string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func assertIntEqual(t *testing.T, name string, want, got int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", name, got, want)
	}
}

func assertStringSliceEqual(t *testing.T, name string, want, got []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s length = %d, want %d: got %v", name, len(got), len(want), got)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}

// TestValidateInProgressState exercises the handoff_state collision check
// directly, because the path through NewServiceConfig cannot reach it:
// ValidateHandoffState rejects any handoffState ∈ activeStates before
// ValidateInProgressState runs, and inProgressState must be ∈ activeStates.
func TestValidateInProgressState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		inProgressState string
		activeStates    []string
		terminalStates  []string
		handoffState    string
		wantErr         bool
		wantField       string
	}{
		{
			name:            "valid — no collision",
			inProgressState: "In Progress",
			activeStates:    []string{"In Progress"},
			terminalStates:  []string{"Done"},
			handoffState:    "Human Review",
			wantErr:         false,
		},
		{
			name:            "absent — empty string is valid",
			inProgressState: "",
			activeStates:    []string{"In Progress"},
			terminalStates:  []string{"Done"},
			handoffState:    "Human Review",
			wantErr:         false,
		},
		{
			name:            "collides with handoff_state",
			inProgressState: "In Progress",
			activeStates:    []string{"In Progress"},
			terminalStates:  []string{"Done"},
			handoffState:    "In Progress",
			wantErr:         true,
			wantField:       "tracker.in_progress_state",
		},
		{
			name:            "collides with handoff_state case-insensitive",
			inProgressState: "IN PROGRESS",
			activeStates:    []string{"IN PROGRESS"},
			terminalStates:  []string{"Done"},
			handoffState:    "in progress",
			wantErr:         true,
			wantField:       "tracker.in_progress_state",
		},
		{
			name:            "no collision when handoff_state is empty",
			inProgressState: "In Progress",
			activeStates:    []string{"In Progress"},
			terminalStates:  []string{"Done"},
			handoffState:    "",
			wantErr:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateInProgressState(tt.inProgressState, tt.activeStates, tt.terminalStates, tt.handoffState)

			if tt.wantErr {
				assertConfigErrorField(t, err, tt.wantField)
				return
			}
			if err != nil {
				t.Fatalf("ValidateInProgressState(%q, ...) unexpected error: %v", tt.inProgressState, err)
			}
		})
	}
}

// setDotEnvPathForTest sets the dotenv path via the public API and
// registers a cleanup to restore the original value. It does not call
// t.Parallel() — callers are responsible for sequencing.
func setDotEnvPathForTest(t *testing.T, path string) {
	t.Helper()
	orig := getDotEnvPath()
	SetDotEnvPath(path)
	t.Cleanup(func() { SetDotEnvPath(orig) })
}

// TestNewServiceConfigEnvOverrides covers end-to-end env override behaviour
// through the full NewServiceConfig pipeline. Each subtest uses t.Setenv for
// isolation; none calls t.Parallel() to avoid races on dotenvPathOverride.
func TestNewServiceConfigEnvOverrides(t *testing.T) {
	// Ensure the dotenv path is clean so SORTIE_ENV_FILE subtests work.
	setDotEnvPathForTest(t, "")

	t.Run("TrackerKindOverridesYAML", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_KIND", "file")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"kind": "jira"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.Kind", "file", cfg.Tracker.Kind)
	})

	t.Run("YAMLLeftIntactWhenEnvAbsent", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_KIND", "") // explicitly absent
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"kind": "jira"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.Kind", "jira", cfg.Tracker.Kind)
	})

	t.Run("APIKeyDollarNotExpanded", func(t *testing.T) {
		// A dollar + numeric prefix would be truncated by os.ExpandEnv
		// (e.g. "tok$5abc" → "tok" if $5 is treated as a variable reference).
		// The env override layer must preserve literal dollar signs.
		t.Setenv("SORTIE_TRACKER_API_KEY", "tok$5abc")
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.APIKey", "tok$5abc", cfg.Tracker.APIKey)
	})

	t.Run("DBPathDollarNotExpanded", func(t *testing.T) {
		// Without the envKeys guard, os.ExpandEnv would expand $SORTIE_NOTSET_UNIQUE_XYZ
		// to "" producing "/data//sortie.db".
		t.Setenv("SORTIE_DB_PATH", "/data/$SORTIE_NOTSET_UNIQUE_XYZ/sortie.db")
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "DBPath", "/data/$SORTIE_NOTSET_UNIQUE_XYZ/sortie.db", cfg.DBPath)
	})

	t.Run("WorkspaceRootTildeExpands", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot determine home directory")
		}
		t.Setenv("SORTIE_WORKSPACE_ROOT", "~/ws_ovr_test")
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		want := filepath.Join(home, "ws_ovr_test")
		assertStringEqual(t, "Workspace.Root", want, cfg.Workspace.Root)
	})

	t.Run("PollingIntervalValid", func(t *testing.T) {
		t.Setenv("SORTIE_POLLING_INTERVAL_MS", "5000")
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Polling.IntervalMS", 5000, cfg.Polling.IntervalMS)
	})

	t.Run("PollingIntervalInvalidError", func(t *testing.T) {
		t.Setenv("SORTIE_POLLING_INTERVAL_MS", "abc")
		_, err := NewServiceConfig(map[string]any{})
		assertConfigErrorField(t, err, "polling.interval_ms")
		var ce *ConfigError
		if errors.As(err, &ce) && !strings.Contains(ce.Message, "SORTIE_POLLING_INTERVAL_MS") {
			t.Errorf("ConfigError.Message = %q, want it to contain env var name", ce.Message)
		}
	})

	t.Run("ActiveStatesCSV", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_ACTIVE_STATES", "To Do,In Progress")
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringSliceEqual(t, "Tracker.ActiveStates",
			[]string{"To Do", "In Progress"}, cfg.Tracker.ActiveStates)
	})

	t.Run("CommentsOnDispatchOverride", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_COMMENTS_ON_DISPATCH", "true")
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if !cfg.Tracker.Comments.OnDispatch {
			t.Error("Tracker.Comments.OnDispatch = false, want true")
		}
	})

	t.Run("CommentsOnCompletionOverrideFalse", func(t *testing.T) {
		// Override an existing YAML true → false via env.
		t.Setenv("SORTIE_TRACKER_COMMENTS_ON_COMPLETION", "false")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"comments": map[string]any{"on_completion": true},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if cfg.Tracker.Comments.OnCompletion {
			t.Error("Tracker.Comments.OnCompletion = true, want false (env override)")
		}
	})

	t.Run("NonMapTrackerSectionWithOverrideNoPanic", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_KIND", "file")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": "not-a-map", // invalid YAML type
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		// ensureSubMap replaced the string with a new map containing the override.
		assertStringEqual(t, "Tracker.Kind", "file", cfg.Tracker.Kind)
	})

	t.Run("NilRawMapWithOverride", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_KIND", "file")
		cfg, err := NewServiceConfig(nil)
		if err != nil {
			t.Fatalf("NewServiceConfig(nil): %v", err)
		}
		assertStringEqual(t, "Tracker.Kind", "file", cfg.Tracker.Kind)
	})

	t.Run("DynamicReload", func(t *testing.T) {
		// First call with SORTIE_TRACKER_KIND=file.
		t.Setenv("SORTIE_TRACKER_KIND", "file")
		cfg1, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("first NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.Kind (1st)", "file", cfg1.Tracker.Kind)

		// Simulate dynamic reload by changing the env var.
		t.Setenv("SORTIE_TRACKER_KIND", "jira")
		cfg2, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("second NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.Kind (2nd)", "jira", cfg2.Tracker.Kind)
	})

	t.Run("DotEnvFileIntegration", func(t *testing.T) {
		dotenvFile := writeDotEnvFile(t,
			"SORTIE_TRACKER_KIND=file\nSORTIE_TRACKER_PROJECT=dot-env-project\n")
		t.Setenv("SORTIE_ENV_FILE", dotenvFile)
		// Real env absent — dotenv values should apply.
		t.Setenv("SORTIE_TRACKER_KIND", "")
		t.Setenv("SORTIE_TRACKER_PROJECT", "")

		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.Kind", "file", cfg.Tracker.Kind)
		assertStringEqual(t, "Tracker.Project", "dot-env-project", cfg.Tracker.Project)
	})

	t.Run("DotEnvParseErrorFailsStartup", func(t *testing.T) {
		malformed := writeDotEnvFile(t, "SORTIE_KEY_NO_EQUALS\n")
		t.Setenv("SORTIE_ENV_FILE", malformed)

		_, err := NewServiceConfig(map[string]any{})
		if err == nil {
			t.Fatal("NewServiceConfig: expected error for malformed .env file, got nil")
		}
		if !strings.Contains(err.Error(), "missing '='") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "missing '='")
		}
	})

	t.Run("AllAgentIntOverrides", func(t *testing.T) {
		t.Setenv("SORTIE_AGENT_TURN_TIMEOUT_MS", "9000000")
		t.Setenv("SORTIE_AGENT_READ_TIMEOUT_MS", "9001")
		t.Setenv("SORTIE_AGENT_STALL_TIMEOUT_MS", "99000")
		t.Setenv("SORTIE_AGENT_MAX_CONCURRENT_AGENTS", "7")
		t.Setenv("SORTIE_AGENT_MAX_TURNS", "15")
		t.Setenv("SORTIE_AGENT_MAX_RETRY_BACKOFF_MS", "99999")
		t.Setenv("SORTIE_AGENT_MAX_SESSIONS", "3")
		t.Setenv("SORTIE_AGENT_MAX_TOKENS", "750000")

		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Agent.TurnTimeoutMS", 9000000, cfg.Agent.TurnTimeoutMS)
		assertIntEqual(t, "Agent.ReadTimeoutMS", 9001, cfg.Agent.ReadTimeoutMS)
		assertIntEqual(t, "Agent.StallTimeoutMS", 99000, cfg.Agent.StallTimeoutMS)
		assertIntEqual(t, "Agent.MaxConcurrentAgents", 7, cfg.Agent.MaxConcurrentAgents)
		assertIntEqual(t, "Agent.MaxTurns", 15, cfg.Agent.MaxTurns)
		assertIntEqual(t, "Agent.MaxRetryBackoffMS", 99999, cfg.Agent.MaxRetryBackoffMS)
		assertIntEqual(t, "Agent.MaxSessions", 3, cfg.Agent.MaxSessions)
		assertIntEqual(t, "Agent.MaxTokens", 750000, cfg.Agent.MaxTokens)
	})

	t.Run("MaxTokensOverrideBeatsFrontMatter", func(t *testing.T) {
		t.Setenv("SORTIE_AGENT_MAX_TOKENS", "200000")
		cfg, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": 100000},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens", 200000, cfg.Agent.MaxTokens)
	})

	t.Run("MaxTokensDynamicReload", func(t *testing.T) {
		// First parse with one front-matter value.
		cfg1, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": 100000},
		})
		if err != nil {
			t.Fatalf("first NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens (1st)", 100000, cfg1.Agent.MaxTokens)

		// Simulate dynamic reload: the re-parsed front matter carries a
		// new value, which must be re-applied.
		cfg2, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": 250000},
		})
		if err != nil {
			t.Fatalf("second NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens (2nd)", 250000, cfg2.Agent.MaxTokens)

		// An env override set between reloads applies on the next re-parse.
		t.Setenv("SORTIE_AGENT_MAX_TOKENS", "300000")
		cfg3, err := NewServiceConfig(map[string]any{
			"agent": map[string]any{"max_tokens": 250000},
		})
		if err != nil {
			t.Fatalf("third NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Agent.MaxTokens (3rd)", 300000, cfg3.Agent.MaxTokens)
	})

	t.Run("TrackerStringOverridesAllFields", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_ENDPOINT", "https://override.example.com")
		t.Setenv("SORTIE_TRACKER_PROJECT", "OVRD")
		t.Setenv("SORTIE_TRACKER_QUERY_FILTER", "project=OVRD AND status!=Done")

		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"endpoint":     "https://original.example.com",
				"project":      "ORIG",
				"query_filter": "original filter",
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.Endpoint", "https://override.example.com", cfg.Tracker.Endpoint)
		assertStringEqual(t, "Tracker.Project", "OVRD", cfg.Tracker.Project)
		assertStringEqual(t, "Tracker.QueryFilter", "project=OVRD AND status!=Done", cfg.Tracker.QueryFilter)
	})

	t.Run("TrackerActiveStatesOverrideCSVPreservesCase", func(t *testing.T) {
		// States stored with original casing.
		t.Setenv("SORTIE_TRACKER_ACTIVE_STATES", "To Do,In Progress,In Review")
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringSliceEqual(t, "Tracker.ActiveStates",
			[]string{"To Do", "In Progress", "In Review"}, cfg.Tracker.ActiveStates)
	})

	t.Run("YAMLDollarVarStillExpandedWhenEnvAbsent", func(t *testing.T) {
		// When the SORTIE_* override is absent, existing $VAR resolution must still work.
		t.Setenv("SORTIE_TRACKER_API_KEY", "") // not set
		t.Setenv("MY_REAL_TOKEN", "secret_tok_xyz")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"api_key": "$MY_REAL_TOKEN"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		// $MY_REAL_TOKEN expansion still applies (no env override for api_key).
		assertStringEqual(t, "Tracker.APIKey", "secret_tok_xyz", cfg.Tracker.APIKey)
	})
}

func TestNewServiceConfig_CIFeedback(t *testing.T) {
	t.Parallel()

	t.Run("Absent/ZeroValue", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if cfg.CIFeedback != (CIFeedbackConfig{}) {
			t.Errorf("CIFeedback = %+v, want zero value when ci_feedback absent", cfg.CIFeedback)
		}
	})

	t.Run("KindWithDefaults", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{"kind": "github"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.Kind", "github", cfg.CIFeedback.Kind)
		assertIntEqual(t, "CIFeedback.MaxRetries", 2, cfg.CIFeedback.MaxRetries)
		assertStringEqual(t, "CIFeedback.Escalation", "label", cfg.CIFeedback.Escalation)
		assertStringEqual(t, "CIFeedback.EscalationLabel", "needs-human", cfg.CIFeedback.EscalationLabel)
		assertIntEqual(t, "CIFeedback.WatchWindowMS", ciWatchWindowDefaultMS, cfg.CIFeedback.WatchWindowMS)
	})

	t.Run("ExplicitMaxRetries", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{"kind": "github", "max_retries": 5},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "CIFeedback.MaxRetries", 5, cfg.CIFeedback.MaxRetries)
	})

	t.Run("ValidEscalation/Comment", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{"kind": "github", "escalation": "comment"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.Escalation", "comment", cfg.CIFeedback.Escalation)
	})

	t.Run("ValidEscalation/Label", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{"kind": "github", "escalation": "label"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.Escalation", "label", cfg.CIFeedback.Escalation)
	})

	t.Run("InvalidEscalation/Rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{"kind": "github", "escalation": "slack"},
		})
		assertConfigErrorField(t, err, "ci_feedback.escalation")
	})

	t.Run("CustomEscalationLabel", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{
				"kind":             "github",
				"escalation":       "label",
				"escalation_label": "blocked-by-ci",
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.EscalationLabel", "blocked-by-ci", cfg.CIFeedback.EscalationLabel)
	})

	t.Run("NotLeakedToExtensions", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{"kind": "github"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if _, ok := cfg.extensions["ci_feedback"]; ok {
			t.Error("ci_feedback leaked into cfg.extensions; want absent")
		}
	})
}

func TestNewServiceConfig_SelfReview(t *testing.T) {
	t.Parallel()

	t.Run("Defaults", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if cfg.SelfReview.Enabled {
			t.Error("SelfReview.Enabled = true, want false")
		}
		assertIntEqual(t, "SelfReview.MaxIterations", 3, cfg.SelfReview.MaxIterations)
		assertIntEqual(t, "SelfReview.VerificationTimeoutMS", 120000, cfg.SelfReview.VerificationTimeoutMS)
		assertIntEqual(t, "SelfReview.MaxDiffBytes", 102400, cfg.SelfReview.MaxDiffBytes)
		assertStringEqual(t, "SelfReview.Reviewer", "same", cfg.SelfReview.Reviewer)
	})

	t.Run("Enabled_WithCommands", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{
				"enabled":               true,
				"verification_commands": []any{"make test", "make lint"},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if !cfg.SelfReview.Enabled {
			t.Error("SelfReview.Enabled = false, want true")
		}
		if len(cfg.SelfReview.VerificationCommands) != 2 {
			t.Fatalf("SelfReview.VerificationCommands len = %d, want 2",
				len(cfg.SelfReview.VerificationCommands))
		}
		assertStringEqual(t, "VerificationCommands[0]", "make test", cfg.SelfReview.VerificationCommands[0])
		assertStringEqual(t, "VerificationCommands[1]", "make lint", cfg.SelfReview.VerificationCommands[1])
	})

	t.Run("Enabled_NoCommands", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{
				"enabled": true,
			},
		})
		assertConfigErrorField(t, err, "self_review.verification_commands")
	})

	t.Run("MaxIterations_Below1", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{"enabled": true, "verification_commands": []any{"echo ok"}, "max_iterations": 0},
		})
		assertConfigErrorField(t, err, "self_review.max_iterations")
	})

	t.Run("MaxIterations_Above10", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{"enabled": true, "verification_commands": []any{"echo ok"}, "max_iterations": 11},
		})
		assertConfigErrorField(t, err, "self_review.max_iterations")
	})

	t.Run("MaxIterations_Boundary", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{1, 10} {
			cfg, err := NewServiceConfig(map[string]any{
				"self_review": map[string]any{"enabled": true, "verification_commands": []any{"echo ok"}, "max_iterations": n},
			})
			if err != nil {
				t.Fatalf("max_iterations=%d: unexpected error: %v", n, err)
			}
			assertIntEqual(t, "SelfReview.MaxIterations", n, cfg.SelfReview.MaxIterations)
		}
	})

	t.Run("Reviewer_Invalid", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{"enabled": true, "verification_commands": []any{"echo ok"}, "reviewer": "other-agent"},
		})
		assertConfigErrorField(t, err, "self_review.reviewer")
	})

	t.Run("Reviewer_Same", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{"enabled": true, "verification_commands": []any{"echo ok"}, "reviewer": "same"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "SelfReview.Reviewer", "same", cfg.SelfReview.Reviewer)
	})

	t.Run("IntegerCoercion", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{
				"enabled":                 true,
				"verification_commands":   []any{"echo ok"},
				"max_iterations":          "5",
				"verification_timeout_ms": float64(60000),
				"max_diff_bytes":          "51200",
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "SelfReview.MaxIterations", 5, cfg.SelfReview.MaxIterations)
		assertIntEqual(t, "SelfReview.VerificationTimeoutMS", 60000, cfg.SelfReview.VerificationTimeoutMS)
		assertIntEqual(t, "SelfReview.MaxDiffBytes", 51200, cfg.SelfReview.MaxDiffBytes)
	})

	t.Run("Disabled_SkipsValidation", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{
				"enabled":        false,
				"max_iterations": 0,
				"reviewer":       "nonexistent",
			},
		})
		if err != nil {
			t.Fatalf("Disabled self_review with invalid fields should not error: %v", err)
		}
		if cfg.SelfReview.Enabled {
			t.Error("SelfReview.Enabled = true, want false")
		}
	})

	t.Run("SchemaUnknownKey", func(t *testing.T) {
		t.Parallel()
		// Unknown keys inside self_review must not cause a crash; the
		// schema layer emits a warning but NewServiceConfig still succeeds.
		cfg, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{"unknown_field": "value"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if cfg.SelfReview.Enabled {
			t.Error("SelfReview.Enabled = true, want false")
		}
	})

	t.Run("NotLeakedToExtensions", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"self_review": map[string]any{
				"enabled":               true,
				"verification_commands": []any{"make test"},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if _, ok := cfg.extensions["self_review"]; ok {
			t.Error("self_review leaked into cfg.extensions; want absent")
		}
	})
}

func TestNewServiceConfig_AgentTurnTimeoutMS(t *testing.T) {
	t.Parallel()

	t.Run("Zero", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{"agent": map[string]any{"turn_timeout_ms": 0}})
		assertConfigErrorField(t, err, "agent.turn_timeout_ms")
		var ce *ConfigError
		errors.As(err, &ce)
		assertStringEqual(t, "ConfigError.Message", "must be greater than 0", ce.Message)
	})

	t.Run("Negative", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{"agent": map[string]any{"turn_timeout_ms": -1}})
		assertConfigErrorField(t, err, "agent.turn_timeout_ms")
		var ce *ConfigError
		errors.As(err, &ce)
		assertStringEqual(t, "ConfigError.Message", "must be greater than 0", ce.Message)
	})

	t.Run("AbsentKeyDefaults", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Agent.TurnTimeoutMS", 3600000, cfg.Agent.TurnTimeoutMS)
	})

	t.Run("NullValueDefaults", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{"agent": map[string]any{"turn_timeout_ms": nil}})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "Agent.TurnTimeoutMS", 3600000, cfg.Agent.TurnTimeoutMS)
	})
}

// TestPopulateCIFeedbackFromReactions exercises the bridge function that
// maps a ReactionConfig for the "ci_failure" kind into a CIFeedbackConfig.
func TestPopulateCIFeedbackFromReactions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		rc        ReactionConfig
		want      CIFeedbackConfig
		wantErr   bool
		wantField string
	}{
		{
			name: "ProviderMapsToKind",
			rc: ReactionConfig{
				Provider:        "github-actions",
				MaxRetries:      2,
				Escalation:      "label",
				EscalationLabel: "needs-human",
			},
			want: CIFeedbackConfig{
				Kind:            "github-actions",
				MaxRetries:      2,
				MaxLogLines:     50,
				Escalation:      "label",
				EscalationLabel: "needs-human",
				WatchWindowMS:   ciWatchWindowDefaultMS,
			},
		},
		{
			name: "MaxLogLinesDefault",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    nil,
			},
			want: CIFeedbackConfig{
				Kind:          "github-actions",
				MaxLogLines:   50,
				WatchWindowMS: ciWatchWindowDefaultMS,
			},
		},
		{
			name: "MaxLogLinesFromExtra",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"max_log_lines": 100},
			},
			want: CIFeedbackConfig{
				Kind:          "github-actions",
				MaxLogLines:   100,
				WatchWindowMS: ciWatchWindowDefaultMS,
			},
		},
		{
			name: "MaxLogLinesFromExtraFloat64",
			rc: ReactionConfig{
				Provider: "github",
				Extra:    map[string]any{"max_log_lines": float64(200)},
			},
			want: CIFeedbackConfig{
				Kind:          "github",
				MaxLogLines:   200,
				WatchWindowMS: ciWatchWindowDefaultMS,
			},
		},
		{
			name: "MaxLogLinesNegative",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"max_log_lines": -1},
			},
			wantErr:   true,
			wantField: "reactions.ci_failure.max_log_lines",
		},
		{
			name: "MaxLogLinesNonInteger",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"max_log_lines": "abc"},
			},
			wantErr:   true,
			wantField: "reactions.ci_failure.max_log_lines",
		},
		{
			name: "EmptyProviderReturnsZero",
			rc:   ReactionConfig{Provider: ""},
			want: CIFeedbackConfig{},
		},
		{
			name: "WatchWindowMSFromExtra",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"watch_window_ms": 3600000},
			},
			want: CIFeedbackConfig{
				Kind:          "github-actions",
				MaxLogLines:   50,
				WatchWindowMS: 3600000,
			},
		},
		{
			name: "WatchWindowMSExplicitZeroDisablesBound",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"watch_window_ms": 0},
			},
			want: CIFeedbackConfig{
				Kind:          "github-actions",
				MaxLogLines:   50,
				WatchWindowMS: 0,
			},
		},
		{
			name: "WatchWindowMSNegative",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"watch_window_ms": -1},
			},
			wantErr:   true,
			wantField: "reactions.ci_failure.watch_window_ms",
		},
		{
			name: "WatchWindowMSNonInteger",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"watch_window_ms": "abc"},
			},
			wantErr:   true,
			wantField: "reactions.ci_failure.watch_window_ms",
		},
		{
			name: "EscalationAndLabelPassThrough",
			rc: ReactionConfig{
				Provider:        "circle-ci",
				Escalation:      "comment",
				EscalationLabel: "blocked",
				MaxRetries:      5,
			},
			want: CIFeedbackConfig{
				Kind:            "circle-ci",
				MaxRetries:      5,
				MaxLogLines:     50,
				Escalation:      "comment",
				EscalationLabel: "blocked",
				WatchWindowMS:   ciWatchWindowDefaultMS,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := populateCIFeedbackFromReactions(tt.rc)

			if tt.wantErr {
				assertConfigErrorField(t, err, tt.wantField)
				return
			}
			if err != nil {
				t.Fatalf("populateCIFeedbackFromReactions() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("populateCIFeedbackFromReactions() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestCIFailureMigration verifies the full precedence logic for the
// reactions.ci_failure → CIFeedback migration path through NewServiceConfig.
func TestCIFailureMigration(t *testing.T) {
	t.Parallel()

	t.Run("Reactions/CIFailure/ProviderMapsToKind", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider": "github-actions",
				},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.Kind", "github-actions", cfg.CIFeedback.Kind)
	})

	t.Run("Reactions/CIFailure/MaxLogLinesFromExtra", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider":      "github-actions",
					"max_log_lines": 100,
				},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "CIFeedback.MaxLogLines", 100, cfg.CIFeedback.MaxLogLines)
	})

	t.Run("Reactions/CIFailure/MaxLogLinesDefault", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider": "github-actions",
				},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertIntEqual(t, "CIFeedback.MaxLogLines", 50, cfg.CIFeedback.MaxLogLines)
	})

	t.Run("Reactions/CIFailure/MaxLogLinesNegative", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider":      "github-actions",
					"max_log_lines": -1,
				},
			},
		})
		assertConfigErrorField(t, err, "reactions.ci_failure.max_log_lines")
	})

	t.Run("Reactions/CIFailure/MaxLogLinesNonInteger", func(t *testing.T) {
		t.Parallel()
		_, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider":      "github-actions",
					"max_log_lines": "abc",
				},
			},
		})
		assertConfigErrorField(t, err, "reactions.ci_failure.max_log_lines")
	})

	t.Run("Reactions/CIFailure/RemovedFromReactionsMap", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider": "github-actions",
				},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if _, ok := cfg.Reactions["ci_failure"]; ok {
			t.Error("Reactions[\"ci_failure\"] still present; want removed after migration")
		}
	})

	t.Run("Reactions/CIFailure/OtherReactionsPreserved", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider": "github-actions",
				},
				"review_comments": map[string]any{
					"max_retries": 3,
				},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if _, ok := cfg.Reactions["review_comments"]; !ok {
			t.Error("Reactions[\"review_comments\"] missing; want preserved")
		}
	})

	t.Run("Reactions/CIFailure/EmptyProvider", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"ci_failure": map[string]any{},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.Kind", "", cfg.CIFeedback.Kind)
	})

	t.Run("Precedence/BothPresent/ReactionsWins", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{
				"kind":        "circle-ci",
				"max_retries": 1,
			},
			"reactions": map[string]any{
				"ci_failure": map[string]any{
					"provider":         "github-actions",
					"max_retries":      4,
					"max_log_lines":    75,
					"escalation":       "comment",
					"escalation_label": "ci-blocked",
				},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.Kind", "github-actions", cfg.CIFeedback.Kind)
		assertIntEqual(t, "CIFeedback.MaxRetries", 4, cfg.CIFeedback.MaxRetries)
		assertIntEqual(t, "CIFeedback.MaxLogLines", 75, cfg.CIFeedback.MaxLogLines)
		assertStringEqual(t, "CIFeedback.Escalation", "comment", cfg.CIFeedback.Escalation)
		assertStringEqual(t, "CIFeedback.EscalationLabel", "ci-blocked", cfg.CIFeedback.EscalationLabel)
	})

	t.Run("Precedence/CIFeedbackOnly", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"ci_feedback": map[string]any{
				"kind":        "github",
				"max_retries": 3,
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "CIFeedback.Kind", "github", cfg.CIFeedback.Kind)
		assertIntEqual(t, "CIFeedback.MaxRetries", 3, cfg.CIFeedback.MaxRetries)
	})

	t.Run("Precedence/NeitherPresent", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		if cfg.CIFeedback != (CIFeedbackConfig{}) {
			t.Errorf("CIFeedback = %+v, want zero value when neither section present", cfg.CIFeedback)
		}
	})

	t.Run("Provider/ParsedForNonCIReaction", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"reactions": map[string]any{
				"review_comments": map[string]any{
					"provider": "github",
				},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		rc, ok := cfg.Reactions["review_comments"]
		if !ok {
			t.Fatal("Reactions[\"review_comments\"] missing")
		}
		assertStringEqual(t, "Reactions[review_comments].Provider", "github", rc.Provider)
	})
}

// TestResolveExtensionEnvRefs covers the recursive walker at the unit level:
// nested maps, slices of strings, slices of maps, non-string leaves, nil map,
// the braced ${VAR} form, and the $$ -> "" contract.
func TestResolveExtensionEnvRefs(t *testing.T) {
	t.Run("NilMap", func(t *testing.T) {
		t.Parallel()
		snapshot := resolveExtensionEnvRefs(nil)
		if snapshot != nil {
			t.Errorf("resolveExtensionEnvRefs(nil) = %v, want nil", snapshot)
		}
	})

	t.Run("NestedMap", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_512_NESTED_A", "tok-abc")

		ext := map[string]any{
			"myext": map[string]any{
				"api_key": "$SORTIE_TEST_512_NESTED_A",
				"host":    "literal-host",
			},
		}
		snapshot := resolveExtensionEnvRefs(ext)

		inner, ok := ext["myext"].(map[string]any)
		if !ok {
			t.Fatal("ext[myext] is not a map")
		}
		assertStringEqual(t, "myext.api_key resolved", "tok-abc", inner["api_key"].(string))
		assertStringEqual(t, "myext.host unchanged", "literal-host", inner["host"].(string))

		if snapshot == nil {
			t.Fatal("snapshot is nil, want non-nil")
		}
		if _, ok := snapshot["myext.api_key"]; !ok {
			t.Error("snapshot missing key myext.api_key")
		}
		if _, ok := snapshot["myext.host"]; ok {
			t.Error("snapshot must not record literal (no $) values")
		}
	})

	t.Run("SliceOfStrings", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_512_HOST", "deploy.example.com")

		ext := map[string]any{
			"worker": map[string]any{
				"ssh_hosts": []any{"$SORTIE_TEST_512_HOST", "literal-host"},
			},
		}
		snapshot := resolveExtensionEnvRefs(ext)

		worker := ext["worker"].(map[string]any)
		hosts := worker["ssh_hosts"].([]any)
		assertStringEqual(t, "ssh_hosts[0] resolved", "deploy.example.com", hosts[0].(string))
		assertStringEqual(t, "ssh_hosts[1] literal unchanged", "literal-host", hosts[1].(string))

		if snapshot == nil {
			t.Fatal("snapshot is nil")
		}
		if _, ok := snapshot["worker.ssh_hosts[0]"]; !ok {
			t.Error("snapshot missing key worker.ssh_hosts[0]")
		}
		if _, ok := snapshot["worker.ssh_hosts[1]"]; ok {
			t.Error("snapshot must not record literal (no $) element")
		}
	})

	t.Run("SliceOfMaps", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_512_SMAP_KEY", "nested-value")

		ext := map[string]any{
			"myext": map[string]any{
				"servers": []any{
					map[string]any{"key": "$SORTIE_TEST_512_SMAP_KEY"},
					map[string]any{"key": "plain"},
				},
			},
		}
		snapshot := resolveExtensionEnvRefs(ext)

		inner := ext["myext"].(map[string]any)
		servers := inner["servers"].([]any)
		first := servers[0].(map[string]any)
		second := servers[1].(map[string]any)
		assertStringEqual(t, "servers[0].key resolved", "nested-value", first["key"].(string))
		assertStringEqual(t, "servers[1].key literal", "plain", second["key"].(string))

		if snapshot == nil {
			t.Fatal("snapshot is nil")
		}
		if _, ok := snapshot["myext.servers[0].key"]; !ok {
			t.Error("snapshot missing myext.servers[0].key")
		}
		if _, ok := snapshot["myext.servers[1].key"]; ok {
			t.Error("snapshot must not record literal value")
		}
	})

	t.Run("NonStringLeavesUntouched", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		ext := map[string]any{
			"myext": map[string]any{
				"port":    8080,
				"ratio":   float64(3.14),
				"enabled": true,
				"limit":   int64(100),
				"ts":      ts,
				"nothing": nil,
			},
		}
		snapshot := resolveExtensionEnvRefs(ext)

		inner := ext["myext"].(map[string]any)
		if inner["port"] != 8080 {
			t.Errorf("port = %v, want 8080", inner["port"])
		}
		if inner["ratio"] != float64(3.14) {
			t.Errorf("ratio = %v, want 3.14", inner["ratio"])
		}
		if inner["enabled"] != true {
			t.Errorf("enabled = %v, want true", inner["enabled"])
		}
		if inner["limit"] != int64(100) {
			t.Errorf("limit = %v, want int64(100)", inner["limit"])
		}
		if inner["ts"] != ts {
			t.Errorf("ts = %v, want %v", inner["ts"], ts)
		}
		if inner["nothing"] != nil {
			t.Errorf("nothing = %v, want nil", inner["nothing"])
		}
		if snapshot != nil {
			t.Errorf("snapshot = %v, want nil (no $ leaves)", snapshot)
		}
	})

	t.Run("LiteralsWithoutDollar", func(t *testing.T) {
		t.Parallel()

		ext := map[string]any{
			"logging": map[string]any{"level": "debug"},
		}
		snapshot := resolveExtensionEnvRefs(ext)

		inner := ext["logging"].(map[string]any)
		assertStringEqual(t, "logging.level unchanged", "debug", inner["level"].(string))
		if snapshot != nil {
			t.Errorf("snapshot = %v, want nil (no $ in any leaf)", snapshot)
		}
	})

	t.Run("BracedForm", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_512_BRACE", "braced-value")

		ext := map[string]any{
			"myext": map[string]any{"key": "${SORTIE_TEST_512_BRACE}"},
		}
		snapshot := resolveExtensionEnvRefs(ext)

		inner := ext["myext"].(map[string]any)
		assertStringEqual(t, "myext.key resolved via ${}", "braced-value", inner["key"].(string))
		if snapshot == nil {
			t.Fatal("snapshot is nil")
		}
		if _, ok := snapshot["myext.key"]; !ok {
			t.Error("snapshot missing myext.key for ${VAR} form")
		}
	})

	t.Run("DollarDollar", func(t *testing.T) {
		t.Parallel()
		// os.ExpandEnv("$$") returns "" — the $$ sequence is consumed
		// and maps to an empty variable name which expands to empty.
		// This is the documented behavior (no custom $$ escape).
		ext := map[string]any{"myext": map[string]any{"val": "$$"}}
		resolveExtensionEnvRefs(ext)

		inner := ext["myext"].(map[string]any)
		assertStringEqual(t, "myext.val after $$ expansion", "", inner["val"].(string))
	})
}

// TestNewServiceConfigExtensions covers extension-walker integration through
// NewServiceConfig: SORTIE_* precedence, unknown leaf types, and slice-of-strings.
func TestNewServiceConfigExtensions(t *testing.T) {
	t.Run("SortiePrecedence", func(t *testing.T) {
		// SORTIE_TRACKER_API_KEY overrides the YAML value for the core field;
		// a sibling extension $VAR is resolved against the same environment.
		t.Setenv("SORTIE_TRACKER_API_KEY", "from-env-override")
		t.Setenv("SORTIE_TEST_512_EXT_KEY", "ext-resolved-value")

		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{
				"kind":    "file",
				"api_key": "$SORTIE_TEST_512_EXT_KEY",
			},
			"myext": map[string]any{
				"api_key": "$SORTIE_TEST_512_EXT_KEY",
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		// Core field: the SORTIE_* override wins.
		assertStringEqual(t, "Tracker.APIKey", "from-env-override", cfg.Tracker.APIKey)
		// Extension field: $VAR resolved against process environment.
		extMap, ok := cfg.extensions["myext"].(map[string]any)
		if !ok {
			t.Fatal("cfg.extensions[myext] not a map")
		}
		assertStringEqual(t, "myext.api_key resolved", "ext-resolved-value", extMap["api_key"].(string))
	})

	t.Run("NestedSliceOfStrings", func(t *testing.T) {
		t.Setenv("SORTIE_TEST_512_DEPLOY", "prod.example.com")

		cfg, err := NewServiceConfig(map[string]any{
			"worker": map[string]any{
				"ssh_hosts": []any{"$SORTIE_TEST_512_DEPLOY", "literal-host"},
			},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		workerExt, ok := cfg.extensions["worker"].(map[string]any)
		if !ok {
			t.Fatal("cfg.extensions[worker] not a map")
		}
		hosts, ok := workerExt["ssh_hosts"].([]any)
		if !ok {
			t.Fatal("worker.ssh_hosts not a []any")
		}
		assertStringEqual(t, "ssh_hosts[0] resolved", "prod.example.com", hosts[0].(string))
		assertStringEqual(t, "ssh_hosts[1] literal", "literal-host", hosts[1].(string))
	})

	t.Run("NoPanicOnUnexpectedLeafType", func(t *testing.T) {
		t.Parallel()
		// An int32 value is not produced by yaml.v3 decode-to-any but can
		// appear in Go-constructed test maps. The walker MUST NOT panic.
		raw := map[string]any{
			"myext": map[string]any{
				"count": int32(42),
				"name":  "literal",
			},
		}
		// Verify no panic.
		cfg, err := NewServiceConfig(raw)
		if err != nil {
			t.Fatalf("NewServiceConfig with int32 leaf: %v", err)
		}
		extMap, ok := cfg.extensions["myext"].(map[string]any)
		if !ok {
			t.Fatal("cfg.extensions[myext] not a map")
		}
		if extMap["count"] != int32(42) {
			t.Errorf("myext.count = %v (%T), want int32(42)", extMap["count"], extMap["count"])
		}
	})

	t.Run("WalkerPanicFreeOnUnknownLeafType", func(t *testing.T) {
		t.Parallel()
		// A func() value is definitely not a yaml.v3 leaf type.
		// The walker default arm must return it unchanged without panicking.
		fn := func() {}
		ext := map[string]any{
			"myext": map[string]any{
				"callback": fn,
				"name":     "plain",
			},
		}
		// Must not panic.
		snapshot := resolveExtensionEnvRefs(ext)

		inner := ext["myext"].(map[string]any)
		// callback must be returned unchanged (non-nil, same function value).
		if inner["callback"] == nil {
			t.Error("callback became nil; want original func value preserved")
		}
		// No snapshot entry for non-string leaves.
		if snapshot != nil {
			t.Errorf("snapshot = %v, want nil for non-string ext with no $ leaves", snapshot)
		}
	})
}

// --- AgentAdapterConfig tests ---

// TestAgentAdapterConfig_ExactlyFiveKeysWithNoExtensions asserts that
// AgentAdapterConfig returns exactly the five documented keys when
// cfg.extensions carries no sub-object for kind.
func TestAgentAdapterConfig_ExactlyFiveKeysWithNoExtensions(t *testing.T) {
	t.Parallel()

	cfg := ServiceConfig{
		Agent: AgentConfig{
			Kind:           "claude-code",
			Command:        "claude",
			TurnTimeoutMS:  3600000,
			ReadTimeoutMS:  5000,
			StallTimeoutMS: 300000,
		},
	}

	got := AgentAdapterConfig(cfg, "claude-code")

	want := map[string]any{
		"kind":             "claude-code",
		"command":          "claude",
		"turn_timeout_ms":  3600000,
		"read_timeout_ms":  5000,
		"stall_timeout_ms": 300000,
	}
	if len(got) != len(want) {
		t.Fatalf("AgentAdapterConfig() = %v (len %d), want exactly %d keys: %v", got, len(got), len(want), want)
	}
	for key, wantVal := range want {
		if gotVal, ok := got[key]; !ok || gotVal != wantVal {
			t.Errorf("AgentAdapterConfig()[%q] = %v, want %v", key, gotVal, wantVal)
		}
	}
}

// TestAgentAdapterConfig_KindParameterOverridesAgentKind asserts that the
// "kind" key carries the kind parameter rather than cfg.Agent.Kind, so a
// dispatch-rule-routed kind resolves correctly.
func TestAgentAdapterConfig_KindParameterOverridesAgentKind(t *testing.T) {
	t.Parallel()

	cfg := ServiceConfig{Agent: AgentConfig{Kind: "claude-code"}}

	got := AgentAdapterConfig(cfg, "codex")

	if got["kind"] != "codex" {
		t.Errorf(`AgentAdapterConfig(cfg, "codex")["kind"] = %v, want "codex"`, got["kind"])
	}
}

// TestAgentAdapterConfig_ExtensionCollisionWithOrchestratorOnlyFieldSurvives
// asserts that an extension key whose name collides with an
// orchestrator-only field (max_turns is consumed through the typed
// AgentConfig, never placed in this map) survives the merge untouched,
// proving the exclusion the godoc documents actually holds rather than
// being shadowed by a pre-set key of the same name.
func TestAgentAdapterConfig_ExtensionCollisionWithOrchestratorOnlyFieldSurvives(t *testing.T) {
	t.Parallel()

	cfg := ServiceConfig{
		Agent: AgentConfig{
			Kind:     "codex",
			Command:  "codex",
			MaxTurns: 20,
		},
		extensions: map[string]any{
			"codex": map[string]any{
				"max_turns":       float64(5),
				"approval_policy": "never",
			},
		},
	}

	got := AgentAdapterConfig(cfg, "codex")

	if got["max_turns"] != float64(5) {
		t.Errorf(`AgentAdapterConfig()["max_turns"] = %v, want the extension value 5 (not shadowed by cfg.Agent.MaxTurns)`, got["max_turns"])
	}
	if got["approval_policy"] != "never" {
		t.Errorf(`AgentAdapterConfig()["approval_policy"] = %v, want "never"`, got["approval_policy"])
	}

	// The exact key set: the five documented keys plus the two merged
	// extension keys, nothing else.
	wantKeys := []string{"kind", "command", "turn_timeout_ms", "read_timeout_ms", "stall_timeout_ms", "max_turns", "approval_policy"}
	if len(got) != len(wantKeys) {
		t.Fatalf("AgentAdapterConfig() = %v (len %d), want exactly the keys %v", got, len(got), wantKeys)
	}
	for _, key := range wantKeys {
		if _, ok := got[key]; !ok {
			t.Errorf("AgentAdapterConfig() missing key %q; got %v", key, got)
		}
	}
}

// TestAgentAdapterConfig_DoesNotOverwriteDocumentedKeys asserts that an
// extension sub-object cannot shadow any of the five documented keys:
// the merge only fills keys absent from the map built from cfg.Agent's
// typed fields.
func TestAgentAdapterConfig_DoesNotOverwriteDocumentedKeys(t *testing.T) {
	t.Parallel()

	cfg := ServiceConfig{
		Agent: AgentConfig{Kind: "codex", Command: "codex"},
		extensions: map[string]any{
			"codex": map[string]any{
				"command": "should-not-win",
			},
		},
	}

	got := AgentAdapterConfig(cfg, "codex")

	if got["command"] != "codex" {
		t.Errorf(`AgentAdapterConfig()["command"] = %v, want "codex" (the typed field, not the extension override)`, got["command"])
	}
}

// TestAgentAdapterConfig_FreshMapPerCall asserts the returned map is
// freshly allocated on every call: mutating one call's result must not
// affect another.
func TestAgentAdapterConfig_FreshMapPerCall(t *testing.T) {
	t.Parallel()

	cfg := ServiceConfig{Agent: AgentConfig{Kind: "codex", Command: "codex"}}

	first := AgentAdapterConfig(cfg, "codex")
	first["command"] = "mutated"

	second := AgentAdapterConfig(cfg, "codex")
	if second["command"] != "codex" {
		t.Errorf(`AgentAdapterConfig() second call ["command"] = %v, want "codex" (unaffected by mutating the first call's map)`, second["command"])
	}
}

// --- ResolveAgentSettings tests ---

// TestResolveAgentSettings_MCPConfigPath covers every row of the
// mcp_config resolution table: the kind's block may be absent or not
// an object, the block may carry no mcp_config or a non-string one,
// the value may be empty, absolute, or relative with the workflow
// directory present or empty.
func TestResolveAgentSettings_MCPConfigPath(t *testing.T) {
	t.Parallel()

	// filepath.FromSlash("/abs/op.json") yields a drive-relative path
	// on Windows, which filepath.IsAbs correctly rejects. Resolve it to
	// a genuinely absolute path so the case tests absoluteness rather
	// than the host's path syntax.
	absOperatorPath, err := filepath.Abs(filepath.FromSlash("/abs/op.json"))
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}

	tests := []struct {
		name        string
		cfg         ServiceConfig
		kind        string
		workflowDir string
		wantPath    string
	}{
		{
			name:        "kind has no block",
			cfg:         ServiceConfig{Agent: AgentConfig{Kind: "claude-code"}},
			kind:        "codex",
			workflowDir: "/wf",
			wantPath:    "",
		},
		{
			name: "block is not an object",
			cfg: ServiceConfig{extensions: map[string]any{
				"codex": "not-a-map",
			}},
			kind:        "codex",
			workflowDir: "/wf",
			wantPath:    "",
		},
		{
			name: "block has no mcp_config key",
			cfg: ServiceConfig{extensions: map[string]any{
				"codex": map[string]any{"approval_policy": "never"},
			}},
			kind:        "codex",
			workflowDir: "/wf",
			wantPath:    "",
		},
		{
			name: "mcp_config is not a string",
			cfg: ServiceConfig{extensions: map[string]any{
				"codex": map[string]any{"mcp_config": 42},
			}},
			kind:        "codex",
			workflowDir: "/wf",
			wantPath:    "",
		},
		{
			name: "mcp_config is the empty string",
			cfg: ServiceConfig{extensions: map[string]any{
				"codex": map[string]any{"mcp_config": ""},
			}},
			kind:        "codex",
			workflowDir: "/wf",
			wantPath:    "",
		},
		{
			name: "mcp_config is an absolute path",
			cfg: ServiceConfig{extensions: map[string]any{
				"codex": map[string]any{"mcp_config": absOperatorPath},
			}},
			kind:        "codex",
			workflowDir: "/wf",
			wantPath:    absOperatorPath,
		},
		{
			name: "mcp_config is relative and workflowDir is non-empty",
			cfg: ServiceConfig{extensions: map[string]any{
				"codex": map[string]any{"mcp_config": "op.json"},
			}},
			kind:        "codex",
			workflowDir: "/wf",
			wantPath:    filepath.Join("/wf", "op.json"),
		},
		{
			name: "mcp_config is relative and workflowDir is empty",
			cfg: ServiceConfig{extensions: map[string]any{
				"codex": map[string]any{"mcp_config": "op.json"},
			}},
			kind:        "codex",
			workflowDir: "",
			wantPath:    "op.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ResolveAgentSettings(tt.cfg, tt.kind, tt.workflowDir)

			if got.MCPConfigPath != tt.wantPath {
				t.Errorf("ResolveAgentSettings(cfg, %q, %q).MCPConfigPath = %q, want %q", tt.kind, tt.workflowDir, got.MCPConfigPath, tt.wantPath)
			}
			if got.Kind != tt.kind {
				t.Errorf("ResolveAgentSettings(cfg, %q, %q).Kind = %q, want %q", tt.kind, tt.workflowDir, got.Kind, tt.kind)
			}
		})
	}
}

// TestResolveAgentSettings_PassthroughMatchesAgentAdapterConfig asserts
// that AgentSettings.Passthrough carries exactly what AgentAdapterConfig
// returns for the same kind, so the two producers can never disagree.
func TestResolveAgentSettings_PassthroughMatchesAgentAdapterConfig(t *testing.T) {
	t.Parallel()

	cfg := ServiceConfig{
		Agent: AgentConfig{Kind: "claude-code", Command: "claude"},
		extensions: map[string]any{
			"codex": map[string]any{"approval_policy": "never"},
		},
	}

	got := ResolveAgentSettings(cfg, "codex", "")
	want := AgentAdapterConfig(cfg, "codex")

	if len(got.Passthrough) != len(want) {
		t.Fatalf("ResolveAgentSettings().Passthrough = %v (len %d), want %v (len %d)", got.Passthrough, len(got.Passthrough), want, len(want))
	}
	for k, wantVal := range want {
		if gotVal, ok := got.Passthrough[k]; !ok || gotVal != wantVal {
			t.Errorf("ResolveAgentSettings().Passthrough[%q] = %v, want %v", k, gotVal, wantVal)
		}
	}
}

// TestResolveAgentSettings_IndependentAllocationAcrossCalls asserts that
// two calls return independently allocated Passthrough maps: mutating
// one call's result must not affect another.
func TestResolveAgentSettings_IndependentAllocationAcrossCalls(t *testing.T) {
	t.Parallel()

	cfg := ServiceConfig{Agent: AgentConfig{Kind: "codex", Command: "codex"}}

	first := ResolveAgentSettings(cfg, "codex", "")
	first.Passthrough["command"] = "mutated"

	second := ResolveAgentSettings(cfg, "codex", "")
	if second.Passthrough["command"] != "codex" {
		t.Errorf(`ResolveAgentSettings() second call Passthrough["command"] = %v, want "codex" (unaffected by mutating the first call's map)`, second.Passthrough["command"])
	}
}

// --- MergeAdapterExtensions tests ---
//
// These cases re-home the coverage cmd/sortie's now-deleted
// TestMergeExtensions gave the duplicate mergeExtensions helper before
// it was removed in favor of this primitive: nil maps, non-overwrite of
// an existing key, and the claude-code extension merge.

func TestMergeAdapterExtensions(t *testing.T) {
	t.Parallel()

	t.Run("copies extension keys", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file"}
		cfg := ServiceConfig{extensions: map[string]any{
			"file": map[string]any{"path": "issues.json", "extra": 42},
		}}

		MergeAdapterExtensions(dst, cfg, "file")

		if dst["path"] != "issues.json" {
			t.Errorf("dst[%q] = %v, want %q", "path", dst["path"], "issues.json")
		}
		if dst["extra"] != 42 {
			t.Errorf("dst[%q] = %v, want %d", "extra", dst["extra"], 42)
		}
	})

	t.Run("does not overwrite existing keys", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file", "path": "original.json"}
		cfg := ServiceConfig{extensions: map[string]any{
			"file": map[string]any{"path": "overridden.json"},
		}}

		MergeAdapterExtensions(dst, cfg, "file")

		if dst["path"] != "original.json" {
			t.Errorf("dst[%q] = %v, want %q (must not overwrite)", "path", dst["path"], "original.json")
		}
	})

	t.Run("missing kind is no-op", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "jira"}
		cfg := ServiceConfig{extensions: map[string]any{
			"file": map[string]any{"path": "issues.json"},
		}}

		MergeAdapterExtensions(dst, cfg, "jira")

		if _, ok := dst["path"]; ok {
			t.Error("dst[\"path\"] present, want absent when kind has no extensions block")
		}
	})

	t.Run("nil extensions is no-op", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file"}
		cfg := ServiceConfig{}

		MergeAdapterExtensions(dst, cfg, "file")

		if len(dst) != 1 {
			t.Errorf("len(dst) = %d, want 1 (nil Extensions must be a no-op)", len(dst))
		}
	})

	t.Run("non-map extension value is no-op", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file"}
		cfg := ServiceConfig{extensions: map[string]any{
			"file": "not a map",
		}}

		MergeAdapterExtensions(dst, cfg, "file")

		if len(dst) != 1 {
			t.Errorf("len(dst) = %d, want 1 (non-map extension value must be a no-op)", len(dst))
		}
	})

	t.Run("adapter max_turns passthrough", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "claude-code"}
		cfg := ServiceConfig{extensions: map[string]any{
			"claude-code": map[string]any{"max_turns": float64(50)},
		}}

		MergeAdapterExtensions(dst, cfg, "claude-code")

		got, ok := dst["max_turns"]
		if !ok {
			t.Fatal("dst[\"max_turns\"] not present after MergeAdapterExtensions")
		}
		if got != float64(50) {
			t.Errorf("dst[%q] = %v, want 50 (adapter value, not orchestrator value)", "max_turns", got)
		}
	})
}

// --- buildWorkspaceConfig / workspace.retention_days tests ---

func TestBuildWorkspaceConfig(t *testing.T) {
	t.Parallel()

	t.Run("no retention_days key defaults to 0", func(t *testing.T) {
		t.Parallel()

		cfg, err := buildWorkspaceConfig(map[string]any{}, map[string]bool{})
		if err != nil {
			t.Fatalf("buildWorkspaceConfig: unexpected error: %v", err)
		}
		if cfg.RetentionDays != 0 {
			t.Errorf("RetentionDays = %d, want 0", cfg.RetentionDays)
		}
	})

	tests := []struct {
		name      string
		retention int
		wantErr   bool
	}{
		{name: "negative rejected", retention: -1, wantErr: true},
		{name: "zero accepted (disables the bound)", retention: 0, wantErr: false},
		{name: "one rejected (below the floor)", retention: 1, wantErr: true},
		{name: "twenty-nine rejected (below the floor)", retention: 29, wantErr: true},
		{name: "thirty accepted (at the floor)", retention: 30, wantErr: false},
		{name: "three-sixty-five accepted", retention: 365, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := buildWorkspaceConfig(map[string]any{"retention_days": tt.retention}, map[string]bool{})

			if tt.wantErr {
				var ce *ConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("buildWorkspaceConfig(retention_days=%d) error type = %T, want *ConfigError", tt.retention, err)
				}
				if ce.Field != "workspace.retention_days" {
					t.Errorf("ConfigError.Field = %q, want %q", ce.Field, "workspace.retention_days")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildWorkspaceConfig(retention_days=%d) unexpected error: %v", tt.retention, err)
			}
			if cfg.RetentionDays != tt.retention {
				t.Errorf("RetentionDays = %d, want %d", cfg.RetentionDays, tt.retention)
			}
		})
	}
}
