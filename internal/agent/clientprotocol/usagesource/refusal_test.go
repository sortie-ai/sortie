package usagesource

import (
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
)

func TestCheckDeclarationInitializeMeta(t *testing.T) {
	t.Parallel()

	tests := []struct {
		meta        string
		hasLaunches bool
	}{
		{`{"events":["a"]}`, false},
		{`["a"]`, true},
	}

	for _, tt := range tests {
		d := Default
		d.InitializeMeta = json.RawMessage(tt.meta)
		if tt.hasLaunches {
			d.Launches = func(agentcore.LaunchTarget) bool { return true }
		}

		if err := checkDeclaration(d, struct{}{}); err == nil {
			t.Errorf("checkDeclaration(meta %s, launches %t) = nil, want an error", tt.meta, tt.hasLaunches)
		}
	}
}

func TestGeminiPromptError(t *testing.T) {
	t.Parallel()

	for code, wantRefusal := range map[int]bool{429: true, 503: false, 500: false} {
		got := geminiPromptError(jsonrpc.Error{Code: code}).Refusal

		if (got != nil) != wantRefusal || wantRefusal && (!got.Limit.ModelOnly || !got.WindowReset) {
			t.Errorf("geminiPromptError(code %d).Refusal = %+v, want refusal %t, model-only with a window reset", code, got, wantRefusal)
		}
	}
}

func TestGeminiBucketReadingStatesItsModel(t *testing.T) {
	t.Parallel()

	share := 0.5
	for _, tokenType := range []string{"", "REQUESTS"} {
		readings := geminiBucketReadings([]geminiBucket{{ModelID: "gemini-2.5-pro", TokenType: tokenType, RemainingFraction: &share}})

		if len(readings) != 1 || readings[0].Span.Model != "gemini-2.5-pro" {
			t.Errorf("geminiBucketReadings(token type %q) = %+v, want one reading with Span.Model %q", tokenType, readings, "gemini-2.5-pro")
		}
	}
}

func TestKiroPromptError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		data                       string
		wantRefusal, wantTransient bool
	}{
		{`{"errorType":"UsageLimitReachedError","retryErrorType":"THROTTLING"}`, true, false},
		{`{"retryErrorType":"THROTTLING"}`, false, true},
	}

	for _, tt := range tests {
		got := kiroPromptError(jsonrpc.Error{Data: json.RawMessage(tt.data)})

		if (got.Refusal != nil) != tt.wantRefusal || got.Transient != tt.wantTransient {
			t.Errorf("kiroPromptError(%s) = {refusal %t, transient %t}, want {%t, %t}", tt.data, got.Refusal != nil, got.Transient, tt.wantRefusal, tt.wantTransient)
		}
	}
}
