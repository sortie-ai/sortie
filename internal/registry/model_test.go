package registry_test

import (
	"maps"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

func TestModelSetting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		want        string
		wantGot     string
	}{
		{name: "nil map"},
		{name: "null value", passthrough: map[string]any{registry.ModelKey: nil}},
		{name: "string kept byte for byte", passthrough: map[string]any{registry.ModelKey: "  Provider/Sortie-Model.7\t"}, want: "  Provider/Sortie-Model.7\t"},
		{name: "integer", passthrough: map[string]any{registry.ModelKey: 7}, wantGot: "integer"},
		{name: "map", passthrough: map[string]any{registry.ModelKey: map[string]any{"name": "m"}}, wantGot: "mapping"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := maps.Clone(tt.passthrough)

			got, fault := registry.ModelSetting(tt.passthrough)

			if got != tt.want || (fault == nil) != (tt.wantGot == "") || (fault != nil && (fault.Key != "model" || fault.Want != "string" || fault.Got != tt.wantGot)) {
				t.Errorf("ModelSetting(%v) = %q, %+v, want %q and a fault of got %q", tt.passthrough, got, fault, tt.want, tt.wantGot)
			}
			if !reflect.DeepEqual(tt.passthrough, before) {
				t.Errorf("ModelSetting mutated its input: %v, want %v", tt.passthrough, before)
			}
		})
	}
}
