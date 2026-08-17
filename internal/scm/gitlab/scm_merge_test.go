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

// mrBaselineSHA is mergeRequestFixture's own default "sha" value. A
// head_pipeline override that wants the status mapping exercised, rather
// than the head-comparison deferral, sets its own "sha" to this value;
// headPipeline does that by default.
const mrBaselineSHA = "d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0"

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
		"sha":                   mrBaselineSHA,
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

// headPipeline returns a head_pipeline override map whose "sha" defaults
// to mrBaselineSHA, the merge request fixture's own sha, so the fixture
// exercises the status mapping rather than the head-comparison deferral,
// unless fields already sets "sha" itself.
func headPipeline(fields map[string]any) map[string]any {
	out := map[string]any{"sha": mrBaselineSHA}
	maps.Copy(out, fields)
	return out
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
		{"success maps to CIGateSuccess", map[string]any{"head_pipeline": headPipeline(map[string]any{"status": "success"})}, "success", false, ""},
		{"failed maps to CIGateFailing", map[string]any{"head_pipeline": headPipeline(map[string]any{"status": "failed"})}, "failing", false, ""},
		{"canceled maps to CIGateFailing", map[string]any{"head_pipeline": headPipeline(map[string]any{"status": "canceled"})}, "failing", false, ""},
		{"an unrecognized status defers to CIGatePending and logs a warning", map[string]any{"head_pipeline": headPipeline(map[string]any{"status": "expired"})}, "pending", true, "expired"},
		{"an empty head_pipeline status defers to CIGatePending and logs a warning", map[string]any{"head_pipeline": headPipeline(map[string]any{"status": ""})}, "pending", true, ""},
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

			var srv *httptest.Server
			if status == "manual" {
				mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
				srv = mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(manualJobSetBody(t))
				})
			} else {
				fixture := mergeRequestFixture(t, map[string]any{"head_pipeline": headPipeline(map[string]any{"status": status})})
				srv = serveJSON(t, fixture)
			}
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			log, buf := newCapturingLogger()
			adapter.log = log

			got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
			if err != nil {
				t.Fatalf("GetCIStatus: unexpected error: %v", err)
			}

			if got != want {
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

	wantSuffix := "/merge_requests/" + strconv.Itoa(testPRNumber)

	run := func(t *testing.T, overrides map[string]any) {
		t.Helper()

		fixture := mergeRequestFixture(t, overrides)

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

	statuses := []string{
		"created", "waiting_for_resource", "preparing", "waiting_for_callback",
		"pending", "running", "success", "failed", "canceling", "canceled",
		"skipped", "scheduled",
	}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			run(t, map[string]any{"head_pipeline": headPipeline(map[string]any{"status": status})})
		})
	}

	t.Run("absent head_pipeline", func(t *testing.T) {
		t.Parallel()
		run(t, map[string]any{"head_pipeline": nil})
	})
}

// --- GetCIStatus: head-comparison deferral ---

// divergentPipelineSHA names a commit that is never the merge request's
// own sha in any fixture in this file, so a head_pipeline carrying it
// always describes a superseded commit.
const divergentPipelineSHA = "1111222233334444555566667777888899990000"

// assertHeadComparisonWarn asserts that logOutput carries exactly one
// deferral WARN naming mrSHA, pipelineSHA, and pipelineID as its
// merge_request_sha, head_pipeline_sha, and pipeline_id attributes.
func assertHeadComparisonWarn(t *testing.T, logOutput, mrSHA, pipelineSHA string, pipelineID int64) {
	t.Helper()

	const msg = "head pipeline does not describe the merge request head"
	if n := strings.Count(logOutput, msg); n != 1 {
		t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", msg, n, logOutput)
	}
	if want := "merge_request_sha=" + mrSHA; !strings.Contains(logOutput, want) {
		t.Errorf("log output = %q, want it to carry %q", logOutput, want)
	}
	if want := "head_pipeline_sha=" + quoteIfEmpty(pipelineSHA); !strings.Contains(logOutput, want) {
		t.Errorf("log output = %q, want it to carry %q", logOutput, want)
	}
	if want := "pipeline_id=" + strconv.FormatInt(pipelineID, 10); !strings.Contains(logOutput, want) {
		t.Errorf("log output = %q, want it to carry %q", logOutput, want)
	}
}

func TestGetCIStatus_HeadComparisonDeferral(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, overrides map[string]any, wantPipelineSHA string) {
		t.Helper()

		fixture := mergeRequestFixture(t, overrides)

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if strings.Contains(r.URL.Path, "/statuses") {
				t.Errorf("unexpected request to %s, want no statuses request", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "pending" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "pending")
		}
		if got == "" || got == "success" {
			t.Errorf("GetCIStatus() = %q, want neither the empty string nor %q", got, "success")
		}
		if n := requests.Load(); n != 1 {
			t.Errorf("requests = %d, want 1", n)
		}
		assertHeadComparisonWarn(t, buf.String(), mrBaselineSHA, wantPipelineSHA, manualPipelineID)
	}

	statuses := []string{"success", "skipped", "failed", "canceled", "manual"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			run(t, map[string]any{"head_pipeline": map[string]any{
				"status": status,
				"id":     manualPipelineID,
				"sha":    divergentPipelineSHA,
			}}, divergentPipelineSHA)
		})
	}

	t.Run("an empty pipeline sha against a non-empty merge-request sha also defers, not a defensive pass-through", func(t *testing.T) {
		t.Parallel()
		run(t, map[string]any{"head_pipeline": map[string]any{
			"status": "success",
			"id":     manualPipelineID,
			"sha":    "",
		}}, "")
	})
}

// --- GetCIStatus: absent head_pipeline ---

func TestGetCIStatus_NilHeadPipelineIssuesNoWarning(t *testing.T) {
	t.Parallel()

	fixture := mergeRequestFixture(t, map[string]any{"head_pipeline": nil})

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	log, buf := newCapturingLogger()
	adapter.log = log

	got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("GetCIStatus: unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("GetCIStatus() = %q, want empty string", got)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
	if logOutput := buf.String(); strings.Contains(logOutput, "level=WARN") {
		t.Errorf("log output = %q, want no WARN of any kind", logOutput)
	}
}

// --- GetCIStatus: missing merge-request sha ---

func TestGetCIStatus_MissingMergeRequestSHA(t *testing.T) {
	t.Parallel()

	fixture := mergeRequestFixture(t, map[string]any{
		"sha": "",
		"head_pipeline": map[string]any{
			"status": "success",
			"id":     manualPipelineID,
			"sha":    manualPipelineSHA,
		},
	})

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
	if got != "" {
		t.Errorf("GetCIStatus() = %q, want empty string", got)
	}
	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)

	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// --- GetCIStatus: case-insensitive head comparison ---

func TestGetCIStatus_HeadComparisonCaseInsensitive(t *testing.T) {
	t.Parallel()

	t.Run("a non-manual status maps normally despite a case difference", func(t *testing.T) {
		t.Parallel()

		fixture := mergeRequestFixture(t, map[string]any{
			"head_pipeline": map[string]any{
				"status": "success",
				"sha":    strings.ToUpper(mrBaselineSHA),
			},
		})
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "success" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "success")
		}
		if logOutput := buf.String(); strings.Contains(logOutput, "head pipeline does not describe the merge request head") {
			t.Errorf("log output = %q, want no deferral WARN", logOutput)
		}
	})

	t.Run("a manual status still runs the job-set read", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, strings.ToUpper(manualPipelineSHA), manualPipelineID, map[string]any{"sha": manualPipelineSHA})
		statusesBody := manualJobSetBody(t)

		var statusesRequested atomic.Bool
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			statusesRequested.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(statusesBody)
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "success" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "success")
		}
		if !statusesRequested.Load() {
			t.Error("no /statuses request issued, want the job-set read to run despite the case difference")
		}
	})
}

// --- GetCIStatus: generated merge-ref exemption ---

// exemptPipelineSHA names a commit that is never the merge request's own
// sha in any fixture in this file, matching the shape of a merged
// results or merge-train pipeline: a sha that exists in neither branch.
const exemptPipelineSHA = "2222333344445555666677778888999900001111"

// assertGeneratedMergeRefDebug asserts that logOutput carries exactly
// one exemption Debug line naming ref, id, and source as its
// pipeline_ref, pipeline_id, and pipeline_source attributes.
func assertGeneratedMergeRefDebug(t *testing.T, logOutput, ref string, id int64, source string) {
	t.Helper()

	const msg = "head pipeline runs on a generated merge-request ref"
	if n := strings.Count(logOutput, msg); n != 1 {
		t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", msg, n, logOutput)
	}
	if want := "pipeline_ref=" + ref; !strings.Contains(logOutput, want) {
		t.Errorf("log output = %q, want it to carry %q", logOutput, want)
	}
	if want := "pipeline_id=" + strconv.FormatInt(id, 10); !strings.Contains(logOutput, want) {
		t.Errorf("log output = %q, want it to carry %q", logOutput, want)
	}
	if want := "pipeline_source=" + source; !strings.Contains(logOutput, want) {
		t.Errorf("log output = %q, want it to carry %q", logOutput, want)
	}
}

func TestGetCIStatus_GeneratedMergeRefExemption(t *testing.T) {
	t.Parallel()

	mergeRef := "refs/merge-requests/" + strconv.Itoa(testPRNumber) + "/merge"
	trainRef := "refs/merge-requests/" + strconv.Itoa(testPRNumber) + "/train"

	t.Run("a merged-results pipeline is exempt and maps normally", func(t *testing.T) {
		t.Parallel()

		const exemptPipelineID = int64(4001)
		fixture := mergeRequestFixture(t, map[string]any{
			"head_pipeline": map[string]any{
				"status": "success",
				"id":     exemptPipelineID,
				"sha":    exemptPipelineSHA,
				"ref":    mergeRef,
				"source": "merge_request_event",
			},
		})
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "success" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "success")
		}

		logOutput := buf.String()
		if strings.Contains(logOutput, "head pipeline does not describe the merge request head") {
			t.Errorf("log output = %q, want no deferral WARN", logOutput)
		}
		assertGeneratedMergeRefDebug(t, logOutput, mergeRef, exemptPipelineID, "merge_request_event")
	})

	t.Run("a merge-train pipeline is exempt and its manual status is addressed at the pipeline's own sha and id", func(t *testing.T) {
		t.Parallel()

		const exemptPipelineID = int64(4002)
		mrFixture := mergeRequestFixture(t, map[string]any{
			"head_pipeline": map[string]any{
				"status": "manual",
				"id":     exemptPipelineID,
				"sha":    exemptPipelineSHA,
				"ref":    trainRef,
				"source": "merge_request_event",
			},
		})
		statusesBody := commitStatusesJSON(t, []map[string]any{
			commitStatusEntry(1, "manual-one", "manual", exemptPipelineID),
			commitStatusEntry(2, "manual-two", "manual", exemptPipelineID),
		})
		wantStatusesSuffix := "/repository/commits/" + exemptPipelineSHA + "/statuses"

		var statusesPath, statusesPipelineID string
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			statusesPath = r.URL.Path
			statusesPipelineID = r.URL.Query().Get("pipeline_id")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(statusesBody)
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "success" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "success")
		}
		if !strings.HasSuffix(statusesPath, wantStatusesSuffix) {
			t.Errorf("statuses request path = %q, want it to end with %q", statusesPath, wantStatusesSuffix)
		}
		if want := strconv.FormatInt(exemptPipelineID, 10); statusesPipelineID != want {
			t.Errorf("statuses request pipeline_id query param = %q, want %q", statusesPipelineID, want)
		}

		assertGeneratedMergeRefDebug(t, buf.String(), trainRef, exemptPipelineID, "merge_request_event")
	})
}

// --- GetCIStatus: detached merge-request pipeline ---

func TestGetCIStatus_DetachedMergeRequestPipelineInsideComparison(t *testing.T) {
	t.Parallel()

	headRef := "refs/merge-requests/" + strconv.Itoa(testPRNumber) + "/head"

	t.Run("equal shas map normally", func(t *testing.T) {
		t.Parallel()

		fixture := mergeRequestFixture(t, map[string]any{
			"head_pipeline": map[string]any{
				"status": "success",
				"sha":    mrBaselineSHA,
				"ref":    headRef,
				"source": "merge_request_event",
			},
		})
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "success" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "success")
		}
	})

	t.Run("differing shas defer to pending", func(t *testing.T) {
		t.Parallel()

		fixture := mergeRequestFixture(t, map[string]any{
			"head_pipeline": map[string]any{
				"status": "success",
				"sha":    divergentPipelineSHA,
				"ref":    headRef,
				"source": "merge_request_event",
			},
		})
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "pending" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "pending")
		}
	})
}

// --- GetCIStatus: shapes that imitate the exemption ---

func TestGetCIStatus_ExemptionRejectsImitatingShapes(t *testing.T) {
	t.Parallel()

	mergeRef := "refs/merge-requests/" + strconv.Itoa(testPRNumber) + "/merge"
	otherMergeRef := "refs/merge-requests/" + strconv.Itoa(testPRNumber+1) + "/merge"

	tests := []struct {
		name   string
		ref    string
		source string
	}{
		{"an ordinary branch name", "feature/scm-reads", "push"},
		{"a branch literally named after the generated merge ref, created by a push", mergeRef, "push"},
		{"a branch literally named after the generated merge ref, reported externally", mergeRef, "external"},
		{"a generated ref anchored to a different merge request", otherMergeRef, "merge_request_event"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := mergeRequestFixture(t, map[string]any{
				"head_pipeline": map[string]any{
					"status": "success",
					"sha":    divergentPipelineSHA,
					"ref":    tt.ref,
					"source": tt.source,
				},
			})
			srv := serveJSON(t, fixture)
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
			if err != nil {
				t.Fatalf("GetCIStatus: unexpected error: %v", err)
			}
			if got != "pending" {
				t.Errorf("GetCIStatus() = %q, want %q (the exemption must not admit this shape)", got, "pending")
			}
		})
	}
}

// --- GetCIStatus: manual head pipeline job-set fold ---

// manualPipelineSHA and manualPipelineID are the full 40-character sha
// and pipeline id every manual-path test below addresses the statuses
// route with, so the addressing guard never rejects a fixture as
// abbreviated or scopeless.
const (
	manualPipelineSHA = "aaaa1111bbbb2222cccc3333dddd4444eeee5555"
	manualPipelineID  = int64(9001)
)

// manualHeadPipelineFixture returns a merge-request fixture whose
// head_pipeline reports "manual" at sha and pipelineID. The merge
// request's own "sha" defaults to sha, the pipeline's own value, so the
// job-set fold is reached rather than the head-comparison deferral,
// unless overrides sets "sha" itself. Any additional top-level
// merge-request field overrides are applied on top.
func manualHeadPipelineFixture(t *testing.T, sha string, pipelineID int64, overrides map[string]any) []byte {
	t.Helper()

	fields := map[string]any{
		"sha": sha,
		"head_pipeline": map[string]any{
			"status": "manual",
			"id":     pipelineID,
			"sha":    sha,
		},
	}
	maps.Copy(fields, overrides)
	return mergeRequestFixture(t, fields)
}

// commitStatusEntry returns one commit-status wire entry with
// allow_failure false, which every manual-path job-set shape below
// carries.
func commitStatusEntry(id int64, name, status string, pipelineID int64) map[string]any {
	return map[string]any{
		"id":            id,
		"name":          name,
		"status":        status,
		"allow_failure": false,
		"target_url":    nil,
		"pipeline_id":   pipelineID,
	}
}

// commitStatusesJSON marshals entries into a commit-status page body, or
// fails the test.
func commitStatusesJSON(t *testing.T, entries []map[string]any) []byte {
	t.Helper()

	body, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal commit statuses: %v", err)
	}
	return body
}

// manualJobSetBody returns the "manual plus manual, nothing else" job
// set the documentation's shape table folds to success, scoped to
// manualPipelineID.
func manualJobSetBody(t *testing.T) []byte {
	t.Helper()

	return commitStatusesJSON(t, []map[string]any{
		commitStatusEntry(1, "gate-a", "manual", manualPipelineID),
		commitStatusEntry(2, "gate-b", "manual", manualPipelineID),
	})
}

// mergeRequestAndStatusesServer serves mrFixture from the merge-request
// detail route and delegates any request whose path contains "/statuses"
// to statusesHandler. Any other path fails the test, so a read
// mis-addressed at a route other than the two the manual path is allowed
// to use cannot pass silently.
func mergeRequestAndStatusesServer(t *testing.T, mrFixture []byte, statusesHandler http.HandlerFunc) *httptest.Server {
	t.Helper()

	wantMRSuffix := "/merge_requests/" + strconv.Itoa(testPRNumber)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/statuses"):
			statusesHandler(w, r)
		case strings.HasSuffix(r.URL.Path, wantMRSuffix):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mrFixture)
		default:
			t.Errorf("unexpected request to %s, want the merge-request detail route or the statuses route", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestGetCIStatus_ManualRequestShape calls manualGate directly, rather
// than through GetCIStatus, because a pipeline whose sha differs from
// every merge-request fixture in this file no longer reaches manualGate
// through GetCIStatus: it defers before the manual arm is considered.
// The direct call preserves the addressing assertion this test has
// always made, independent of that routing.
func TestGetCIStatus_ManualRequestShape(t *testing.T) {
	t.Parallel()

	pipeline := &gitlabPipeline{Status: "manual", ID: manualPipelineID, SHA: manualPipelineSHA}
	statusesBody := manualJobSetBody(t)
	wantStatusesSuffix := "/repository/commits/" + manualPipelineSHA + "/statuses"

	var requestPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPaths = append(requestPaths, r.URL.Path)
		if got := r.URL.Query().Get("pipeline_id"); got != strconv.FormatInt(manualPipelineID, 10) {
			t.Errorf("statuses request pipeline_id query param = %q, want %q", got, strconv.FormatInt(manualPipelineID, 10))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(statusesBody)
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	if _, err := adapter.manualGate(context.Background(), scmOwner, scmRepo, pipeline); err != nil {
		t.Fatalf("manualGate: unexpected error: %v", err)
	}

	if len(requestPaths) != 1 {
		t.Fatalf("requests = %d, want 1 (manualGate must issue exactly one request); paths: %v", len(requestPaths), requestPaths)
	}
	if !strings.HasSuffix(requestPaths[0], wantStatusesSuffix) {
		t.Errorf("request path = %q, want it to end with %q (the pipeline's own sha %q must be addressed)", requestPaths[0], wantStatusesSuffix, manualPipelineSHA)
	}
}

func TestGetCIStatus_ManualJobSetShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []map[string]any
		want    string
	}{
		{
			"manual plus failed, same stage, folds to failing",
			[]map[string]any{
				commitStatusEntry(1, "manual-gate", "manual", manualPipelineID),
				commitStatusEntry(2, "failing-job", "failed", manualPipelineID),
			},
			"failing",
		},
		{
			"success plus manual plus failed, needs DAG, folds to failing",
			[]map[string]any{
				commitStatusEntry(1, "build-job", "success", manualPipelineID),
				commitStatusEntry(2, "manual-gate", "manual", manualPipelineID),
				commitStatusEntry(3, "failing-job", "failed", manualPipelineID),
			},
			"failing",
		},
		{
			"manual plus manual, nothing else, folds to success",
			[]map[string]any{
				commitStatusEntry(1, "manual-one", "manual", manualPipelineID),
				commitStatusEntry(2, "manual-two", "manual", manualPipelineID),
			},
			"success",
		},
		{
			"manual gate blocking a later-stage created job folds to pending, not a merge-eligible verdict",
			[]map[string]any{
				commitStatusEntry(1, "manual-gate", "manual", manualPipelineID),
				commitStatusEntry(2, "deploy-job", "created", manualPipelineID),
			},
			"pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
			statusesBody := commitStatusesJSON(t, tt.entries)
			srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(statusesBody)
			})
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
			if err != nil {
				t.Fatalf("GetCIStatus: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetCIStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetCIStatus_ManualReadFailure(t *testing.T) {
	t.Parallel()

	t.Run("a non-2xx statuses read yields an error and the empty string", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write(loadFixture(t, "error_500.json"))
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if got != "" {
			t.Errorf("GetCIStatus() = %q, want empty string on a failed job-set read", got)
		}
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMTransport)
	})

	t.Run("an undecodable statuses body yields ErrSCMPayload", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if got != "" {
			t.Errorf("GetCIStatus() = %q, want empty string on a decode failure", got)
		}
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}

func TestGetCIStatus_ManualPagination(t *testing.T) {
	t.Parallel()

	mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
	page1 := commitStatusesJSON(t, []map[string]any{
		commitStatusEntry(1, "manual-gate", "manual", manualPipelineID),
	})
	page2 := commitStatusesJSON(t, []map[string]any{
		commitStatusEntry(2, "failing-job", "failed", manualPipelineID),
	})
	statusesSuffix := "/repository/commits/" + manualPipelineSHA + "/statuses"

	var srv *httptest.Server
	srv = mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write(page2)
			return
		}
		w.Header().Set("Link", `<`+srv.URL+statusesSuffix+`?page=2>; rel="next"`)
		_, _ = w.Write(page1)
	})
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("GetCIStatus: unexpected error: %v", err)
	}
	if got != "failing" {
		t.Errorf("GetCIStatus() = %q, want %q (the failing entry on the second page must be folded in)", got, "failing")
	}
}

func TestGetCIStatus_ManualScoping(t *testing.T) {
	t.Parallel()

	t.Run("an empty scoped job set yields pending, not the empty string", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got == "" {
			t.Errorf("GetCIStatus() = %q, want a non-empty verdict (an empty job set must not report CIGateAbsent)", got)
		}
		if got != "pending" {
			t.Errorf("GetCIStatus() = %q, want %q", got, "pending")
		}
	})

	t.Run("entries carrying a foreign pipeline_id are discarded after decoding", func(t *testing.T) {
		t.Parallel()

		const foreignPipelineID = int64(4242)

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
		statusesBody := commitStatusesJSON(t, []map[string]any{
			commitStatusEntry(1, "manual-one", "manual", manualPipelineID),
			commitStatusEntry(2, "manual-two", "manual", manualPipelineID),
			commitStatusEntry(3, "foreign-failing-job", "failed", foreignPipelineID),
		})
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(statusesBody)
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}
		if got != "success" {
			t.Errorf("GetCIStatus() = %q, want %q (the foreign pipeline's failing entry must be discarded)", got, "success")
		}
	})
}

func TestGetCIStatus_ManualPayloadGuard(t *testing.T) {
	t.Parallel()

	t.Run("zero id issues no statuses request", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, 0, nil)

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if strings.Contains(r.URL.Path, "/statuses") {
				t.Errorf("unexpected request to %s, want no statuses request", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mrFixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo)
		if got != "" {
			t.Errorf("GetCIStatus() = %q, want empty string", got)
		}
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)

		if n := requests.Load(); n != 1 {
			t.Errorf("requests = %d, want 1 (the merge-request read alone; no statuses request)", n)
		}
	})

	// An empty pipeline sha reaches manualGate only through the
	// generated-merge-ref exempt path once a non-empty merge-request sha
	// is present, since a non-exempt divergence answers the
	// head-comparison deferral first. The direct call keeps manualGate's
	// own guard covered independent of that routing.
	t.Run("empty sha issues no statuses request", func(t *testing.T) {
		t.Parallel()

		pipeline := &gitlabPipeline{Status: "manual", ID: manualPipelineID, SHA: ""}

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			t.Errorf("unexpected request to %s, want no request issued", r.URL.Path)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.manualGate(context.Background(), scmOwner, scmRepo, pipeline)
		if got != "" {
			t.Errorf("manualGate() = %q, want empty string", got)
		}
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)

		if n := requests.Load(); n != 0 {
			t.Errorf("requests = %d, want 0 (an empty pipeline sha must not reach the network)", n)
		}
	})
}

func TestMapPipelineStatus_RecognizesAllStatuses(t *testing.T) {
	t.Parallel()

	statuses := []string{
		"created", "waiting_for_resource", "preparing", "waiting_for_callback",
		"pending", "running", "success", "failed", "canceling", "canceled",
		"skipped", "scheduled", "manual",
	}

	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			_, recognized := mapPipelineStatus(status)
			if !recognized {
				t.Errorf("mapPipelineStatus(%q) recognized = false, want true", status)
			}
		})
	}
}

func TestGetCIStatus_ManualWarnings(t *testing.T) {
	t.Parallel()

	t.Run("an unrecognized job status logs one WARN", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
		statusesBody := commitStatusesJSON(t, []map[string]any{
			commitStatusEntry(1, "manual-gate", "manual", manualPipelineID),
			commitStatusEntry(2, "odd-job", "expired", manualPipelineID),
		})
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(statusesBody)
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		if _, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}

		logOutput := buf.String()
		if n := strings.Count(logOutput, "unrecognized gitlab job status"); n != 1 {
			t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", "unrecognized gitlab job status", n, logOutput)
		}
		if !strings.Contains(logOutput, "status=expired") {
			t.Errorf("log output = %q, want it to carry the observed status as an attribute", logOutput)
		}
	})

	t.Run("every entry recognized logs no WARN", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
		statusesBody := manualJobSetBody(t)
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(statusesBody)
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		if _, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}

		if logOutput := buf.String(); strings.Contains(logOutput, "unrecognized gitlab job status") {
			t.Errorf("log output = %q, want no unrecognized-status WARN", logOutput)
		}
	})

	t.Run("an empty scoped job set logs one WARN carrying the pipeline id", func(t *testing.T) {
		t.Parallel()

		mrFixture := manualHeadPipelineFixture(t, manualPipelineSHA, manualPipelineID, nil)
		srv := mergeRequestAndStatusesServer(t, mrFixture, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		})
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		if _, err := adapter.GetCIStatus(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
			t.Fatalf("GetCIStatus: unexpected error: %v", err)
		}

		logOutput := buf.String()
		if n := strings.Count(logOutput, "manual head pipeline carried no job status"); n != 1 {
			t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", "manual head pipeline carried no job status", n, logOutput)
		}
		wantAttr := "pipeline_id=" + strconv.FormatInt(manualPipelineID, 10)
		if !strings.Contains(logOutput, wantAttr) {
			t.Errorf("log output = %q, want it to carry %q", logOutput, wantAttr)
		}
	})
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
