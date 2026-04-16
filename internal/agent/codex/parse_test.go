package codex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/domain"
)

// stubTool is a minimal [domain.AgentTool] implementation for testing
// buildDynamicTools.
type stubTool struct {
	name        string
	description string
	schema      json.RawMessage
}

func (s *stubTool) Name() string                 { return s.name }
func (s *stubTool) Description() string          { return s.description }
func (s *stubTool) InputSchema() json.RawMessage { return s.schema }
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

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
				"model":               "o4-mini",
				"effort":              "high",
				"approval_policy":     "never",
				"thread_sandbox":      "workspaceWrite",
				"personality":         "helpful",
				"skip_git_repo_check": true,
				"turn_sandbox_policy": map[string]any{"networkAccess": true},
			},
			want: passthroughConfig{
				Model:             "o4-mini",
				Effort:            "high",
				ApprovalPolicy:    "never",
				ThreadSandbox:     "workspaceWrite",
				Personality:       "helpful",
				SkipGitRepoCheck:  true,
				TurnSandboxPolicy: map[string]any{"networkAccess": true},
			},
		},
		{
			name: "wrong types use zero-value defaults",
			config: map[string]any{
				"model":               42,
				"effort":              false,
				"approval_policy":     123,
				"thread_sandbox":      []string{"not-a-string"},
				"skip_git_repo_check": "yes",
				"turn_sandbox_policy": "not-a-map",
			},
			want: passthroughConfig{},
		},
		{
			name: "explicit false skip_git_repo_check",
			config: map[string]any{
				"skip_git_repo_check": false,
			},
			want: passthroughConfig{SkipGitRepoCheck: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parsePassthroughConfig(tt.config)
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
			if got.SkipGitRepoCheck != tt.want.SkipGitRepoCheck {
				t.Errorf("SkipGitRepoCheck = %v, want %v", got.SkipGitRepoCheck, tt.want.SkipGitRepoCheck)
			}
			if len(got.TurnSandboxPolicy) != len(tt.want.TurnSandboxPolicy) {
				t.Errorf("TurnSandboxPolicy len = %d, want %d", len(got.TurnSandboxPolicy), len(tt.want.TurnSandboxPolicy))
			}
		})
	}
}

func TestParseMessage_Response(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":1,"result":{"serverInfo":{"name":"codex-app-server"}}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage() error = %v", msg.Err)
	}
	if !msg.IsResponse {
		t.Error("IsResponse = false, want true")
	}
	if msg.IsNotification {
		t.Error("IsNotification = true, want false")
	}
	if msg.Response.ID != 1 {
		t.Errorf("Response.ID = %d, want 1", msg.Response.ID)
	}
}

func TestParseMessage_Notification(t *testing.T) {
	t.Parallel()

	line := []byte(`{"method":"thread/started","params":{"threadId":"abc-123"}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage() error = %v", msg.Err)
	}
	if !msg.IsNotification {
		t.Error("IsNotification = false, want true")
	}
	if msg.IsResponse {
		t.Error("IsResponse = true, want false")
	}
	if msg.Notification.Method != "thread/started" {
		t.Errorf("Notification.Method = %q, want %q", msg.Notification.Method, "thread/started")
	}
}

func TestParseMessage_ServerRequest(t *testing.T) {
	t.Parallel()

	// item/tool/call carries both method and id — it is routed as a
	// notification so the event loop dispatches on Method, with the
	// request ID preserved in Response.ID for sending the reply.
	line := []byte(`{"id":42,"method":"item/tool/call","params":{"tool":"my_tool","arguments":{}}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage() error = %v", msg.Err)
	}
	if !msg.IsNotification {
		t.Error("IsNotification = false, want true (server request routed as notification)")
	}
	if msg.Notification.Method != "item/tool/call" {
		t.Errorf("Notification.Method = %q, want %q", msg.Notification.Method, "item/tool/call")
	}
	if msg.Response.ID != 42 {
		t.Errorf("Response.ID = %d, want 42 (request ID preserved for response)", msg.Response.ID)
	}
}

func TestParseMessage_Malformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line []byte
	}{
		{"invalid JSON", []byte(`not json`)},
		{"truncated JSON", []byte(`{`)},
		{"empty object no method or id", []byte(`{}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := parseMessage(tc.line)
			if msg.Err == nil {
				t.Errorf("parseMessage(%q) err = nil, want non-nil", tc.line)
			}
		})
	}
}

func TestNormalizeUsage(t *testing.T) {
	t.Parallel()

	t.Run("nil input returns zero usage", func(t *testing.T) {
		t.Parallel()
		got := normalizeUsage(nil)
		if got != (domain.TokenUsage{}) {
			t.Errorf("normalizeUsage(nil) = %+v, want zero", got)
		}
	})

	t.Run("fields mapped correctly", func(t *testing.T) {
		t.Parallel()
		u := &turnUsage{
			InputTokens:       100,
			OutputTokens:      50,
			CachedInputTokens: 20,
		}
		got := normalizeUsage(u)
		if got.InputTokens != 100 {
			t.Errorf("InputTokens = %d, want 100", got.InputTokens)
		}
		if got.OutputTokens != 50 {
			t.Errorf("OutputTokens = %d, want 50", got.OutputTokens)
		}
		if got.TotalTokens != 150 {
			t.Errorf("TotalTokens = %d, want 150 (input+output)", got.TotalTokens)
		}
		if got.CacheReadTokens != 20 {
			t.Errorf("CacheReadTokens = %d, want 20", got.CacheReadTokens)
		}
	})

	t.Run("zero usage struct", func(t *testing.T) {
		t.Parallel()
		got := normalizeUsage(&turnUsage{})
		if got != (domain.TokenUsage{}) {
			t.Errorf("normalizeUsage(&turnUsage{}) = %+v, want zero", got)
		}
	})
}

func TestMapTurnStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status string
		want   domain.AgentEventType
	}{
		{"completed", domain.EventTurnCompleted},
		{"interrupted", domain.EventTurnCancelled},
		{"failed", domain.EventTurnFailed},
		{"unknown", domain.EventTurnFailed},
		{"", domain.EventTurnFailed},
		{"COMPLETED", domain.EventTurnFailed},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			t.Parallel()
			got := mapTurnStatus(tt.status)
			if got != tt.want {
				t.Errorf("mapTurnStatus(%q) = %q, want %q", tt.status, got, tt.want)
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

	t.Run("long item truncated at 200 runes", func(t *testing.T) {
		t.Parallel()
		// Prefix "[agentMessage] " is 15 chars; ID of 250 chars makes 265 total.
		longID := strings.Repeat("x", 250)
		got := summarizeItem("agentMessage", longID)
		runeCount := utf8.RuneCountInString(got)
		if runeCount != 200 {
			t.Errorf("summarizeItem() rune count = %d, want 200", runeCount)
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

func TestToolResultFor(t *testing.T) {
	t.Parallel()

	t.Run("success result", func(t *testing.T) {
		t.Parallel()
		result := toolResultFor(true, "all good")
		if result["success"] != true {
			t.Errorf("success = %v, want true", result["success"])
		}
		if result["output"] != "all good" {
			t.Errorf("output = %v, want %q", result["output"], "all good")
		}
		items, ok := result["contentItems"].([]map[string]any)
		if !ok || len(items) == 0 {
			t.Fatalf("contentItems missing or wrong type: %T", result["contentItems"])
		}
		if items[0]["text"] != "all good" {
			t.Errorf("contentItems[0].text = %v, want %q", items[0]["text"], "all good")
		}
		if items[0]["type"] != "inputText" {
			t.Errorf("contentItems[0].type = %v, want %q", items[0]["type"], "inputText")
		}
	})

	t.Run("failure result", func(t *testing.T) {
		t.Parallel()
		result := toolResultFor(false, "tool error")
		if result["success"] != false {
			t.Errorf("success = %v, want false", result["success"])
		}
		if result["output"] != "tool error" {
			t.Errorf("output = %v, want %q", result["output"], "tool error")
		}
	})
}

func TestBuildDynamicTools(t *testing.T) {
	t.Parallel()

	t.Run("nil tools returns nil", func(t *testing.T) {
		t.Parallel()
		got := buildDynamicTools(nil)
		if got != nil {
			t.Errorf("buildDynamicTools(nil) = %v, want nil", got)
		}
	})

	t.Run("empty slice returns nil", func(t *testing.T) {
		t.Parallel()
		got := buildDynamicTools([]domain.AgentTool{})
		if got != nil {
			t.Errorf("buildDynamicTools(empty) = %v, want nil", got)
		}
	})

	t.Run("single tool with schema", func(t *testing.T) {
		t.Parallel()
		tool := &stubTool{
			name:        "my_tool",
			description: "does something useful",
			schema:      json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		}
		result := buildDynamicTools([]domain.AgentTool{tool})
		if len(result) != 1 {
			t.Fatalf("buildDynamicTools() len = %d, want 1", len(result))
		}
		entry := result[0]
		if entry["name"] != "my_tool" {
			t.Errorf("entry.name = %v, want %q", entry["name"], "my_tool")
		}
		if entry["description"] != "does something useful" {
			t.Errorf("entry.description = %v, want %q", entry["description"], "does something useful")
		}
		if entry["inputSchema"] == nil {
			t.Error("inputSchema is nil, want non-nil")
		}
	})

	t.Run("tool without schema omits inputSchema key", func(t *testing.T) {
		t.Parallel()
		tool := &stubTool{name: "no_schema", description: "no schema tool"}
		result := buildDynamicTools([]domain.AgentTool{tool})
		if len(result) != 1 {
			t.Fatalf("len = %d, want 1", len(result))
		}
		if _, has := result[0]["inputSchema"]; has {
			t.Error("inputSchema present, want absent when schema is nil")
		}
	})

	t.Run("multiple tools preserved in input order", func(t *testing.T) {
		t.Parallel()
		tools := []domain.AgentTool{
			&stubTool{name: "alpha", description: "first"},
			&stubTool{name: "beta", description: "second"},
		}
		result := buildDynamicTools(tools)
		if len(result) != 2 {
			t.Fatalf("len = %d, want 2", len(result))
		}
		if result[0]["name"] != "alpha" {
			t.Errorf("result[0].name = %v, want %q", result[0]["name"], "alpha")
		}
		if result[1]["name"] != "beta" {
			t.Errorf("result[1].name = %v, want %q", result[1]["name"], "beta")
		}
	})

	t.Run("invalid schema JSON omits inputSchema key", func(t *testing.T) {
		t.Parallel()
		tool := &stubTool{
			name:        "bad_schema",
			description: "tool with invalid schema JSON",
			schema:      json.RawMessage(`{not valid json`),
		}
		result := buildDynamicTools([]domain.AgentTool{tool})
		if len(result) != 1 {
			t.Fatalf("len = %d, want 1", len(result))
		}
		if _, has := result[0]["inputSchema"]; has {
			t.Error("inputSchema present, want absent for invalid JSON schema")
		}
	})
}

func TestBuildSandboxPolicy_Default(t *testing.T) {
	t.Parallel()

	state := &sessionState{workspacePath: "/workspace/abc"}
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

	state := &sessionState{workspacePath: "/workspace/abc"}
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
