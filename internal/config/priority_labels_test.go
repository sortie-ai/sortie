package config

import (
	"errors"
	"maps"
	"math"
	"reflect"
	"testing"
)

func TestPriorityLabelsConfig(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"gitea", "github", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			cfg, err := NewServiceConfig(map[string]any{"tracker": map[string]any{
				"kind": kind, "terminal_states": []any{"done"},
				"priority_labels": map[string]any{"PRIORITY/High": 2, "min": math.MinInt, "max": math.MaxInt},
			}})
			if err != nil {
				t.Fatalf("NewServiceConfig(%s) = %v", kind, err)
			}
			want := map[string]int{"priority/high": 2, "min": math.MinInt, "max": math.MaxInt}
			if !reflect.DeepEqual(cfg.Tracker.PriorityLabels, want) {
				t.Errorf("NewServiceConfig(%s).Tracker.PriorityLabels = %v, want %v", kind, cfg.Tracker.PriorityLabels, want)
			}
		})
	}
}

func TestPriorityLabelsConfigRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		kind   string
		raw    any
		states map[string]any
	}{
		{name: "jira", kind: "jira", raw: map[string]any{"p1": 1}},
		{name: "linear", kind: "linear", raw: map[string]any{"p1": 1}},
		{name: "file even empty", kind: "file", raw: map[string]any{}},
		{name: "unknown", kind: "custom", raw: map[string]any{"p1": 1}},
		{name: "null", raw: nil},
		{name: "string", raw: "p1: 1"},
		{name: "sequence", raw: []any{"p1"}},
		{name: "empty key", raw: map[string]any{"": 1}},
		{name: "blank key", raw: map[string]any{"  ": 1}},
		{name: "duplicate case", raw: map[string]any{"P1": 1, "p1": 2}},
		{name: "fractional", raw: map[string]any{"p1": 1.5}},
		{name: "float", raw: map[string]any{"p1": 1.0}},
		{name: "numeric string", raw: map[string]any{"p1": "1"}},
		{name: "boolean", raw: map[string]any{"p1": true}},
		{name: "null value", raw: map[string]any{"p1": nil}},
		{name: "overflow", raw: map[string]any{"p1": uint64(math.MaxUint64)}},
		{name: "active state", raw: map[string]any{"P1": 1}, states: map[string]any{"active_states": []any{"p1"}}},
		{name: "terminal state", raw: map[string]any{"DONE": 1}},
		{name: "handoff state", raw: map[string]any{"Review": 1}, states: map[string]any{"handoff_state": "review"}},
		{name: "in progress state", raw: map[string]any{"Doing": 1}, states: map[string]any{"active_states": []any{"doing"}, "in_progress_state": "doing"}},
		{name: "no change state", raw: map[string]any{"Review": 1}, states: map[string]any{"handoff_state": "review", "no_change_state": "review"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind := tt.kind
			if kind == "" {
				kind = "gitea"
			}
			tracker := map[string]any{"kind": kind, "terminal_states": []any{"done"}, "priority_labels": tt.raw}
			maps.Copy(tracker, tt.states)
			_, err := NewServiceConfig(map[string]any{"tracker": tracker})
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("NewServiceConfig(%s) error = %v, want *ConfigError", tt.name, err)
			}
			if ce.Field != "tracker.priority_labels" {
				t.Errorf("NewServiceConfig(%s) error field = %q, want tracker.priority_labels", tt.name, ce.Field)
			}
		})
	}
}

func TestPriorityLabelsAbsent(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"gitea", "github", "gitlab", "jira", "linear", "file"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			cfg, err := NewServiceConfig(map[string]any{"tracker": map[string]any{
				"kind": kind, "active_states": []any{"todo"}, "terminal_states": []any{"done"},
			}})
			if err != nil || cfg.Tracker.PriorityLabels != nil {
				t.Errorf("NewServiceConfig(%s, no mapping).PriorityLabels = %v, %v, want nil, nil", kind, cfg.Tracker.PriorityLabels, err)
			}
		})
	}
}

func TestPriorityLabelsSchema(t *testing.T) {
	t.Parallel()
	raw := map[string]any{"tracker": map[string]any{
		"kind": "gitea", "terminal_states": []any{"done"}, "priority_labels": map[string]any{"priority/high": 1, "P1": 2},
	}}
	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig(priority_labels) = %v", err)
	}
	for _, warning := range ValidateFrontMatter(raw, cfg) {
		if warning.Field == "tracker.priority_labels" || warning.Field == "tracker.priority_labels.priority/high" || warning.Field == "tracker.priority_labels.P1" {
			t.Errorf("ValidateFrontMatter(priority_labels) = %+v, want no priority_labels warnings", warning)
		}
	}
}
