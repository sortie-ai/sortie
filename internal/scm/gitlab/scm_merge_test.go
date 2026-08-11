package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// decodeRequestBody decodes r's JSON body into v, failing the test if the
// decode itself fails.
func decodeRequestBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}

// mergeRequestFixture returns a JSON-encoded merge request object built
// from a documented baseline shape, with overrides applied on top. This
// lets the detailed_merge_status and head_pipeline enumeration tables
// vary one field at a time without a nearly-duplicate testdata file per
// case.
func mergeRequestFixture(t *testing.T, overrides map[string]any) []byte {
	t.Helper()

	fields := map[string]any{
		"iid":                   testPRNumber,
		"state":                 "opened",
		"draft":                 false,
		"sha":                   "d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0",
		"source_branch":         "feature/scm-reads",
		"target_branch":         "main",
		"merge_commit_sha":      "",
		"detailed_merge_status": "mergeable",
		"head_pipeline":         nil,
	}
	maps.Copy(fields, overrides)

	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal merge request fixture: %v", err)
	}
	return data
}

func serveJSON(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

// --- GetMergeability: detailed_merge_status table ---

func TestGetMergeability_StatusTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    string
		wantState domain.MergeabilityState
		wantWarn  bool
	}{
		{"mergeable maps to clean", "mergeable", domain.MergeabilityClean, false},
		{"conflict maps to dirty", "conflict", domain.MergeabilityDirty, false},
		{"unchecked maps to unknown", "unchecked", domain.MergeabilityUnknown, false},
		{"checking maps to unknown", "checking", domain.MergeabilityUnknown, false},
		{"preparing maps to unknown", "preparing", domain.MergeabilityUnknown, false},
		{"approvals_syncing maps to unknown", "approvals_syncing", domain.MergeabilityUnknown, false},
		{"an unrecognized value maps to blocked and logs a warning", "need_rebase", domain.MergeabilityBlocked, true},
		{"an empty value maps to blocked and logs a warning", "", domain.MergeabilityBlocked, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := mergeRequestFixture(t, map[string]any{"detailed_merge_status": tt.status})
			srv := serveJSON(t, fixture)
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			log, buf := newCapturingLogger()
			adapter.log = log

			got, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
			if err != nil {
				t.Fatalf("GetMergeability: unexpected error: %v", err)
			}
			if got.Mergeability != tt.wantState {
				t.Errorf("GetMergeability().Mergeability = %q, want %q", got.Mergeability, tt.wantState)
			}
			if got.Mergeability == domain.MergeabilityUnstable {
				t.Errorf("GetMergeability().Mergeability = %q, want it never to be MergeabilityUnstable", got.Mergeability)
			}

			logOutput := buf.String()
			gotWarn := strings.Contains(logOutput, "unrecognized gitlab detailed_merge_status value")
			if gotWarn != tt.wantWarn {
				t.Errorf("WARN logged = %v, want %v (log output: %q)", gotWarn, tt.wantWarn, logOutput)
			}
			if tt.wantWarn && !strings.Contains(logOutput, "detailed_merge_status="+quoteIfEmpty(tt.status)) {
				t.Errorf("log output = %q, want it to carry the observed value as an attribute", logOutput)
			}
		})
	}
}

// quoteIfEmpty mirrors how slog's TextHandler renders an empty string
// attribute value, so the WARN assertion above can match it literally.
func quoteIfEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

// --- GetMergeability: merged and open dispositions ---

func TestGetMergeability_MergedDisposition(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "mr_merged.json")
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("GetMergeability: unexpected error: %v", err)
	}

	if !got.Merged {
		t.Errorf("GetMergeability().Merged = %v, want true", got.Merged)
	}
	const wantSHA = "9f8e7d6c5b4a392817061524334251627384950a"
	if got.MergeCommitSHA != wantSHA {
		t.Errorf("GetMergeability().MergeCommitSHA = %q, want %q", got.MergeCommitSHA, wantSHA)
	}
}

func TestGetMergeability_OpenDispositionIgnoresMergeCommitSHA(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "mr_open_with_commit_sha.json")
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("GetMergeability: unexpected error: %v", err)
	}

	if got.Merged {
		t.Errorf("GetMergeability().Merged = %v, want false (state is opened)", got.Merged)
	}
	if got.MergeCommitSHA != "" {
		t.Errorf("GetMergeability().MergeCommitSHA = %q, want empty (the fixture's non-empty value must be gated on Merged)", got.MergeCommitSHA)
	}
}

// --- GetMergeability: field population ---

func TestGetMergeability_FieldPopulation(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "mr_basic.json")
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("GetMergeability: unexpected error: %v", err)
	}

	if got.HeadSHA != "d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0" {
		t.Errorf("GetMergeability().HeadSHA = %q, want the fixture's sha", got.HeadSHA)
	}
	if got.BranchName != "feature/scm-reads" {
		t.Errorf("GetMergeability().BranchName = %q, want %q", got.BranchName, "feature/scm-reads")
	}
	if got.BaseBranch != "main" {
		t.Errorf("GetMergeability().BaseBranch = %q, want %q", got.BaseBranch, "main")
	}
	if got.Draft {
		t.Errorf("GetMergeability().Draft = %v, want false", got.Draft)
	}
	if got.ReviewDecision != "" {
		t.Errorf("GetMergeability().ReviewDecision = %q, want empty (left unset)", got.ReviewDecision)
	}
	if got.CIConclusion != "" {
		t.Errorf("GetMergeability().CIConclusion = %q, want empty (left unset)", got.CIConclusion)
	}

	t.Run("draft true propagates", func(t *testing.T) {
		t.Parallel()

		draftFixture := mergeRequestFixture(t, map[string]any{"draft": true})
		draftSrv := serveJSON(t, draftFixture)
		defer draftSrv.Close()

		draftAdapter := mustSCMAdapter(t, draftSrv.URL)
		got, err := draftAdapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetMergeability: unexpected error: %v", err)
		}
		if !got.Draft {
			t.Errorf("GetMergeability().Draft = %v, want true", got.Draft)
		}
	})
}

// --- GetCIStatus: head_pipeline mapping ---

func TestGetCIStatus_HeadPipelineMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		overrides map[string]any
		want      string
		wantWarn  bool
		wantAttr  string // pipeline_status attribute value; checked only when wantWarn is true
	}{
		{"success maps to CIGateSuccess", map[string]any{"head_pipeline": map[string]any{"status": "success"}}, "success", false, ""},
		{"failed maps to CIGateFailing", map[string]any{"head_pipeline": map[string]any{"status": "failed"}}, "failing", false, ""},
		{"canceled maps to CIGateFailing", map[string]any{"head_pipeline": map[string]any{"status": "canceled"}}, "failing", false, ""},
		{"an unrecognized status defers to CIGatePending and logs a warning", map[string]any{"head_pipeline": map[string]any{"status": "expired"}}, "pending", true, "expired"},
		{"an empty head_pipeline status defers to CIGatePending and logs a warning", map[string]any{"head_pipeline": map[string]any{"status": ""}}, "pending", true, ""},
		{"a nil head_pipeline maps to CIGateAbsent (empty string)", map[string]any{"head_pipeline": nil}, "", false, ""},
		{"a populated top-level pipeline is never substituted for a nil head_pipeline", map[string]any{"head_pipeline": nil, "pipeline": map[string]any{"status": "success"}}, "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := mergeRequestFixture(t, tt.overrides)
			srv := serveJSON(t, fixture)
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			log, buf := newCapturingLogger()
			adapter.log = log

			got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
			if err != nil {
				t.Fatalf("GetCIStatus: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetCIStatus() = %q, want %q", got, tt.want)
			}

			logOutput := buf.String()
			gotWarn := strings.Contains(logOutput, "unrecognized gitlab pipeline status")
			if gotWarn != tt.wantWarn {
				t.Errorf("WARN logged = %v, want %v (log output: %q)", gotWarn, tt.wantWarn, logOutput)
			}
			if tt.wantWarn {
				if n := strings.Count(logOutput, "unrecognized gitlab pipeline status"); n != 1 {
					t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", "unrecognized gitlab pipeline status", n, logOutput)
				}
				if !strings.Contains(logOutput, "pipeline_status="+quoteIfEmpty(tt.wantAttr)) {
					t.Errorf("log output = %q, want it to carry the observed value as an attribute", logOutput)
				}
			}
		})
	}
}

// --- GetCIStatus: agreement with the CI provider's own normalization over
// the full 13-value pipeline-status enum ---

func TestGetCIStatus_PipelineStatusEnumAgreement(t *testing.T) {
	t.Parallel()

	statuses := []string{
		"created", "waiting_for_resource", "preparing", "waiting_for_callback",
		"pending", "running", "success", "failed", "canceling", "canceled",
		"skipped", "scheduled", "manual",
	}

	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			conclusion, runStatus, recognized := mapJobOutcome(status, false)
			if !recognized {
				t.Fatalf("mapJobOutcome(%q, false) recognized = false, want true (every enumerated pipeline status must be recognized by the CI provider's own normalization)", status)
			}
			runs := []domain.CheckRun{
				{Name: "job-1", Status: runStatus, Conclusion: conclusion},
				{Name: "job-2", Status: runStatus, Conclusion: conclusion},
			}
			want := string(scmcore.MergeGate(runs))

			fixture := mergeRequestFixture(t, map[string]any{"head_pipeline": map[string]any{"status": status}})
			srv := serveJSON(t, fixture)
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			log, buf := newCapturingLogger()
			adapter.log = log

			got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
			if err != nil {
				t.Fatalf("GetCIStatus: unexpected error: %v", err)
			}

			if status == "manual" {
				if got != string(scmcore.CIGatePending) {
					t.Errorf("GetCIStatus() for pipeline status %q = %q, want %q", status, got, string(scmcore.CIGatePending))
				}
				if got == want {
					t.Errorf("GetCIStatus() for pipeline status %q = %q, want it to differ from the computed fold %q (manual is a deliberate divergence)", status, got, want)
				}
			} else if got != want {
				t.Errorf("GetCIStatus() for pipeline status %q = %q, want %q (the fold of mapJobOutcome(%q, false) through scmcore.MergeGate)", status, got, want, status)
			}

			logOutput := buf.String()
			if strings.Contains(logOutput, "unrecognized gitlab pipeline status") {
				t.Errorf("GetCIStatus() for pipeline status %q logged an unrecognized-status WARN, want none (log output: %q)", status, logOutput)
			}
		})
	}
}

// --- GetCIStatus: request budget ---

func TestGetCIStatus_SingleRequestRouteScope(t *testing.T) {
	t.Parallel()

	fixture := mergeRequestFixture(t, map[string]any{"head_pipeline": map[string]any{"status": "success"}})

	wantSuffix := "/merge_requests/" + strconv.Itoa(testPRNumber)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.Contains(r.URL.Path, "/statuses") || strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") {
			t.Errorf("unexpected request to %s, want only the merge-request read", r.URL.Path)
		}
		if !strings.HasSuffix(r.URL.Path, wantSuffix) {
			t.Errorf("request path = %q, want it to end with %q", r.URL.Path, wantSuffix)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	if _, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
		t.Fatalf("GetCIStatus: unexpected error: %v", err)
	}

	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1 (GetCIStatus must issue exactly one request)", n)
	}
}

// --- Error status coverage ---

func TestGetMergeability_ErrorStatuses(t *testing.T) {
	t.Parallel()

	runSCMErrorStatusTable(t, func(t *testing.T, srv *httptest.Server) error {
		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
		return err
	})

	t.Run("malformed 2xx body is a payload error, not transport", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})

	t.Run("failure returns the zero PRMergeStatus", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_404_issue.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err == nil {
			t.Fatal("GetMergeability: want an error")
		}
		if got != (domain.PRMergeStatus{}) {
			t.Errorf("GetMergeability() result = %+v, want the zero value on failure", got)
		}
	})
}

func TestGetCIStatus_ErrorStatuses(t *testing.T) {
	t.Parallel()

	runSCMErrorStatusTable(t, func(t *testing.T, srv *httptest.Server) error {
		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		return err
	})

	t.Run("malformed 2xx body is a payload error, not transport", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}

// --- MergePR: success, strategy encoding, and request shape (AC3, AC13, R35) ---

func TestMergePR_StrategyEncodingAndRequestShape(t *testing.T) {
	t.Parallel()

	const (
		wantSHA            = "sha-expected-head-001"
		wantMergeCommitSHA = "9f8e7d6c5b4a392817061524334251627384950a"
		commitTitle        = "Merge feature branch"
		commitMessage      = "Closes the review cycle."
	)
	wantMessage := commitTitle + "\n\n" + commitMessage

	tests := []struct {
		name       string
		strategy   domain.MergeStrategy
		wantSquash bool
	}{
		{"merge strategy sends merge_commit_message", domain.StrategyMerge, false},
		{"squash strategy sends squash true and squash_commit_message", domain.StrategySquash, true},
		{"rebase strategy sends merge_commit_message, governed by project merge_method", domain.StrategyRebase, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := mergeRequestFixture(t, map[string]any{
				"state":            "merged",
				"merge_commit_sha": wantMergeCommitSHA,
			})

			var gotMethod, gotPath string
			var gotBody gitlabMergeAccept
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.EscapedPath()
				decodeRequestBody(t, r, &gotBody)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture)
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			got, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, tt.strategy, commitTitle, commitMessage, wantSHA)
			if err != nil {
				t.Fatalf("MergePR: unexpected error: %v", err)
			}

			if gotMethod != http.MethodPut {
				t.Errorf("request method = %q, want %q", gotMethod, http.MethodPut)
			}
			wantPath := "/api/v4/projects/" + projectPath(scmOwner, scmRepo) + "/merge_requests/" + strconv.Itoa(testPRNumber) + "/merge"
			if gotPath != wantPath {
				t.Errorf("request path = %q, want %q", gotPath, wantPath)
			}
			if gotBody.SHA != wantSHA {
				t.Errorf("request body sha = %q, want %q", gotBody.SHA, wantSHA)
			}
			if gotBody.Squash != tt.wantSquash {
				t.Errorf("request body squash = %v, want %v", gotBody.Squash, tt.wantSquash)
			}
			if tt.wantSquash {
				if gotBody.SquashCommitMessage != wantMessage {
					t.Errorf("request body squash_commit_message = %q, want %q", gotBody.SquashCommitMessage, wantMessage)
				}
				if gotBody.MergeCommitMessage != "" {
					t.Errorf("request body merge_commit_message = %q, want empty for the squash strategy", gotBody.MergeCommitMessage)
				}
			} else {
				if gotBody.MergeCommitMessage != wantMessage {
					t.Errorf("request body merge_commit_message = %q, want %q", gotBody.MergeCommitMessage, wantMessage)
				}
				if gotBody.SquashCommitMessage != "" {
					t.Errorf("request body squash_commit_message = %q, want empty for the %s strategy", gotBody.SquashCommitMessage, tt.strategy)
				}
			}

			want := domain.MergeResult{SHA: wantMergeCommitSHA, Merged: true}
			if got != want {
				t.Errorf("MergePR(...) = %+v, want %+v", got, want)
			}
		})
	}
}

func TestMergePR_RebaseStrategyLogsGovernanceWarning(t *testing.T) {
	t.Parallel()

	fixture := mergeRequestFixture(t, map[string]any{
		"state":            "merged",
		"merge_commit_sha": "9f8e7d6c5b4a392817061524334251627384950a",
	})
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	log, buf := newCapturingLogger()
	adapter.log = log

	if _, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyRebase, "", "", "sha-expected-head-001"); err != nil {
		t.Fatalf("MergePR: unexpected error: %v", err)
	}

	logOutput := buf.String()
	const wantWarn = "gitlab rebase strategy is governed by the project merge method"
	if got := strings.Count(logOutput, wantWarn); got != 1 {
		t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", wantWarn, got, logOutput)
	}
}

func TestMergePR_ResponseStateNotMerged(t *testing.T) {
	t.Parallel()

	fixture := mergeRequestFixture(t, map[string]any{"state": "opened"})
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

	se := assertMergeConflict(t, err)
	if strings.Contains(strings.ToLower(se.Message), "already merged") {
		t.Errorf("SCMError.Message = %q, want it NOT to contain %q (a 200 whose state is not merged carries no marker)", se.Message, "already merged")
	}
	if got != (domain.MergeResult{}) {
		t.Errorf("MergePR(...) result = %+v, want the zero value", got)
	}
}

func TestMergePR_PayloadValidation(t *testing.T) {
	t.Parallel()

	t.Run("an empty expectedHeadSHA returns ErrSCMPayload with no request issued", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "")
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
		if got != (domain.MergeResult{}) {
			t.Errorf("MergePR(...) result = %+v, want the zero value", got)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("requests = %d, want 0 (an empty expectedHeadSHA must not reach the network)", n)
		}
	})

	t.Run("an unsupported strategy returns ErrSCMPayload with no request issued", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.MergeStrategy("bogus"), "", "", "sha-expected-head-001")
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
		if got != (domain.MergeResult{}) {
			t.Errorf("MergePR(...) result = %+v, want the zero value", got)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("requests = %d, want 0 (an unsupported strategy must not reach the network)", n)
		}
	})
}

// --- MergePR: conflict promotion, already-merged marker, and auth enrichment (AC4, AC5, AC10, R10-R14) ---

func TestMergePR_ConflictPromotion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		rejectionBody []byte
	}{
		{"405 rejection", http.StatusMethodNotAllowed, loadFixture(t, "error_405.json")},
		{"409 rejection", http.StatusConflict, loadFixture(t, "error_409.json")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rereadFixture := mergeRequestFixture(t, map[string]any{"state": "opened"})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
					w.WriteHeader(tt.status)
					_, _ = w.Write(tt.rejectionBody)
				case r.Method == http.MethodGet:
					_, _ = w.Write(rereadFixture)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			_, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

			se := assertMergeConflict(t, err)
			if strings.Contains(strings.ToLower(se.Message), "already merged") {
				t.Errorf("SCMError.Message = %q, want it NOT to contain %q (the re-read shows the merge request still open)", se.Message, "already merged")
			}
		})
	}
}

func TestMergePR_AlreadyMergedMarker(t *testing.T) {
	t.Parallel()

	statuses := []struct {
		name          string
		status        int
		rejectionBody []byte
	}{
		{"405 rejection", http.StatusMethodNotAllowed, loadFixture(t, "error_405.json")},
		{"409 rejection", http.StatusConflict, loadFixture(t, "error_409.json")},
	}

	for _, tt := range statuses {
		t.Run(tt.name+" plus a merged re-read carries the already-merged marker", func(t *testing.T) {
			t.Parallel()

			rereadFixture := loadFixture(t, "mr_merged.json")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
					w.WriteHeader(tt.status)
					_, _ = w.Write(tt.rejectionBody)
				case r.Method == http.MethodGet:
					_, _ = w.Write(rereadFixture)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			_, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

			adaptertest.AssertAlreadyMergedMarker(t, err)
		})
	}

	t.Run("a failed re-read leaves the conflict without the already-merged marker", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write(loadFixture(t, "error_409.json"))
			case r.Method == http.MethodGet:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write(loadFixture(t, "error_500.json"))
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

		se := assertMergeConflict(t, err)
		if strings.Contains(strings.ToLower(se.Message), "already merged") {
			t.Errorf("SCMError.Message = %q, want it NOT to contain %q (a failed re-read must not synthesize the marker)", se.Message, "already merged")
		}
	})
}

func TestMergePR_AuthEnrichment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   []byte
	}{
		{"401 from the merge route", http.StatusUnauthorized, loadFixture(t, "error_401.json")},
		{"403 from the merge route", http.StatusForbidden, loadFixture(t, "error_403.json")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write(tt.body)
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			_, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")
			adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAuth)

			var se *domain.SCMError
			if !errors.As(err, &se) {
				t.Fatalf("error type = %T, want *domain.SCMError", err)
			}
			if !strings.Contains(se.Message, "credential is invalid") {
				t.Errorf("SCMError.Message = %q, want it to name the invalid-credential cause", se.Message)
			}
			if !strings.Contains(se.Message, "may not merge into the target branch") {
				t.Errorf("SCMError.Message = %q, want it to name the branch-protection-refusal cause", se.Message)
			}
		})
	}
}

// assertMergeConflict fails the test if err is not a *domain.SCMError of
// kind ErrSCMConflict, returning the typed error for further message
// inspection.
func assertMergeConflict(t *testing.T, err error) *domain.SCMError {
	t.Helper()
	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMConflict)
	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	return se
}

// --- DeleteBranch (AC6) ---

func TestDeleteBranch_AbsentBranchDisposition(t *testing.T) {
	t.Parallel()

	errorBody := loadFixture(t, "error_404_project.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(errorBody)
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	err := adapter.DeleteBranch(context.Background(), scmOwner, scmRepo, "feature/gone")
	adaptertest.AssertBranchAbsentDisposition(t, err)
}

func TestDeleteBranch_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	if err := adapter.DeleteBranch(context.Background(), scmOwner, scmRepo, "feature/done"); err != nil {
		t.Fatalf("DeleteBranch: unexpected error: %v", err)
	}
}

func TestDeleteBranch_SlashBearingNameEncoded(t *testing.T) {
	t.Parallel()

	var gotPath, gotEscapedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEscapedPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	if err := adapter.DeleteBranch(context.Background(), scmOwner, scmRepo, "feature/needs-slash"); err != nil {
		t.Fatalf("DeleteBranch: unexpected error: %v", err)
	}

	wantPath := "/api/v4/projects/" + scmOwner + "/" + scmRepo + "/repository/branches/feature/needs-slash"
	if gotPath != wantPath {
		t.Errorf("decoded request path = %q, want %q", gotPath, wantPath)
	}
	const wantEscapedSuffix = "/repository/branches/feature%2Fneeds-slash"
	if !strings.HasSuffix(gotEscapedPath, wantEscapedSuffix) {
		t.Errorf("escaped request path = %q, want it to end with %q (the branch slash percent-encoded)", gotEscapedPath, wantEscapedSuffix)
	}
}

// --- Cross-kind conflict isolation (AC9, R17, R36) ---

func TestGitLabSCM_ConflictKindBelongsToMergeOnly(t *testing.T) {
	t.Parallel()

	statuses := []struct {
		name          string
		status        int
		rejectionBody []byte
	}{
		{"405", http.StatusMethodNotAllowed, loadFixture(t, "error_405.json")},
		{"409", http.StatusConflict, loadFixture(t, "error_409.json")},
	}

	for _, st := range statuses {
		t.Run(st.name+" on DeleteBranch", func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(st.status)
				_, _ = w.Write(st.rejectionBody)
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			err := adapter.DeleteBranch(context.Background(), scmOwner, scmRepo, "feature/blocked-by-race")
			adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAPI)
		})

		t.Run(st.name+" on RemoveLabel", func(t *testing.T) {
			t.Parallel()

			mrFixture := mergeRequestFixture(t, map[string]any{"labels": []string{"Sortie:Review"}})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					_, _ = w.Write(mrFixture)
				case http.MethodPut:
					w.WriteHeader(st.status)
					_, _ = w.Write(st.rejectionBody)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			err := adapter.RemoveLabel(context.Background(), testPRNumber, scmOwner, scmRepo, "sortie:review")
			adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAPI)
		})
	}
}

// --- MergePR: message composition and non-conflict failure classes ---

func TestComposeMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		title string
		body  string
		want  string
	}{
		{"both sides join with a blank line", "Merge feature", "Closes the cycle.", "Merge feature\n\nCloses the cycle."},
		{"an empty body yields the title alone", "Merge feature", "", "Merge feature"},
		{"an empty title yields the body alone", "", "Closes the cycle.", "Closes the cycle."},
		{"both empty leaves the platform default", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := composeMessage(tt.title, tt.body); got != tt.want {
				t.Errorf("composeMessage(%q, %q) = %q, want %q", tt.title, tt.body, got, tt.want)
			}
		})
	}
}

func TestMergePR_NonConflictErrorPassthrough(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(loadFixture(t, "error_500.json"))
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMTransport)
	if got != (domain.MergeResult{}) {
		t.Errorf("MergePR(...) result = %+v, want the zero value", got)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1 (a non-conflict rejection must not trigger a merge request re-read)", n)
	}
}

func TestMergePR_MalformedSuccessBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state": "merged"`))
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.MergePR(context.Background(), testPRNumber, scmOwner, scmRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	if got != (domain.MergeResult{}) {
		t.Errorf("MergePR(...) result = %+v, want the zero value", got)
	}
}
