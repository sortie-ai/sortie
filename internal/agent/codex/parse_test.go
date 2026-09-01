package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestNormalizeBreakdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   tokenUsageBreakdown
		want domain.TokenUsage
	}{
		{
			name: "inputTokens already includes cachedInputTokens",
			in:   tokenUsageBreakdown{InputTokens: 27428, CachedInputTokens: 13696, OutputTokens: 30, ReasoningOutputTokens: 18, TotalTokens: 27458},
			want: domain.TokenUsage{InputTokens: 27428, OutputTokens: 30, TotalTokens: 27458, CacheReadTokens: 13696},
		},
		{
			name: "zero value",
			in:   tokenUsageBreakdown{},
			want: domain.TokenUsage{},
		},
		{
			name: "totalTokens field ignored, recomputed from input plus output",
			in:   tokenUsageBreakdown{InputTokens: 100, OutputTokens: 50, TotalTokens: 999999},
			want: domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeBreakdown(tt.in)
			if got != tt.want {
				t.Errorf("normalizeBreakdown(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseTokenUsageUpdated(t *testing.T) {
	t.Parallel()

	t.Run("valid payload", func(t *testing.T) {
		t.Parallel()
		raw := json.RawMessage(`{"threadId":"th-1","turnId":"tn-1","tokenUsage":{"last":{"totalTokens":150,"inputTokens":100,"cachedInputTokens":10,"outputTokens":50},"total":{"totalTokens":300,"inputTokens":200,"cachedInputTokens":20,"outputTokens":100}}}`)
		got, err := parseTokenUsageUpdated(raw)
		if err != nil {
			t.Fatalf("parseTokenUsageUpdated() error = %v", err)
		}
		if got.ThreadID != "th-1" || got.TurnID != "tn-1" {
			t.Errorf("ThreadID/TurnID = %q/%q, want %q/%q", got.ThreadID, got.TurnID, "th-1", "tn-1")
		}
		if got.TokenUsage.Total.InputTokens != 200 {
			t.Errorf("TokenUsage.Total.InputTokens = %d, want 200", got.TokenUsage.Total.InputTokens)
		}
		if got.TokenUsage.Last.OutputTokens != 50 {
			t.Errorf("TokenUsage.Last.OutputTokens = %d, want 50", got.TokenUsage.Last.OutputTokens)
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

func TestSubtractUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b domain.TokenUsage
		want domain.TokenUsage
	}{
		{
			name: "positive difference",
			a:    domain.TokenUsage{InputTokens: 27549, OutputTokens: 82, CacheReadTokens: 27392},
			b:    domain.TokenUsage{InputTokens: 13731, OutputTokens: 54, CacheReadTokens: 13700},
			want: domain.TokenUsage{InputTokens: 13818, OutputTokens: 28, CacheReadTokens: 13692, TotalTokens: 13846},
		},
		{
			name: "floored at zero when b exceeds a",
			a:    domain.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2},
			b:    domain.TokenUsage{InputTokens: 100, OutputTokens: 100, CacheReadTokens: 100},
			want: domain.TokenUsage{},
		},
		{
			name: "equal values yield zero delta",
			a:    domain.TokenUsage{InputTokens: 50, OutputTokens: 20, CacheReadTokens: 5},
			b:    domain.TokenUsage{InputTokens: 50, OutputTokens: 20, CacheReadTokens: 5},
			want: domain.TokenUsage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := subtractUsage(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("subtractUsage(%+v, %+v) = %+v, want %+v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestMaxUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b domain.TokenUsage
		want domain.TokenUsage
	}{
		{
			name: "componentwise maximum, mixed",
			a:    domain.TokenUsage{InputTokens: 100, OutputTokens: 5, CacheReadTokens: 50},
			b:    domain.TokenUsage{InputTokens: 20, OutputTokens: 40, CacheReadTokens: 10},
			want: domain.TokenUsage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 50, TotalTokens: 140},
		},
		{
			name: "b entirely zero returns a",
			a:    domain.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			b:    domain.TokenUsage{},
			want: domain.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := maxUsage(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("maxUsage(%+v, %+v) = %+v, want %+v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestMapCodexErrorInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		info string
		want domain.AgentErrorKind
	}{
		{"Unauthorized", domain.ErrResponseError},
		{"BadRequest", domain.ErrResponseError},
		{"ContextWindowExceeded", domain.ErrTurnFailed},
		{"UsageLimitExceeded", domain.ErrTurnFailed},
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

func TestSummarizeItem(t *testing.T) {
	t.Parallel()

	t.Run("short item not truncated", func(t *testing.T) {
		t.Parallel()
		got := summarizeItem("agentMessage", "item-001")
		want := "[agentMessage] item-001"
		if got != want {
			t.Errorf("summarizeItem() = %q, want %q", got, want)
		}
	})

	t.Run("long item truncated with ellipsis suffix", func(t *testing.T) {
		t.Parallel()
		// Prefix "[agentMessage] " is 15 chars; ID of 250 chars makes 265 total.
		// TruncateRunes keeps first 200 runes then appends "…" (1 rune) → 201 runes.
		longID := strings.Repeat("x", 250)
		got := summarizeItem("agentMessage", longID)
		runeCount := utf8.RuneCountInString(got)
		if runeCount != 201 {
			t.Errorf("summarizeItem() rune count = %d, want 201", runeCount)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("summarizeItem() does not end with ellipsis: %q", got[max(0, len(got)-10):])
		}
	})

	t.Run("exactly 200 runes not truncated", func(t *testing.T) {
		t.Parallel()
		// Prefix is 15 chars; ID of 185 chars makes exactly 200 total.
		exactID := strings.Repeat("z", 185)
		got := summarizeItem("agentMessage", exactID)
		want := "[agentMessage] " + exactID
		if got != want {
			t.Errorf("summarizeItem() = %q, want %q", got, want)
		}
	})
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
