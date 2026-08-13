package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// testCommitSHA and testPipelineID are the full SHA and last_pipeline.id
// carried by testdata/commit_resolved.json, so a test can address the
// status and pipeline-jobs routes without re-parsing the fixture.
const (
	testCommitSHA  = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	testPipelineID = 501
)

// --- helpers ---

// mustCIProvider constructs a *GitLabCIProvider against endpoint with a
// throwaway token and testProject, or fails the test.
func mustCIProvider(t *testing.T, endpoint string, maxLogLines int) *GitLabCIProvider {
	t.Helper()
	p, err := NewGitLabCIProvider(maxLogLines, map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
		"project":  testProject,
	})
	if err != nil {
		t.Fatalf("NewGitLabCIProvider: %v", err)
	}
	return p.(*GitLabCIProvider)
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

// staticJSONHandler returns a handler that serves body as a 200
// application/json response, for a route whose content does not depend
// on the request.
func staticJSONHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint:errcheck // test helper
	}
}

// withCommitResolution registers the commit-resolution route for the
// "main" ref on s, serving fixture as the response body.
func withCommitResolution(t *testing.T, s *fakeServer, fixture []byte) {
	t.Helper()
	s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/main", staticJSONHandler(fixture))
}

// withCommitStatuses registers fn as the handler for [testCommitSHA]'s
// commit-status route on s.
func withCommitStatuses(t *testing.T, s *fakeServer, fn http.HandlerFunc) {
	t.Helper()
	s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/"+testCommitSHA+"/statuses", fn)
}

// withPipelineJobs registers the pipeline-jobs route for [testPipelineID]
// on s, serving body as the response.
func withPipelineJobs(t *testing.T, s *fakeServer, body []byte) {
	t.Helper()
	s.handle("/api/v4/projects/"+testEscapedProject+"/pipelines/"+strconv.Itoa(testPipelineID)+"/jobs", staticJSONHandler(body))
}

// withJobTrace registers the job-trace route for jobID on s, serving body
// as a 200 text response.
func withJobTrace(t *testing.T, s *fakeServer, jobID int64, body []byte) {
	t.Helper()
	s.handle("/api/v4/projects/"+testEscapedProject+"/jobs/"+strconv.FormatInt(jobID, 10)+"/trace", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint:errcheck // test helper
	})
}

// buildStatusesPage returns the JSON body of a commit-statuses page
// carrying n entries of the given status, ids numbered from start and
// scoped to [testPipelineID], so a multi-page test can synthesize pages
// without committing a fixture of that size.
func buildStatusesPage(t *testing.T, status string, start, n int) []byte {
	t.Helper()
	entries := make([]map[string]any, n)
	for i := range n {
		entries[i] = map[string]any{
			"id":            start + i,
			"name":          fmt.Sprintf("check-%d", start+i),
			"status":        status,
			"allow_failure": false,
			"target_url":    nil,
			"pipeline_id":   testPipelineID,
		}
	}
	body, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal statuses page: %v", err)
	}
	return body
}

// --- job outcome mapping ---

func TestMapJobOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		status         string
		allowFailure   bool
		wantConclusion domain.CheckConclusion
		wantRunStatus  domain.CheckRunStatus
		wantRecognized bool
	}{
		{"success", "success", false, domain.CheckConclusionSuccess, domain.CheckRunStatusCompleted, true},
		{"failed without allow_failure", "failed", false, domain.CheckConclusionFailure, domain.CheckRunStatusCompleted, true},
		{"failed with allow_failure folds to neutral", "failed", true, domain.CheckConclusionNeutral, domain.CheckRunStatusCompleted, true},
		{"canceled", "canceled", false, domain.CheckConclusionCancelled, domain.CheckRunStatusCompleted, true},
		{"skipped", "skipped", false, domain.CheckConclusionSkipped, domain.CheckRunStatusCompleted, true},
		{"manual", "manual", false, domain.CheckConclusionNeutral, domain.CheckRunStatusCompleted, true},
		{"created queues", "created", false, domain.CheckConclusionPending, domain.CheckRunStatusQueued, true},
		{"pending queues", "pending", false, domain.CheckConclusionPending, domain.CheckRunStatusQueued, true},
		{"waiting_for_resource queues", "waiting_for_resource", false, domain.CheckConclusionPending, domain.CheckRunStatusQueued, true},
		{"waiting_for_callback queues", "waiting_for_callback", false, domain.CheckConclusionPending, domain.CheckRunStatusQueued, true},
		{"preparing queues", "preparing", false, domain.CheckConclusionPending, domain.CheckRunStatusQueued, true},
		{"scheduled queues", "scheduled", false, domain.CheckConclusionPending, domain.CheckRunStatusQueued, true},
		{"running is in progress", "running", false, domain.CheckConclusionPending, domain.CheckRunStatusInProgress, true},
		{"canceling is in progress", "canceling", false, domain.CheckConclusionPending, domain.CheckRunStatusInProgress, true},
		{"an unrecognized status defers to pending/in-progress and is unrecognized", "expired", false, domain.CheckConclusionPending, domain.CheckRunStatusInProgress, false},
		{"an empty status defers to pending/in-progress and is unrecognized", "", false, domain.CheckConclusionPending, domain.CheckRunStatusInProgress, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conclusion, runStatus, recognized := mapJobOutcome(tt.status, tt.allowFailure)

			if conclusion != tt.wantConclusion {
				t.Errorf("mapJobOutcome(%q, %v) conclusion = %q, want %q", tt.status, tt.allowFailure, conclusion, tt.wantConclusion)
			}
			if runStatus != tt.wantRunStatus {
				t.Errorf("mapJobOutcome(%q, %v) status = %q, want %q", tt.status, tt.allowFailure, runStatus, tt.wantRunStatus)
			}
			if recognized != tt.wantRecognized {
				t.Errorf("mapJobOutcome(%q, %v) recognized = %v, want %v", tt.status, tt.allowFailure, recognized, tt.wantRecognized)
			}
		})
	}
}

// --- trace sanitization ---

func TestTraceExcerpt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		maxLines int
		want     string
	}{
		{"maxLines zero yields empty", "a\nb\nc", 0, ""},
		{"negative maxLines yields empty", "a\nb\nc", -1, ""},
		{"empty input yields empty", "", 5, ""},
		{"a runner timestamp and stream-token prefix is stripped", "2026-08-10T14:24:53.1000000Z 00O hello world", 5, "hello world"},
		{"a continuation-form prefix ending in + with no trailing space is stripped", "2026-08-10T14:24:53.1000000Z 00O+continued output", 5, "continued output"},
		{"an ANSI CSI sequence is stripped", "\x1b[0;31mFAIL\x1b[0m", 5, "FAIL"},
		{"a section_start/section_end marker pair is stripped", "section_start:1691568000:step_script\nbuild output\nsection_end:1691568010:step_script", 5, "build output"},
		{"a carriage return is removed and trailing whitespace trimmed", "line one \t\r\nline two", 5, "line one\nline two"},
		{"a line left empty after sanitization is dropped before the line-count limit", "\x1b[0m\nkept line", 5, "kept line"},
		{"more surviving lines than maxLines keeps the tail", "a\nb\nc\nd", 2, "c\nd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := traceExcerpt([]byte(tt.raw), tt.maxLines)

			if got != tt.want {
				t.Errorf("traceExcerpt(%q, %d) = %q, want %q", tt.raw, tt.maxLines, got, tt.want)
			}
		})
	}
}

// --- registration and construction ---

func TestGitLabCIProviderRegistration(t *testing.T) {
	t.Parallel()

	if !registry.CIProviders.Has("gitlab") {
		t.Fatal(`CIProviders.Has("gitlab") = false, want true`)
	}

	constructor, err := registry.CIProviders.Get("gitlab")
	if err != nil {
		t.Fatalf(`CIProviders.Get("gitlab") = %v, want registered constructor`, err)
	}

	provider, err := constructor(0, map[string]any{
		"api_key": "test-token",
		"project": testProject,
	})
	if err != nil {
		t.Fatalf("registered gitlab CI constructor(...) = %v, want nil error", err)
	}
	if _, ok := provider.(*GitLabCIProvider); !ok {
		t.Errorf("registered gitlab CI constructor(...) = %T, want *GitLabCIProvider", provider)
	}
	if !registry.Trackers.Has("gitlab") {
		t.Error(`Trackers.Has("gitlab") = false, want true (CI registration must not disturb the tracker registration)`)
	}
	if !registry.SCMAdapters.Has("gitlab") {
		t.Error(`SCMAdapters.Has("gitlab") = false, want true (CI registration must not disturb the SCM registration)`)
	}
}

func TestNewGitLabCIProvider_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   map[string]any
		wantKind domain.CIErrorKind
	}{
		{
			name:     "missing api_key returns ErrCIAuth",
			config:   map[string]any{"project": testProject, "endpoint": "http://gitlab.invalid"},
			wantKind: domain.ErrCIAuth,
		},
		{
			name:     "missing project returns ErrCIPayload",
			config:   map[string]any{"api_key": "test-token", "endpoint": "http://gitlab.invalid"},
			wantKind: domain.ErrCIPayload,
		},
		{
			name:     "invalid endpoint scheme returns ErrCIPayload",
			config:   map[string]any{"api_key": "test-token", "project": testProject, "endpoint": "ftp://gitlab.invalid"},
			wantKind: domain.ErrCIPayload,
		},
		{
			name:     "invalid endpoint with no host returns ErrCIPayload",
			config:   map[string]any{"api_key": "test-token", "project": testProject, "endpoint": "http://"},
			wantKind: domain.ErrCIPayload,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewGitLabCIProvider(0, tt.config)

			assertCIErrorKind(t, err, tt.wantKind)
		})
	}

	t.Run("invalid endpoint redacts userinfo", func(t *testing.T) {
		t.Parallel()

		const endpoint = "ftp://operator:secret@gitlab.example.com/group"
		provider, err := NewGitLabCIProvider(0, map[string]any{
			"api_key":  "test-token",
			"project":  testProject,
			"endpoint": endpoint,
		})

		assertCIErrorKind(t, err, domain.ErrCIPayload)
		if provider != nil {
			t.Error("provider should be nil on error")
		}
		var ciErr *domain.CIError
		if !errors.As(err, &ciErr) {
			t.Fatalf("error type = %T, want *domain.CIError", err)
		}
		assertMessageRedacted(t, ciErr.Message, "ftp://gitlab.example.com/group", "operator", "secret")
	})

	t.Run("an absent endpoint defaults to https://gitlab.com", func(t *testing.T) {
		t.Parallel()

		_, err := NewGitLabCIProvider(0, map[string]any{
			"api_key": "test-token",
			"project": testProject,
		})

		if err != nil {
			t.Fatalf("NewGitLabCIProvider without endpoint: unexpected error: %v", err)
		}
	})

	t.Run("a three-segment project reaches the server as one percent-encoded path segment", func(t *testing.T) {
		t.Parallel()

		const nestedProject = "acme/team/widgets"
		s := newFakeServer(t)
		var gotEscapedPath string
		s.handle("/api/v4/projects/"+url.PathEscape(nestedProject)+"/repository/commits/main", func(w http.ResponseWriter, r *http.Request) {
			gotEscapedPath = r.URL.EscapedPath()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "commit_no_pipeline.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()

		p, err := NewGitLabCIProvider(0, map[string]any{
			"api_key":  "test-token",
			"project":  nestedProject,
			"endpoint": srv.URL,
		})
		if err != nil {
			t.Fatalf("NewGitLabCIProvider: %v", err)
		}

		_, err = p.(*GitLabCIProvider).FetchCIStatus(context.Background(), "main")
		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}

		const wantSuffix = "/repository/commits/main"
		if !strings.HasSuffix(gotEscapedPath, wantSuffix) {
			t.Errorf("request path = %q, want suffix %q", gotEscapedPath, wantSuffix)
		}
		if got := strings.Count(gotEscapedPath, "%2F"); got != 2 {
			t.Errorf("request path %q carries %d occurrences of %%2F, want 2 (one percent-encoded segment for a three-part project)", gotEscapedPath, got)
		}
	})
}

// --- ref and pipeline resolution ---

func TestFetchCIStatus_RefResolution(t *testing.T) {
	t.Parallel()

	t.Run("a branch ref resolves the commit first and scopes the status request to its pipeline", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		var seq, commitSeq, statusSeq atomic.Int32
		var gotPipelineID string
		s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/main", func(w http.ResponseWriter, r *http.Request) {
			commitSeq.Store(seq.Add(1))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "commit_resolved.json")) //nolint:errcheck // test helper
		})
		s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/"+testCommitSHA+"/statuses", func(w http.ResponseWriter, r *http.Request) {
			statusSeq.Store(seq.Add(1))
			gotPipelineID = r.URL.Query().Get("pipeline_id")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "statuses_empty.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		_, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if commitSeq.Load() == 0 {
			t.Fatal("commit-resolution route not requested")
		}
		if statusSeq.Load() == 0 {
			t.Fatal("status route not requested")
		}
		if commitSeq.Load() >= statusSeq.Load() {
			t.Errorf("commit-resolution request order = %d, status request order = %d, want commit resolution issued first", commitSeq.Load(), statusSeq.Load())
		}
		wantPipelineID := strconv.Itoa(testPipelineID)
		if gotPipelineID != wantPipelineID {
			t.Errorf("status request pipeline_id query param = %q, want %q", gotPipelineID, wantPipelineID)
		}
	})

	t.Run("a slash-bearing ref is percent-encoded exactly once in the commit-resolution path", func(t *testing.T) {
		t.Parallel()

		const ref = "feature/needs-slash"
		s := newFakeServer(t)
		var gotEscapedPath string
		s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/feature%2Fneeds-slash", func(w http.ResponseWriter, r *http.Request) {
			gotEscapedPath = r.URL.EscapedPath()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "commit_no_pipeline.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		_, err := provider.FetchCIStatus(context.Background(), ref)

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		const wantSuffix = "/repository/commits/feature%2Fneeds-slash"
		if !strings.HasSuffix(gotEscapedPath, wantSuffix) {
			t.Errorf("commit-resolution request path = %q, want suffix %q", gotEscapedPath, wantSuffix)
		}
	})

	t.Run("an unresolvable ref maps to ErrCINotFound and issues no status request", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/missing", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"404 Commit Not Found"}`)) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		_, err := provider.FetchCIStatus(context.Background(), "missing")

		assertCIErrorKind(t, err, domain.ErrCINotFound)
	})
}

// --- aggregate outcomes and details URL ---

func TestFetchCIStatus_AggregateOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		statusesFixture  string
		wantStatus       domain.CIStatus
		wantFailingCount int
	}{
		{
			name:             "all entries succeed",
			statusesFixture:  "statuses_all_success.json",
			wantStatus:       domain.CIStatusPassing,
			wantFailingCount: 0,
		},
		{
			name:             "one hard failure among successes",
			statusesFixture:  "statuses_one_failed.json",
			wantStatus:       domain.CIStatusFailing,
			wantFailingCount: 1,
		},
		{
			name:             "an allow_failure failure beside a success stays passing",
			statusesFixture:  "statuses_warning_only.json",
			wantStatus:       domain.CIStatusPassing,
			wantFailingCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newFakeServer(t)
			withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
			withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, tt.statusesFixture)))
			srv := httptest.NewServer(s)
			defer srv.Close()
			provider := mustCIProvider(t, srv.URL, 0)

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
			if got.Ref != "main" {
				t.Errorf("FetchCIStatus().Ref = %q, want %q", got.Ref, "main")
			}
			adaptertest.AssertCIAggregateMatchesCore(t, got)
		})
	}

	t.Run("a warning-only run's conclusion is neutral", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_warning_only.json")))
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		var found bool
		for _, run := range got.CheckRuns {
			if run.Conclusion == domain.CheckConclusionNeutral {
				found = true
			}
		}
		if !found {
			t.Errorf("CheckRuns = %+v, want one run with Conclusion %q", got.CheckRuns, domain.CheckConclusionNeutral)
		}
	})

	t.Run("an empty status list on a commit carrying a pipeline yields non-nil empty CheckRuns and pending", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_empty.json")))
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.CheckRuns == nil {
			t.Error("FetchCIStatus().CheckRuns = nil, want non-nil")
		}
		if len(got.CheckRuns) != 0 {
			t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want 0", len(got.CheckRuns))
		}
		if got.Status != domain.CIStatusPending {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusPending)
		}
	})

	t.Run("a commit with no pipeline yields non-nil empty CheckRuns, pending, and no status request", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_no_pipeline.json"))
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.CheckRuns == nil {
			t.Error("FetchCIStatus().CheckRuns = nil, want non-nil")
		}
		if len(got.CheckRuns) != 0 {
			t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want 0", len(got.CheckRuns))
		}
		if got.Status != domain.CIStatusPending {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusPending)
		}
	})

	t.Run("DetailsURL passthrough: a null target_url yields empty, a populated one is verbatim", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_details_url.json")))
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if len(got.CheckRuns) != 2 {
			t.Fatalf("len(FetchCIStatus().CheckRuns) = %d, want 2", len(got.CheckRuns))
		}
		if got.CheckRuns[0].DetailsURL != "" {
			t.Errorf("CheckRuns[0].DetailsURL = %q, want empty (fixture carries a null target_url)", got.CheckRuns[0].DetailsURL)
		}
		const wantURL = "https://gitlab.example.invalid/acme/widgets/-/jobs/3002"
		if got.CheckRuns[1].DetailsURL != wantURL {
			t.Errorf("CheckRuns[1].DetailsURL = %q, want %q", got.CheckRuns[1].DetailsURL, wantURL)
		}
	})
}

// --- pagination ---

func TestFetchCIStatus_StatusPagination(t *testing.T) {
	t.Parallel()

	page1 := buildStatusesPage(t, "success", 1, 100)
	page2 := buildStatusesPage(t, "success", 101, 100)
	page3 := buildStatusesPage(t, "failed", 201, 1)

	var srv *httptest.Server
	s := newFakeServer(t)
	withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
	withCommitStatuses(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		statusesPath := "/api/v4/projects/" + testEscapedProject + "/repository/commits/" + testCommitSHA + "/statuses"
		wantPipeline := strconv.Itoa(testPipelineID)
		// The platform echoes every request parameter into the Link URLs
		// it emits, so a later page that arrived without the pipeline
		// scope would read the whole commit instead of the one pipeline.
		if got := r.URL.Query().Get("pipeline_id"); got != wantPipeline {
			t.Errorf("statuses request %q carried pipeline_id=%q, want %q", r.URL.RequestURI(), got, wantPipeline)
		}
		nextLink := func(page int) string {
			return `<` + srv.URL + statusesPath + `?order_by=id&page=` + strconv.Itoa(page) +
				`&per_page=100&pipeline_id=` + wantPipeline + `&sha=` + testCommitSHA + `>; rel="next"`
		}
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", nextLink(2))
			w.WriteHeader(http.StatusOK)
			w.Write(page1) //nolint:errcheck // test helper
		case "2":
			w.Header().Set("Link", nextLink(3))
			w.WriteHeader(http.StatusOK)
			w.Write(page2) //nolint:errcheck // test helper
		case "3":
			w.WriteHeader(http.StatusOK)
			w.Write(page3) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv = httptest.NewServer(s)
	defer srv.Close()
	provider := mustCIProvider(t, srv.URL, 0)

	got, err := provider.FetchCIStatus(context.Background(), "main")

	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	const wantLen = 201
	if len(got.CheckRuns) != wantLen {
		t.Fatalf("len(FetchCIStatus().CheckRuns) = %d, want %d", len(got.CheckRuns), wantLen)
	}
	for i, run := range got.CheckRuns {
		wantName := fmt.Sprintf("check-%d", i+1)
		if run.Name != wantName {
			t.Errorf("CheckRuns[%d].Name = %q, want %q (page order not preserved)", i, run.Name, wantName)
		}
	}
	if got.Status != domain.CIStatusFailing {
		t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
	}
	adaptertest.AssertCIAggregateMatchesCore(t, got)
}

func TestFetchCIStatus_PageCeiling(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	var requestCount atomic.Int32
	s := newFakeServer(t)
	withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
	withCommitStatuses(t, s, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		statusesPath := "/api/v4/projects/" + testEscapedProject + "/repository/commits/" + testCommitSHA + "/statuses"
		w.Header().Set("Link", `<`+srv.URL+statusesPath+`?page=next>; rel="next"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"id":1,"name":"x","status":"success","allow_failure":false,"target_url":null,"pipeline_id":` + strconv.Itoa(testPipelineID) + `}]`)) //nolint:errcheck // test helper
	})
	srv = httptest.NewServer(s)
	defer srv.Close()

	log, buf := newCapturingLogger()
	provider := mustCIProvider(t, srv.URL, 0)
	provider.log = log

	got, err := provider.FetchCIStatus(context.Background(), "main")

	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if n := requestCount.Load(); n != int32(maxPages) {
		t.Errorf("request count = %d, want %d (the walk must stop at the cap)", n, maxPages)
	}
	if len(got.CheckRuns) != maxPages {
		t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want %d", len(got.CheckRuns), maxPages)
	}

	out := buf.String()
	if n := strings.Count(out, "pagination limit reached"); n != 1 {
		t.Errorf("log output contains %d WARN entries for the limit, want exactly 1", n)
	}
	wantEndpoint := "/projects/" + testEscapedProject + "/repository/commits/" + testCommitSHA + "/statuses"
	if !strings.Contains(out, "endpoint="+wantEndpoint) {
		t.Errorf("log output = %q, want the path carried as the endpoint attribute %q", out, wantEndpoint)
	}
	if !strings.Contains(out, "max_pages="+strconv.Itoa(maxPages)) {
		t.Errorf("log output = %q, want the cap carried as the max_pages attribute", out)
	}
}

// --- superseded pipeline scoping ---

func TestFetchCIStatus_SupersededPipelineScoping(t *testing.T) {
	t.Parallel()

	t.Run("a server honoring the pipeline_id parameter serves only the current pipeline's entries", func(t *testing.T) {
		t.Parallel()

		var entries []map[string]any
		if err := json.Unmarshal(loadFixture(t, "statuses_two_pipelines.json"), &entries); err != nil {
			t.Fatalf("unmarshal fixture: %v", err)
		}

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, func(w http.ResponseWriter, r *http.Request) {
			wantPipelineID := r.URL.Query().Get("pipeline_id")
			filtered := make([]map[string]any, 0, len(entries))
			for _, entry := range entries {
				if fmt.Sprintf("%.0f", entry["pipeline_id"].(float64)) == wantPipelineID {
					filtered = append(filtered, entry)
				}
			}
			body, err := json.Marshal(filtered)
			if err != nil {
				t.Fatalf("marshal filtered entries: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(body) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusPassing {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusPassing)
		}
		if got.FailingCount != 0 {
			t.Errorf("FetchCIStatus().FailingCount = %d, want 0", got.FailingCount)
		}
		if len(got.CheckRuns) != 2 {
			t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want 2 (only the current pipeline's entries)", len(got.CheckRuns))
		}
		adaptertest.AssertCIAggregateMatchesCore(t, got)
	})

	t.Run("a server ignoring the pipeline_id parameter still scopes CheckRuns via the post-decode filter", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_two_pipelines.json")))
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusPassing {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusPassing)
		}
		if got.FailingCount != 0 {
			t.Errorf("FetchCIStatus().FailingCount = %d, want 0", got.FailingCount)
		}
		if len(got.CheckRuns) != 2 {
			t.Errorf("len(FetchCIStatus().CheckRuns) = %d, want 2 (only the current pipeline's entries)", len(got.CheckRuns))
		}
		adaptertest.AssertCIAggregateMatchesCore(t, got)
	})
}

// --- log excerpt ---

func TestFetchCIStatus_LogExcerpt(t *testing.T) {
	t.Parallel()

	t.Run("maxLogLines zero disables log fetching entirely", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_one_failed.json")))
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusFailing {
			t.Fatalf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
		}
		if got.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty when maxLogLines is zero", got.LogExcerpt)
		}
	})

	t.Run("a job-shaped failing entry yields a sanitized trace tail", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_one_failed.json")))
		withPipelineJobs(t, s, loadFixture(t, "pipeline_jobs.json"))
		withJobTrace(t, s, 6002, loadFixture(t, "job_trace.txt"))
		srv := httptest.NewServer(s)
		defer srv.Close()
		const maxLogLines = 5
		provider := mustCIProvider(t, srv.URL, maxLogLines)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.LogExcerpt == "" {
			t.Fatal("LogExcerpt is empty, want a sanitized trace tail")
		}
		lines := strings.Split(got.LogExcerpt, "\n")
		if len(lines) > maxLogLines {
			t.Errorf("LogExcerpt has %d lines, want at most %d", len(lines), maxLogLines)
		}
		if strings.Contains(got.LogExcerpt, "\x1b") {
			t.Error("LogExcerpt contains a raw ANSI escape byte")
		}
		if runnerPrefixPattern.MatchString(got.LogExcerpt) {
			t.Errorf("LogExcerpt = %q, want no line-leading runner timestamp prefix", got.LogExcerpt)
		}
		if strings.Contains(got.LogExcerpt, "section_start") || strings.Contains(got.LogExcerpt, "section_end") {
			t.Errorf("LogExcerpt = %q, want no section marker", got.LogExcerpt)
		}
		if !strings.Contains(got.LogExcerpt, "exit code 1") {
			t.Errorf("LogExcerpt = %q, want it to carry the trace's tail", got.LogExcerpt)
		}
	})

	t.Run("the excerpt is taken from the later job-shaped failing entry, not the earlier external status", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_external_first.json")))
		withPipelineJobs(t, s, loadFixture(t, "pipeline_jobs.json"))
		var externalTraceRequests, jobTraceRequests atomic.Int32
		s.handle("/api/v4/projects/"+testEscapedProject+"/jobs/9001/trace", func(w http.ResponseWriter, r *http.Request) {
			externalTraceRequests.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("should never be fetched")) //nolint:errcheck // test helper
		})
		s.handle("/api/v4/projects/"+testEscapedProject+"/jobs/6004/trace", func(w http.ResponseWriter, r *http.Request) {
			jobTraceRequests.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "job_trace.txt")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 10)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.LogExcerpt == "" {
			t.Fatal("LogExcerpt is empty, want the later job-shaped entry's trace")
		}
		if n := externalTraceRequests.Load(); n != 0 {
			t.Errorf("trace endpoint for the external status (job 9001) received %d requests, want 0", n)
		}
		if n := jobTraceRequests.Load(); n != 1 {
			t.Errorf("trace endpoint for job 6004 received %d requests, want 1", n)
		}
	})

	t.Run("a trace read returning 404 leaves the verdict unchanged with an empty excerpt", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_one_failed.json")))
		withPipelineJobs(t, s, loadFixture(t, "pipeline_jobs.json"))
		s.handle("/api/v4/projects/"+testEscapedProject+"/jobs/6002/trace", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"404 Not Found"}`)) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 10)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusFailing {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
		}
		if got.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty when the trace read fails", got.LogExcerpt)
		}
	})

	t.Run("a trace read returning 500 leaves the verdict unchanged with an empty excerpt", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_one_failed.json")))
		withPipelineJobs(t, s, loadFixture(t, "pipeline_jobs.json"))
		s.handle("/api/v4/projects/"+testEscapedProject+"/jobs/6002/trace", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"boom"}`)) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 10)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusFailing {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
		}
		if got.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty when the trace read fails", got.LogExcerpt)
		}
	})

	t.Run("no failing entry is a member of the pipeline's job set", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
		withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_one_failed.json")))
		withPipelineJobs(t, s, []byte(`[]`))
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 10)

		got, err := provider.FetchCIStatus(context.Background(), "main")

		if err != nil {
			t.Fatalf("FetchCIStatus: unexpected error: %v", err)
		}
		if got.Status != domain.CIStatusFailing {
			t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusFailing)
		}
		if got.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty when no failing entry is a job", got.LogExcerpt)
		}
	})
}

// --- error mapping and context cancellation ---

func TestFetchCIStatus_ErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		body       string
		wantKind   domain.CIErrorKind
	}{
		{"401 unauthorized maps to ErrCIAuth", http.StatusUnauthorized, `{"message":"401 Unauthorized"}`, domain.ErrCIAuth},
		{"403 forbidden maps to ErrCIAuth", http.StatusForbidden, `{"message":"403 Forbidden"}`, domain.ErrCIAuth},
		{"404 not found maps to ErrCINotFound", http.StatusNotFound, `{"message":"404 Commit Not Found"}`, domain.ErrCINotFound},
		{"429 too many requests maps to ErrCIAPI", http.StatusTooManyRequests, `{"message":"429 Too Many Requests"}`, domain.ErrCIAPI},
		{"500 server error maps to ErrCITransport", http.StatusInternalServerError, `{"message":"boom"}`, domain.ErrCITransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newFakeServer(t)
			s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/main", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body)) //nolint:errcheck // test helper
			})
			srv := httptest.NewServer(s)
			defer srv.Close()
			provider := mustCIProvider(t, srv.URL, 0)

			_, err := provider.FetchCIStatus(context.Background(), "main")

			assertCIErrorKind(t, err, tt.wantKind)
		})
	}

	t.Run("a malformed 2xx commit response maps to ErrCIPayload", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/main", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{not valid json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		provider := mustCIProvider(t, srv.URL, 0)

		_, err := provider.FetchCIStatus(context.Background(), "main")

		assertCIErrorKind(t, err, domain.ErrCIPayload)
	})
}

func TestFetchCIStatus_ContextCancelled(t *testing.T) {
	t.Parallel()

	s := newFakeServer(t)
	s.handle("/api/v4/projects/"+testEscapedProject+"/repository/commits/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(loadFixture(t, "commit_no_pipeline.json")) //nolint:errcheck // test helper
	})
	srv := httptest.NewServer(s)
	defer srv.Close()
	provider := mustCIProvider(t, srv.URL, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := provider.FetchCIStatus(ctx, "main")

	if !errors.Is(err, context.Canceled) {
		t.Errorf("FetchCIStatus(cancelled context) = %v, want context.Canceled in the chain", err)
	}
	var ciErr *domain.CIError
	if errors.As(err, &ciErr) {
		t.Errorf("FetchCIStatus(cancelled context) = %v, want the context error without CIError conversion", ciErr)
	}
}

// --- unrecognized status warning ---

func TestFetchCIStatus_UnrecognizedStatusWarning(t *testing.T) {
	t.Parallel()

	s := newFakeServer(t)
	withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
	withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_unknown_status.json")))
	srv := httptest.NewServer(s)
	defer srv.Close()

	log, buf := newCapturingLogger()
	provider := mustCIProvider(t, srv.URL, 0)
	provider.log = log

	got, err := provider.FetchCIStatus(context.Background(), "main")

	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	if len(got.CheckRuns) != 1 {
		t.Fatalf("len(FetchCIStatus().CheckRuns) = %d, want 1", len(got.CheckRuns))
	}
	if got.CheckRuns[0].Conclusion != domain.CheckConclusionPending {
		t.Errorf("CheckRuns[0].Conclusion = %q, want %q", got.CheckRuns[0].Conclusion, domain.CheckConclusionPending)
	}
	if got.CheckRuns[0].Status != domain.CheckRunStatusInProgress {
		t.Errorf("CheckRuns[0].Status = %q, want %q", got.CheckRuns[0].Status, domain.CheckRunStatusInProgress)
	}
	if got.Status != domain.CIStatusPending {
		t.Errorf("FetchCIStatus().Status = %q, want %q", got.Status, domain.CIStatusPending)
	}

	out := buf.String()
	if n := strings.Count(out, "unrecognized gitlab job status"); n != 1 {
		t.Errorf("log output contains %d WARN entries for the unrecognized status, want exactly 1", n)
	}
	if !strings.Contains(out, "status=expired") {
		t.Errorf("log output = %q, want it to carry the example status value", out)
	}
	if !strings.Contains(out, "count=1") {
		t.Errorf("log output = %q, want it to carry the count", out)
	}
}
