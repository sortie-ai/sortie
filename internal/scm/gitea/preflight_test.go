package gitea

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// fastPreflightBackoff replaces the package-level preflightBackoff with
// near-zero delays for the duration of a test so a retry path does not sleep
// for seconds, restoring the original schedule on cleanup.
//
// preflightBackoff is package-global, so no test that calls this (or any test
// that otherwise reads or writes preflightBackoff) may run in parallel with
// another such test: doing so races the shared slice under -race.
func fastPreflightBackoff(t *testing.T) {
	t.Helper()
	original := preflightBackoff
	preflightBackoff = []time.Duration{time.Microsecond, time.Microsecond, time.Microsecond}
	t.Cleanup(func() { preflightBackoff = original })
}

func TestSleepContext(t *testing.T) {
	t.Parallel()

	t.Run("returns nil once the delay elapses", func(t *testing.T) {
		t.Parallel()

		if err := sleepContext(context.Background(), time.Microsecond); err != nil {
			t.Fatalf("sleepContext(_, 1us) = %v, want nil", err)
		}
	})

	t.Run("returns ctx.Err on cancellation without waiting the delay", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := sleepContext(ctx, time.Hour)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("sleepContext(cancelled, 1h) = %v, want context.Canceled", err)
		}
	})
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"transport error is retryable", &domain.TrackerError{Kind: domain.ErrTrackerTransport}, true},
		{"api error is retryable", &domain.TrackerError{Kind: domain.ErrTrackerAPI}, true},
		{"auth error is not retryable", &domain.TrackerError{Kind: domain.ErrTrackerAuth}, false},
		{"not found error is not retryable", &domain.TrackerError{Kind: domain.ErrTrackerNotFound}, false},
		{"payload error is not retryable", &domain.TrackerError{Kind: domain.ErrTrackerPayload}, false},
		{"non-tracker error is not retryable", errors.New("boom"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isRetryable(tt.err)
			if got != tt.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestWithRetry and TestRunPreflight below all touch the package-level
// preflightBackoff (directly or through withRetry's internal read of its
// length), so none of their subtests call t.Parallel(); they run serially by
// default, avoiding any race on the shared slice.

func TestWithRetry(t *testing.T) {
	t.Run("succeeds on first try", func(t *testing.T) {
		var calls int
		err := withRetry(context.Background(), func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("withRetry: %v", err)
		}
		if calls != 1 {
			t.Errorf("call count = %d, want 1", calls)
		}
	})

	t.Run("non-retryable error returns immediately without retry", func(t *testing.T) {
		var calls int
		err := withRetry(context.Background(), func() error {
			calls++
			return &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "bad token"}
		})
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
		if calls != 1 {
			t.Errorf("call count = %d, want 1 (non-retryable must not retry)", calls)
		}
	})

	t.Run("retryable error retries the full schedule then fails", func(t *testing.T) {
		fastPreflightBackoff(t)

		var calls int
		err := withRetry(context.Background(), func() error {
			calls++
			return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}
		})
		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

		wantCalls := len(preflightBackoff) + 1
		if calls != wantCalls {
			t.Errorf("call count = %d, want %d (initial + one per backoff entry)", calls, wantCalls)
		}
	})

	t.Run("retryable error then success recovers", func(t *testing.T) {
		fastPreflightBackoff(t)

		var calls int
		err := withRetry(context.Background(), func() error {
			calls++
			if calls < 2 {
				return &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "rate limited"}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("withRetry should recover after one transient failure: %v", err)
		}
		if calls != 2 {
			t.Errorf("call count = %d, want 2 (one retry then success)", calls)
		}
	})

	t.Run("cancellation during backoff returns ctx.Err without waiting the schedule", func(t *testing.T) {
		// Uses the production backoff schedule deliberately: the context is
		// already cancelled before the call, so sleepContext returns
		// immediately regardless of the configured delay.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var calls int
		err := withRetry(ctx, func() error {
			calls++
			return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("withRetry error = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("call count = %d, want 1 (cancellation must stop before a retry)", calls)
		}
	})
}

// preflightServer builds an httptest.Server simulating the two preflight
// routes, with independently controllable status codes and atomic call
// counters for the user and repo checks.
//
// userSucceedFromCall, when set to a positive N, makes the user route ignore
// userStatus and return 200 starting with the Nth call, so a transient-then-
// recovers scenario is deterministic: no goroutine or timing race is needed
// to "catch" the request between a failing attempt and a healthy retry.
type preflightServer struct {
	srv                 *httptest.Server
	userStatus          atomic.Int32
	repoStatus          atomic.Int32
	userCalls           atomic.Int32
	repoCalls           atomic.Int32
	userSucceedFromCall atomic.Int32
}

func newPreflightServer(t *testing.T) *preflightServer {
	t.Helper()
	ps := &preflightServer{}
	ps.userStatus.Store(http.StatusOK)
	ps.repoStatus.Store(http.StatusOK)

	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		n := ps.userCalls.Add(1)
		if threshold := ps.userSucceedFromCall.Load(); threshold > 0 && n >= threshold {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "user.json")) //nolint:errcheck // test helper
			return
		}
		status := int(ps.userStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write(loadFixture(t, "user.json")) //nolint:errcheck // test helper
		}
	})
	mux.HandleFunc("/repos/"+testOwner+"/"+testRepo, func(w http.ResponseWriter, r *http.Request) {
		ps.repoCalls.Add(1)
		w.WriteHeader(int(ps.repoStatus.Load()))
	})

	ps.srv = httptest.NewServer(mux)
	t.Cleanup(ps.srv.Close)
	return ps
}

func TestRunPreflight(t *testing.T) {
	t.Run("succeeds when both routes return 200", func(t *testing.T) {
		ps := newPreflightServer(t)
		client := newGiteaClient(ps.srv.URL, "test-token", "sortie/test")

		if err := runPreflight(context.Background(), client, testOwner, testRepo); err != nil {
			t.Fatalf("runPreflight: %v", err)
		}
		if got := ps.userCalls.Load(); got != 1 {
			t.Errorf("user calls = %d, want 1", got)
		}
		if got := ps.repoCalls.Load(); got != 1 {
			t.Errorf("repo calls = %d, want 1", got)
		}
	})

	t.Run("401 on user check is a non-retryable auth error", func(t *testing.T) {
		ps := newPreflightServer(t)
		ps.userStatus.Store(http.StatusUnauthorized)
		client := newGiteaClient(ps.srv.URL, "test-token", "sortie/test")

		err := runPreflight(context.Background(), client, testOwner, testRepo)
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)

		if got := ps.userCalls.Load(); got != 1 {
			t.Errorf("user calls = %d, want 1 (auth error must not retry)", got)
		}
		if got := ps.repoCalls.Load(); got != 0 {
			t.Errorf("repo calls = %d, want 0 (repo check must not run after a failed user check)", got)
		}
	})

	t.Run("404 on repo check is a non-retryable not-found error", func(t *testing.T) {
		ps := newPreflightServer(t)
		ps.repoStatus.Store(http.StatusNotFound)
		client := newGiteaClient(ps.srv.URL, "test-token", "sortie/test")

		err := runPreflight(context.Background(), client, testOwner, testRepo)
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)

		if got := ps.userCalls.Load(); got != 1 {
			t.Errorf("user calls = %d, want 1", got)
		}
		if got := ps.repoCalls.Load(); got != 1 {
			t.Errorf("repo calls = %d, want 1 (not-found error must not retry)", got)
		}
	})

	t.Run("transient error on user check retries then fails", func(t *testing.T) {
		fastPreflightBackoff(t)

		ps := newPreflightServer(t)
		ps.userStatus.Store(http.StatusInternalServerError)
		client := newGiteaClient(ps.srv.URL, "test-token", "sortie/test")

		err := runPreflight(context.Background(), client, testOwner, testRepo)
		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

		wantCalls := int32(len(preflightBackoff) + 1)
		if got := ps.userCalls.Load(); got != wantCalls {
			t.Errorf("user calls = %d, want %d (initial + one per backoff entry)", got, wantCalls)
		}
	})

	t.Run("transient error then success recovers", func(t *testing.T) {
		fastPreflightBackoff(t)

		ps := newPreflightServer(t)
		ps.userStatus.Store(http.StatusServiceUnavailable)
		ps.userSucceedFromCall.Store(2) // fails call 1, succeeds from call 2 onward
		client := newGiteaClient(ps.srv.URL, "test-token", "sortie/test")

		if err := runPreflight(context.Background(), client, testOwner, testRepo); err != nil {
			t.Fatalf("runPreflight should recover after a transient failure: %v", err)
		}
		if got := ps.userCalls.Load(); got != 2 {
			t.Errorf("user calls = %d, want 2 (one retry then success)", got)
		}
	})

	t.Run("pre-cancelled context fails immediately with no request issued", func(t *testing.T) {
		// A context cancelled before runPreflight is even called fails at the
		// underlying http.Client.Do call itself, before any request reaches
		// the server; cancellation mid-backoff (between a real attempt and
		// its retry) is covered at the withRetry level in TestWithRetry.
		ps := newPreflightServer(t)
		ps.userStatus.Store(http.StatusInternalServerError)
		client := newGiteaClient(ps.srv.URL, "test-token", "sortie/test")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := runPreflight(ctx, client, testOwner, testRepo)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runPreflight error = %v, want context.Canceled", err)
		}
		if got := ps.userCalls.Load(); got != 0 {
			t.Errorf("user calls = %d, want 0 (a pre-cancelled context must not reach the network)", got)
		}
	})
}
