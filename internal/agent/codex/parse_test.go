package codex

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestParsePassthroughConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
		want   passthroughConfig
	}{
		{
			name:   "nil config returns zero values",
			config: nil,
			want:   passthroughConfig{},
		},
		{
			name:   "empty config returns zero values",
			config: map[string]any{},
			want:   passthroughConfig{},
		},
		{
			name: "all fields present",
			config: map[string]any{
				"model":           "o4-mini",
				"effort":          "high",
				"approval_policy": "never",
				"thread_sandbox":  "workspaceWrite",
				"personality":     "helpful",
				"turn_sandbox_policy": map[string]any{
					"networkAccess": true,
					"writableRoots": []any{"/workspace/abc", "/tmp"},
				},
			},
			want: passthroughConfig{
				Model:          "o4-mini",
				Effort:         "high",
				ApprovalPolicy: "never",
				ThreadSandbox:  "workspaceWrite",
				Personality:    "helpful",
				TurnSandboxPolicy: map[string]any{
					"networkAccess": true,
					"writableRoots": []any{"/workspace/abc", "/tmp"},
				},
			},
		},
		{
			// turn_sandbox_policy is forwarded verbatim, so a nested
			// value survives parsing uninterpreted.
			name: "nested turn_sandbox_policy value passes through",
			config: map[string]any{
				"turn_sandbox_policy": map[string]any{
					"networkAccess": false,
					"nested":        map[string]any{"inner": "value"},
				},
			},
			want: passthroughConfig{
				TurnSandboxPolicy: map[string]any{
					"networkAccess": false,
					"nested":        map[string]any{"inner": "value"},
				},
			},
		},
		{
			name: "wrong non-string type uses zero-value default",
			config: map[string]any{
				"turn_sandbox_policy": "not-a-map",
			},
			want: passthroughConfig{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, fault := parsePassthroughConfig(tt.config)
			if fault != nil {
				t.Fatalf("parsePassthroughConfig: %v", fault)
			}
			if got.Model != tt.want.Model {
				t.Errorf("Model = %q, want %q", got.Model, tt.want.Model)
			}
			if got.Effort != tt.want.Effort {
				t.Errorf("Effort = %q, want %q", got.Effort, tt.want.Effort)
			}
			if got.ApprovalPolicy != tt.want.ApprovalPolicy {
				t.Errorf("ApprovalPolicy = %q, want %q", got.ApprovalPolicy, tt.want.ApprovalPolicy)
			}
			if got.ThreadSandbox != tt.want.ThreadSandbox {
				t.Errorf("ThreadSandbox = %q, want %q", got.ThreadSandbox, tt.want.ThreadSandbox)
			}
			if got.Personality != tt.want.Personality {
				t.Errorf("Personality = %q, want %q", got.Personality, tt.want.Personality)
			}
			// Compared with reflect.DeepEqual rather than maps.Equal:
			// the values are any, and nested maps and the []any that
			// YAML sequences decode into are not comparable with ==.
			if !reflect.DeepEqual(got.TurnSandboxPolicy, tt.want.TurnSandboxPolicy) {
				t.Errorf("TurnSandboxPolicy = %#v, want %#v", got.TurnSandboxPolicy, tt.want.TurnSandboxPolicy)
			}
		})
	}

	t.Run("wrong string type reports a fault", func(t *testing.T) {
		t.Parallel()
		_, fault := parsePassthroughConfig(map[string]any{"model": 42})
		if fault == nil {
			t.Fatal("parsePassthroughConfig: got nil fault, want non-nil")
		}
		if fault.Key != "model" {
			t.Errorf("fault.Key = %q, want %q", fault.Key, "model")
		}
	})
}

func TestParseTokenUsageUpdated(t *testing.T) {
	t.Parallel()

	t.Run("valid payload", func(t *testing.T) {
		t.Parallel()
		raw := json.RawMessage(`{"threadId":"th-1","turnId":"tn-1","tokenUsage":{"last":{"totalTokens":150,"inputTokens":100,"cachedInputTokens":10,"outputTokens":50},"total":{"totalTokens":300,"inputTokens":200,"cachedInputTokens":20,"cacheWriteInputTokens":7,"outputTokens":100}}}`)
		got, err := parseTokenUsageUpdated(raw)
		if err != nil {
			t.Fatalf("parseTokenUsageUpdated() error = %v", err)
		}
		if got.ThreadID != "th-1" || got.TurnID != "tn-1" {
			t.Errorf("ThreadID/TurnID = %q/%q, want %q/%q", got.ThreadID, got.TurnID, "th-1", "tn-1")
		}
		if in := got.TokenUsage.Total.InputTokens; in == nil || *in != 200 {
			t.Errorf("TokenUsage.Total.InputTokens = %v, want 200", in)
		}
		if cw := got.TokenUsage.Total.CacheWriteInputTokens; cw == nil || *cw != 7 {
			t.Errorf("TokenUsage.Total.CacheWriteInputTokens = %v, want 7", cw)
		}
		if out := got.TokenUsage.Last.OutputTokens; out == nil || *out != 50 {
			t.Errorf("TokenUsage.Last.OutputTokens = %v, want 50", out)
		}
		if cw := got.TokenUsage.Last.CacheWriteInputTokens; cw != nil {
			t.Errorf("TokenUsage.Last.CacheWriteInputTokens = %d, want nil", *cw)
		}
	})

	t.Run("malformed payload returns error", func(t *testing.T) {
		t.Parallel()
		_, err := parseTokenUsageUpdated(json.RawMessage(`not json`))
		if err == nil {
			t.Fatal("parseTokenUsageUpdated(malformed) error = nil, want non-nil")
		}
	})
}

func TestParseModelRerouted_MalformedPayload(t *testing.T) {
	t.Parallel()

	_, err := parseModelRerouted(json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("parseModelRerouted(malformed) error = nil, want non-nil")
	}
}

func TestMapCodexErrorInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		info string
		want domain.AgentErrorKind
	}{
		{"Unauthorized", domain.ErrCredentialUnverified},
		{"unauthorized", domain.ErrCredentialUnverified},
		{"BadRequest", domain.ErrResponseError},
		{"ContextWindowExceeded", domain.ErrTurnFailed},
		{"UsageLimitExceeded", domain.ErrUsageLimit},
		{"SandboxError", domain.ErrTurnFailed},
		{"HttpConnectionFailed", domain.ErrTurnFailed},
		{"ResponseStreamConnectionFailed", domain.ErrTurnFailed},
		{"ResponseStreamDisconnected", domain.ErrTurnFailed},
		{"ResponseTooManyFailedAttempts", domain.ErrTurnFailed},
		{"InternalServerError", domain.ErrTurnFailed},
		{"Other", domain.ErrTurnFailed},
		{"SomeUnknownValue", domain.ErrTurnFailed},
		{"", domain.ErrTurnFailed},
	}

	for _, tt := range tests {
		t.Run(tt.info, func(t *testing.T) {
			t.Parallel()
			got := mapCodexErrorInfo(tt.info)
			if got != tt.want {
				t.Errorf("mapCodexErrorInfo(%q) = %q, want %q", tt.info, got, tt.want)
			}
		})
	}
}

func TestNormalizeSandbox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"workspaceWrite", "workspace-write"},
		{"readOnly", "read-only"},
		{"dangerFullAccess", "danger-full-access"},
		{"externalSandbox", "external-sandbox"},
		{"workspace-write", "workspace-write"},
		{"read-only", "read-only"},
		{"", ""},
		{"custom-value", "custom-value"},
	}

	for _, tt := range tests {
		name := tt.input
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := normalizeSandbox(tt.input)
			if got != tt.want {
				t.Errorf("normalizeSandbox(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDenormalizeSandbox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"workspace-write", "workspaceWrite"},
		{"read-only", "readOnly"},
		{"danger-full-access", "dangerFullAccess"},
		{"external-sandbox", "externalSandbox"},
		{"workspaceWrite", "workspaceWrite"},
		{"readOnly", "readOnly"},
		{"", ""},
		{"custom-value", "custom-value"},
	}

	for _, tt := range tests {
		name := tt.input
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := denormalizeSandbox(tt.input)
			if got != tt.want {
				t.Errorf("denormalizeSandbox(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildSandboxPolicy_Default(t *testing.T) {
	t.Parallel()

	state := &sessionState{target: agentcore.LaunchTarget{WorkspacePath: "/workspace/abc"}}
	policy := buildSandboxPolicy(state, passthroughConfig{})

	if policy["type"] != "workspaceWrite" {
		t.Errorf("type = %v, want %q", policy["type"], "workspaceWrite")
	}

	roots, ok := policy["writableRoots"].([]string)
	if !ok {
		t.Fatalf("writableRoots type = %T, want []string", policy["writableRoots"])
	}
	if len(roots) != 1 || roots[0] != "/workspace/abc" {
		t.Errorf("writableRoots = %v, want [\"/workspace/abc\"]", roots)
	}

	networkAccess, ok := policy["networkAccess"].(bool)
	if !ok {
		t.Fatalf("networkAccess type = %T, want bool", policy["networkAccess"])
	}
	if networkAccess {
		t.Error("networkAccess = true, want false")
	}
}

func TestBuildSandboxPolicy_Override(t *testing.T) {
	t.Parallel()

	state := &sessionState{target: agentcore.LaunchTarget{WorkspacePath: "/workspace/abc"}}
	pt := passthroughConfig{
		ThreadSandbox: "dangerouslyUnrestricted",
		TurnSandboxPolicy: map[string]any{
			"networkAccess": true,
			"customField":   "custom-value",
		},
	}
	policy := buildSandboxPolicy(state, pt)

	if policy["type"] != "dangerouslyUnrestricted" {
		t.Errorf("type = %v, want %q", policy["type"], "dangerouslyUnrestricted")
	}

	networkAccess, ok := policy["networkAccess"].(bool)
	if !ok {
		t.Fatalf("networkAccess type = %T, want bool", policy["networkAccess"])
	}
	if !networkAccess {
		t.Error("networkAccess = false, want true (overridden)")
	}

	if policy["customField"] != "custom-value" {
		t.Errorf("customField = %v, want %q", policy["customField"], "custom-value")
	}

	if _, ok := policy["writableRoots"]; !ok {
		t.Error("writableRoots missing from policy after override")
	}
}
