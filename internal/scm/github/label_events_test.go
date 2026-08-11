package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// journalBaseTime anchors the synthetic created_at timestamps used across
// the label-event journal fixtures in this file.
var journalBaseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// truncatedJournalPageCount exceeds maxLabelEventPages so the paging tests
// exercise the tail-truncation path. Page N (1-indexed) is the Nth-oldest
// entry; page truncatedJournalPageCount is the newest.
const truncatedJournalPageCount = maxLabelEventPages + 5

// newTruncatedJournalServer serves a label-event journal of
// truncatedJournalPageCount single-event pages, walkable only via the Link
// "last" relation and then "prev", mirroring the real issue-events
// pagination contract. The initial (unpaged) request's body is irrelevant
// per the adapter's own contract: it is discarded once a "last" relation
// is found, so only its Link header matters.
func newTruncatedJournalServer(t *testing.T) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		pageParam := r.URL.Query().Get("page")
		if pageParam == "" {
			lastURL := fmt.Sprintf("%s/repos/o/r/issues/1/events?page=%d", srv.URL, truncatedJournalPageCount)
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="last"`, lastURL))
			_, _ = w.Write([]byte(`[]`))
			return
		}

		page, err := strconv.Atoi(pageParam)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		createdAt := journalBaseTime.Add(time.Duration(page) * time.Second).Format(time.RFC3339)
		body := fmt.Sprintf(`[{"id":%d,"event":"labeled","actor":{"login":"alice"},"label":{"name":"sortie:review"},"created_at":%q}]`,
			page, createdAt)

		if page > 1 {
			prevURL := fmt.Sprintf("%s/repos/o/r/issues/1/events?page=%d", srv.URL, page-1)
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="prev"`, prevURL))
		}
		_, _ = w.Write([]byte(body))
	}))
	return srv
}

// --- ListLabelEvents tests ---

func TestListLabelEvents_Normalization(t *testing.T) {
	t.Parallel()

	fixture := `[
		{"id":1,"event":"labeled","actor":{"login":"alice"},"label":{"name":"Sortie:Review"},"created_at":"2026-01-01T10:00:00Z"},
		{"id":2,"event":"unlabeled","actor":null,"label":{"name":"sortie:review"},"created_at":"2026-01-01T11:00:00Z"},
		{"id":3,"event":"commented","actor":{"login":"bob"},"label":null,"created_at":"2026-01-01T12:00:00Z"},
		{"id":4,"event":"labeled","actor":{"login":"carol"},"label":{"name":"bug"},"created_at":"2026-01-01T13:00:00Z"}
	]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	events, err := adapter.ListLabelEvents(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("ListLabelEvents() len = %d, want 3 (the commented event is excluded)", len(events))
	}

	if events[0].ID != scmcore.SortableEventID(1) || events[1].ID != scmcore.SortableEventID(2) || events[2].ID != scmcore.SortableEventID(4) {
		t.Fatalf("ListLabelEvents() ids = [%s %s %s], want [1 2 4] normalized (oldest-first)",
			events[0].ID, events[1].ID, events[2].ID)
	}

	if events[0].Label != "sortie:review" {
		t.Errorf("events[0].Label = %q, want %q (lowercased)", events[0].Label, "sortie:review")
	}
	if events[0].Actor != "alice" {
		t.Errorf("events[0].Actor = %q, want %q", events[0].Actor, "alice")
	}
	if !events[0].Added {
		t.Error("events[0].Added = false, want true (labeled)")
	}
	if events[0].At.IsZero() {
		t.Error("events[0].At is zero, want parsed created_at")
	}

	if events[1].Actor != "" {
		t.Errorf("events[1].Actor = %q, want empty (actor is null)", events[1].Actor)
	}
	if events[1].Added {
		t.Error("events[1].Added = true, want false (unlabeled)")
	}

	if events[2].Label != "bug" {
		t.Errorf("events[2].Label = %q, want %q", events[2].Label, "bug")
	}
	if events[2].Actor != "carol" {
		t.Errorf("events[2].Actor = %q, want %q", events[2].Actor, "carol")
	}
	adaptertest.AssertLabelEventsOrdered(t, events)
}

func TestListLabelEvents_NewestRetainedPaging(t *testing.T) {
	t.Parallel()

	srv := newTruncatedJournalServer(t)
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	events, err := adapter.ListLabelEvents(context.Background(), 1, "o", "r")
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}

	if len(events) != maxLabelEventPages {
		t.Fatalf("ListLabelEvents() len = %d, want %d (retained window)", len(events), maxLabelEventPages)
	}

	wantOldestID := scmcore.SortableEventID(int64(truncatedJournalPageCount - maxLabelEventPages + 1))
	if events[0].ID != wantOldestID {
		t.Errorf("ListLabelEvents()[0].ID = %q, want %q (oldest ID retained within the cap)", events[0].ID, wantOldestID)
	}
	wantNewestID := scmcore.SortableEventID(int64(truncatedJournalPageCount))
	if events[len(events)-1].ID != wantNewestID {
		t.Errorf("ListLabelEvents()[last].ID = %q, want %q (newest event present despite the cap)",
			events[len(events)-1].ID, wantNewestID)
	}
	for _, e := range events {
		if e.ID == scmcore.SortableEventID(1) {
			t.Error("ListLabelEvents() contains the oldest journal entry (id=1); want it truncated away")
		}
	}
}

func TestListLabelEvents_TruncationWarns(t *testing.T) {
	// Swaps the process-wide default slog handler to capture the
	// package-level Warn this adapter emits (ListLabelEvents takes no
	// logger parameter). Must not run in parallel with other tests that
	// depend on slog.Default().
	srv := newTruncatedJournalServer(t)
	defer srv.Close()

	var buf strings.Builder
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	adapter := newTestSCMAdapter(t, srv.URL)
	if _, err := adapter.ListLabelEvents(context.Background(), 777, "o", "r"); err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "pr_number=777") {
		t.Errorf("Warn log = %q, want to contain pr_number=777", logOutput)
	}
	if n := strings.Count(logOutput, "label events response truncated at page limit"); n != 1 {
		t.Errorf("Warn log message count = %d, want exactly 1", n)
	}
}

func TestListLabelEvents_SinglePageNoLinkHeader(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"event":"labeled","actor":{"login":"alice"},"label":{"name":"sortie:review"},"created_at":"2026-01-01T00:00:00Z"}]`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	events, err := adapter.ListLabelEvents(context.Background(), 1, "o", "r")
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ListLabelEvents() len = %d, want 1", len(events))
	}
	if n := requestCount.Load(); n != 1 {
		t.Errorf("request count = %d, want 1 (no tail-jump attempted without a Link header)", n)
	}
}

func TestListLabelEvents_NoEvents(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	events, err := adapter.ListLabelEvents(context.Background(), 1, "o", "r")
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	adaptertest.AssertEmptyNonNil(t, events, "ListLabelEvents")
}

func TestListLabelEvents_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	_, err := adapter.ListLabelEvents(context.Background(), 1, "o", "r")
	assertSCMErrorKind(t, err, domain.ErrSCMTransport)
}

// --- RemoveLabel tests ---

func TestRemoveLabel_AbsentLabelIsNoOp(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	err := adapter.RemoveLabel(context.Background(), 1, "owner", "repo", "urgent")
	adaptertest.AssertLabelAbsentDisposition(t, err)
}

func TestRemoveLabel_409IsNotPromotedToConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"conflict"}`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	err := adapter.RemoveLabel(context.Background(), 1, "owner", "repo", "urgent")

	// Only a merge write path promotes 405/409 to ErrSCMConflict; RemoveLabel
	// is not on that path.
	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAPI)
}

func TestListLabelEvents_MalformedTimestampIsPayloadError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"event":"labeled","actor":{"login":"alice"},"label":{"name":"sortie:review"},"created_at":"not-a-timestamp"}]`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	events, err := adapter.ListLabelEvents(context.Background(), 1, "o", "r")
	adaptertest.AssertLabelEventTimestampRejected(t, events, err)

	var scmErr *domain.SCMError
	if !errors.As(err, &scmErr) {
		t.Fatalf("ListLabelEvents() error = %v, want *domain.SCMError", err)
	}
	const wantMessage = "failed to parse issue event created_at"
	if scmErr.Message != wantMessage {
		t.Errorf("ListLabelEvents() error Message = %q, want %q", scmErr.Message, wantMessage)
	}
}

func TestListLabelEvents_MalformedTimestampOnSkippedEventDoesNotFail(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":2,"event":"commented","actor":{"login":"bob"},"label":null,"created_at":"not-a-timestamp"},
			{"id":1,"event":"labeled","actor":{"login":"alice"},"label":{"name":"sortie:review"},"created_at":"2026-01-01T00:00:00Z"}
		]`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	events, err := adapter.ListLabelEvents(context.Background(), 1, "o", "r")
	if err != nil {
		t.Fatalf("ListLabelEvents: unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("ListLabelEvents() len = %d, want 1 (the malformed commented event is skipped before its timestamp is parsed)", len(events))
	}
	if events[0].Label != "sortie:review" {
		t.Errorf("events[0].Label = %q, want %q", events[0].Label, "sortie:review")
	}
}
