package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- GetReviewDecision tests ---

// TestGetReviewDecision_GraphQL_EnumMapping covers the four authoritative
// review decision values plus the null case via a table-driven test.
func TestGetReviewDecision_GraphQL_EnumMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		reviewDecision string // JSON value for the reviewDecision field
		want           domain.ReviewDecision
	}{
		{
			name:           "approved",
			reviewDecision: `"APPROVED"`,
			want:           domain.ReviewDecisionApproved,
		},
		{
			name:           "changes_requested",
			reviewDecision: `"CHANGES_REQUESTED"`,
			want:           domain.ReviewDecisionChangesRequested,
		},
		{
			name:           "review_required",
			reviewDecision: `"REVIEW_REQUIRED"`,
			want:           domain.ReviewDecisionReviewRequired,
		},
		{
			name:           "null_not_required",
			reviewDecision: `null`,
			want:           domain.ReviewDecisionNotRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := `{"data":{"repository":{"pullRequest":{"reviewDecision":` + tt.reviewDecision + `}}}}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			a := newTestSCMAdapter(t, srv.URL)
			got, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
			if err != nil {
				t.Fatalf("GetReviewDecision() = _, %v, want nil error", err)
			}
			if got != tt.want {
				t.Errorf("GetReviewDecision() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetReviewDecision_GraphQL_UnknownEnumValue verifies that an unknown
// reviewDecision enum value maps to ReviewDecisionNotRequired and emits a
// WARN log entry carrying the unknown value.
func TestGetReviewDecision_GraphQL_UnknownEnumValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewDecision":"FUTURE_VALUE"}}}}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := newTestSCMAdapter(t, srv.URL)
	got, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetReviewDecision() = _, %v, want nil error", err)
	}
	if got != domain.ReviewDecisionNotRequired {
		t.Errorf("GetReviewDecision() = %q, want %q", got, domain.ReviewDecisionNotRequired)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "FUTURE_VALUE") {
		t.Errorf("WARN log output = %q, want substring %q", logOutput, "FUTURE_VALUE")
	}
}

// TestGetReviewDecision_GraphQL_ErrorsArray verifies that a GraphQL 200
// response with a non-empty errors array surfaces as ErrSCMAPI with the
// first error message text included in the SCMError message.
func TestGetReviewDecision_GraphQL_ErrorsArray(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"message":"Could not resolve to a Repository with the name"}]}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMAPI)

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if !strings.Contains(se.Message, "Could not resolve to a Repository") {
		t.Errorf("SCMError.Message = %q, want substring %q", se.Message, "Could not resolve to a Repository")
	}
}

// TestGetReviewDecision_GraphQL_MalformedJSON verifies that a GraphQL 200
// response with non-JSON body surfaces as ErrSCMPayload.
func TestGetReviewDecision_GraphQL_MalformedJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not valid json {{{`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMPayload)
}

// TestGetReviewDecision_GraphQL_HTTP401 verifies that a 401 response
// surfaces as ErrSCMAuth.
func TestGetReviewDecision_GraphQL_HTTP401(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMAuth)
}

// TestGetReviewDecision_GraphQL_HTTP403RateLimit verifies that a 403
// response with X-Ratelimit-Remaining: 0 surfaces as ErrSCMAPI with a
// message containing "rate limited".
func TestGetReviewDecision_GraphQL_HTTP403RateLimit(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMAPI)

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if !strings.Contains(se.Message, "rate limited") {
		t.Errorf("SCMError.Message = %q, want substring %q", se.Message, "rate limited")
	}
}

// TestGetReviewDecision_GraphQL_HTTP500 verifies that a 5xx response
// surfaces as ErrSCMTransport.
func TestGetReviewDecision_GraphQL_HTTP500(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMTransport)
}

// TestGetReviewDecision_GraphQL_MissingPullRequest verifies that a 200
// response with a null pullRequest field surfaces as ErrSCMPayload with
// the expected message.
func TestGetReviewDecision_GraphQL_MissingPullRequest(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMPayload)

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if se.Message != "graphql response missing pullRequest payload" {
		t.Errorf("SCMError.Message = %q, want %q", se.Message, "graphql response missing pullRequest payload")
	}
}

// TestGetReviewDecision_GraphQL_RequestShape verifies that the POST body
// sent to the GraphQL endpoint carries the exact reviewDecisionQuery
// constant text and the expected variables map.
func TestGetReviewDecision_GraphQL_RequestShape(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewDecision":"APPROVED"}}}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetReviewDecision() unexpected error: %v", err)
	}

	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("json.Unmarshal(request body) = %v, want nil error", err)
	}
	if payload.Query != reviewDecisionQuery {
		t.Errorf("request query = %q, want reviewDecisionQuery constant", payload.Query)
	}

	wantVars := map[string]any{
		"owner":  "owner",
		"repo":   "repo",
		"number": float64(1),
	}
	for k, wantV := range wantVars {
		gotV, ok := payload.Variables[k]
		if !ok {
			t.Errorf("request variables[%q] missing, want %v", k, wantV)
			continue
		}
		if gotV != wantV {
			t.Errorf("request variables[%q] = %v, want %v", k, gotV, wantV)
		}
	}
}

// TestGetReviewDecision_GraphQL_RequestURL verifies that the GraphQL POST
// is issued to /graphql via POST with the correct Authorization header.
func TestGetReviewDecision_GraphQL_RequestURL(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewDecision":"APPROVED"}}}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetReviewDecision() unexpected error: %v", err)
	}

	if gotPath != "/graphql" {
		t.Errorf("request path = %q, want %q", gotPath, "/graphql")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-token")
	}
}

// TestGetReviewDecision_GraphQL405_NotPromotedToConflict verifies that a
// GraphQL-originated HTTP 405 surfaces as ErrSCMAPI rather than being
// promoted to ErrSCMConflict. The duplicate-merge promotion is reserved
// for the REST MergePR write path.
func TestGetReviewDecision_GraphQL405_NotPromotedToConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"Method not allowed"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMAPI)

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if se.Kind == domain.ErrSCMConflict {
		t.Errorf("SCMError.Kind = %q, want NOT %q (graphql 405 must not promote to conflict)",
			se.Kind, domain.ErrSCMConflict)
	}
}

// TestGetReviewDecision_GraphQL409_NotPromotedToConflict verifies that a
// GraphQL-originated HTTP 409 surfaces as ErrSCMAPI rather than being
// promoted to ErrSCMConflict.
func TestGetReviewDecision_GraphQL409_NotPromotedToConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Conflict on graphql endpoint"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMAPI)

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if se.Kind == domain.ErrSCMConflict {
		t.Errorf("SCMError.Kind = %q, want NOT %q (graphql 409 must not promote to conflict)",
			se.Kind, domain.ErrSCMConflict)
	}
}

// TestGetReviewDecision_GraphQL200ErrorsAlreadyMerged_NotPromotedToConflict
// verifies that a GraphQL HTTP 200 response carrying an errors array
// with an "already merged" message surfaces as ErrSCMAPI rather than
// ErrSCMConflict. The read path must not inherit the duplicate-merge
// disposition reserved for the MergePR write path.
func TestGetReviewDecision_GraphQL200ErrorsAlreadyMerged_NotPromotedToConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"message":"Pull request was already merged in upstream"}]}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMAPI)

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if se.Kind == domain.ErrSCMConflict {
		t.Errorf("SCMError.Kind = %q, want NOT %q (graphql 200 errors[] with 'already merged' must not promote)",
			se.Kind, domain.ErrSCMConflict)
	}
	if !strings.Contains(se.Message, "already merged") {
		t.Errorf("SCMError.Message = %q, want substring %q", se.Message, "already merged")
	}
}

// TestGraphqlBasePath_RewriteTable exercises graphqlBasePath with the
// six endpoint configurations from the GHES rewrite rule specification.
func TestGraphqlBasePath_RewriteTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			name:     "github.com_no_trailing_slash",
			endpoint: "https://api.github.com",
			want:     "https://api.github.com",
		},
		{
			name:     "github.com_trailing_slash",
			endpoint: "https://api.github.com/",
			want:     "https://api.github.com",
		},
		{
			name:     "ghes_api_v3",
			endpoint: "https://ghe.example.com/api/v3",
			want:     "https://ghe.example.com/api",
		},
		{
			name:     "ghes_api_v3_trailing_slash",
			endpoint: "https://ghe.example.com/api/v3/",
			want:     "https://ghe.example.com/api",
		},
		{
			name:     "ghes_api_v3_subpath_not_rewritten",
			endpoint: "https://ghe.example.com/api/v3/foo",
			want:     "https://ghe.example.com/api/v3/foo",
		},
		{
			name:     "custom_mount_not_rewritten",
			endpoint: "https://ghe.example.com/custom-mount",
			want:     "https://ghe.example.com/custom-mount",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := newTestSCMAdapter(t, tt.endpoint)
			got := a.graphqlBasePath()
			if got != tt.want {
				t.Errorf("graphqlBasePath(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

// --- GetCIStatus tests ---

func TestGetCIStatus_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123","ref":"feature/x"},"draft":false,"mergeable_state":"clean"}`))
		case strings.Contains(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"success","statuses":[{"state":"success"}]}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"status":"completed","conclusion":"success"}]}`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetCIStatus: %v", err)
	}
	if status != "success" {
		t.Errorf("GetCIStatus() = %q, want %q", status, "success")
	}
}

func TestGetCIStatus_Pending(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123"},"draft":false,"mergeable_state":"clean"}`))
		case strings.Contains(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"pending","statuses":[{"state":"pending"}]}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetCIStatus: %v", err)
	}
	if status != "pending" {
		t.Errorf("GetCIStatus() = %q, want %q", status, "pending")
	}
}

func TestGetCIStatus_NoSignals_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123"},"draft":false,"mergeable_state":"clean"}`))
		case strings.Contains(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"","statuses":[]}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetCIStatus: %v", err)
	}
	if status != "" {
		t.Errorf("GetCIStatus() = %q, want empty (no signals)", status)
	}
}

func TestGetCIStatus_PaginatorErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		failingRoute string
	}{
		{name: "combined_status", failingRoute: "/status"},
		{name: "check_runs", failingRoute: "/check-runs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/pulls/"):
					_, _ = w.Write([]byte(`{"head":{"sha":"abc123"}}`))
				case strings.Contains(r.URL.Path, tt.failingRoute):
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
				case strings.Contains(r.URL.Path, "/status"):
					_, _ = w.Write([]byte(`{"state":"","statuses":[]}`))
				}
			}))
			defer srv.Close()

			a := newTestSCMAdapter(t, srv.URL)
			_, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
			assertSCMErrorKind(t, err, domain.ErrSCMTransport)
		})
	}
}

// combinedStatusPageJSON builds one combined-status page carrying n
// same-state entries, matching the wire shape [decodeCombinedStatusPage]
// unmarshals.
func combinedStatusPageJSON(t *testing.T, n int, state string) []byte {
	t.Helper()
	statuses := make([]map[string]string, n)
	for i := range statuses {
		statuses[i] = map[string]string{"state": state}
	}
	body, err := json.Marshal(map[string]any{"state": state, "statuses": statuses})
	if err != nil {
		t.Fatalf("marshal combined status page: %v", err)
	}
	return body
}

// checkRunsPageJSON builds one check-runs page carrying n same-status
// entries, matching the wire shape [decodeCheckRunsPage] unmarshals.
func checkRunsPageJSON(t *testing.T, n int, status, conclusion string) []byte {
	t.Helper()
	runs := make([]map[string]string, n)
	for i := range runs {
		runs[i] = map[string]string{"status": status, "conclusion": conclusion}
	}
	body, err := json.Marshal(map[string]any{"total_count": n, "check_runs": runs})
	if err != nil {
		t.Fatalf("marshal check runs page: %v", err)
	}
	return body
}

// TestGetCIStatus_Pagination_FailingSignalOnSecondPage covers a commit
// whose only failing combined status and only failing check run each sit
// past the first page: the full-page-size first page carries no failure,
// and the gate only sees the failure by walking to page two on both
// routes.
func TestGetCIStatus_Pagination_FailingSignalOnSecondPage(t *testing.T) {
	t.Parallel()

	var statusPage2Requested, checksPage2Requested atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123"},"draft":false,"mergeable_state":"clean"}`))
		case strings.Contains(r.URL.Path, "/status"):
			if page == "2" {
				statusPage2Requested.Store(true)
				_, _ = w.Write(combinedStatusPageJSON(t, 1, "failure"))
				return
			}
			_, _ = w.Write(combinedStatusPageJSON(t, 100, "success"))
		case strings.Contains(r.URL.Path, "/check-runs"):
			if page == "2" {
				checksPage2Requested.Store(true)
				_, _ = w.Write(checkRunsPageJSON(t, 1, "completed", "failure"))
				return
			}
			_, _ = w.Write(checkRunsPageJSON(t, 100, "completed", "success"))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetCIStatus: %v", err)
	}
	if status != "failing" {
		t.Errorf("GetCIStatus() = %q, want %q", status, "failing")
	}
	if !statusPage2Requested.Load() {
		t.Error("combined-status route: page 2 was never requested")
	}
	if !checksPage2Requested.Load() {
		t.Error("check-runs route: page 2 was never requested")
	}
}

// TestGetCIStatus_CombinedStatusStateValues covers the combined-status
// vocabulary: an "error" combined status fails the gate exactly like
// "failure", and a value the gate does not recognize defers rather than
// passing.
func TestGetCIStatus_CombinedStatusStateValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
		want  string
	}{
		{"error_state_fails", "error", "failing"},
		{"unrecognized_state_defers", "some-custom-state", "pending"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/pulls/"):
					_, _ = w.Write([]byte(`{"head":{"sha":"abc123"},"draft":false,"mergeable_state":"clean"}`))
				case strings.Contains(r.URL.Path, "/status"):
					_, _ = w.Write(combinedStatusPageJSON(t, 1, tt.state))
				case strings.Contains(r.URL.Path, "/check-runs"):
					_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
				}
			}))
			defer srv.Close()

			a := newTestSCMAdapter(t, srv.URL)
			status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
			if err != nil {
				t.Fatalf("GetCIStatus: %v", err)
			}
			if status != tt.want {
				t.Errorf("GetCIStatus() with combined status %q = %q, want %q", tt.state, status, tt.want)
			}
		})
	}
}

// --- GetMergeability tests ---

func TestGetMergeability_Clean(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"head":{"sha":"sha1","ref":"feature/x"},"draft":false,"mergeable_state":"clean"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if status.Mergeability != domain.MergeabilityClean {
		t.Errorf("GetMergeability().Mergeability = %q, want %q", status.Mergeability, domain.MergeabilityClean)
	}
	if status.Draft {
		t.Error("GetMergeability().Draft = true, want false")
	}
	if status.HeadSHA != "sha1" {
		t.Errorf("GetMergeability().HeadSHA = %q, want %q", status.HeadSHA, "sha1")
	}
}

// TestGetMergeability_Closed verifies that the pull request's state field,
// already decoded from the same object GetMergeability fetches, populates
// Closed with no additional request. GitHub reports a merged pull request
// as state "closed" too, so a merged PR's Closed is also true and Merged
// remains the discriminator for the closed-without-merge condition.
func TestGetMergeability_Closed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{"closed state maps to Closed true", "closed", true},
		{"open state maps to Closed false", "open", false},
		{"case-insensitive match", "CLOSED", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"head":{"sha":"sha-closed"},"draft":false,"mergeable_state":"clean","state":%q}`, tt.state)
			}))
			defer srv.Close()

			a := newTestSCMAdapter(t, srv.URL)
			status, err := a.GetMergeability(t.Context(), 1, "owner", "repo")
			if err != nil {
				t.Fatalf("GetMergeability: %v", err)
			}
			if status.Closed != tt.want {
				t.Errorf("GetMergeability().Closed = %v, want %v for state %q", status.Closed, tt.want, tt.state)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("HTTP request count = %d, want 1 (Closed rides the existing PR fetch)", got)
			}
		})
	}
}

func TestGetMergeability_Draft(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"head":{"sha":"sha-draft"},"draft":true,"mergeable_state":"clean"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if !status.Draft {
		t.Error("GetMergeability().Draft = false, want true for draft PR")
	}
}

// TestGetMergeability_BaseBranch verifies that GetMergeability reads the PR's
// real base branch from the base.ref field of the single pull-request object
// it already fetches, issuing no second HTTP request for the base.
func TestGetMergeability_BaseBranch(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.Contains(r.URL.Path, "/pulls/") {
			t.Errorf("unexpected request path %q; GetMergeability must only fetch the PR object", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"head":{"sha":"head-rel","ref":"feature/conflict"},"base":{"ref":"release/2.0"},"draft":false,"mergeable_state":"dirty"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}

	if status.BaseBranch != "release/2.0" {
		t.Errorf("GetMergeability().BaseBranch = %q, want %q", status.BaseBranch, "release/2.0")
	}
	if status.Mergeability != domain.MergeabilityDirty {
		t.Errorf("GetMergeability().Mergeability = %q, want %q", status.Mergeability, domain.MergeabilityDirty)
	}
	if status.HeadSHA != "head-rel" {
		t.Errorf("GetMergeability().HeadSHA = %q, want %q", status.HeadSHA, "head-rel")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("HTTP request count = %d, want 1 (base ref rides the existing PR fetch, no second request)", got)
	}
}

// pinnedMergeCommitOID is the live-verified oid testdata/graphql_merge_commit.json
// carries for sortie-ai/sortie pull request 772. A future recapture that
// moves the fixtures to a different pull request must update this value in
// the same change.
const pinnedMergeCommitOID = "52512b6736c84bd66169f80f3fa851cfb951a8c2"

// mergeabilityFixtureHandler builds an httptest handler that serves prBody
// on any request whose path contains "/pulls/" and graphqlBody on the
// literal path "/graphql", incrementing the matching atomic counter for
// each request observed.
func mergeabilityFixtureHandler(t *testing.T, prBody, graphqlBody []byte, restRequests, graphqlRequests *atomic.Int64) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/graphql":
			graphqlRequests.Add(1)
			_, _ = w.Write(graphqlBody)
		case strings.Contains(r.URL.Path, "/pulls/"):
			restRequests.Add(1)
			_, _ = w.Write(prBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// TestGetMergeability_Merged verifies that a merged pull request sources
// Merged from the REST pull-request object and MergeCommitSHA from a second,
// gated GraphQL read, issuing exactly one request on each surface.
func TestGetMergeability_Merged(t *testing.T) {
	t.Parallel()

	var restRequests, graphqlRequests atomic.Int64
	srv := httptest.NewServer(mergeabilityFixtureHandler(t,
		loadFixture(t, "pr_merged.json"), loadFixture(t, "graphql_merge_commit.json"),
		&restRequests, &graphqlRequests))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 772, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if !status.Merged {
		t.Error("GetMergeability().Merged = false, want true")
	}
	if status.MergeCommitSHA != pinnedMergeCommitOID {
		t.Errorf("GetMergeability().MergeCommitSHA = %q, want %q", status.MergeCommitSHA, pinnedMergeCommitOID)
	}
	if got := restRequests.Load(); got != 1 {
		t.Errorf("REST request count = %d, want 1", got)
	}
	if got := graphqlRequests.Load(); got != 1 {
		t.Errorf("GraphQL request count = %d, want 1", got)
	}
}

// TestGetMergeability_Open verifies that an open pull request reports an
// empty MergeCommitSHA with no GraphQL request issued at all: the request
// is gated on Merged, so the adapter never needs to distinguish GitHub's
// test-merge commit from the real one.
func TestGetMergeability_Open(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			t.Errorf("unexpected /graphql request for an open pull request")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadFixture(t, "pr_open.json"))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if status.Merged {
		t.Error("GetMergeability().Merged = true, want false for an open PR")
	}
	if status.MergeCommitSHA != "" {
		t.Errorf("GetMergeability().MergeCommitSHA = %q, want empty for an open PR", status.MergeCommitSHA)
	}
}

// TestGetMergeability_MergedNullMergeCommit verifies that a merged pull
// request whose GraphQL response reports a null mergeCommit yields a nil
// error, Merged true, and an empty MergeCommitSHA rather than an error.
func TestGetMergeability_MergedNullMergeCommit(t *testing.T) {
	t.Parallel()

	var restRequests, graphqlRequests atomic.Int64
	srv := httptest.NewServer(mergeabilityFixtureHandler(t,
		loadFixture(t, "pr_merged.json"), loadFixture(t, "graphql_merge_commit_null.json"),
		&restRequests, &graphqlRequests))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 772, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if !status.Merged {
		t.Error("GetMergeability().Merged = false, want true")
	}
	if status.MergeCommitSHA != "" {
		t.Errorf("GetMergeability().MergeCommitSHA = %q, want empty when GraphQL reports a null mergeCommit", status.MergeCommitSHA)
	}
}

// TestGetMergeability_GraphQLFailureModes covers every GraphQL failure mode
// that can occur once a merged pull request gates the second read: the
// error GetMergeability returns must unwrap to the *domain.SCMError kind
// the failure model specifies for each mode.
func TestGetMergeability_GraphQLFailureModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		respond  func(w http.ResponseWriter)
		wantKind domain.SCMErrorKind
	}{
		{
			name: "not_found_errors_array",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(loadFixture(t, "graphql_pull_request_not_found.json"))
			},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name: "malformed_json",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`not valid json {{{`))
			},
			wantKind: domain.ErrSCMPayload,
		},
		{
			name: "null_pull_request_no_errors",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}}}`))
			},
			wantKind: domain.ErrSCMPayload,
		},
		{
			name: "http_401",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			},
			wantKind: domain.ErrSCMAuth,
		},
		{
			name: "http_500",
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
			},
			wantKind: domain.ErrSCMTransport,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prBody := loadFixture(t, "pr_merged.json")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/graphql" {
					tt.respond(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(prBody)
			}))
			defer srv.Close()

			a := newTestSCMAdapter(t, srv.URL)
			_, err := a.GetMergeability(t.Context(), 772, "owner", "repo")
			assertSCMErrorKind(t, err, tt.wantKind)
		})
	}
}

// TestGetMergeability_MergeCommitQueryRequestShape verifies that the
// GraphQL POST body issued for a merged pull request carries the exact
// mergeCommitQuery constant text and the expected owner/repo/number
// variables.
func TestGetMergeability_MergeCommitQueryRequestShape(t *testing.T) {
	t.Parallel()

	prBody := loadFixture(t, "pr_merged.json")
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			body, _ := io.ReadAll(r.Body)
			gotBody = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, "graphql_merge_commit.json"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prBody)
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.GetMergeability(t.Context(), 772, "owner-x", "repo-y")
	if err != nil {
		t.Fatalf("GetMergeability() unexpected error: %v", err)
	}

	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("json.Unmarshal(request body) = %v, want nil error", err)
	}
	if payload.Query != mergeCommitQuery {
		t.Errorf("request query = %q, want mergeCommitQuery constant", payload.Query)
	}

	wantVars := map[string]any{
		"owner":  "owner-x",
		"repo":   "repo-y",
		"number": float64(772),
	}
	for k, wantV := range wantVars {
		gotV, ok := payload.Variables[k]
		if !ok {
			t.Errorf("request variables[%q] missing, want %v", k, wantV)
			continue
		}
		if gotV != wantV {
			t.Errorf("request variables[%q] = %v, want %v", k, gotV, wantV)
		}
	}
}

// TestGitHubMergeFixtures_Provenance pins the merged-path fixtures' load-
// bearing provenance: the merged-PR REST fixture carries no merge_commit_sha
// key, and both merged-path fixtures describe the same live-verified pull
// request. A future recapture that moves the fixtures to a different pull
// request must update both expected values here in the same change.
func TestGitHubMergeFixtures_Provenance(t *testing.T) {
	t.Parallel()

	t.Run("no_merge_commit_sha_key", func(t *testing.T) {
		t.Parallel()

		var pr map[string]any
		if err := json.Unmarshal(loadFixture(t, "pr_merged.json"), &pr); err != nil {
			t.Fatalf("json.Unmarshal(pr_merged.json) = %v, want nil error", err)
		}
		if _, present := pr["merge_commit_sha"]; present {
			t.Error(`pr_merged.json carries a "merge_commit_sha" key, want absent`)
		}
		if merged, _ := pr["merged"].(bool); !merged {
			t.Errorf(`pr_merged.json["merged"] = %v, want true`, pr["merged"])
		}
	})

	t.Run("pinned_identifiers", func(t *testing.T) {
		t.Parallel()

		var pr map[string]any
		if err := json.Unmarshal(loadFixture(t, "pr_merged.json"), &pr); err != nil {
			t.Fatalf("json.Unmarshal(pr_merged.json) = %v, want nil error", err)
		}
		if number, _ := pr["number"].(float64); number != 772 {
			t.Errorf(`pr_merged.json["number"] = %v, want 772`, pr["number"])
		}

		var envelope struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						MergeCommit struct {
							OID string `json:"oid"`
						} `json:"mergeCommit"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(loadFixture(t, "graphql_merge_commit.json"), &envelope); err != nil {
			t.Fatalf("json.Unmarshal(graphql_merge_commit.json) = %v, want nil error", err)
		}
		if got := envelope.Data.Repository.PullRequest.MergeCommit.OID; got != pinnedMergeCommitOID {
			t.Errorf("graphql_merge_commit.json oid = %q, want %q", got, pinnedMergeCommitOID)
		}
	})
}

// --- mapMergeableState tests ---

func TestMapMergeableState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  domain.MergeabilityState
	}{
		{"clean", domain.MergeabilityClean},
		{"CLEAN", domain.MergeabilityClean},
		{"unstable", domain.MergeabilityUnstable},
		{"blocked", domain.MergeabilityBlocked},
		{"behind", domain.MergeabilityBlocked},
		{"draft", domain.MergeabilityBlocked},
		{"dirty", domain.MergeabilityDirty},
		{"unknown", domain.MergeabilityUnknown},
		{"", domain.MergeabilityUnknown},
		{"computing", domain.MergeabilityUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := mapMergeableState(tt.input)
			if got != tt.want {
				t.Errorf("mapMergeableState(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- MergePR tests ---

func TestMergePR_Success(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sha":"merged-sha-1","merged":true,"message":"Pull Request successfully merged"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	result, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategySquash, "Title", "Body", "expected-sha")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if !result.Merged {
		t.Error("MergeResult.Merged = false, want true")
	}
	if result.SHA != "merged-sha-1" {
		t.Errorf("MergeResult.SHA = %q, want %q", result.SHA, "merged-sha-1")
	}
	if gotBody["merge_method"] != "squash" {
		t.Errorf("request merge_method = %q, want %q", gotBody["merge_method"], "squash")
	}
	if gotBody["sha"] != "expected-sha" {
		t.Errorf("request sha = %q, want %q", gotBody["sha"], "expected-sha")
	}
}

func TestMergePR_MergedFalse_ReturnsConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sha":"","merged":false,"message":"not merged"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategyMerge, "", "", "")
	assertSCMErrorKind(t, err, domain.ErrSCMConflict)
}

func TestMergePR_405_ReturnsSCMConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"Branch protection rules reject merge"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategySquash, "", "", "sha")
	assertSCMErrorKind(t, err, domain.ErrSCMConflict)
}

func TestMergePR_409_ReturnsSCMConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Head branch was modified. Review and try the merge again."}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategySquash, "", "", "sha")
	assertSCMErrorKind(t, err, domain.ErrSCMConflict)
}

// TestMergePR_409ThenConfirmedMerged_CarriesAlreadyMergedMarker covers the
// race a prior Sortie attempt or an external actor can win: the merge
// endpoint rejects with 409, and re-reading the PR confirms it is merged.
func TestMergePR_409ThenConfirmedMerged_CarriesAlreadyMergedMarker(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"Pull Request is not mergeable"}`))
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123"},"merged":true,"draft":false,"mergeable_state":"clean"}`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategySquash, "", "", "sha")
	adaptertest.AssertAlreadyMergedMarker(t, err)
}

// --- DeleteBranch tests ---

func TestDeleteBranch_Success(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	if err := a.DeleteBranch(t.Context(), "owner", "repo", "feature/done"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("request method = %q, want DELETE", gotMethod)
	}
	if !strings.Contains(gotPath, "feature") {
		t.Errorf("request path = %q; want path containing branch name", gotPath)
	}
}

func TestDeleteBranch_404_ReturnsSCMNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Reference does not exist"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	err := a.DeleteBranch(t.Context(), "owner", "repo", "already-gone")
	adaptertest.AssertBranchAbsentDisposition(t, err)
}

func TestDeleteBranch_409IsNotPromotedToConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"conflict"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	err := a.DeleteBranch(t.Context(), "owner", "repo", "feature/x")

	// Only a merge write path promotes 405/409 to ErrSCMConflict;
	// DeleteBranch is not on that path.
	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAPI)
}

// --- VerifyAutoMergeScopes tests ---

func TestVerifyAutoMergeScopes_LegacyRepoScope(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "repo, gist")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want nil (legacy repo scope covers all)", missing)
	}
	if len(scopes) == 0 {
		t.Error("scopes is empty; want at least [repo gist]")
	}
}

func TestVerifyAutoMergeScopes_MissingScope(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Provide contents:write but not pull_requests:write.
		w.Header().Set("X-OAuth-Scopes", "contents:write")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("missing is empty; want [pull_requests:write]")
	}
	if missing[0] != "pull_requests:write" {
		t.Errorf("missing[0] = %q, want %q", missing[0], "pull_requests:write")
	}
}

func TestVerifyAutoMergeScopes_AllFineGrainedScopes(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "pull_requests:write, contents:write")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want nil", missing)
	}
}

func TestVerifyAutoMergeScopes_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, _, err := a.VerifyAutoMergeScopes(t.Context(), false)
	if err == nil {
		t.Fatal("VerifyAutoMergeScopes err = nil; want error on 503")
	}
	assertSCMErrorKind(t, err, domain.ErrSCMTransport)
}

// TestVerifyAutoMergeScopes_EmptyHeaderReturnsUnableToVerify verifies that an
// empty X-OAuth-Scopes header is reported as "unable to verify" (nil scopes,
// nil missing, nil error) rather than as every required scope missing.
// Fine-grained PATs and GitHub App installation tokens commonly omit content
// from this header.
func TestVerifyAutoMergeScopes_EmptyHeaderReturnsUnableToVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if scopes != nil {
		t.Errorf("scopes = %v, want nil (unable to verify)", scopes)
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil (unable to verify)", missing)
	}
}

// TestVerifyAutoMergeScopes_AbsentHeaderReturnsUnableToVerify verifies that a
// response with no X-OAuth-Scopes header at all is treated identically to an
// empty header: nil scopes, nil missing, nil error.
func TestVerifyAutoMergeScopes_AbsentHeaderReturnsUnableToVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if scopes != nil {
		t.Errorf("scopes = %v, want nil (unable to verify)", scopes)
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil (unable to verify)", missing)
	}
}

// TestVerifyAutoMergeScopes_WhitespaceOnlyHeaderReturnsUnableToVerify verifies
// that a header containing only whitespace is treated as empty.
func TestVerifyAutoMergeScopes_WhitespaceOnlyHeaderReturnsUnableToVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "   ")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if scopes != nil {
		t.Errorf("scopes = %v, want nil (unable to verify)", scopes)
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil (unable to verify)", missing)
	}
}

// --- splitScopes tests ---

func TestSplitScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"repo", []string{"repo"}},
		{"repo, gist", []string{"repo", "gist"}},
		{"pull_requests:write, contents:write", []string{"pull_requests:write", "contents:write"}},
		{"  repo ,  gist  ", []string{"repo", "gist"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := splitScopes(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitScopes(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitScopes(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}
