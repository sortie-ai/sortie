package gitlab

import (
	"context"
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

// --- ListLabelEvents ---

func TestListLabelEvents_SkipsNullLabel(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "label_events_null_label.json")
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.ListLabelEvents(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListLabelEvents() len = %d, want 1 (the null-label entry must be dropped)", len(got))
	}
	if got[0].ID != scmcore.SortableEventID(201) {
		t.Errorf("ListLabelEvents()[0].ID = %q, want %q", got[0].ID, scmcore.SortableEventID(201))
	}
}

func TestListLabelEvents_MapsActionAndActor(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "label_events_action_actor.json")
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.ListLabelEvents(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListLabelEvents() len = %d, want 2", len(got))
	}

	add, remove := got[0], got[1]
	if add.Label != "priority" {
		t.Errorf("events[0].Label = %q, want %q (lowercased from Priority)", add.Label, "priority")
	}
	if !add.Added || add.Actor != "alice" {
		t.Errorf("events[0] = %+v, want Added=true Actor=alice", add)
	}
	if remove.Added || remove.Actor != "bob" {
		t.Errorf("events[1] = %+v, want Added=false Actor=bob", remove)
	}
	if remove.ID != scmcore.SortableEventID(302) {
		t.Errorf("events[1].ID = %q, want %q", remove.ID, scmcore.SortableEventID(302))
	}
}

func TestListLabelEvents_OrderedViaAssertLabelEventsOrdered(t *testing.T) {
	t.Parallel()

	// Served newest first, with entries 401 and 402 sharing a timestamp so
	// the (At, ID) tiebreak is falsifiable: a sort keyed on At alone would
	// leave 401 and 402 in their served (ID-descending) order.
	fixture := loadFixture(t, "label_events_newest_first_tiebreak.json")
	srv := serveJSON(t, fixture)
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.ListLabelEvents(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListLabelEvents() len = %d, want 3", len(got))
	}

	adaptertest.AssertLabelEventsOrdered(t, got)

	wantIDs := []string{scmcore.SortableEventID(401), scmcore.SortableEventID(402), scmcore.SortableEventID(403)}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("events[%d].ID = %q, want %q (oldest first, tie broken ascending by id)", i, got[i].ID, want)
		}
	}
}

func TestListLabelEvents_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.ListLabelEvents(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	adaptertest.AssertEmptyNonNil(t, got, "ListLabelEvents")
}

func TestListLabelEvents_ErrorStatuses(t *testing.T) {
	t.Parallel()

	runSCMErrorStatusTable(t, func(t *testing.T, srv *httptest.Server) error {
		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.ListLabelEvents(context.Background(), testPRNumber, scmOwner, scmRepo)
		return err
	})

	t.Run("malformed journal body is a payload error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.ListLabelEvents(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}

// --- RemoveLabel (AC7) ---

func TestRemoveLabel_AbsentLabelNoOp(t *testing.T) {
	t.Parallel()

	mrFixture := mergeRequestFixture(t, map[string]any{"labels": []string{"unrelated-label"}})
	var putCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write(mrFixture)
		case http.MethodPut:
			putCalls.Add(1)
			_, _ = w.Write(mrFixture)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	err := adapter.RemoveLabel(context.Background(), testPRNumber, scmOwner, scmRepo, "sortie:review")
	adaptertest.AssertLabelAbsentDisposition(t, err)

	if n := putCalls.Load(); n != 0 {
		t.Errorf("PUT calls = %d, want 0 (no stored variant matches, so no write is issued)", n)
	}
}

func TestRemoveLabel_CaseVariantResolution(t *testing.T) {
	t.Parallel()

	const storedLabel = "Sortie:Review"
	mrFixture := mergeRequestFixture(t, map[string]any{"labels": []string{storedLabel, "unrelated-label"}})

	var gotMethod, gotPath string
	var gotBody gitlabMergeRequestUpdate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write(mrFixture)
		case http.MethodPut:
			gotMethod = r.Method
			gotPath = r.URL.EscapedPath()
			decodeRequestBody(t, r, &gotBody)
			_, _ = w.Write(mrFixture)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	if err := adapter.RemoveLabel(context.Background(), testPRNumber, scmOwner, scmRepo, "sortie:review"); err != nil {
		t.Fatalf("RemoveLabel: unexpected error: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("request method = %q, want %q", gotMethod, http.MethodPut)
	}
	wantPath := "/api/v4/projects/" + projectPath(scmOwner, scmRepo) + "/merge_requests/" + strconv.Itoa(testPRNumber)
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if len(gotBody.RemoveLabels) != 1 || gotBody.RemoveLabels[0] != storedLabel {
		t.Errorf("request body remove_labels = %v, want [%q] (the stored spelling, not the configured lowercase form)", gotBody.RemoveLabels, storedLabel)
	}
}

func TestRemoveLabel_NotFoundMapsToNil(t *testing.T) {
	t.Parallel()

	t.Run("404 on the merge-request read maps to nil with one WARN", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_404_project.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		err := adapter.RemoveLabel(context.Background(), testPRNumber, scmOwner, scmRepo, "sortie:review")
		if err != nil {
			t.Errorf("RemoveLabel = %v, want nil", err)
		}

		logOutput := buf.String()
		const wantWarn = "gitlab merge request not found during label removal"
		if got := strings.Count(logOutput, wantWarn); got != 1 {
			t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", wantWarn, got, logOutput)
		}
		if !strings.Contains(logOutput, "pr_number="+strconv.Itoa(testPRNumber)) {
			t.Errorf("log output = %q, want it to carry pr_number=%d", logOutput, testPRNumber)
		}
	})

	t.Run("404 on the merge-request update maps to nil with one WARN", func(t *testing.T) {
		t.Parallel()

		mrFixture := mergeRequestFixture(t, map[string]any{"labels": []string{"Sortie:Review"}})
		errorBody := loadFixture(t, "error_404_project.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(mrFixture)
			case http.MethodPut:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write(errorBody)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		err := adapter.RemoveLabel(context.Background(), testPRNumber, scmOwner, scmRepo, "sortie:review")
		if err != nil {
			t.Errorf("RemoveLabel = %v, want nil", err)
		}

		logOutput := buf.String()
		const wantWarn = "gitlab merge request not found during label removal"
		if got := strings.Count(logOutput, wantWarn); got != 1 {
			t.Errorf("occurrences of %q in log output = %d, want 1 (log output: %q)", wantWarn, got, logOutput)
		}
	})
}
