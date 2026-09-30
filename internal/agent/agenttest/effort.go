package agenttest

import (
	"errors"
	"maps"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// EffortProbe starts a session of the kind under test from passthrough
// through the kind's own constructor and invocation builder, as a
// credential-verification session when verification is true, and returns
// the level each of its first turns invocations carries ("" for none),
// one entry per turn. A construction failure is returned as the error.
//
// A probe reads each entry off the invocation the adapter itself built for
// that turn, never off passthrough.
type EffortProbe func(t *testing.T, passthrough map[string]any, verification bool, turns int) ([]string, error)

const (
	effortProbeLevel        = "Sortie Effort-Probe.7"
	effortWorkingTurns      = 2
	effortVerificationTurns = 1
)

// AssertEffortForwarding runs the shared effort cases against probe: an
// absent and an empty level reach no invocation, a set level reaches every
// turn of a working and of a credential-verification session byte for
// byte, and a non-string level fails construction with a type fault on
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
		turns        int
	}{
		{verification: false, turns: effortWorkingTurns},
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
			got, err := probe(t, maps.Clone(level.passthrough), session.verification, session.turns)
			if err != nil {
				r.Errorf("case %q: probe(%v, verification=%t, turns=%d) error = %v, want nil", level.name, level.passthrough, session.verification, session.turns, err)
				continue
			}
			if len(got) != session.turns {
				r.Errorf("case %q: probe(%v, verification=%t, turns=%d) returned %d entries = %q, want one per turn", level.name, level.passthrough, session.verification, session.turns, len(got), got)
				continue
			}
			for turn, carried := range got {
				if carried != level.want {
					r.Errorf("case %q: probe(%v, verification=%t, turns=%d) turn %d carries %q, want %q", level.name, level.passthrough, session.verification, session.turns, turn+1, carried, level.want)
				}
			}
		}
	}

	wrongType := map[string]any{registry.EffortKey: 7}
	_, err := probe(t, maps.Clone(wrongType), false, effortWorkingTurns)
	fault, ok := errors.AsType[*typeutil.TypeFault](err)
	if !ok {
		r.Errorf("case %q: probe(%v) error = %v, want a *typeutil.TypeFault", "wrong type", wrongType, err)
		return
	}
	if fault.Key != registry.EffortKey || fault.Want != "string" || fault.Got != "integer" {
		r.Errorf("case %q: probe(%v) fault = {Key:%q Want:%q Got:%q}, want {Key:%q Want:\"string\" Got:\"integer\"}", "wrong type", wrongType, fault.Key, fault.Want, fault.Got, registry.EffortKey)
	}
}
