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
		// os.ExpandEnv replaces undefined vars with empty string;
		// expandPath does not error — result is empty sentinel.
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": "$SORTIE_UNSET_VAR_XYZ",
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=$SORTIE_UNSET_VAR_XYZ) unexpected error: %v", err)
		}
		assertStringEqual(t, "DBPath", "", cfg.DBPath)
	})

	t.Run("DBPath/NonStringIgnored", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"db_path": 42,
		})
		if err != nil {
			t.Fatalf("NewServiceConfig(db_path=42) unexpected error: %v", err)
		}
		// extractString returns "" for non-string values.
		assertStringEqual(t, "DBPath", "", cfg.DBPath)
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
