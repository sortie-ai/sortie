package gitea

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// buildLabelTimelinePage returns a JSON array of n synthetic label-add
// timeline entries with sequential ids starting at startID, modeling one
// page-number page without hand-authoring a repetitive fixture file.
func buildLabelTimelinePage(t *testing.T, startID int64, n int) []byte {
	t.Helper()
	entries := make([]map[string]any, n)
	for i := range n {
		entries[i] = map[string]any{
			"id":         startID + int64(i),
			"type":       "label",
			"body":       "1",
			"user":       map[string]string{"login": "alice"},
			"label":      map[string]string{"name": "paged"},
			"created_at": "2026-07-14T10:00:00Z",
		}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal timeline page: %v", err)
	}
	return data
}

func TestGiteaSCMListLabelEvents(t *testing.T) {
	t.Parallel()

	t.Run("maps add and remove, lowercases labels, skips non-label entries, keeps oldest-first order", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "timeline_issue4_shaped.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		events, err := adapter.ListLabelEvents(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("ListLabelEvents: unexpected error: %v", err)
		}
		if len(events) != 4 {
			t.Fatalf("ListLabelEvents() len = %d, want 4 (comment and pull_push entries excluded)", len(events))
		}

		wantIDs := []int64{1, 9, 10, 12}
		for i, id := range wantIDs {
			if events[i].ID != sortableEventID(id) {
				t.Errorf("events[%d].ID = %q, want %q (oldest-first order)", i, events[i].ID, sortableEventID(id))
			}
		}

		if events[0].Label != "bug" || !events[0].Added || events[0].Actor != "alice" {
			t.Errorf("events[0] = %+v, want Label=bug Added=true Actor=alice", events[0])
		}
		if events[1].Label != "review" || !events[1].Added {
			t.Errorf("events[1] = %+v, want Label=review (lowercased from REVIEW) Added=true", events[1])
		}
		if events[2].Label != "backlog" || events[2].Actor != "carol" {
			t.Errorf("events[2] = %+v, want Label=backlog Actor=carol", events[2])
		}
		if events[3].Label != "bug" || events[3].Added {
			t.Errorf("events[3] = %+v, want Label=bug Added=false (empty body is a remove)", events[3])
		}

		if events[1].ID >= events[2].ID {
			t.Errorf("events[1].ID=%q must sort lexically before events[2].ID=%q despite sharing a timestamp (ids 9 and 10 cross a digit boundary)",
				events[1].ID, events[2].ID)
		}
	})

	t.Run("follows page number to exhaustion with no Link header", func(t *testing.T) {
		t.Parallel()

		const limit = 50
		page1 := buildLabelTimelinePage(t, 1, limit)
		page2 := buildLabelTimelinePage(t, limit+1, 2)

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				_, _ = w.Write(page1)
				return
			}
			_, _ = w.Write(page2)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		events, err := adapter.ListLabelEvents(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("ListLabelEvents: unexpected error: %v", err)
		}
		if len(events) != limit+2 {
			t.Fatalf("ListLabelEvents() len = %d, want %d (both pages traversed by page number)", len(events), limit+2)
		}
		if n := requestCount.Load(); n != 2 {
			t.Errorf("request count = %d, want 2 (the response carries no Link header; a Link-based paginator would stop at 1)", n)
		}
	})

	t.Run("empty timeline returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		events, err := adapter.ListLabelEvents(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("ListLabelEvents: unexpected error: %v", err)
		}
		if events == nil {
			t.Error("ListLabelEvents() = nil, want non-nil empty slice")
		}
		if len(events) != 0 {
			t.Errorf("ListLabelEvents() len = %d, want 0", len(events))
		}
	})

	t.Run("malformed timeline response is a payload error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.ListLabelEvents(context.Background(), 6, testOwner, testRepo)
		assertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}
