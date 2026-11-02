package agenttest

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// ContinuityStart starts a session of the kind under test through its own
// adapter, against a runtime that holds the session held names and no other,
// asking it to resume resumeID ("" for a fresh start). It stops the session at
// test cleanup and returns a StartSession failure.
type ContinuityStart func(t *testing.T, held, resumeID string) (domain.Session, error)

// AssertSessionContinuity fails t when a fresh start reports carried history,
// when resuming the held session does not continue it under its own id, or
// when resuming an unheld session reports carried history or keeps its id.
func AssertSessionContinuity(t *testing.T, start ContinuityStart) {
	t.Helper()

	const (
		held   = "4f9c1d7a-2b3e-4a60-9c8d-1e2f3a4b5c6d"
		unheld = "8a1b2c3d-4e5f-4a7b-8c9d-0e1f2a3b4c5d"
	)

	fresh, err := start(t, held, "")
	if err != nil {
		t.Fatalf("start(held=%q, resume=%q) error = %v", held, "", err)
	}
	if fresh.CarriedHistory {
		t.Errorf("start(held=%q, resume=%q).CarriedHistory = true, want false", held, "")
	}

	resumed, err := start(t, held, held)
	if err != nil {
		t.Fatalf("start(held=%q, resume=%q) error = %v", held, held, err)
	}
	if !resumed.CarriedHistory || resumed.ID != held {
		t.Errorf("start(held=%q, resume=%q) = {CarriedHistory: %v, ID: %q}, want {true, %q}", held, held, resumed.CarriedHistory, resumed.ID, held)
	}

	replaced, err := start(t, held, unheld)
	if err != nil {
		t.Fatalf("start(held=%q, resume=%q) error = %v", held, unheld, err)
	}
	if replaced.CarriedHistory || replaced.ID == unheld {
		t.Errorf("start(held=%q, resume=%q) = {CarriedHistory: %v, ID: %q}, want {false, an ID other than %q}", held, unheld, replaced.CarriedHistory, replaced.ID, unheld)
	}
}
