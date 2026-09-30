package agenttest

import (
	"errors"
	"maps"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// EffortProbe starts a session of the kind under test with settings, as a
// credential-verification session when verification is true and a resumed
// one when resumed is true, and returns the level each of its first turns
// invocations carries ("" for none). A StartSession failure is returned.
//
// A probe reads each entry off the invocation the adapter itself built for
// that turn, never off settings.
type EffortProbe func(t *testing.T, settings map[string]any, verification, resumed bool, turns int) ([]string, error)

const (
	effortProbeLevel        = "Sortie Effort-Probe.7"
	effortWorkingTurns      = 2
	effortVerificationTurns = 1
)

// AssertEffortForwarding runs the shared effort cases against probe: an
// absent and an empty level reach no invocation, a set level reaches every
// turn of a working, a resumed and a credential-verification session byte
// for byte, and a non-string level fails session start with a type fault on
// [registry.EffortKey].
func AssertEffortForwarding(t *testing.T, probe EffortProbe) {
	t.Helper()
	assertEffortForwarding(t, t, probe)
}

// assertEffortForwarding reports through r so a package-internal test can
// drive it against a double; probe still receives t.
func assertEffortForwarding(t *testing.T, r contractReporter, probe EffortProbe) {
	t.Helper()

	sessions := []struct {
		verification bool
		resumed      bool
		turns        int
	}{
		{turns: effortWorkingTurns},
		{resumed: true, turns: effortWorkingTurns},
		{verification: true, turns: effortVerificationTurns},
	}
	levels := []struct {
		name        string
		passthrough map[string]any
		want        string
	}{
		{name: "absent", passthrough: map[string]any{}, want: ""},
		{name: "empty", passthrough: map[string]any{registry.EffortKey: ""}, want: ""},
		{name: "set", passthrough: map[string]any{registry.EffortKey: effortProbeLevel}, want: effortProbeLevel},
	}

	for _, level := range levels {
		for _, session := range sessions {
			got, err := probe(t, maps.Clone(level.passthrough), session.verification, session.resumed, session.turns)
			if err != nil {
				r.Errorf("case %q: probe(%v, verification=%t, resumed=%t, turns=%d) error = %v, want nil", level.name, level.passthrough, session.verification, session.resumed, session.turns, err)
				continue
			}
			if len(got) != session.turns {
				r.Errorf("case %q: probe(%v, verification=%t, resumed=%t, turns=%d) returned %d entries = %q, want one per turn", level.name, level.passthrough, session.verification, session.resumed, session.turns, len(got), got)
				continue
			}
			for turn, carried := range got {
				if carried != level.want {
					r.Errorf("case %q: probe(%v, verification=%t, resumed=%t, turns=%d) turn %d carries %q, want %q", level.name, level.passthrough, session.verification, session.resumed, session.turns, turn+1, carried, level.want)
				}
			}
		}
	}

	wrongType := map[string]any{registry.EffortKey: 7}
	_, err := probe(t, maps.Clone(wrongType), false, false, effortWorkingTurns)
	fault, ok := errors.AsType[*typeutil.TypeFault](err)
	if !ok {
		r.Errorf("case %q: probe(%v) error = %v, want a *typeutil.TypeFault", "wrong type", wrongType, err)
		return
	}
	if fault.Key != registry.EffortKey || fault.Want != "string" || fault.Got != "integer" {
		r.Errorf("case %q: probe(%v) fault = {Key:%q Want:%q Got:%q}, want {Key:%q Want:\"string\" Got:\"integer\"}", "wrong type", wrongType, fault.Key, fault.Want, fault.Got, registry.EffortKey)
	}
}
