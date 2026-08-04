package gitlab

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// fastPreflightBackoff replaces the package-level preflightBackoff with
// near-zero delays for the duration of a test so a retry path does not
// sleep for seconds, restoring the original schedule on cleanup.
//
// preflightBackoff is package-global, so no test that calls this (or any
// test that otherwise reads or writes preflightBackoff) may run in parallel
// with another such test: doing so races the shared slice under -race.
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
// length), so none of their subtests call t.Parallel(); they run serially
// by default, avoiding any race on the shared slice.

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

// preflightServer builds an httptest.Server simulating the three preflight
// routes, with independently controllable status codes and atomic call
// counters for the token, project, and label-catalog checks. labelsFlaky,
// when set, makes the labels route fail once with a 500 and succeed with
// an empty catalog on every call after, regardless of labelsStatus.
type preflightServer struct {
	srv           *httptest.Server
	tokenStatus   atomic.Int32
	projectStatus atomic.Int32
	labelsStatus  atomic.Int32
	labelsFlaky   atomic.Bool
	tokenCalls    atomic.Int32
	projectCalls  atomic.Int32
	labelsCalls   atomic.Int32
}

func newRawPreflightServer(t *testing.T) *preflightServer {
	t.Helper()
	ps := &preflightServer{}
	ps.tokenStatus.Store(http.StatusOK)
	ps.projectStatus.Store(http.StatusOK)
	ps.labelsStatus.Store(http.StatusOK)

	mux := http.NewServeMux()
	mux.HandleFunc("/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
		ps.tokenCalls.Add(1)
		status := int(ps.tokenStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write(loadFixture(t, "token_self.json")) //nolint:errcheck // test helper
		}
	})
	mux.HandleFunc("/projects/"+testEscapedProject, func(w http.ResponseWriter, r *http.Request) {
		ps.projectCalls.Add(1)
		status := int(ps.projectStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusNotFound {
			w.Write(loadFixture(t, "error_404_project.json")) //nolint:errcheck // test helper
		}
	})
	mux.HandleFunc("/projects/"+testEscapedProject+"/labels", func(w http.ResponseWriter, r *http.Request) {
		n := ps.labelsCalls.Add(1)
		if ps.labelsFlaky.Load() && n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write(loadFixture(t, "error_500.json")) //nolint:errcheck // test helper
			return
		}
		status := int(ps.labelsStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write([]byte(`[]`)) //nolint:errcheck // test helper
			return
		}
		w.Write(loadFixture(t, "error_500.json")) //nolint:errcheck // test helper
	})

	ps.srv = httptest.NewServer(mux)
	t.Cleanup(ps.srv.Close)
	return ps
}

func TestRunPreflight(t *testing.T) {
	t.Run("succeeds when both routes return 200", func(t *testing.T) {
		ps := newRawPreflightServer(t)
		client := newGitLabClient(ps.srv.URL, "test-token", "sortie/test")

		if _, err := runPreflight(context.Background(), client, testEscapedProject, nil, slog.Default()); err != nil {
			t.Fatalf("runPreflight: %v", err)
		}
		if got := ps.tokenCalls.Load(); got != 1 {
			t.Errorf("token calls = %d, want 1", got)
		}
		if got := ps.projectCalls.Load(); got != 1 {
			t.Errorf("project calls = %d, want 1", got)
		}
	})

	t.Run("introspection failure alone does not block construction", func(t *testing.T) {
		ps := newRawPreflightServer(t)
		ps.tokenStatus.Store(http.StatusInternalServerError)
		client := newGitLabClient(ps.srv.URL, "test-token", "sortie/test")

		if _, err := runPreflight(context.Background(), client, testEscapedProject, nil, slog.Default()); err != nil {
			t.Fatalf("runPreflight: %v (introspection failure must not block a healthy project check)", err)
		}
		if got := ps.projectCalls.Load(); got != 1 {
			t.Errorf("project calls = %d, want 1", got)
		}
	})

	t.Run("401 on project check is a non-retryable auth error", func(t *testing.T) {
		ps := newRawPreflightServer(t)
		ps.projectStatus.Store(http.StatusUnauthorized)
		client := newGitLabClient(ps.srv.URL, "test-token", "sortie/test")

		_, err := runPreflight(context.Background(), client, testEscapedProject, nil, slog.Default())
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)

		if got := ps.projectCalls.Load(); got != 1 {
			t.Errorf("project calls = %d, want 1 (auth error must not retry)", got)
		}
	})

	t.Run("404 on project check is a non-retryable not-found error naming the introspection outcome", func(t *testing.T) {
		ps := newRawPreflightServer(t)
		ps.projectStatus.Store(http.StatusNotFound)
		client := newGitLabClient(ps.srv.URL, "test-token", "sortie/test")

		_, err := runPreflight(context.Background(), client, testEscapedProject, nil, slog.Default())
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)

		if got := ps.projectCalls.Load(); got != 1 {
			t.Errorf("project calls = %d, want 1 (not-found error must not retry)", got)
		}
	})

	t.Run("transient error on project check retries then fails", func(t *testing.T) {
		fastPreflightBackoff(t)

		ps := newRawPreflightServer(t)
		ps.projectStatus.Store(http.StatusInternalServerError)
		client := newGitLabClient(ps.srv.URL, "test-token", "sortie/test")

		_, err := runPreflight(context.Background(), client, testEscapedProject, nil, slog.Default())
		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

		wantCalls := int32(len(preflightBackoff) + 1)
		if got := ps.projectCalls.Load(); got != wantCalls {
			t.Errorf("project calls = %d, want %d (initial + one per backoff entry)", got, wantCalls)
		}
	})

	t.Run("pre-cancelled context fails immediately with no request issued", func(t *testing.T) {
		ps := newRawPreflightServer(t)
		ps.projectStatus.Store(http.StatusInternalServerError)
		client := newGitLabClient(ps.srv.URL, "test-token", "sortie/test")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := runPreflight(ctx, client, testEscapedProject, nil, slog.Default())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runPreflight error = %v, want context.Canceled", err)
		}
		if got := ps.projectCalls.Load(); got != 0 {
			t.Errorf("project calls = %d, want 0 (a pre-cancelled context must not reach the network)", got)
		}
	})

	t.Run("does not log the token", func(t *testing.T) {
		ps := newRawPreflightServer(t)
		ps.tokenStatus.Store(http.StatusInternalServerError)
		client := newGitLabClient(ps.srv.URL, "super-secret-token", "sortie/test")

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		if _, err := runPreflight(context.Background(), client, testEscapedProject, nil, log); err != nil {
			t.Fatalf("runPreflight: %v", err)
		}
		if strings.Contains(buf.String(), "super-secret-token") {
			t.Errorf("runPreflight logged the token\noutput: %s", buf.String())
		}
	})

	t.Run("catalog read failing on every retry fails construction", func(t *testing.T) {
		fastPreflightBackoff(t)

		ps := newRawPreflightServer(t)
		ps.labelsStatus.Store(http.StatusInternalServerError)
		client := newGitLabClient(ps.srv.URL, "super-secret-token", "sortie/test")

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		_, err := runPreflight(context.Background(), client, testEscapedProject, []string{"review", "done"}, log)
		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

		wantCalls := int32(len(preflightBackoff) + 1)
		if got := ps.labelsCalls.Load(); got != wantCalls {
			t.Errorf("labels calls = %d, want %d (initial + one per backoff entry)", got, wantCalls)
		}
		if strings.Contains(buf.String(), "super-secret-token") {
			t.Errorf("runPreflight logged the token\noutput: %s", buf.String())
		}
	})

	t.Run("catalog read succeeding after one transient failure succeeds", func(t *testing.T) {
		fastPreflightBackoff(t)

		ps := newRawPreflightServer(t)
		ps.labelsFlaky.Store(true)
		client := newGitLabClient(ps.srv.URL, "super-secret-token", "sortie/test")

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		if _, err := runPreflight(context.Background(), client, testEscapedProject, []string{"review", "done"}, log); err != nil {
			t.Fatalf("runPreflight: %v (a single transient catalog failure must be absorbed by withRetry)", err)
		}
		if got := ps.labelsCalls.Load(); got != 2 {
			t.Errorf("labels calls = %d, want 2 (one failure then one success, bounded by preflightBackoff)", got)
		}
		if strings.Contains(buf.String(), "super-secret-token") {
			t.Errorf("runPreflight logged the token\noutput: %s", buf.String())
		}
	})

	t.Run("a configured state label absent from the catalog does not fail construction and emits no WARN", func(t *testing.T) {
		ps := newRawPreflightServer(t)
		client := newGitLabClient(ps.srv.URL, "super-secret-token", "sortie/test")

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		casing, err := runPreflight(context.Background(), client, testEscapedProject, []string{"review", "done"}, log)
		if err != nil {
			t.Fatalf("runPreflight: %v", err)
		}
		if len(casing) != 0 {
			t.Errorf("casing = %v, want empty (the empty catalog resolves neither configured label)", casing)
		}
		output := buf.String()
		if strings.Contains(output, "level=WARN") {
			t.Errorf("runPreflight logged a WARN for an absent configured label, want none\noutput: %s", output)
		}
		if !strings.Contains(output, "configured state label absent from project") {
			t.Errorf("log output missing the absent-label Debug message\noutput: %s", output)
		}
		if strings.Contains(output, "super-secret-token") {
			t.Errorf("runPreflight logged the token\noutput: %s", output)
		}
	})
}
