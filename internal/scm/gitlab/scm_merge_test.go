package gitlab

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

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
	}{
		{"success maps to CIGateSuccess", map[string]any{"head_pipeline": map[string]any{"status": "success"}}, "success"},
		{"failed maps to CIGateFailing", map[string]any{"head_pipeline": map[string]any{"status": "failed"}}, "failing"},
		{"canceled maps to CIGateFailing", map[string]any{"head_pipeline": map[string]any{"status": "canceled"}}, "failing"},
		{"an unrecognized status defers to CIGatePending", map[string]any{"head_pipeline": map[string]any{"status": "skipped"}}, "pending"},
		{"a nil head_pipeline maps to CIGateAbsent (empty string)", map[string]any{"head_pipeline": nil}, ""},
		{"a populated top-level pipeline is never substituted for a nil head_pipeline", map[string]any{"head_pipeline": nil, "pipeline": map[string]any{"status": "success"}}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := mergeRequestFixture(t, tt.overrides)
			srv := serveJSON(t, fixture)
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
