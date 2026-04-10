package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		if cfg.Extensions == nil {
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
		serverExt, ok := cfg.Extensions["server"]
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
		if _, ok := cfg.Extensions["worker"]; !ok {
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
		if _, ok := cfg.Extensions["db_path"]; ok {
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

// TestValidateInProgressState exercises rule 4 (collision with handoff_state)
// directly, because the path through NewServiceConfig cannot reach it:
// validateHandoffState rejects any handoffState ∈ activeStates before
// validateInProgressState runs, and inProgressState must be ∈ activeStates.
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

			err := validateInProgressState(tt.inProgressState, tt.activeStates, tt.terminalStates, tt.handoffState)

			if tt.wantErr {
				assertConfigErrorField(t, err, tt.wantField)
				return
			}
			if err != nil {
				t.Fatalf("validateInProgressState(%q, ...) unexpected error: %v", tt.inProgressState, err)
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
		if _, ok := cfg.Extensions["ci_feedback"]; ok {
			t.Error("ci_feedback leaked into cfg.Extensions; want absent")
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
		if _, ok := cfg.Extensions["self_review"]; ok {
			t.Error("self_review leaked into cfg.Extensions; want absent")
		}
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
			},
		},
		{
			name: "MaxLogLinesDefault",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    nil,
			},
			want: CIFeedbackConfig{
				Kind:        "github-actions",
				MaxLogLines: 50,
			},
		},
		{
			name: "MaxLogLinesFromExtra",
			rc: ReactionConfig{
				Provider: "github-actions",
				Extra:    map[string]any{"max_log_lines": 100},
			},
			want: CIFeedbackConfig{
				Kind:        "github-actions",
				MaxLogLines: 100,
			},
		},
		{
			name: "MaxLogLinesFromExtraFloat64",
			rc: ReactionConfig{
				Provider: "github",
				Extra:    map[string]any{"max_log_lines": float64(200)},
			},
			want: CIFeedbackConfig{
				Kind:        "github",
				MaxLogLines: 200,
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
