package agenttest

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

func effortProbeFrom(carry func(level string, verification, resumed bool, turn int) string) EffortProbe {
	return func(t *testing.T, passthrough map[string]any, verification, resumed bool, turns int) ([]string, error) {
		t.Helper()

		level, fault := registry.EffortSetting(passthrough)
		if fault != nil {
			return nil, fault
		}
		got := make([]string, turns)
		for i := range got {
			got[i] = carry(level, verification, resumed, i+1)
		}
		return got, nil
	}
}

func TestAssertEffortForwarding(t *testing.T) {
	t.Parallel()

	const (
		setTurnPrefix = `case %q: probe(%v, verification=%t, resumed=%t, turns=%d) turn %d carries`
		wrongTypeFail = `case %q: probe(%v) error`
	)

	tests := []struct {
		name         string
		probe        EffortProbe
		wantFailures []string
	}{
		{
			name:  "conformant probe passes",
			probe: effortProbeFrom(func(level string, _, _ bool, _ int) string { return level }),
		},
		{
			name: "drops the level on turn 2",
			probe: effortProbeFrom(func(level string, _, _ bool, turn int) string {
				if turn > 1 {
					return ""
				}
				return level
			}),
			wantFailures: []string{setTurnPrefix, setTurnPrefix},
		},
		{
			name: "drops the level on the verification session",
			probe: effortProbeFrom(func(level string, verification, _ bool, _ int) string {
				if verification {
					return ""
				}
				return level
			}),
			wantFailures: []string{setTurnPrefix},
		},
		{
			name: "drops the level on the first turn of a resumed session",
			probe: effortProbeFrom(func(level string, _, resumed bool, turn int) string {
				if resumed && turn == 1 {
					return ""
				}
				return level
			}),
			wantFailures: []string{setTurnPrefix},
		},
		{
			name: "alters the level",
			probe: effortProbeFrom(func(level string, _, _ bool, _ int) string {
				return strings.ToLower(strings.TrimSpace(level))
			}),
			wantFailures: []string{setTurnPrefix, setTurnPrefix, setTurnPrefix, setTurnPrefix, setTurnPrefix},
		},
		{
			name: "reads the level from passthrough and accepts the wrong-type input",
			probe: func(t *testing.T, passthrough map[string]any, _, _ bool, turns int) ([]string, error) {
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

	AssertEffortForwarding(t, effortProbeFrom(func(level string, _, _ bool, _ int) string { return level }))
}
