package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func newTestCIProvider(t *testing.T, baseURL string, maxLogLines int) *GitHubCIProvider {
	t.Helper()
	p, err := NewGitHubCIProvider(maxLogLines, map[string]any{
		"endpoint": baseURL,
		"api_key":  "test-token",
		"project":  "owner/repo",
	})
	if err != nil {
		t.Fatalf("NewGitHubCIProvider: %v", err)
	}
	return p.(*GitHubCIProvider)
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

func TestMapCheckRunStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  domain.CheckRunStatus
	}{
		{"queued", "queued", domain.CheckRunStatusQueued},
		{"in_progress", "in_progress", domain.CheckRunStatusInProgress},
		{"completed", "completed", domain.CheckRunStatusCompleted},
		{"unknown returns Queued", "unknown", domain.CheckRunStatusQueued},
		{"empty returns Queued", "", domain.CheckRunStatusQueued},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapCheckRunStatus(tt.input)
			if got != tt.want {
				t.Errorf("mapCheckRunStatus(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapCheckConclusion(t *testing.T) {
	t.Parallel()

	str := func(s string) *string { return &s }

	tests := []struct {
		name  string
		input *string
		want  domain.CheckConclusion
	}{
		{"nil returns Pending", nil, domain.CheckConclusionPending},
		{"success", str("success"), domain.CheckConclusionSuccess},
		{"failure", str("failure"), domain.CheckConclusionFailure},
		{"cancelled", str("cancelled"), domain.CheckConclusionCancelled},
		{"timed_out", str("timed_out"), domain.CheckConclusionTimedOut},
		{"neutral", str("neutral"), domain.CheckConclusionNeutral},
		{"skipped", str("skipped"), domain.CheckConclusionSkipped},
		{"action_required returns Failure", str("action_required"), domain.CheckConclusionFailure},
		{"stale returns Pending", str("stale"), domain.CheckConclusionPending},
		{"unknown returns Pending", str("unknown_val"), domain.CheckConclusionPending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapCheckConclusion(tt.input)
			if got != tt.want {
				inputStr := "<nil>"
				if tt.input != nil {
					inputStr = *tt.input
				}
				t.Errorf("mapCheckConclusion(%q) = %q, want %q", inputStr, got, tt.want)
			}
		})
	}
}

func TestNewGitHubCIProvider_Valid(t *testing.T) {
	t.Parallel()

	p, err := NewGitHubCIProvider(100, map[string]any{
		"endpoint": "https://api.github.com",
		"api_key":  "tok",
		"project":  "org/repo",
	})
	if err != nil {
		t.Fatalf("NewGitHubCIProvider: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("NewGitHubCIProvider returned nil provider")
	}
}

func TestNewGitHubCIProvider_MissingAPIKey(t *testing.T) {
	t.Parallel()

	_, err := NewGitHubCIProvider(0, map[string]any{
		"project": "org/repo",
	})
	assertCIErrorKind(t, err, domain.ErrCIAuth)
}

func TestNewGitHubCIProvider_MissingProject(t *testing.T) {
	t.Parallel()

	_, err := NewGitHubCIProvider(0, map[string]any{
		"api_key": "tok",
	})
	assertCIErrorKind(t, err, domain.ErrCIPayload)
}

func TestNewGitHubCIProvider_TypeFaultVsAbsentKey(t *testing.T) {
	t.Parallel()

	_, err := NewGitHubCIProvider(0, map[string]any{
		"api_key": 4242,
		"project": "org/repo",
	})

	var ce *domain.CIError
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want *domain.CIError", err)
	}
	if ce.Kind != domain.ErrCIPayload {
		t.Errorf("CIError.Kind = %q, want %q", ce.Kind, domain.ErrCIPayload)
	}
	if ce.Message != "api_key: expected string, got integer" {
		t.Errorf("CIError.Message = %q, want %q", ce.Message, "api_key: expected string, got integer")
	}
}

func TestNewGitHubCIProvider_MalformedProject(t *testing.T) {
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
			_, err := NewGitHubCIProvider(0, map[string]any{
				"api_key": "tok",
				"project": tt.project,
			})
			assertCIErrorKind(t, err, domain.ErrCIPayload)
		})
	}
}

func TestNewGitHubCIProvider_DefaultEndpoint(t *testing.T) {
	t.Parallel()

	p, err := NewGitHubCIProvider(0, map[string]any{
		"api_key": "tok",
		"project": "org/repo",
	})
	if err != nil {
		t.Fatalf("NewGitHubCIProvider without endpoint: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("NewGitHubCIProvider returned nil provider")
	}
}

func TestNewGitHubCIProvider_MaxLogLinesStored(t *testing.T) {
	t.Parallel()

	p, err := NewGitHubCIProvider(42, map[string]any{
		"api_key": "tok",
		"project": "org/repo",
	})
	if err != nil {
		t.Fatalf("NewGitHubCIProvider: %v", err)
	}
	gh, ok := p.(*GitHubCIProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *GitHubCIProvider", p)
	}
	if gh.maxLogLines != 42 {
		t.Errorf("maxLogLines = %d, want 42", gh.maxLogLines)
	}
}

func TestFetchCIStatus_AllPassing(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "check_runs_passing.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(fixture) //nolint:errcheck // test helper
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.Status != domain.CIStatusPassing {
		t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusPassing)
	}
	if result.LogExcerpt != "" {
		t.Errorf("LogExcerpt = %q, want empty", result.LogExcerpt)
	}
	if result.FailingCount != 0 {
		t.Errorf("FailingCount = %d, want 0", result.FailingCount)
	}
	if len(result.CheckRuns) != 2 {
		t.Errorf("len(CheckRuns) = %d, want 2", len(result.CheckRuns))
	}
}

func TestFetchCIStatus_Failing(t *testing.T) {
	t.Parallel()

	f := fakeForJob(t, 2001, loadFixture(t, "job_101566349874_steps.json"), loadFixture(t, "job_101566349874.log"))
	f.checkRuns = loadFixture(t, "check_runs_failing.json")

	result := fetchFromFake(context.Background(), t, f, 50)

	if result.Status != domain.CIStatusFailing {
		t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusFailing)
	}
	if first, _, _ := strings.Cut(result.LogExcerpt, "\n"); first != locatedNote("Run tests") {
		t.Errorf("LogExcerpt first line = %q, want %q", first, locatedNote("Run tests"))
	}
	if result.FailingCount != 1 {
		t.Errorf("FailingCount = %d, want 1", result.FailingCount)
	}
	if len(result.CheckRuns) != 2 {
		t.Errorf("len(CheckRuns) = %d, want 2", len(result.CheckRuns))
	}
	if n := f.requested("/repos/owner/repo/actions/jobs/2001"); n != 1 {
		t.Errorf("job object requested %d times for the failing check run's id, want 1", n)
	}
}

func TestFetchCIStatus_MixedRunsMatchesCore(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "check_runs_mixed.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(fixture) //nolint:errcheck // test helper
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if len(result.CheckRuns) != 3 {
		t.Fatalf("len(CheckRuns) = %d, want 3", len(result.CheckRuns))
	}
	adaptertest.AssertCIAggregateMatchesCore(t, result)
}

func TestFetchCIStatus_Pending(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "check_runs_pending.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(fixture) //nolint:errcheck // test helper
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.Status != domain.CIStatusPending {
		t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusPending)
	}
	if result.LogExcerpt != "" {
		t.Errorf("LogExcerpt = %q, want empty for pending CI", result.LogExcerpt)
	}
}

func TestFetchCIStatus_EmptyCheckRuns(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "check_runs_empty.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.Status != domain.CIStatusPending {
		t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusPending)
	}
	if result.CheckRuns == nil {
		t.Error("CheckRuns is nil, want non-nil empty slice")
	}
	if len(result.CheckRuns) != 0 {
		t.Errorf("len(CheckRuns) = %d, want 0", len(result.CheckRuns))
	}
}

func TestFetchCIStatus_LogTruncation(t *testing.T) {
	t.Parallel()

	f := fakeForJob(t, 2001, loadFixture(t, "job_101566349874_steps.json"), loadFixture(t, "job_101566349874.log"))

	got := excerptFrom(t, f, 5)

	want := []string{locatedNote("Run tests"), "##[group]Run go test -count=1 ./...", omitted(71)}
	if len(got) != 7 {
		t.Fatalf("LogExcerpt has %d lines, want 7 (note, one head line, one omission, four tail lines): %q", len(got), got)
	}
	if !slices.Equal(got[:3], want) {
		t.Errorf("LogExcerpt head = %q, want %q", got[:3], want)
	}
	if last := got[len(got)-1]; last != sortieLastLine {
		t.Errorf("LogExcerpt last line = %q, want %q", last, sortieLastLine)
	}
}

func TestFetchCIStatus_LogDisabledWhenZero(t *testing.T) {
	t.Parallel()

	checkRunsFixture := loadFixture(t, "check_runs_failing.json")
	var logCalled atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(checkRunsFixture) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			logCalled.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("some log")) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 0)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.LogExcerpt != "" {
		t.Errorf("LogExcerpt = %q, want empty when maxLogLines=0", result.LogExcerpt)
	}
	if n := logCalled.Load(); n != 0 {
		t.Errorf("log endpoint called %d times, want 0 when maxLogLines=0", n)
	}
}

func TestFetchCIStatus_ANSIStripped(t *testing.T) {
	t.Parallel()

	jobLog := loadFixture(t, "job_101566349874.log")
	if !strings.Contains(string(jobLog), "\x1b[36;1mgo test") {
		t.Fatal("fixture holds no escape sequence inside the failing step")
	}
	f := fakeForJob(t, 2001, loadFixture(t, "job_101566349874_steps.json"), jobLog)

	got := excerptFrom(t, f, 100)

	if len(got) < 3 || got[2] != "go test -count=1 ./..." {
		t.Fatalf("LogExcerpt = %q, want the command line without its color codes as the second body line", got)
	}
	if joined := strings.Join(got, "\n"); strings.Contains(joined, "\x1b") {
		t.Error("LogExcerpt contains ANSI escape sequences after stripping")
	}
}

func TestFetchCIStatus_LogFetchFailure_NonFatal(t *testing.T) {
	t.Parallel()

	f := fakeForJob(t, 2001, loadFixture(t, "job_101566349874_steps.json"), nil)
	f.checkRuns = loadFixture(t, "check_runs_failing.json")
	f.jobs[2001] = reply{status: http.StatusInternalServerError}
	f.logs[2001] = reply{status: http.StatusInternalServerError}

	result := fetchFromFake(context.Background(), t, f, 50)

	if result.Status != domain.CIStatusFailing {
		t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusFailing)
	}
	if result.LogExcerpt != "" {
		t.Errorf("LogExcerpt = %q, want empty when log fetch fails", result.LogExcerpt)
	}
}

func TestFetchCIStatus_APIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 0)
	_, err := provider.FetchCIStatus(context.Background(), "main")
	assertCIErrorKind(t, err, domain.ErrCIAuth)
}

func TestFetchCIStatus_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 0)
	_, err := provider.FetchCIStatus(context.Background(), "main")
	assertCIErrorKind(t, err, domain.ErrCINotFound)
}

func TestFetchCIStatus_ContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	provider := newTestCIProvider(t, srv.URL, 0)

	errCh := make(chan error, 1)
	go func() {
		_, err := provider.FetchCIStatus(ctx, "main")
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("FetchCIStatus: expected error after context cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestFetchCIStatus_NonActionsApp(t *testing.T) {
	t.Parallel()

	const checkRunsJSON = `{
		"total_count": 1,
		"check_runs": [{
			"id": 7001,
			"name": "netlify-preview",
			"status": "completed",
			"conclusion": "failure",
			"html_url": "https://github.com/owner/repo/runs/7001",
			"app": {"slug": "netlify"}
		}]
	}`

	var logCalled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(checkRunsJSON)) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			logCalled.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("some log")) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.LogExcerpt != "" {
		t.Errorf("LogExcerpt = %q, want empty for non-github-actions failing run", result.LogExcerpt)
	}
	if n := logCalled.Load(); n != 0 {
		t.Errorf("log endpoint called %d times, want 0 for non-github-actions app", n)
	}
}

func TestFetchCIStatus_Pagination(t *testing.T) {
	t.Parallel()

	const page1JSON = `{
		"total_count": 3,
		"check_runs": [
			{"id": 101, "name": "build", "status": "completed", "conclusion": "success",
			 "html_url": "https://github.com/owner/repo/runs/101", "app": {"slug": "github-actions"}},
			{"id": 102, "name": "lint", "status": "completed", "conclusion": "success",
			 "html_url": "https://github.com/owner/repo/runs/102", "app": {"slug": "github-actions"}}
		]
	}`

	const page2JSON = `{
		"total_count": 3,
		"check_runs": [
			{"id": 103, "name": "test", "status": "completed", "conclusion": "failure",
			 "html_url": "https://github.com/owner/repo/runs/103", "app": {"slug": "github-actions"}}
		]
	}`

	const logContent = "test output\nFAIL: something failed"

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.URL.Query().Get("page") == "2":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(page2JSON)) //nolint:errcheck // test helper
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			linkURL := fmt.Sprintf(`<%s/repos/owner/repo/commits/main/check-runs?page=2>; rel="next"`, srvURL)
			w.Header().Set("Link", linkURL)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(page1JSON)) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/103/logs"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(logContent)) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.Status != domain.CIStatusFailing {
		t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusFailing)
	}
	if len(result.CheckRuns) != 3 {
		t.Errorf("len(CheckRuns) = %d, want 3 (from both pages)", len(result.CheckRuns))
	}
	if result.FailingCount != 1 {
		t.Errorf("FailingCount = %d, want 1", result.FailingCount)
	}
}

func TestFetchCIStatus_RefPassthrough(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "check_runs_passing.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	const ref = "feature/my-branch"
	provider := newTestCIProvider(t, srv.URL, 0)
	result, err := provider.FetchCIStatus(context.Background(), ref)
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.Ref != ref {
		t.Errorf("Ref = %q, want %q", result.Ref, ref)
	}
}

func TestFetchCIStatus_MixedFirstAndThirdPartyFailing(t *testing.T) {
	t.Parallel()

	const checkRunsJSON = `{
		"total_count": 2,
		"check_runs": [
			{"id": 8001, "name": "netlify-preview", "status": "completed", "conclusion": "failure",
			 "html_url": "https://github.com/owner/repo/runs/8001", "app": {"slug": "netlify"}},
			{"id": 8002, "name": "test", "status": "completed", "conclusion": "failure",
			 "html_url": "https://github.com/owner/repo/runs/8002", "app": {"slug": "github-actions"}}
		]
	}`

	var thirdPartyLogCalled atomic.Int32
	var actionsLogCalled atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(checkRunsJSON)) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/8001/logs"):
			thirdPartyLogCalled.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("netlify log")) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/8002/logs"):
			actionsLogCalled.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("test failure output")) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if result.LogExcerpt == "" {
		t.Error("LogExcerpt is empty, want log from the github-actions failing run")
	}
	if n := thirdPartyLogCalled.Load(); n != 0 {
		t.Errorf("third-party log endpoint called %d times, want 0", n)
	}
	if n := actionsLogCalled.Load(); n != 1 {
		t.Errorf("github-actions log endpoint called %d times, want 1", n)
	}
}
