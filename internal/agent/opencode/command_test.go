package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
)

// envLookup returns the value for key in an env []string slice.
func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

// assertEnvPresent fails unless key is present in env with the given value.
func assertEnvPresent(t *testing.T, env []string, key, wantVal string) {
	t.Helper()
	got, ok := envLookup(env, key)
	if !ok {
		t.Errorf("env %q absent, want %q=%q", key, key, wantVal)
		return
	}
	if got != wantVal {
		t.Errorf("env %q = %q, want %q", key, got, wantVal)
	}
}

// assertEnvAbsent fails if key is present in env.
func assertEnvAbsent(t *testing.T, env []string, key string) {
	t.Helper()
	if _, ok := envLookup(env, key); ok {
		t.Errorf("env %q is present, want absent", key)
	}
}

// assertHasArgPair fails if flag and value do not appear as consecutive
// elements in args.
func assertHasArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("buildRunArgs() missing %q %q in [%s]", flag, value, strings.Join(args, " "))
}

// assertHasFlag fails if flag does not appear in args.
func assertHasFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			return
		}
	}
	t.Errorf("buildRunArgs() missing flag %q in [%s]", flag, strings.Join(args, " "))
}

// assertNoFlag fails if flag appears anywhere in args.
func assertNoFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			t.Errorf("buildRunArgs() unexpected flag %q in [%s]", flag, strings.Join(args, " "))
			return
		}
	}
}

// newTestSessionState returns a sessionState suitable for buildRunArgs tests.
func newTestSessionState(workspacePath, sessionID string) *sessionState {
	return &sessionState{
		target: agentcore.LaunchTarget{
			WorkspacePath: workspacePath,
		},
		sessionID: sessionID,
	}
}

func TestNewOpenCodeAdapter_ParsePassthroughConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    map[string]any
		wantErr   bool
		checkFunc func(t *testing.T, pt passthroughConfig)
	}{
		{
			name:   "defaults",
			config: map[string]any{},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if !pt.DangerousSkipPermissions {
					t.Error("DangerousSkipPermissions = false, want true (default)")
				}
				if !pt.DisableAutocompact {
					t.Error("DisableAutocompact = false, want true (default)")
				}
			},
		},
		{
			name: "allowed_tools_parse",
			config: map[string]any{
				"allowed_tools": []any{"read", "edit"},
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if len(pt.AllowedTools) != 2 {
					t.Fatalf("AllowedTools len = %d, want 2", len(pt.AllowedTools))
				}
				if pt.AllowedTools[0] != "read" {
					t.Errorf("AllowedTools[0] = %q, want %q", pt.AllowedTools[0], "read")
				}
				if pt.AllowedTools[1] != "edit" {
					t.Errorf("AllowedTools[1] = %q, want %q", pt.AllowedTools[1], "edit")
				}
			},
		},
		{
			name: "denied_tools_parse",
			config: map[string]any{
				"denied_tools": []any{"bash"},
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if len(pt.DeniedTools) != 1 {
					t.Fatalf("DeniedTools len = %d, want 1", len(pt.DeniedTools))
				}
				if pt.DeniedTools[0] != "bash" {
					t.Errorf("DeniedTools[0] = %q, want %q", pt.DeniedTools[0], "bash")
				}
			},
		},
		{
			name: "unknown_key_preserved",
			config: map[string]any{
				"allowed_tools": []any{"customtool"},
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if len(pt.AllowedTools) != 1 || pt.AllowedTools[0] != "customtool" {
					t.Errorf("AllowedTools = %v, want [customtool]", pt.AllowedTools)
				}
			},
		},
		{
			name: "overlap_error",
			config: map[string]any{
				"allowed_tools": []any{"bash"},
				"denied_tools":  []any{"bash"},
			},
			wantErr: true,
		},
		{
			name: "model_and_flags",
			config: map[string]any{
				"model":                        "anthropic/claude-3-5-sonnet",
				"dangerously_skip_permissions": false,
				"disable_autocompact":          false,
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				if pt.Model != "anthropic/claude-3-5-sonnet" {
					t.Errorf("Model = %q, want %q", pt.Model, "anthropic/claude-3-5-sonnet")
				}
				if pt.DangerousSkipPermissions {
					t.Error("DangerousSkipPermissions = true, want false")
				}
				if pt.DisableAutocompact {
					t.Error("DisableAutocompact = true, want false")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := NewOpenCodeAdapter(tt.config)

			if tt.wantErr {
				if err == nil {
					t.Fatal("NewOpenCodeAdapter() error = nil, want error")
				}
				if !strings.Contains(err.Error(), "bash") {
					t.Errorf("error = %q, want it to mention %q", err.Error(), "bash")
				}
				return
			}

			if err != nil {
				t.Fatalf("NewOpenCodeAdapter() error = %v", err)
			}

			oc, ok := a.(*OpenCodeAdapter)
			if !ok {
				t.Fatalf("adapter type = %T, want *OpenCodeAdapter", a)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, oc.passthrough)
			}
		})
	}
}

func TestBuildRunArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		sessionID   string
		pt          passthroughConfig
		prompt      string
		wantPresent []string
		wantPairs   [][2]string
		wantAbsent  []string
	}{
		{
			name:       "fresh_session",
			sessionID:  "",
			pt:         passthroughConfig{},
			prompt:     "do work",
			wantAbsent: []string{"--session"},
		},
		{
			name:      "resume_session",
			sessionID: "ses_abc",
			pt:        passthroughConfig{},
			prompt:    "continue",
			wantPairs: [][2]string{{"--session", "ses_abc"}},
		},
		{
			name:        "skip_permissions_default",
			sessionID:   "",
			pt:          passthroughConfig{DangerousSkipPermissions: true},
			prompt:      "work",
			wantPresent: []string{"--dangerously-skip-permissions"},
		},
		{
			name:       "skip_permissions_disabled",
			sessionID:  "",
			pt:         passthroughConfig{DangerousSkipPermissions: false},
			prompt:     "work",
			wantAbsent: []string{"--dangerously-skip-permissions"},
		},
		{
			name:      "model_flag",
			sessionID: "",
			pt:        passthroughConfig{Model: "anthropic/claude-3-5-sonnet"},
			prompt:    "work",
			wantPairs: [][2]string{{"--model", "anthropic/claude-3-5-sonnet"}},
		},
		{
			name:      "prompt_after_dashdash",
			sessionID: "",
			pt:        passthroughConfig{},
			prompt:    "my --prompt with flags",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newTestSessionState("/tmp/workspace", tt.sessionID)
			args := buildRunArgs(state, tt.prompt, tt.pt)

			for _, flag := range tt.wantPresent {
				assertHasFlag(t, args, flag)
			}
			for _, pair := range tt.wantPairs {
				assertHasArgPair(t, args, pair[0], pair[1])
			}
			for _, flag := range tt.wantAbsent {
				assertNoFlag(t, args, flag)
			}

			// Prompt must be the last argument, after "--".
			if len(args) < 2 {
				t.Fatalf("args too short: %v", args)
			}
			lastTwo := args[len(args)-2:]
			if lastTwo[0] != "--" {
				t.Errorf("second-to-last arg = %q, want %q", lastTwo[0], "--")
			}
			if lastTwo[1] != tt.prompt {
				t.Errorf("last arg = %q, want prompt %q", lastTwo[1], tt.prompt)
			}
		})
	}
}

func TestBuildRunEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		base      []string
		pt        passthroughConfig
		checkFunc func(t *testing.T, env []string)
	}{
		{
			name: "baseline_always_set",
			base: []string{},
			pt:   passthroughConfig{},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_AUTO_SHARE", "false")
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOUPDATE", "true")
				assertEnvPresent(t, env, "OPENCODE_DISABLE_LSP_DOWNLOAD", "true")
			},
		},
		{
			name: "autocompact_default_true",
			base: []string{},
			pt:   passthroughConfig{DisableAutocompact: true},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOCOMPACT", "true")
			},
		},
		{
			name: "autocompact_disabled",
			base: []string{},
			pt:   passthroughConfig{DisableAutocompact: false},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvPresent(t, env, "OPENCODE_DISABLE_AUTOCOMPACT", "false")
			},
		},
		{
			name: "inherited_permission_removed",
			base: []string{"OPENCODE_PERMISSION=old_value", "OTHER_VAR=keep"},
			pt:   passthroughConfig{},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvAbsent(t, env, "OPENCODE_PERMISSION")
				assertEnvPresent(t, env, "OTHER_VAR", "keep")
			},
		},
		{
			name: "allowed_tools_policy",
			base: []string{},
			pt:   passthroughConfig{AllowedTools: []string{"read"}},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				raw, ok := envLookup(env, "OPENCODE_PERMISSION")
				if !ok {
					t.Fatal("OPENCODE_PERMISSION absent")
				}
				var policy map[string]string
				if err := json.Unmarshal([]byte(raw), &policy); err != nil {
					t.Fatalf("OPENCODE_PERMISSION unmarshal: %v", err)
				}
				if policy["read"] != "allow" {
					t.Errorf("OPENCODE_PERMISSION[read] = %q, want %q", policy["read"], "allow")
				}
				if policy["bash"] != "deny" {
					t.Errorf("OPENCODE_PERMISSION[bash] = %q, want %q", policy["bash"], "deny")
				}
			},
		},
		{
			name: "denied_tools_policy",
			base: []string{},
			pt:   passthroughConfig{DeniedTools: []string{"bash"}},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				raw, ok := envLookup(env, "OPENCODE_PERMISSION")
				if !ok {
					t.Fatal("OPENCODE_PERMISSION absent")
				}
				var policy map[string]string
				if err := json.Unmarshal([]byte(raw), &policy); err != nil {
					t.Fatalf("OPENCODE_PERMISSION unmarshal: %v", err)
				}
				if policy["bash"] != "deny" {
					t.Errorf("OPENCODE_PERMISSION[bash] = %q, want %q", policy["bash"], "deny")
				}
			},
		},
		{
			name: "no_policy_no_permission_key",
			base: []string{},
			pt:   passthroughConfig{},
			checkFunc: func(t *testing.T, env []string) {
				t.Helper()
				assertEnvAbsent(t, env, "OPENCODE_PERMISSION")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env, err := buildRunEnv(tt.base, tt.pt)
			if err != nil {
				t.Fatalf("buildRunEnv() error = %v", err)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, env)
			}
		})
	}
}

func TestSSHRemoteCommand(t *testing.T) {
	t.Parallel()

	t.Run("env_prefixed", func(t *testing.T) {
		t.Parallel()

		extra := map[string]string{
			"KEY_A": "value_a",
			"KEY_B": "value_b",
		}
		got := buildSSHRemoteCommand("opencode", extra)

		if !strings.Contains(got, "KEY_A=") {
			t.Errorf("result %q missing KEY_A", got)
		}
		if !strings.Contains(got, "KEY_B=") {
			t.Errorf("result %q missing KEY_B", got)
		}
		if !strings.HasSuffix(got, " opencode") {
			t.Errorf("result %q does not end with remote command", got)
		}
	})

	t.Run("values_shell_quoted", func(t *testing.T) {
		t.Parallel()

		extra := map[string]string{
			"KEY": "value with spaces",
		}
		got := buildSSHRemoteCommand("opencode", extra)

		// ShellQuote wraps in single quotes.
		if !strings.Contains(got, "'value with spaces'") {
			t.Errorf("result %q: value with spaces not single-quoted", got)
		}
	})

	t.Run("no_extra_env_returns_command", func(t *testing.T) {
		t.Parallel()

		got := buildSSHRemoteCommand("opencode run --format json", nil)
		if got != "opencode run --format json" {
			t.Errorf("result = %q, want %q", got, "opencode run --format json")
		}
	})

	t.Run("no_arbitrary_env", func(t *testing.T) {
		t.Parallel()

		extra := map[string]string{
			"MY_KEY": "my_val",
		}
		got := buildSSHRemoteCommand("opencode", extra)

		// Only MY_KEY should appear as an env prefix; no other KEY= patterns.
		parts := strings.Fields(got)
		envCount := 0
		for _, p := range parts {
			if strings.Contains(p, "=") && p != "opencode" {
				envCount++
			}
		}
		if envCount != 1 {
			t.Errorf("env prefix count = %d, want 1; result = %q", envCount, got)
		}
	})
}
