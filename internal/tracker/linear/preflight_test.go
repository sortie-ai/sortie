package linear

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// fastBackoff replaces the preflight backoff with near-zero delays for the
// duration of a test so a retry path does not sleep for seconds.
func fastBackoff(t *testing.T) {
	t.Helper()
	original := preflightBackoff
	preflightBackoff = []time.Duration{time.Microsecond, time.Microsecond, time.Microsecond}
	t.Cleanup(func() { preflightBackoff = original })
}

func TestRunPreflight(t *testing.T) {
	t.Parallel()

	t.Run("builds casing cache mapping lowercase to canonical", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)

		cache, _, err := runPreflight(context.Background(), f, "SOR",
			[]string{"backlog", "TODO"}, []string{"done"}, "in review", nil)
		if err != nil {
			t.Fatalf("runPreflight: %v", err)
		}

		tests := map[string]string{
			"todo":        "Todo",
			"backlog":     "Backlog",
			"in progress": "In Progress",
			"in review":   "In Review",
			"done":        "Done",
		}
		for lower, canonical := range tests {
			if cache[lower] != canonical {
				t.Errorf("casingCache[%q] = %q, want %q", lower, cache[lower], canonical)
			}
		}
	})

	t.Run("unknown team is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("query Me", loadFixture(t, "viewer.json"))
		f.queueBody("query TeamStates", loadFixture(t, "team_states_empty.json"))

		_, _, err := runPreflight(context.Background(), f, "NOPE",
			testActiveStates, testTerminalStates, "", nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("missing configured state is a payload error", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)

		_, _, err := runPreflight(context.Background(), f, "SOR",
			[]string{"Backlog", "Nonexistent"}, testTerminalStates, "", nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("wrong-category state warns without failing", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		seedPreflight(f, t)

		var buf strings.Builder
		log := newTextLogger(&buf)

		cache, _, err := runPreflight(context.Background(), f, "SOR",
			[]string{"Done"}, []string{"Todo"}, "", log)
		if err != nil {
			t.Fatalf("runPreflight should not fail on wrong category: %v", err)
		}
		if cache["done"] != "Done" {
			t.Errorf("casingCache[done] = %q, want %q", cache["done"], "Done")
		}

		output := buf.String()
		if !strings.Contains(output, "active state has terminal category") {
			t.Errorf("expected WARN for active state with terminal category\noutput: %s", output)
		}
		if !strings.Contains(output, "terminal state has non-terminal category") {
			t.Errorf("expected WARN for terminal state with non-terminal category\noutput: %s", output)
		}
	})

	t.Run("config error fails immediately without retry", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("query Me", loadFixture(t, "auth_error.json"))

		_, _, err := runPreflight(context.Background(), f, "SOR",
			testActiveStates, testTerminalStates, "", nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)

		if calls := f.callsFor("query Me"); len(calls) != 1 {
			t.Errorf("viewer call count = %d, want 1 (auth error must not retry)", len(calls))
		}
	})

	t.Run("auth error in body fails construction through the adapter seam", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("query Me", loadFixture(t, "auth_error.json"))

		_, err := newAdapter(f, "SOR", testStates(testActiveStates, ""), nil, nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})
}

// TestRunPreflightRetry covers the retry path, which reads and times against the
// package-level preflightBackoff. These subtests run serially and override the
// backoff so they neither race the global nor sleep for the production delays.
func TestRunPreflightRetry(t *testing.T) {
	fastBackoff(t)

	t.Run("transient error retries then fails", func(t *testing.T) {
		f := newFakeClient()
		transient := fakeResponse{err: &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}}
		wantCalls := len(preflightBackoff) + 1
		for range wantCalls {
			f.queueResponse("query Me", transient)
		}

		_, _, err := runPreflight(context.Background(), f, "SOR",
			testActiveStates, testTerminalStates, "", nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

		if calls := f.callsFor("query Me"); len(calls) != wantCalls {
			t.Errorf("viewer call count = %d, want %d (initial + retries)", len(calls), wantCalls)
		}
	})

	t.Run("transient error then success recovers", func(t *testing.T) {
		f := newFakeClient()
		f.queueResponse("query Me", fakeResponse{err: &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "rate"}})
		f.queueBody("query Me", loadFixture(t, "viewer.json"))
		f.queueBody("query TeamStates", loadFixture(t, "team_states.json"))

		_, _, err := runPreflight(context.Background(), f, "SOR",
			testActiveStates, testTerminalStates, "", nil)
		if err != nil {
			t.Fatalf("runPreflight should recover after a transient failure: %v", err)
		}

		if calls := f.callsFor("query Me"); len(calls) != 2 {
			t.Errorf("viewer call count = %d, want 2 (one retry then success)", len(calls))
		}
	})
}

func TestNewAdapterProjectURL(t *testing.T) {
	t.Parallel()

	const organization = `"organization": { "urlKey": "acme" },`
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"workspace url key names the team page", loadFixture(t, "team_states.json"), "https://linear.app/acme/team/SOR"},
		{"response without a workspace gives no page", bytes.ReplaceAll(loadFixture(t, "team_states.json"), []byte(organization), nil), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeClient()
			f.queueBody("query Me", loadFixture(t, "viewer.json"))
			f.queueBody("query TeamStates", tt.body)

			adapter, err := newAdapter(f, "SOR", domain.TrackerStates{}, nil, nil)
			if err != nil {
				t.Fatalf("newAdapter: %v", err)
			}

			if got := adapter.(domain.ProjectLinker).ProjectURL(); got != tt.want {
				t.Errorf("ProjectURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
