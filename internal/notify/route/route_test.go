package route_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/registry"
)

type commentRecorder struct {
	domain.TrackerAdapter

	mu       sync.Mutex
	comments []trackerComment
	err      error
	posted   chan struct{}
}

type trackerComment struct {
	issueID string
	text    string
}

func (r *commentRecorder) CommentIssue(_ context.Context, issueID, text string) error {
	r.mu.Lock()
	r.comments = append(r.comments, trackerComment{issueID: issueID, text: text})
	r.mu.Unlock()
	if r.posted != nil {
		r.posted <- struct{}{}
	}
	return r.err
}

func (r *commentRecorder) recorded() []trackerComment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.comments)
}

type recordingNotifier struct {
	mu       sync.Mutex
	received []domain.Notification
	err      error
	release  chan struct{}
	entered  chan struct{}
	returned atomic.Bool
}

func (n *recordingNotifier) Send(_ context.Context, notification domain.Notification) error {
	n.mu.Lock()
	n.received = append(n.received, notification)
	n.mu.Unlock()
	if n.entered != nil {
		n.entered <- struct{}{}
	}
	if n.release != nil {
		<-n.release
	}
	n.returned.Store(true)
	return n.err
}

func (n *recordingNotifier) notifications() []domain.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.received)
}

type fakeLookup struct {
	mu           sync.Mutex
	notifiers    map[string]*recordingNotifier
	constructed  map[string]int
	lookups      []string
	constructErr map[string]error
	lookupErr    map[string]error
}

func newFakeLookup(kinds ...string) *fakeLookup {
	lookup := &fakeLookup{
		notifiers:    make(map[string]*recordingNotifier),
		constructed:  make(map[string]int),
		constructErr: make(map[string]error),
		lookupErr:    make(map[string]error),
	}
	for _, kind := range kinds {
		lookup.notifiers[kind] = &recordingNotifier{}
	}
	return lookup
}

func (l *fakeLookup) lookup(kind string) (registry.NotifierConstructor, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lookups = append(l.lookups, kind)
	if err := l.lookupErr[kind]; err != nil {
		return nil, err
	}
	return func(map[string]any) (domain.Notifier, error) {
		l.mu.Lock()
		defer l.mu.Unlock()

		l.constructed[kind]++
		if err := l.constructErr[kind]; err != nil {
			return nil, err
		}
		return l.notifiers[kind], nil
	}, nil
}

func (l *fakeLookup) constructions(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.constructed[kind]
}

func (l *fakeLookup) looked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lookups)
}

func entry(kind string, events ...domain.EventType) config.NotificationBackend {
	return config.NotificationBackend{
		Kind:           kind,
		Events:         events,
		EventsDeclared: true,
		Config:         map[string]any{"url": "https://example.com/" + kind},
	}
}

func notificationOf(eventType domain.EventType) domain.Notification {
	return domain.Notification{
		Envelope: domain.NotificationEnvelope{IssueID: "ISS-1", Identifier: "PROJ-1", EventType: eventType},
		Message:  domain.NotificationMessage{Severity: "info", Title: "PROJ-1: " + string(eventType), Body: "body text"},
	}
}

func mustUpdate(t *testing.T, router *route.Router, in route.Inputs) {
	t.Helper()

	if err := router.Update(in); err != nil {
		t.Fatalf("Router.Update: %v", err)
	}
}

func destinations(outcomes route.Outcomes) []string {
	names := make([]string, len(outcomes))
	for i, outcome := range outcomes {
		names[i] = outcome.Destination
	}
	return names
}

func TestRouter_Route_Targets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tracker bool
		in      route.Inputs
		event   domain.EventType
		want    []string
	}{
		{
			name:  "no tracker and no entries routes nothing",
			event: domain.EventAutoMergeMerged,
		},
		{
			name:    "no explicit entry posts the auto merge success",
			tracker: true,
			event:   domain.EventAutoMergeMerged,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "no explicit entry posts the budget hold",
			tracker: true,
			event:   domain.EventBudgetHeld,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "session started is not posted by default",
			tracker: true,
			event:   domain.EventTypeSessionStarted,
		},
		{
			name:    "on_dispatch subscribes session started",
			tracker: true,
			in:      route.Inputs{Comments: config.TrackerCommentsConfig{OnDispatch: true}},
			event:   domain.EventTypeSessionStarted,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "on_completion subscribes completed",
			tracker: true,
			in:      route.Inputs{Comments: config.TrackerCommentsConfig{OnCompletion: true}},
			event:   domain.EventSessionCompleted,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "on_completion subscribes stopped",
			tracker: true,
			in:      route.Inputs{Comments: config.TrackerCommentsConfig{OnCompletion: true}},
			event:   domain.EventSessionStopped,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "on_completion does not subscribe failed",
			tracker: true,
			in:      route.Inputs{Comments: config.TrackerCommentsConfig{OnCompletion: true}},
			event:   domain.EventSessionFailed,
		},
		{
			name:    "on_failure subscribes failed",
			tracker: true,
			in:      route.Inputs{Comments: config.TrackerCommentsConfig{OnFailure: true}},
			event:   domain.EventSessionFailed,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "comment escalation subscribes its event",
			tracker: true,
			in:      route.Inputs{CommentEscalations: []domain.EventType{domain.EventEscalationBotReview}},
			event:   domain.EventEscalationBotReview,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "comment escalation leaves the other escalations alone",
			tracker: true,
			in:      route.Inputs{CommentEscalations: []domain.EventType{domain.EventEscalationBotReview}},
			event:   domain.EventEscalationCIFailure,
		},
		{
			name:    "explicit entry replaces the implicit auto merge success",
			tracker: true,
			in:      route.Inputs{Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind)}},
			event:   domain.EventAutoMergeMerged,
		},
		{
			name:    "explicit entry replaces the implicit budget hold",
			tracker: true,
			in:      route.Inputs{Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind)}},
			event:   domain.EventBudgetHeld,
		},
		{
			name:    "explicit entry posts what it lists",
			tracker: true,
			in:      route.Inputs{Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventBudgetHeld)}},
			event:   domain.EventBudgetHeld,
			want:    []string{"tracker_comment"},
		},
		{
			name:    "explicit entry never withdraws a deprecated flag",
			tracker: true,
			in: route.Inputs{
				Entries:  []config.NotificationBackend{entry(domain.TrackerCommentKind)},
				Comments: config.TrackerCommentsConfig{OnFailure: true},
			},
			event: domain.EventSessionFailed,
			want:  []string{"tracker_comment"},
		},
		{
			name:  "explicit entry without a tracker posts nothing",
			in:    route.Inputs{Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventSessionFailed)}},
			event: domain.EventSessionFailed,
		},
		{
			name:    "agent message is never routed",
			tracker: true,
			in:      route.Inputs{Entries: []config.NotificationBackend{entry("webhook", domain.EventAgentMessage)}},
			event:   domain.EventAgentMessage,
		},
		{
			name:  "entry receives the events it lists",
			in:    route.Inputs{Entries: []config.NotificationBackend{entry("webhook", domain.EventSessionFailed)}},
			event: domain.EventSessionFailed,
			want:  []string{"notifications[0]"},
		},
		{
			name:  "entry does not receive an event it omits",
			in:    route.Inputs{Entries: []config.NotificationBackend{entry("webhook", domain.EventSessionFailed)}},
			event: domain.EventBudgetHeld,
		},
		{
			name:    "tracker comment comes first and entries follow by index",
			tracker: true,
			in: route.Inputs{Entries: []config.NotificationBackend{
				entry("slack", domain.EventBudgetHeld),
				entry(domain.TrackerCommentKind, domain.EventBudgetHeld),
				entry("webhook", domain.EventBudgetHeld),
			}},
			event: domain.EventBudgetHeld,
			want:  []string{"tracker_comment", "notifications[0]", "notifications[2]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookup := newFakeLookup("slack", "webhook")
			var tracker domain.TrackerAdapter
			if tt.tracker {
				tracker = &commentRecorder{}
			}
			router := route.NewRouter(tracker, lookup.lookup)
			mustUpdate(t, router, tt.in)

			delivery := router.Route(notificationOf(tt.event))
			got := destinations(delivery.Deliver(context.Background()))

			if !slices.Equal(got, tt.want) {
				t.Errorf("Route(%s) destinations = %v, want %v", tt.event, got, tt.want)
			}
			if delivery.Empty() != (len(tt.want) == 0) {
				t.Errorf("Route(%s).Empty() = %v, want %v", tt.event, delivery.Empty(), len(tt.want) == 0)
			}
		})
	}
}

func TestRouter_Route_OldKeyAndMatchingSubscriptionPostOnce(t *testing.T) {
	t.Parallel()

	tracker := &commentRecorder{}
	router := route.NewRouter(tracker, nil)
	mustUpdate(t, router, route.Inputs{
		Entries:  []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventTypeSessionStarted)},
		Comments: config.TrackerCommentsConfig{OnDispatch: true},
	})

	router.Route(notificationOf(domain.EventTypeSessionStarted)).Deliver(context.Background())

	got := tracker.recorded()
	if len(got) != 1 {
		t.Fatalf("tracker comments after one session.started = %+v, want exactly one", got)
	}
	if want := (trackerComment{issueID: "ISS-1", text: "body text"}); got[0] != want {
		t.Errorf("tracker comment = %+v, want %+v", got[0], want)
	}
}

func TestRouter_Route_StampsEnvelope(t *testing.T) {
	t.Parallel()

	host, hostErr := os.Hostname()
	lookup := newFakeLookup("webhook")
	router := route.NewRouter(nil, lookup.lookup)
	mustUpdate(t, router, route.Inputs{Entries: []config.NotificationBackend{entry("webhook", domain.EventBudgetHeld)}})

	router.Route(notificationOf(domain.EventBudgetHeld)).Deliver(context.Background())
	preset := notificationOf(domain.EventBudgetHeld)
	preset.Envelope.NotificationID = "fixed-id"
	preset.Envelope.Timestamp = "2026-01-02T03:04:05Z"
	preset.Envelope.Source = "fixed-host"
	router.Route(preset).Deliver(context.Background())

	got := lookup.notifiers["webhook"].notifications()
	if len(got) != 2 {
		t.Fatalf("webhook received %d notifications, want 2", len(got))
	}

	stamped := got[0].Envelope
	if stamped.NotificationID == "" {
		t.Error("stamped NotificationID = \"\", want a generated id")
	}
	if ts, err := time.Parse(time.RFC3339, stamped.Timestamp); err != nil || ts.Location() != time.UTC {
		t.Errorf("stamped Timestamp = %q, want RFC 3339 in UTC (parse error %v)", stamped.Timestamp, err)
	}
	if hostErr == nil && stamped.Source != host {
		t.Errorf("stamped Source = %q, want host name %q", stamped.Source, host)
	}

	kept := got[1].Envelope
	if kept.NotificationID != "fixed-id" || kept.Timestamp != "2026-01-02T03:04:05Z" || kept.Source != "fixed-host" {
		t.Errorf("preset envelope = %+v, want id, timestamp and source untouched", kept)
	}
}

func TestRouter_Route_EmptyDeliveries(t *testing.T) {
	t.Parallel()

	tracker := &commentRecorder{}
	router := route.NewRouter(tracker, nil)
	mustUpdate(t, router, route.Inputs{Comments: config.TrackerCommentsConfig{OnDispatch: true}})
	var nilRouter *route.Router

	tests := []struct {
		name         string
		router       *route.Router
		notification domain.Notification
	}{
		{"nil router", nilRouter, notificationOf(domain.EventBudgetHeld)},
		{"agent message", router, notificationOf(domain.EventAgentMessage)},
		{"type outside the catalog", router, notificationOf(domain.EventType("session.begun"))},
		{"no event type", router, notificationOf("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			delivery := tt.router.Route(tt.notification)

			if !delivery.Empty() {
				t.Errorf("Route(%s).Empty() = false, want true", tt.name)
			}
			if got := delivery.Deliver(context.Background()); got != nil {
				t.Errorf("Deliver() on an empty delivery = %v, want nil", got)
			}
		})
	}
	if got := tracker.recorded(); len(got) != 0 {
		t.Errorf("tracker comments after empty deliveries = %+v, want none", got)
	}
}

func TestDelivery_SplitTrackerComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		tracker     bool
		entries     []config.NotificationBackend
		wantTracker []string
		wantOthers  []string
	}{
		{
			name:    "tracker comment and entries in order",
			tracker: true,
			entries: []config.NotificationBackend{
				entry("slack", domain.EventBudgetHeld),
				entry("webhook", domain.EventBudgetHeld),
			},
			wantTracker: []string{"tracker_comment"},
			wantOthers:  []string{"notifications[0]", "notifications[1]"},
		},
		{
			name:        "only the tracker comment",
			tracker:     true,
			wantTracker: []string{"tracker_comment"},
		},
		{
			name:       "only entries",
			entries:    []config.NotificationBackend{entry("slack", domain.EventBudgetHeld)},
			wantOthers: []string{"notifications[0]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookup := newFakeLookup("slack", "webhook")
			recorder := &commentRecorder{}
			var tracker domain.TrackerAdapter
			if tt.tracker {
				tracker = recorder
			}
			router := route.NewRouter(tracker, lookup.lookup)
			mustUpdate(t, router, route.Inputs{Entries: tt.entries})

			trackerPart, others := router.Route(notificationOf(domain.EventBudgetHeld)).SplitTrackerComment()

			if got := destinations(trackerPart.Deliver(context.Background())); !slices.Equal(got, tt.wantTracker) {
				t.Errorf("tracker part destinations = %v, want %v", got, tt.wantTracker)
			}
			if got := destinations(others.Deliver(context.Background())); !slices.Equal(got, tt.wantOthers) {
				t.Errorf("others part destinations = %v, want %v", got, tt.wantOthers)
			}
			if trackerPart.Empty() != (len(tt.wantTracker) == 0) || others.Empty() != (len(tt.wantOthers) == 0) {
				t.Errorf("Empty() = tracker:%v others:%v, want tracker:%v others:%v",
					trackerPart.Empty(), others.Empty(), len(tt.wantTracker) == 0, len(tt.wantOthers) == 0)
			}
		})
	}
}

func TestDelivery_SplitTrackerComment_SharesOneStampedNotification(t *testing.T) {
	t.Parallel()

	lookup := newFakeLookup("webhook")
	router := route.NewRouter(&commentRecorder{}, lookup.lookup)
	mustUpdate(t, router, route.Inputs{Entries: []config.NotificationBackend{entry("webhook", domain.EventBudgetHeld)}})

	trackerPart, others := router.Route(notificationOf(domain.EventBudgetHeld)).SplitTrackerComment()
	trackerPart.Deliver(context.Background())
	others.Deliver(context.Background())

	got := lookup.notifiers["webhook"].notifications()
	if len(got) != 1 || got[0].Envelope.NotificationID == "" {
		t.Fatalf("webhook notifications = %+v, want one stamped notification", got)
	}
	if trackerPart.EventType() != domain.EventBudgetHeld || others.EventType() != domain.EventBudgetHeld {
		t.Errorf("EventType() = tracker:%q others:%q, want both %q", trackerPart.EventType(), others.EventType(), domain.EventBudgetHeld)
	}
}

func TestRouter_Update_Failure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(l *fakeLookup)
		wantField string
	}{
		{
			name:      "lookup failure",
			setup:     func(l *fakeLookup) { l.lookupErr["slack"] = errors.New("no such kind") },
			wantField: "notifications[1].kind",
		},
		{
			name:      "constructor failure",
			setup:     func(l *fakeLookup) { l.constructErr["slack"] = errors.New("empty webhook url") },
			wantField: "notifications[1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookup := newFakeLookup("webhook", "slack")
			router := route.NewRouter(nil, lookup.lookup)
			installed := route.Inputs{Entries: []config.NotificationBackend{entry("webhook", domain.EventBudgetHeld)}}
			mustUpdate(t, router, installed)
			tt.setup(lookup)

			err := router.Update(route.Inputs{Entries: []config.NotificationBackend{
				entry("webhook", domain.EventSessionFailed),
				entry("slack", domain.EventSessionFailed),
			}})

			var ce *config.ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("Update() error = %v, want *config.ConfigError", err)
			}
			if ce.Field != tt.wantField {
				t.Errorf("ConfigError.Field = %q, want %q", ce.Field, tt.wantField)
			}

			if got := router.Route(notificationOf(domain.EventBudgetHeld)); got.Empty() {
				t.Error("Route(budget.held) after a failed Update is empty, want the installed table to stay")
			}
			if got := router.Route(notificationOf(domain.EventSessionFailed)); !got.Empty() {
				t.Error("Route(session.failed) after a failed Update is routed, want the rejected table not installed")
			}
		})
	}
}

func TestRouter_Update_RebuildsDestinationsOnlyWhenEntriesDiffer(t *testing.T) {
	t.Parallel()

	lookup := newFakeLookup("webhook")
	router := route.NewRouter(nil, lookup.lookup)
	entries := func(events ...domain.EventType) route.Inputs {
		return route.Inputs{Entries: []config.NotificationBackend{entry("webhook", events...)}}
	}

	mustUpdate(t, router, entries(domain.EventBudgetHeld))
	mustUpdate(t, router, entries(domain.EventBudgetHeld))
	if got := lookup.constructions("webhook"); got != 1 {
		t.Errorf("constructions after two Updates with equal entries = %d, want 1", got)
	}

	withFlag := entries(domain.EventBudgetHeld)
	withFlag.Comments = config.TrackerCommentsConfig{OnFailure: true}
	mustUpdate(t, router, withFlag)
	if got := lookup.constructions("webhook"); got != 1 {
		t.Errorf("constructions after an Update that changes only the comment flags = %d, want 1", got)
	}

	mustUpdate(t, router, entries(domain.EventBudgetHeld, domain.EventSessionFailed))
	if got := lookup.constructions("webhook"); got != 2 {
		t.Errorf("constructions after an Update with different entries = %d, want 2", got)
	}
	if got := router.Route(notificationOf(domain.EventSessionFailed)); got.Empty() {
		t.Error("Route(session.failed) after the entries changed is empty, want the new subscription")
	}
}

func TestDelivery_Deliver_JoinsEverySend(t *testing.T) {
	t.Parallel()

	lookup := newFakeLookup("slack", "webhook")
	slow := lookup.notifiers["slack"]
	slow.release = make(chan struct{})
	slow.entered = make(chan struct{}, 1)
	failing := lookup.notifiers["webhook"]
	failing.err = errors.New("endpoint refused")
	tracker := &commentRecorder{posted: make(chan struct{}, 1)}
	router := route.NewRouter(tracker, lookup.lookup)
	mustUpdate(t, router, route.Inputs{Entries: []config.NotificationBackend{
		entry("slack", domain.EventBudgetHeld),
		entry("webhook", domain.EventBudgetHeld),
	}})

	var outcomes route.Outcomes
	done := make(chan struct{})
	go func() {
		defer close(done)
		outcomes = router.Route(notificationOf(domain.EventBudgetHeld)).Deliver(context.Background())
	}()

	<-slow.entered
	<-tracker.posted
	select {
	case <-done:
		t.Fatal("Deliver returned while a destination was still sending")
	default:
	}

	close(slow.release)
	<-done

	if !slow.returned.Load() {
		t.Error("slow destination's Send had not returned when Deliver returned")
	}
	if got := destinations(outcomes); !slices.Equal(got, []string{"tracker_comment", "notifications[0]", "notifications[1]"}) {
		t.Fatalf("Deliver() destinations = %v, want tracker_comment then entries by index", got)
	}
	if outcomes[0].Err != nil || outcomes[1].Err != nil {
		t.Errorf("Deliver() errors = tracker:%v slack:%v, want nil", outcomes[0].Err, outcomes[1].Err)
	}
	if !errors.Is(outcomes[2].Err, failing.err) {
		t.Errorf("Deliver() webhook error = %v, want %v", outcomes[2].Err, failing.err)
	}
	if outcomes[2].Kind != "webhook" {
		t.Errorf("Deliver() webhook outcome Kind = %q, want %q", outcomes[2].Kind, "webhook")
	}
}

func TestOutcomes_TrackerComment(t *testing.T) {
	t.Parallel()

	sendErr := errors.New("tracker unavailable")
	tests := []struct {
		name         string
		outcomes     route.Outcomes
		wantReceived bool
		wantErr      error
	}{
		{name: "no outcomes"},
		{
			name:     "only other destinations",
			outcomes: route.Outcomes{{Destination: "notifications[0]", Kind: "webhook", Err: sendErr}},
		},
		{
			name:         "tracker comment accepted",
			outcomes:     route.Outcomes{{Destination: "tracker_comment", Kind: "tracker_comment"}},
			wantReceived: true,
		},
		{
			name: "tracker comment failed",
			outcomes: route.Outcomes{
				{Destination: "tracker_comment", Kind: "tracker_comment", Err: sendErr},
				{Destination: "notifications[0]", Kind: "webhook"},
			},
			wantReceived: true,
			wantErr:      sendErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			received, err := tt.outcomes.TrackerComment()

			if received != tt.wantReceived {
				t.Errorf("TrackerComment() received = %v, want %v", received, tt.wantReceived)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("TrackerComment() err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestRouter_RouteDuringUpdate(t *testing.T) {
	t.Parallel()

	lookup := newFakeLookup("webhook")
	router := route.NewRouter(&commentRecorder{}, lookup.lookup)
	mustUpdate(t, router, route.Inputs{})

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				router.Route(notificationOf(domain.EventBudgetHeld)).Deliver(context.Background())
			}
		})
	}
	for i := range 100 {
		in := route.Inputs{Comments: config.TrackerCommentsConfig{OnFailure: i%2 == 0}}
		if i%10 == 0 {
			in.Entries = []config.NotificationBackend{entry("webhook", domain.EventBudgetHeld)}
		}
		if err := router.Update(in); err != nil {
			t.Errorf("Update(%d): %v", i, err)
		}
	}
	wg.Wait()
}
