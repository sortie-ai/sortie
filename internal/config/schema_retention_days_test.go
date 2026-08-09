package config

import "testing"

// TestValidateFrontMatter_RetentionDays verifies the schema recognizes
// workspace.retention_days as a known FieldInt: bare integers,
// string-encoded integers, zero, and nil values draw no warning, a
// non-coercible value draws a single type_mismatch advisory, and a
// genuinely unrecognized workspace sub-key still draws unknown_sub_key.
func TestValidateFrontMatter_RetentionDays(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        map[string]any
		wantCount  int
		wantChecks []string
		wantFields []string
	}{
		{
			name: "bare integer draws no warning",
			raw: map[string]any{
				"workspace": map[string]any{"root": "/tmp/ws", "retention_days": 30},
			},
			wantCount: 0,
		},
		{
			name: "string-encoded integer draws no warning",
			raw: map[string]any{
				"workspace": map[string]any{"root": "/tmp/ws", "retention_days": "30"},
			},
			wantCount: 0,
		},
		{
			name: "zero draws no warning",
			raw: map[string]any{
				"workspace": map[string]any{"root": "/tmp/ws", "retention_days": 0},
			},
			wantCount: 0,
		},
		{
			name: "nil value draws no warning",
			raw: map[string]any{
				"workspace": map[string]any{"root": "/tmp/ws", "retention_days": nil},
			},
			wantCount: 0,
		},
		{
			name: "non-coercible value draws a single type_mismatch advisory",
			raw: map[string]any{
				"workspace": map[string]any{"root": "/tmp/ws", "retention_days": true},
			},
			wantCount:  1,
			wantChecks: []string{"type_mismatch"},
			wantFields: []string{"workspace.retention_days"},
		},
		{
			name: "unrecognized workspace key still draws unknown_sub_key",
			raw: map[string]any{
				"workspace": map[string]any{"root": "/tmp/ws", "retention_days": 30, "retention_dayz": 5},
			},
			wantCount:  1,
			wantChecks: []string{"unknown_sub_key"},
			wantFields: []string{"workspace.retention_dayz"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ValidateFrontMatter(tt.raw, ServiceConfig{})

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

// TestValidateFrontMatter_RetentionDaysNotUnknownKey is a focused guard:
// retention_days must never surface as an unknown_sub_key.
func TestValidateFrontMatter_RetentionDaysNotUnknownKey(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"workspace": map[string]any{"root": "/tmp/ws", "retention_days": 30},
	}
	for _, w := range ValidateFrontMatter(raw, ServiceConfig{}) {
		if w.Check == "unknown_sub_key" && w.Field == "workspace.retention_days" {
			t.Errorf("retention_days reported as unknown_sub_key: %+v", w)
		}
	}
}

// TestValidateFrontMatter_RetentionDaysFromEnvOverride verifies that a
// value written into the raw map by applyEnvOverrides (rather than
// present in the front matter itself) is recognized the same way as a
// literal value, drawing no warning.
//
// t.Setenv panics when called from a parallel test, so neither this
// test nor any subtest it defines calls t.Parallel.
func TestValidateFrontMatter_RetentionDaysFromEnvOverride(t *testing.T) {
	t.Setenv("SORTIE_WORKSPACE_RETENTION_DAYS", "30")

	raw := map[string]any{}
	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}

	got := ValidateFrontMatter(raw, cfg)
	if len(got) != 0 {
		t.Fatalf("ValidateFrontMatter() returned %d warnings, want 0\nwarnings: %+v", len(got), got)
	}
}
