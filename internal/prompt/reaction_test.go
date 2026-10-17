package prompt

import (
	"strings"
	"testing"
)

func TestBuildReactionTurnPrompt_Selection(t *testing.T) {
	t.Parallel()
	fix := map[string]any{"label_fix": map[string]any{"pr_number": "7", "owner": "o", "repo": "r", "branch": "feat", "actor": "a", "requested_at": "now"}}
	tests := []struct {
		name, body, want string
		turn             int
	}{
		{name: "branch on turn 1", body: "{{ if .label_fix }}FIX{{ else }}WORK{{ end }}", want: "FIX", turn: 1},
		{name: "branch on turn 2", body: "{{ if .label_fix }}FIX{{ else }}WORK{{ end }}", want: "FIX", turn: 2},
		{name: "no branch", body: "WORK", want: "## Fix this pull request", turn: 1},
		{name: "whitespace-only branch", body: "{{ if .label_fix }} {{ else }}WORK{{ end }}", want: "## Fix this pull request", turn: 1},
		{name: "unguarded key", body: "{{ .label_fix.branch }}", want: "feat", turn: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := BuildReactionTurnPrompt(mustParseAnalyze(t, tt.body), nil, nil, tt.turn, 3, fix)
			if err != nil || !strings.HasPrefix(got, tt.want) {
				t.Errorf("BuildReactionTurnPrompt(%q, turn %d) = %q, %v, want prefix %q", tt.body, tt.turn, got, err, tt.want)
			}
		})
	}
}

func TestBuiltinPromptsRenderForEveryContinuationKey(t *testing.T) {
	t.Parallel()
	for _, k := range continuationKeys {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			fields := map[string]any{}
			for f, children := range templateFieldSchema[k.name] {
				fields[f] = "v-" + f
				if children != nil {
					fields[f] = []any{map[string]any{"name": "v-name", "status": "v-status", "conclusion": "v-conclusion", "details_url": "v-url"}}
				}
			}
			data := any(fields)
			if len(fields) == 0 {
				data = []any{map[string]any{"id": "v-id", "file": "v-file", "start_line": "v-start", "end_line": "v-end", "reviewer": "v-reviewer", "body": "v-body"}}
			}
			for turn := 1; turn <= 2; turn++ {
				got, err := BuildReactionTurnPrompt(mustParseAnalyze(t, "plain"), map[string]any{"identifier": "ID-1", "title": "T"}, nil, turn, 3, map[string]any{k.name: data})
				if err != nil || strings.TrimSpace(got) == "" || got == "plain" || (turn == 1 && !strings.Contains(got, "v-")) {
					t.Errorf("BuildReactionTurnPrompt(%q, turn %d) = %q, %v, want the built-in prompt holding a sample value", k.name, turn, got, err)
				}
			}
		})
	}
}
