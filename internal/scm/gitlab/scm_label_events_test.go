package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
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
