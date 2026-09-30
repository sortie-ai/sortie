package registry_test

import (
	"maps"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

func TestEffortSetting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		want        string
		wantGot     string
	}{
		{name: "nil map", passthrough: nil},
		{name: "absent key", passthrough: map[string]any{"model": "m"}},
		{name: "null value", passthrough: map[string]any{registry.EffortKey: nil}},
		{name: "empty string", passthrough: map[string]any{registry.EffortKey: ""}},
		{name: "arbitrary string kept byte for byte", passthrough: map[string]any{registry.EffortKey: "Sortie Effort-Probe.7"}, want: "Sortie Effort-Probe.7"},
		{name: "surrounding whitespace kept", passthrough: map[string]any{registry.EffortKey: "  high\t"}, want: "  high\t"},
		{name: "mixed case kept", passthrough: map[string]any{registry.EffortKey: "XHigh"}, want: "XHigh"},
		{name: "integer", passthrough: map[string]any{registry.EffortKey: 7}, wantGot: "integer"},
		{name: "boolean", passthrough: map[string]any{registry.EffortKey: true}, wantGot: "boolean"},
		{name: "float", passthrough: map[string]any{registry.EffortKey: 1.5}, wantGot: "float"},
		{name: "list", passthrough: map[string]any{registry.EffortKey: []any{"high"}}, wantGot: "list"},
		{name: "map", passthrough: map[string]any{registry.EffortKey: map[string]any{"level": "high"}}, wantGot: "mapping"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := maps.Clone(tt.passthrough)

			got, fault := registry.EffortSetting(tt.passthrough)

			if tt.wantGot != "" {
				if fault == nil {
					t.Fatalf("EffortSetting(%v) fault = nil, want a type fault", tt.passthrough)
				}
				if fault.Key != "effort" || fault.Want != "string" || fault.Got != tt.wantGot {
					t.Errorf("EffortSetting(%v) fault = {Key:%q Want:%q Got:%q}, want {Key:\"effort\" Want:\"string\" Got:%q}",
						tt.passthrough, fault.Key, fault.Want, fault.Got, tt.wantGot)
				}
				if got != "" {
					t.Errorf("EffortSetting(%v) = %q with a fault, want \"\"", tt.passthrough, got)
				}
			} else {
				if fault != nil {
					t.Fatalf("EffortSetting(%v) fault = %v, want nil", tt.passthrough, fault)
				}
				if got != tt.want {
					t.Errorf("EffortSetting(%v) = %q, want %q", tt.passthrough, got, tt.want)
				}
			}
			if !reflect.DeepEqual(tt.passthrough, before) {
				t.Errorf("EffortSetting mutated its input: %v, want %v", tt.passthrough, before)
			}
		})
	}
}
