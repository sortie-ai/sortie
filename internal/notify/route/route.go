// Package route selects the destinations of an outbound event and
// delivers it to them. A [Router] turns the notifications configuration
// and the deprecated tracker comment settings into one subscription
// table, owns the Slack and webhook destinations the main process
// builds, and wraps the tracker adapter as the built-in tracker_comment
// destination.
//
// The package registers no notifier kind and imports no notifier
// package: the caller supplies a [NotifierLookup], so the orchestrator
// can publish events without resolving a kind itself. Start with
// [NewRouter], then call [Router.Update] on every configuration
// snapshot, [Router.Route] at a producer's decision point, and
// [Delivery.Deliver] where the write runs.
package route

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// NotifierLookup resolves a notifications kind to its constructor.
type NotifierLookup func(kind string) (registry.NotifierConstructor, error)

// Inputs is everything the subscription table derives from.
type Inputs struct {
	Entries  []config.NotificationBackend
	Comments config.TrackerCommentsConfig

	// CommentEscalations lists the escalation types whose producer runs
	// escalation: comment.
	CommentEscalations []domain.EventType
}

// target is one destination a delivery sends to.
type target struct {
	destination string
	kind        string
	notifier    domain.Notifier
}

type eventSet map[domain.EventType]struct{}

func newEventSet(types ...domain.EventType) eventSet {
	set := make(eventSet, len(types))
	for _, t := range types {
		set[t] = struct{}{}
	}
	return set
}

func (s eventSet) has(t domain.EventType) bool {
	_, ok := s[t]
	return ok
}

// snapshot is one immutable routing state.
type snapshot struct {
	entries       []config.NotificationBackend
	destinations  []domain.Notifier // aligned with entries; nil where an entry is no main-process destination
	entryEvents   []eventSet        // aligned with entries; nil for a tracker_comment entry
	trackerEvents eventSet
}

// Router routes orchestrator events to the destinations the
// configuration subscribes to them. Route is safe for concurrent use;
// Update must be called from a single goroutine.
type Router struct {
	tracker domain.TrackerAdapter
	lookup  NotifierLookup
	source  string
	state   atomic.Pointer[snapshot]
}

// NewRouter returns a Router with empty inputs installed. A nil tracker
// means no tracker_comment destination exists. The host name is read
// once and stamps every event that carries no source.
func NewRouter(tracker domain.TrackerAdapter, lookup NotifierLookup) *Router {
	r := &Router{tracker: tracker, lookup: lookup}
	if host, err := os.Hostname(); err == nil {
		r.source = host
	}
	r.state.Store(buildSnapshot(Inputs{}, r.tracker != nil, nil))
	return r
}

// Update installs the subscription table built from in. The
// main-process destinations are rebuilt only when in.Entries differs
// from the installed entries. A lookup or constructor failure returns
// the error [CheckDestinations] reports and leaves the installed state
// untouched.
func (r *Router) Update(in Inputs) error {
	current := r.state.Load()

	destinations := current.destinations
	if !reflect.DeepEqual(in.Entries, current.entries) {
		built, err := buildDestinations(in.Entries, r.lookup)
		if err != nil {
			return err
		}
		destinations = built
	}

	r.state.Store(buildSnapshot(in, r.tracker != nil, destinations))
	return nil
}

func buildSnapshot(in Inputs, hasTracker bool, destinations []domain.Notifier) *snapshot {
	snap := &snapshot{
		entries:       in.Entries,
		destinations:  destinations,
		entryEvents:   make([]eventSet, len(in.Entries)),
		trackerEvents: newEventSet(),
	}

	var explicit *config.NotificationBackend
	for i := range in.Entries {
		entry := &in.Entries[i]
		if entry.Kind == domain.TrackerCommentKind {
			explicit = entry
			continue
		}
		snap.entryEvents[i] = newEventSet(entry.Events...)
	}

	if !hasTracker {
		return snap
	}

	tracker := snap.trackerEvents
	if explicit != nil {
		for _, t := range explicit.Events {
			tracker[t] = struct{}{}
		}
	}
	if in.Comments.OnDispatch {
		tracker[domain.EventTypeSessionStarted] = struct{}{}
	}
	if in.Comments.OnCompletion {
		tracker[domain.EventSessionCompleted] = struct{}{}
		tracker[domain.EventSessionStopped] = struct{}{}
	}
	if in.Comments.OnFailure {
		tracker[domain.EventSessionFailed] = struct{}{}
	}
	for _, t := range in.CommentEscalations {
		tracker[t] = struct{}{}
	}
	if explicit == nil {
		tracker[domain.EventAutoMergeMerged] = struct{}{}
		tracker[domain.EventBudgetHeld] = struct{}{}
	}
	return snap
}

// Route selects the destinations of n against the installed state and
// stamps the envelope fields the producer left empty. A nil Router and
// an event type that [domain.EventType.FromOrchestrator] rejects yield
// an empty [Delivery].
func (r *Router) Route(n domain.Notification) Delivery {
	if r == nil || !n.Envelope.EventType.FromOrchestrator() {
		return Delivery{}
	}

	snap := r.state.Load()
	eventType := n.Envelope.EventType

	var targets []target
	if r.tracker != nil && snap.trackerEvents.has(eventType) {
		targets = append(targets, target{
			destination: domain.TrackerCommentKind,
			kind:        domain.TrackerCommentKind,
			notifier:    trackerComment{adapter: r.tracker},
		})
	}
	for i, destination := range snap.destinations {
		if destination == nil || !snap.entryEvents[i].has(eventType) {
			continue
		}
		targets = append(targets, target{
			destination: fmt.Sprintf("notifications[%d]", i),
			kind:        snap.entries[i].Kind,
			notifier:    destination,
		})
	}
	if len(targets) == 0 {
		return Delivery{}
	}

	r.stamp(&n.Envelope)
	return Delivery{notification: n, targets: targets}
}

func (r *Router) stamp(envelope *domain.NotificationEnvelope) {
	if envelope.NotificationID == "" {
		envelope.NotificationID = rand.Text()
	}
	if envelope.Timestamp == "" {
		envelope.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if envelope.Source == "" {
		envelope.Source = r.source
	}
}

// Delivery is the set of destinations one routed event reaches. The
// zero value is empty.
type Delivery struct {
	notification domain.Notification
	targets      []target
}

// Empty reports whether the event has no destination.
func (d Delivery) Empty() bool {
	return len(d.targets) == 0
}

// EventType returns the type of the routed event, empty for an empty
// delivery built by the zero value.
func (d Delivery) EventType() domain.EventType {
	return d.notification.Envelope.EventType
}

// Deliver sends the event to every destination concurrently with ctx,
// waits for all sends, and returns one outcome per destination:
// tracker_comment first, then the notifications entries by index. An
// empty delivery returns nil without sending.
func (d Delivery) Deliver(ctx context.Context) Outcomes {
	if d.Empty() {
		return nil
	}

	outcomes := make(Outcomes, len(d.targets))
	var wg sync.WaitGroup
	for i, t := range d.targets {
		wg.Go(func() {
			outcomes[i] = Outcome{
				Destination: t.destination,
				Kind:        t.kind,
				Err:         t.notifier.Send(ctx, d.notification),
			}
		})
	}
	wg.Wait()
	return outcomes
}

// SplitTrackerComment divides d into the delivery holding its
// tracker_comment target, if it has one, and the delivery holding every
// other target in order. Both carry the same stamped notification, and
// either can be empty.
func (d Delivery) SplitTrackerComment() (tracker, others Delivery) {
	tracker.notification = d.notification
	others.notification = d.notification
	for _, t := range d.targets {
		if t.kind == domain.TrackerCommentKind {
			tracker.targets = append(tracker.targets, t)
			continue
		}
		others.targets = append(others.targets, t)
	}
	return tracker, others
}

// Outcome is the result of one destination's send.
type Outcome struct {
	// Destination is "tracker_comment" or "notifications[<i>]".
	Destination string

	// Kind is the destination's notifications kind.
	Kind string

	// Err is the send error, nil on success.
	Err error
}

// Outcomes lists the outcomes of one delivery in target order.
type Outcomes []Outcome

// TrackerComment reports whether tracker_comment was a target and, when
// it was, its send error.
func (o Outcomes) TrackerComment() (received bool, err error) {
	for _, outcome := range o {
		if outcome.Kind == domain.TrackerCommentKind {
			return true, outcome.Err
		}
	}
	return false, nil
}
