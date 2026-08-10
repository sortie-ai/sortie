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

// TestRunPreflight below touches the package-level preflightBackoff
// (through [httpkit.RetryWithBackoff]'s internal read of its length), so
// none of its subtests call t.Parallel(); they run serially by default,
// avoiding any race on the shared slice.

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
