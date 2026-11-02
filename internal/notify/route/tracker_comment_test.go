package route_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/storage/sqlite"
)

type failingRecorder struct{}

func (failingRecorder) AddPostedComment(context.Context, string, string) error {
	return errors.New("disk full")
}

func TestTrackerComment_SendRecordsIdentityAcrossRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sortie.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q): %v", path, err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Store.Migrate: %v", err)
	}
	router := route.NewRouter(&commentRecorder{}, store, nil, nil)
	mustUpdate(t, router, route.Inputs{Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventSessionStopped)}})

	router.Route(notificationOf(domain.EventSessionStopped)).Deliver(ctx)
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close: %v", err)
	}

	reopened, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) after restart: %v", path, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.ListPostedComments(ctx, "ISS-1")
	if err != nil {
		t.Fatalf("ListPostedComments(%q): %v", "ISS-1", err)
	}

	if want := []string{postedCommentID}; !slices.Equal(got, want) {
		t.Errorf("ListPostedComments(%q) = %v, want %v", "ISS-1", got, want)
	}
}

func TestTrackerComment_SendSucceedsWhenRecordFails(t *testing.T) {
	t.Parallel()

	tracker := &commentRecorder{}
	router := route.NewRouter(tracker, failingRecorder{}, nil, nil)
	mustUpdate(t, router, route.Inputs{Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventSessionStopped)}})

	received, err := router.Route(notificationOf(domain.EventSessionStopped)).Deliver(context.Background()).TrackerComment()

	if !received || err != nil {
		t.Errorf("TrackerComment() = (%v, %v), want (true, nil)", received, err)
	}
	if got := len(tracker.recorded()); got != 1 {
		t.Errorf("CommentIssue calls = %d, want 1", got)
	}
}
