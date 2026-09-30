package agenttest

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

func effortProbeFrom(carry func(level string, verification bool, turn int) string) EffortProbe {
	return func(t *testing.T, passthrough map[string]any, verification bool, turns int) ([]string, error) {
		t.Helper()

		level, fault := registry.EffortSetting(passthrough)
		if fault != nil {
			return nil, fault
		}
		got := make([]string, turns)
		for i := range got {
			got[i] = carry(level, verification, i+1)
		}
		return got, nil
	}
}

func TestAssertEffortForwarding(t *testing.T) {
	t.Parallel()

	const (
		setTurnPrefix = `case %q: probe(%v, verification=%t, turns=%d) turn %d carries`
		wrongTypeFail = `case %q: probe(%v) error`
	)

	tests := []struct {
		name         string
		probe        EffortProbe
		wantFailures []string
	}{
		{
			name:  "conformant probe passes",
			probe: effortProbeFrom(func(level string, _ bool, _ int) string { return level }),
		},
		{
			name: "drops the level on turn 2",
			probe: effortProbeFrom(func(level string, _ bool, turn int) string {
				if turn > 1 {
					return ""
				}
				return level
			}),
			wantFailures: []string{setTurnPrefix},
		},
		{
			name: "drops the level on the verification session",
			probe: effortProbeFrom(func(level string, verification bool, _ int) string {
				if verification {
					return ""
				}
				return level
			}),
			wantFailures: []string{setTurnPrefix},
		},
		{
			name: "alters the level",
			probe: effortProbeFrom(func(level string, _ bool, _ int) string {
				return strings.ToLower(strings.TrimSpace(level))
			}),
			wantFailures: []string{setTurnPrefix, setTurnPrefix, setTurnPrefix},
		},
		{
			name: "reads the level from passthrough and accepts the wrong-type input",
			probe: func(t *testing.T, passthrough map[string]any, _ bool, turns int) ([]string, error) {
				t.Helper()

				level, _ := passthrough[registry.EffortKey].(string)
				got := make([]string, turns)
				for i := range got {
					got[i] = level
				}
				return got, nil
			},
			wantFailures: []string{wrongTypeFail},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reporter fakeReporter

			assertEffortForwarding(t, &reporter, tt.probe)

			requireFailures(t, reporter.errors, tt.wantFailures)
		})
	}
}

func TestAssertEffortForwarding_ConformantProbeThroughTestingT(t *testing.T) {
	t.Parallel()

	AssertEffortForwarding(t, effortProbeFrom(func(level string, _ bool, _ int) string { return level }))
}
