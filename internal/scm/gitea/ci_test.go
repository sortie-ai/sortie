package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// --- helpers ---

// mustCIProvider constructs a *GiteaCIProvider against endpoint with a
// throwaway token and maxLogLines, or fails the test.
func mustCIProvider(t *testing.T, endpoint string, maxLogLines int) *GiteaCIProvider {
	t.Helper()
	p, err := NewGiteaCIProvider(maxLogLines, map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
		"project":  testOwner + "/" + testRepo,
	})
	if err != nil {
		t.Fatalf("NewGiteaCIProvider: %v", err)
	}
	return p.(*GiteaCIProvider)
}

func assertCIErrorKind(t *testing.T, err error, want domain.CIErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected CIError with kind %q, got nil", want)
	}
	var ce *domain.CIError
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want *domain.CIError", err)
	}
	if ce.Kind != want {
		t.Errorf("CIError.Kind = %q, want %q", ce.Kind, want)
	}
}

// buildStatusPage returns the JSON body of a combined-status page carrying n
// statuses of the given status value, with contexts numbered from start, so a
// multi-page test can synthesize pages whose contexts stay distinct across the
// whole walk without committing a fixture of that size. Distinct contexts match
// the route's wire shape, which reports one entry per context. total_count is
// set to n, matching the wire fact that the per-page count equals the page
// length rather than the grand total.
func buildStatusPage(t *testing.T, status string, start, n int) []byte {
	t.Helper()
	statuses := make([]map[string]string, n)
	for i := range n {
		statuses[i] = map[string]string{
			"status":  status,
			"context": fmt.Sprintf("ci/check-%d", start+i),
		}
	}
	body, err := json.Marshal(map[string]any{
		"state":       status,
		"total_count": n,
		"statuses":    statuses,
	})
	if err != nil {
		t.Fatalf("marshal status page: %v", err)
	}
	return body
}

// --- status and conclusion mapping ---

func TestGiteaMapRunStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  domain.CheckRunStatus
	}{
		{"success completes", "success", domain.CheckRunStatusCompleted},
		{"failure completes", "failure", domain.CheckRunStatusCompleted},
		{"error completes", "error", domain.CheckRunStatusCompleted},
		{"warning completes", "warning", domain.CheckRunStatusCompleted},
		{"pending is in progress", "pending", domain.CheckRunStatusInProgress},
		{"unknown value defers to in progress", "unrecognized", domain.CheckRunStatusInProgress},
		{"empty value defers to in progress", "", domain.CheckRunStatusInProgress},
		{"uppercase input matches the lowercased case", "SUCCESS", domain.CheckRunStatusCompleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mapRunStatus(tt.input)

			if got != tt.want {
				t.Errorf("mapRunStatus(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGiteaMapConclusion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  domain.CheckConclusion
	}{
		{"success", "success", domain.CheckConclusionSuccess},
		{"failure", "failure", domain.CheckConclusionFailure},
		{"error folds to failure", "error", domain.CheckConclusionFailure},
		{"warning folds to neutral", "warning", domain.CheckConclusionNeutral},
		{"pending", "pending", domain.CheckConclusionPending},
		{"unknown value defers to pending", "unrecognized", domain.CheckConclusionPending},
		{"empty value defers to pending", "", domain.CheckConclusionPending},
		{"uppercase input matches the lowercased case", "FAILURE", domain.CheckConclusionFailure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mapConclusion(tt.input)

			if got != tt.want {
				t.Errorf("mapConclusion(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- FetchCIStatus normalization ---

func TestGiteaFetchCIStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		fixture          string
		wantStatus       domain.CIStatus
		wantFailingCount int
		wantCheckRunLen  int
	}{
		{
			name:             "all success statuses yield passing",
			fixture:          "status_all_success.json",
			wantStatus:       domain.CIStatusPassing,
			wantFailingCount: 0,
			wantCheckRunLen:  2,
		},
		{
			name:             "a failure among successes yields failing",
			fixture:          "status_has_failure.json",
			wantStatus:       domain.CIStatusFailing,
			wantFailingCount: 1,
			wantCheckRunLen:  2,
		},
		{
			name:             "no statuses yield pending from the empty slice",
			fixture:          "status_no_checks.json",
			wantStatus:       domain.CIStatusPending,
			wantFailingCount: 0,
			wantCheckRunLen:  0,
		},
		{
			name:             "a pending status with no failure yields pending",
			fixture:          "status_has_pending.json",
			wantStatus:       domain.CIStatusPending,
			wantFailingCount: 0,
			wantCheckRunLen:  2,
		},
		{
			name:             "a warning among successes yields passing",
			fixture:          "status_warning_only.json",
			wantStatus:       domain.CIStatusPassing,
			wantFailingCount: 0,
			wantCheckRunLen:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := loadFixture(t, tt.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(fixture)
			}))
			defer srv.Close()
			provider := mustCIProvider(t, srv.URL, 50)

			got, err := provider.FetchCIStatus(context.Background(), "main")

			if err != nil {
				t.Fatalf("FetchCIStatus: unexpected error: %v", err)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.FailingCount != tt.wantFailingCount {
				t.Errorf("FetchCIStatus().FailingCount = %d, want %d", got.FailingCount, tt.wantFailingCount)
			}
			if got.CheckRuns == nil {
				t.Error("FetchCIStatus().CheckRuns = nil, want non-nil")
			}
			if len(got.CheckRuns) != tt.wantCheckRunLen {
				t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want %d", len(got.CheckRuns), tt.wantCheckRunLen)
			}
			if tt.wantStatus != domain.CIStatusFailing && got.LogExcerpt != "" {
				t.Errorf("FetchCIStatus().LogExcerpt = %q, want empty for a non-failing status", got.LogExcerpt)
			}
			if got.Ref != "main" {
				t.Errorf("FetchCIStatus().Ref = %q, want %q", got.Ref, "main")
			}
		})
	}

	t.Run("check run Name and DetailsURL map from context and target_url", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "status_all_success.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 50)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		wantNames := []string{"ci/build", "ci/test"}
		if len(got.CheckRuns) != len(wantNames) {
			t.Fatalf("len(FetchCIStatus().CheckRuns) = %d, want %d", len(got.CheckRuns), len(wantNames))
		}
		for i, run := range got.CheckRuns {
			if run.Name != wantNames[i] {
				t.Errorf("CheckRuns[%d].Name = %q, want %q", i, run.Name, wantNames[i])
			}
			if run.DetailsURL != "" {
				t.Errorf("CheckRuns[%d].DetailsURL = %q, want empty (fixture carries no target_url)", i, run.DetailsURL)
			}
		}
	})
}

// TestGiteaFetchCIStatus_MixedRunsMatchesCore pins the aggregate contract
// against a run set carrying a completed-failing, an in-progress, and a
// completed-success run together, so the verdict this provider reports is
// provably the same rule scmcore.AggregateCIStatus implements.
func TestGiteaFetchCIStatus_MixedRunsMatchesCore(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "status_mixed.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()
	provider := mustCIProvider(t, srv.URL, 50)

	got, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if len(got.CheckRuns) != 3 {
		t.Fatalf("len(CheckRuns) = %d, want 3", len(got.CheckRuns))
	}
	adaptertest.AssertCIAggregateMatchesCore(t, got)
}

// --- FetchCIStatus pagination ---

func TestGiteaFetchCIStatusPagination(t *testing.T) {
	t.Parallel()

	t.Run("a failing status on page two flips the aggregate to failing", func(t *testing.T) {
		t.Parallel()

		page1 := buildStatusPage(t, "success", 0, 50)
		page2 := buildStatusPage(t, "failure", 50, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			switch r.URL.Query().Get("page") {
			case "2":
				_, _ = w.Write(page2)
			default:
				_, _ = w.Write(page1)
			}
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusFailing {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
		}
		if got.FailingCount < 1 {
			t.Errorf("FetchCIStatus().FailingCount = %d, want >= 1", got.FailingCount)
		}
		const wantLen = 51
		if len(got.CheckRuns) != wantLen {
			t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want %d", len(got.CheckRuns), wantLen)
		}
	})

	t.Run("a single short page issues exactly one combined status request", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "status_all_success.json")
		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusPassing {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusPassing)
		}
		if len(got.CheckRuns) != 2 {
			t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want %d", len(got.CheckRuns), 2)
		}
		if n := requestCount.Load(); n != 1 {
			t.Errorf("combined status request count = %d, want 1", n)
		}
	})
}

// --- log excerpt ---

func TestGiteaStripANSI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text is unchanged", "unit tests failed", "unit tests failed"},
		{"CSI color sequence is stripped", "\x1b[0;31mFAIL\x1b[0m", "FAIL"},
		{"OSC sequence terminated by BEL is stripped", "\x1b]0;title\atext", "text"},
		{"OSC sequence terminated by ST is stripped", "\x1b]0;title\x1b\\text", "text"},
		{"empty string is unchanged", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripANSI(tt.input)

			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGiteaTruncateLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		maxLines int
		want     string
	}{
		{
			name:     "zero maxLines yields empty",
			input:    "a\nb\nc",
			maxLines: 0,
			want:     "",
		},
		{
			name:     "negative maxLines yields empty",
			input:    "a\nb\nc",
			maxLines: -1,
			want:     "",
		},
		{
			name:     "empty input yields empty",
			input:    "",
			maxLines: 5,
			want:     "",
		},
		{
			name:     "input at or below maxLines is unchanged",
			input:    "a\nb",
			maxLines: 5,
			want:     "a\nb",
		},
		{
			name:     "input above maxLines keeps the tail",
			input:    "a\nb\nc\nd",
			maxLines: 2,
			want:     "c\nd",
		},
		{
			name:     "maxLines of one keeps only the last line",
			input:    "unit tests failed\nhttps://ci.example.invalid/builds/10",
			maxLines: 1,
			want:     "https://ci.example.invalid/builds/10",
		},
		{
			name:     "CRLF line endings have their trailing carriage return trimmed",
			input:    "a\r\nb\r\nc",
			maxLines: 5,
			want:     "a\nb\nc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateLines(tt.input, tt.maxLines)

			if got != tt.want {
				t.Errorf("truncateLines(%q, %d) = %q, want %q", tt.input, tt.maxLines, got, tt.want)
			}
		})
	}
}

func TestGiteaCILogExcerpt(t *testing.T) {
	t.Parallel()

	t.Run("description and target_url from the first failing entry join into a non-empty excerpt", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "ci_status_failing_target.json")
		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 10)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusFailing {
			t.Fatalf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
		}
		if !strings.Contains(got.LogExcerpt, "unit tests failed") {
			t.Errorf("LogExcerpt = %q, want it to contain the first failing entry's description", got.LogExcerpt)
		}
		if !strings.Contains(got.LogExcerpt, "https://ci.example.invalid/builds/10") {
			t.Errorf("LogExcerpt = %q, want it to contain the first failing entry's target_url", got.LogExcerpt)
		}
		if strings.Contains(got.LogExcerpt, "lint failed") {
			t.Errorf("LogExcerpt = %q, want only the first failing entry, not a later one", got.LogExcerpt)
		}
		if n := requestCount.Load(); n != 1 {
			t.Errorf("combined status endpoint received %d requests, want 1 (no network request for target_url)", n)
		}
	})

	t.Run("maxLogLines zero disables the excerpt", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "ci_status_failing_target.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty when maxLogLines is zero", got.LogExcerpt)
		}
	})

	t.Run("a failing entry with empty description and empty target_url yields an empty excerpt", func(t *testing.T) {
		t.Parallel()

		const body = `{"state":"failure","total_count":1,"statuses":[{"id":20,"status":"failure","context":"ci/test"}]}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 10)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusFailing {
			t.Fatalf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
		}
		if got.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty when the failing entry has no description or target_url", got.LogExcerpt)
		}
	})

	t.Run("a description-only failing entry still yields a non-empty excerpt", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "status_has_failure.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 50)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		const want = "Tests failed"
		if got.LogExcerpt != want {
			t.Errorf("LogExcerpt = %q, want %q", got.LogExcerpt, want)
		}
	})

	t.Run("a passing status yields an empty excerpt regardless of maxLogLines", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "status_all_success.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 50)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty for a passing status", got.LogExcerpt)
		}
	})
}

// --- error mapping ---

func TestGiteaToCIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		fixture    string
		wantKind   domain.CIErrorKind
	}{
		{
			name:       "401 unauthorized maps to ErrCIAuth",
			statusCode: http.StatusUnauthorized,
			fixture:    "error_401.json",
			wantKind:   domain.ErrCIAuth,
		},
		{
			name:       "404 not found maps to ErrCINotFound",
			statusCode: http.StatusNotFound,
			fixture:    "error_404.json",
			wantKind:   domain.ErrCINotFound,
		},
		{
			name:       "5xx server error maps to ErrCITransport",
			statusCode: http.StatusInternalServerError,
			fixture:    "error_401.json",
			wantKind:   domain.ErrCITransport,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := loadFixture(t, tt.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write(fixture)
			}))
			defer srv.Close()
			provider := mustCIProvider(t, srv.URL, 0)

			_, err := provider.FetchCIStatus(context.Background(), "main")

			assertCIErrorKind(t, err, tt.wantKind)
		})
	}

	t.Run("malformed 2xx body maps to ErrCIPayload", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{not valid json"))
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		_, err := provider.FetchCIStatus(context.Background(), "main")

		assertCIErrorKind(t, err, domain.ErrCIPayload)
	})

	t.Run("a cancelled context is returned as-is, not converted to a CIError", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"success","total_count":0,"statuses":[]}`))
		}))
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := provider.FetchCIStatus(ctx, "main")

		if !errors.Is(err, context.Canceled) {
			t.Errorf("FetchCIStatus(cancelled context) = %v, want context.Canceled in the chain", err)
		}
		if ciErr, ok := errors.AsType[*domain.CIError](err); ok {
			t.Errorf("FetchCIStatus(cancelled context) = %v, want the context error without CIError conversion", ciErr)
		}
	})
}

// --- registration, request shape, and construction ---

func TestGiteaCIRegistration(t *testing.T) {
	t.Parallel()

	if !registry.CIProviders.Has("gitea") {
		t.Fatal(`CIProviders.Has("gitea") = false, want true`)
	}

	constructor, err := registry.CIProviders.Get("gitea")
	if err != nil {
		t.Fatalf(`CIProviders.Get("gitea") = %v, want registered constructor`, err)
	}

	provider, err := constructor(0, map[string]any{
		"api_key":  "test-token",
		"project":  testOwner + "/" + testRepo,
		"endpoint": "http://gitea.invalid",
	})

	if err != nil {
		t.Fatalf("registered gitea CI constructor(...) = %v, want nil error", err)
	}
	if _, ok := provider.(*GiteaCIProvider); !ok {
		t.Errorf("registered gitea CI constructor(...) = %T, want *GiteaCIProvider", provider)
	}
	if !registry.Trackers.Has("gitea") {
		t.Error(`Trackers.Has("gitea") = false, want true (CI registration must not disturb the tracker registration)`)
	}
	if !registry.SCMAdapters.Has("gitea") {
		t.Error(`SCMAdapters.Has("gitea") = false, want true (CI registration must not disturb the SCM registration)`)
	}
}

func TestGiteaCIRequestShape(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "status_no_checks.json")
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()
	const ref = "main"
	provider := mustCIProvider(t, srv.URL, 0)

	_, err := provider.FetchCIStatus(context.Background(), ref)

	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	wantPath := fmt.Sprintf("/api/v1/repos/%s/%s/commits/%s/status", testOwner, testRepo, ref)
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if strings.Contains(gotQuery, "access_token") {
		t.Errorf("request query = %q, want no access_token query parameter", gotQuery)
	}
	const wantAuth = "token test-token"
	if gotAuth != wantAuth {
		t.Errorf("Authorization header = %q, want %q", gotAuth, wantAuth)
	}
}

func TestNewGiteaCIProvider(t *testing.T) {
	t.Parallel()

	t.Run("missing api_key returns ErrCIAuth", func(t *testing.T) {
		t.Parallel()

		_, err := NewGiteaCIProvider(0, map[string]any{
			"project":  testOwner + "/" + testRepo,
			"endpoint": "http://gitea.invalid",
		})

		assertCIErrorKind(t, err, domain.ErrCIAuth)
	})

	t.Run("missing project returns ErrCIPayload", func(t *testing.T) {
		t.Parallel()

		_, err := NewGiteaCIProvider(0, map[string]any{
			"api_key":  "test-token",
			"endpoint": "http://gitea.invalid",
		})

		assertCIErrorKind(t, err, domain.ErrCIPayload)
	})

	t.Run("malformed project returns ErrCIPayload", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			project string
		}{
			{"no slash", "noslash"},
			{"empty owner", "/repo"},
			{"empty repo", "owner/"},
			{"too many slashes", "owner/repo/extra"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				_, err := NewGiteaCIProvider(0, map[string]any{
					"api_key":  "test-token",
					"project":  tt.project,
					"endpoint": "http://gitea.invalid",
				})

				assertCIErrorKind(t, err, domain.ErrCIPayload)
			})
		}
	})

	t.Run("missing endpoint returns ErrCIPayload", func(t *testing.T) {
		t.Parallel()

		_, err := NewGiteaCIProvider(0, map[string]any{
			"api_key": "test-token",
			"project": testOwner + "/" + testRepo,
		})

		assertCIErrorKind(t, err, domain.ErrCIPayload)
	})

	t.Run("endpoint already ending in /api/v1 is not double-suffixed", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "status_no_checks.json")
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		provider, err := NewGiteaCIProvider(0, map[string]any{
			"api_key":  "test-token",
			"project":  testOwner + "/" + testRepo,
			"endpoint": srv.URL + "/api/v1",
		})
		if err != nil {
			t.Fatalf("NewGiteaCIProvider: %v", err)
		}

		_, err = provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		wantPath := fmt.Sprintf("/api/v1/repos/%s/%s/commits/main/status", testOwner, testRepo)
		if gotPath != wantPath {
			t.Errorf("request path = %q, want %q (endpoint already suffixed with /api/v1 must not be doubled)", gotPath, wantPath)
		}
	})
}

// TestNewGiteaCIProvider_RejectsInvalidEndpoint proves the endpoint values
// in endpointRejectionCases fail construction with ErrCIPayload; none of
// the config maps here register a server, since construction performs no
// network I/O.
func TestNewGiteaCIProvider_RejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()

	for _, tt := range endpointRejectionCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := NewGiteaCIProvider(0, map[string]any{
				"api_key":  "test-token",
				"project":  testOwner + "/" + testRepo,
				"endpoint": tt.endpoint,
			})

			assertCIErrorKind(t, err, domain.ErrCIPayload)
			if p != nil {
				t.Errorf("NewGiteaCIProvider(endpoint=%q) provider = %v, want nil", tt.endpoint, p)
			}
		})
	}
}

// TestNewGiteaCIProvider_RedactsUserinfoInEndpointError proves the
// credential disclosure this fix closes stays closed for the CI provider,
// which previously accepted a userinfo-bearing endpoint silently.
func TestNewGiteaCIProvider_RedactsUserinfoInEndpointError(t *testing.T) {
	t.Parallel()

	const endpoint = "https://operator:s3cr3t@fd00::1:3000"

	_, err := NewGiteaCIProvider(0, map[string]any{
		"api_key":  "test-token",
		"project":  testOwner + "/" + testRepo,
		"endpoint": endpoint,
	})

	var ce *domain.CIError
	if !errors.As(err, &ce) {
		t.Fatalf("NewGiteaCIProvider(endpoint=%q) error type = %T, want *domain.CIError", endpoint, err)
	}
	if ce.Kind != domain.ErrCIPayload {
		t.Errorf("NewGiteaCIProvider(endpoint=%q) CIError.Kind = %q, want %q", endpoint, ce.Kind, domain.ErrCIPayload)
	}
	if !strings.Contains(ce.Message, "fd00::1:3000") {
		t.Errorf("NewGiteaCIProvider(endpoint=%q) message = %q, want it to contain the redacted host %q", endpoint, ce.Message, "fd00::1:3000")
	}
	if strings.Contains(ce.Message, "operator") || strings.Contains(ce.Message, "s3cr3t") {
		t.Errorf("NewGiteaCIProvider(endpoint=%q) message = %q, must not leak userinfo credentials", endpoint, ce.Message)
	}
}

// TestNewGiteaCIProvider_EmptyEndpointMessageIsPinned pins the pre-existing
// empty-endpoint message: the shared httpkit.ParseEndpoint guard must not
// have changed this operator-facing text.
func TestNewGiteaCIProvider_EmptyEndpointMessageIsPinned(t *testing.T) {
	t.Parallel()

	_, err := NewGiteaCIProvider(0, map[string]any{
		"api_key": "test-token",
		"project": testOwner + "/" + testRepo,
	})

	var ce *domain.CIError
	if !errors.As(err, &ce) {
		t.Fatalf("NewGiteaCIProvider(missing endpoint) error type = %T, want *domain.CIError", err)
	}
	if ce.Kind != domain.ErrCIPayload {
		t.Errorf("NewGiteaCIProvider(missing endpoint) CIError.Kind = %q, want %q", ce.Kind, domain.ErrCIPayload)
	}
	const want = "missing required config key: endpoint"
	if ce.Message != want {
		t.Errorf("NewGiteaCIProvider(missing endpoint) message = %q, want %q", ce.Message, want)
	}
}
