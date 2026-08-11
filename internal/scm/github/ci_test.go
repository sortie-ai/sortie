package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- helpers ---

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

// --- TestMapCheckRunStatus ---

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

// --- TestMapCheckConclusion ---

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

// --- TestStripANSI ---

func TestStripANSI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text unchanged", "hello world", "hello world"},
		{"CSI color sequence stripped", "\x1b[0;32mgreen\x1b[0m", "green"},
		{"CSI bold stripped", "\x1b[1mBold\x1b[0m", "Bold"},
		{"OSC with BEL stripped", "\x1b]0;title\a", ""},
		{"OSC with ST stripped", "\x1b]0;title\x1b\\", ""},
		{"timestamp prefix stripped", "2026-01-15T10:30:00.1234567Z hello", "hello"},
		{"ANSI and timestamp together", "2026-01-15T10:30:00.0000000Z \x1b[0;31mFAIL\x1b[0m", "FAIL"},
		{"empty string unchanged", "", ""},
		{"multiple CSI sequences", "\x1b[1mBold\x1b[0m and \x1b[32mnormal\x1b[0m", "Bold and normal"},
		{"no escape sequences unchanged", "plain log line here", "plain log line here"},
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

// --- TestTruncateLog ---

func TestTruncateLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		maxLines  int
		wantExact string
	}{
		{
			name:      "empty string returns empty",
			input:     "",
			maxLines:  10,
			wantExact: "",
		},
		{
			name:      "maxLines zero returns empty",
			input:     "line1\nline2",
			maxLines:  0,
			wantExact: "",
		},
		{
			name:      "negative maxLines returns empty",
			input:     "line1\nline2",
			maxLines:  -1,
			wantExact: "",
		},
		{
			name:      "tail N lines taken",
			input:     "a\nb\nc\nd\ne\nf",
			maxLines:  3,
			wantExact: "d\ne\nf",
		},
		{
			name:      "input shorter than maxLines returns all",
			input:     "a\nb",
			maxLines:  10,
			wantExact: "a\nb",
		},
		{
			name:      "CRLF line endings normalized",
			input:     "line1\r\nline2\r\n",
			maxLines:  5,
			wantExact: "line1\nline2\n",
		},
		{
			name:      "strips ANSI sequences per line",
			input:     "\x1b[0;32mgreen\x1b[0m\nplain",
			maxLines:  5,
			wantExact: "green\nplain",
		},
		{
			name:      "single line no truncation",
			input:     "only one",
			maxLines:  1,
			wantExact: "only one",
		},
		{
			name:      "exactly maxLines returns all",
			input:     "a\nb\nc",
			maxLines:  3,
			wantExact: "a\nb\nc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateLog(tt.input, tt.maxLines)
			if got != tt.wantExact {
				t.Errorf("truncateLog(%q, %d) = %q, want %q", tt.input, tt.maxLines, got, tt.wantExact)
			}
		})
	}
}

func TestTruncateLog_FixtureTail(t *testing.T) {
	t.Parallel()
	raw := string(loadFixture(t, "job_log_sample.txt"))
	const maxLines = 5
	got := truncateLog(raw, maxLines)
	parts := strings.Split(got, "\n")
	if len(parts) > maxLines {
		t.Errorf("truncateLog with maxLines=%d: got %d parts, want ≤%d", maxLines, len(parts), maxLines)
	}
	if strings.Contains(got, "\x1b") {
		t.Error("truncateLog result contains ANSI escape sequences")
	}
}

// --- Constructor tests ---

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
		// endpoint omitted — should default to https://api.github.com
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

// --- FetchCIStatus httptest tests ---

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

	checkRunsFixture := loadFixture(t, "check_runs_failing.json")
	logFixture := loadFixture(t, "job_log_sample.txt")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(checkRunsFixture) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			w.WriteHeader(http.StatusOK)
			w.Write(logFixture) //nolint:errcheck // test helper
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
	if result.Status != domain.CIStatusFailing {
		t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusFailing)
	}
	if result.LogExcerpt == "" {
		t.Error("LogExcerpt is empty, want non-empty for failing CI")
	}
	if result.FailingCount != 1 {
		t.Errorf("FailingCount = %d, want 1", result.FailingCount)
	}
	if len(result.CheckRuns) != 2 {
		t.Errorf("len(CheckRuns) = %d, want 2", len(result.CheckRuns))
	}
}

// TestFetchCIStatus_MixedRunsMatchesCore pins the aggregate contract
// against a run set carrying a completed-failing, an in-progress, and a
// completed-success run together, so the verdict this provider reports
// is provably the same rule scmcore.AggregateCIStatus implements.
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

	checkRunsFixture := loadFixture(t, "check_runs_failing.json")
	logFixture := loadFixture(t, "job_log_sample.txt")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(checkRunsFixture) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			w.WriteHeader(http.StatusOK)
			w.Write(logFixture) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	const maxLines = 5
	provider := newTestCIProvider(t, srv.URL, maxLines)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	parts := strings.Split(result.LogExcerpt, "\n")
	if len(parts) > maxLines {
		t.Errorf("LogExcerpt has %d lines, want ≤%d", len(parts), maxLines)
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

	provider := newTestCIProvider(t, srv.URL, 0) // maxLogLines=0 disables log fetch
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

	checkRunsFixture := loadFixture(t, "check_runs_failing.json")
	logFixture := loadFixture(t, "job_log_sample.txt")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(checkRunsFixture) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			w.WriteHeader(http.StatusOK)
			w.Write(logFixture) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 100)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if strings.Contains(result.LogExcerpt, "\x1b") {
		t.Error("LogExcerpt contains ANSI escape sequences after stripping")
	}
}

func TestFetchCIStatus_LogFetchFailure_NonFatal(t *testing.T) {
	t.Parallel()

	checkRunsFixture := loadFixture(t, "check_runs_failing.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(checkRunsFixture) //nolint:errcheck // test helper
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	provider := newTestCIProvider(t, srv.URL, 50)
	result, err := provider.FetchCIStatus(context.Background(), "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: expected no error when log fetch fails, got: %v", err)
	}
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
