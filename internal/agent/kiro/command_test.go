package kiro

import (
	"slices"
	"testing"
)

func TestParsePassthroughConfig_Fields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
		want   passthroughConfig
	}{
		{
			name:   "empty config defaults trust_all_tools to true",
			config: map[string]any{},
			want:   passthroughConfig{TrustAllTools: true},
		},
		{
			name:   "nil config defaults trust_all_tools to true",
			config: nil,
			want:   passthroughConfig{TrustAllTools: true},
		},
		{
			name: "allowlist fields extracted",
			config: map[string]any{
				"model":       "claude-sonnet-4.6",
				"trust_tools": []any{"read", "grep", "glob"},
				"agent":       "my-agent",
			},
			want: passthroughConfig{
				Model:      "claude-sonnet-4.6",
				TrustTools: []string{"read", "grep", "glob"},
				Agent:      "my-agent",
			},
		},
		{
			name:   "trust_all_tools flag extracted",
			config: map[string]any{"trust_all_tools": true},
			want:   passthroughConfig{TrustAllTools: true},
		},
		{
			name:   "only model set",
			config: map[string]any{"model": "claude-opus-4.7"},
			want:   passthroughConfig{Model: "claude-opus-4.7", TrustAllTools: true},
		},
		{
			name:   "wrong-typed trust_all_tools takes false default",
			config: map[string]any{"trust_all_tools": "yes"},
			want:   passthroughConfig{},
		},
		{
			name:   "wrong-typed trust_tools yields nil slice",
			config: map[string]any{"trust_tools": "read,grep"},
			want:   passthroughConfig{},
		},
		{
			name:   "non-string trust_tools elements are skipped",
			config: map[string]any{"trust_tools": []any{"read", 7, "grep"}},
			want:   passthroughConfig{TrustTools: []string{"read", "grep"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fault := parsePassthroughConfig(tt.config)
			if fault != nil {
				t.Fatalf("parsePassthroughConfig(%v) fault = %v, want nil", tt.config, fault)
			}

			if got.Model != tt.want.Model {
				t.Errorf("parsePassthroughConfig(%v).Model = %q, want %q", tt.config, got.Model, tt.want.Model)
			}
			if got.TrustAllTools != tt.want.TrustAllTools {
				t.Errorf("parsePassthroughConfig(%v).TrustAllTools = %v, want %v", tt.config, got.TrustAllTools, tt.want.TrustAllTools)
			}
			if !slices.Equal(got.TrustTools, tt.want.TrustTools) {
				t.Errorf("parsePassthroughConfig(%v).TrustTools = %v, want %v", tt.config, got.TrustTools, tt.want.TrustTools)
			}
			if got.Agent != tt.want.Agent {
				t.Errorf("parsePassthroughConfig(%v).Agent = %q, want %q", tt.config, got.Agent, tt.want.Agent)
			}
		})
	}

	t.Run("wrong-typed model reports a fault", func(t *testing.T) {
		t.Parallel()
		_, fault := parsePassthroughConfig(map[string]any{"model": 42})
		if fault == nil {
			t.Fatal("parsePassthroughConfig: got nil fault, want non-nil")
		}
		if fault.Key != "model" {
			t.Errorf("fault.Key = %q, want %q", fault.Key, "model")
		}
	})

	t.Run("wrong-typed agent reports a fault", func(t *testing.T) {
		t.Parallel()
		_, fault := parsePassthroughConfig(map[string]any{"agent": true})
		if fault == nil {
			t.Fatal("parsePassthroughConfig: got nil fault, want non-nil")
		}
		if fault.Key != "agent" {
			t.Errorf("fault.Key = %q, want %q", fault.Key, "agent")
		}
	})
}

func TestParsePassthroughConfig_ClonesTrustTools(t *testing.T) {
	t.Parallel()

	source := []any{"read", "grep"}
	pt, err := parsePassthroughConfig(map[string]any{"trust_tools": source})
	if err != nil {
		t.Fatalf("parsePassthroughConfig() error = %v", err)
	}

	source[0] = "mutated"

	if pt.TrustTools[0] != "read" {
		t.Errorf("TrustTools[0] = %q, want %q (parsed slice must not alias source)", pt.TrustTools[0], "read")
	}
}
