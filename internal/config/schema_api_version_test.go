package config

import "testing"

// TestValidateFrontMatter_APIVersion verifies the schema recognizes
// tracker.api_version as a known FieldString: a quoted string draws no
// warning, while a bare YAML integer draws a single non-fatal
// type_mismatch advisory.
func TestValidateFrontMatter_APIVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        map[string]any
		wantCount  int
		wantChecks []string
		wantFields []string
	}{
		{
			name: "quoted string value is recognized, no warning",
			raw: map[string]any{
				"tracker": map[string]any{"kind": "jira", "api_version": "2"},
			},
			wantCount: 0,
		},
		{
			name: "bare integer draws a single type_mismatch advisory",
			raw: map[string]any{
				"tracker": map[string]any{"kind": "jira", "api_version": 2},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"tracker.api_version"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ValidateFrontMatter(tt.raw, ServiceConfig{Tracker: TrackerConfig{Kind: "jira"}})

			if len(got) != tt.wantCount {
				t.Fatalf("ValidateFrontMatter() returned %d warnings, want %d\nwarnings: %+v", len(got), tt.wantCount, got)
			}
			for i, wantCheck := range tt.wantChecks {
				if got[i].Check != wantCheck {
					t.Errorf("warnings[%d].Check = %q, want %q", i, got[i].Check, wantCheck)
				}
			}
			for i, wantField := range tt.wantFields {
				if got[i].Field != wantField {
					t.Errorf("warnings[%d].Field = %q, want %q", i, got[i].Field, wantField)
				}
			}
		})
	}
}

// TestValidateFrontMatter_APIVersionNotUnknownKey is a focused guard:
// api_version must never surface as an unknown_sub_key.
func TestValidateFrontMatter_APIVersionNotUnknownKey(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"tracker": map[string]any{"kind": "jira", "api_version": "3"},
	}
	for _, w := range ValidateFrontMatter(raw, ServiceConfig{Tracker: TrackerConfig{Kind: "jira"}}) {
		if w.Check == "unknown_sub_key" && w.Field == "tracker.api_version" {
			t.Errorf("api_version reported as unknown_sub_key: %+v", w)
		}
	}
}
