package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workspace"
)

type notifierSpy struct {
	mu        sync.Mutex
	received  []domain.Notification
	deadlines []time.Time
	ctxErrs   []error
	err       error
	entered   chan struct{}
	release   chan struct{}
}

func (s *notifierSpy) Send(ctx context.Context, n domain.Notification) error {
	s.mu.Lock()
	s.received = append(s.received, n)
	if deadline, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, deadline)
	}
	s.mu.Unlock()

	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	s.mu.Lock()
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	s.mu.Unlock()
	return s.err
}

func (s *notifierSpy) contextErrsAtReturn() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ctxErrs)
}

func (s *notifierSpy) notifications() []domain.Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.received)
}

func (s *notifierSpy) sendDeadlines() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deadlines)
}

func spyLookup(spy *notifierSpy) route.NotifierLookup {
	return func(string) (registry.NotifierConstructor, error) {
		return func(map[string]any) (domain.Notifier, error) { return spy, nil }, nil
	}
}

func subscribe(kind string, events ...domain.EventType) config.NotificationBackend {
	return config.NotificationBackend{Kind: kind, Events: events, EventsDeclared: true, Config: map[string]any{}}
}

type opsTracker struct {
	*mockTrackerAdapter

	mu         sync.Mutex
	comments   []string
	labels     []string
	commentErr error
	labelErr   error
}

func (o *opsTracker) CommentIssue(_ context.Context, _, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.comments = append(o.comments, text)
	return o.commentErr
}

func (o *opsTracker) AddLabel(_ context.Context, _, label string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.labels = append(o.labels, label)
	return o.labelErr
}

type escalationCounters struct {
	domain.NoopMetrics

	mu      sync.Mutex
	actions map[string][]string
}

func (c *escalationCounters) record(kind, action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.actions == nil {
		c.actions = make(map[string][]string)
	}
	c.actions[kind] = append(c.actions[kind], action)
}

func (c *escalationCounters) recorded(kind string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.actions[kind])
}

func (c *escalationCounters) IncCIEscalations(action string)        { c.record("ci", action) }
func (c *escalationCounters) IncReviewEscalations(action string)    { c.record("review", action) }
func (c *escalationCounters) IncBotReviewEscalations(action string) { c.record("bot_review", action) }
func (c *escalationCounters) IncMergeConflictEscalations(a string)  { c.record("merge_conflict", a) }
func (c *escalationCounters) IncAutoMergeReactions(result string)   { c.record("auto_merge", result) }

func mustRouter(t *testing.T, tracker domain.TrackerAdapter, lookup route.NotifierLookup, in route.Inputs) *route.Router {
	t.Helper()

	router := route.NewRouter(tracker, lookup)
	if err := router.Update(in); err != nil {
		t.Fatalf("Router.Update: %v", err)
	}
	return router
}

func commentRouter(t *testing.T, tracker domain.TrackerAdapter, events ...domain.EventType) *route.Router {
	t.Helper()

	return mustRouter(t, tracker, nil, route.Inputs{CommentEscalations: events})
}

func escalationRouter(t *testing.T, tracker domain.TrackerAdapter, event domain.EventType, mode string) *route.Router {
	t.Helper()

	if mode != "comment" {
		return commentRouter(t, tracker)
	}
	return commentRouter(t, tracker, event)
}

type escalationKind struct {
	name      string
	event     domain.EventType
	counter   string
	configure func(p *ReconcileParams, mode string)
	escalate  func(state *State, p ReconcileParams, pending *PendingReaction, m domain.Metrics)
}

func escalationKinds() []escalationKind {
	ctx := context.Background()
	log := discardLogger()
	trigger := EscalationTriggerBudget

	return []escalationKind{
		{
			name:    "ci",
			event:   domain.EventEscalationCIFailure,
			counter: "ci",
			configure: func(p *ReconcileParams, mode string) {
				p.CIFeedback = config.CIFeedbackConfig{Escalation: mode, EscalationLabel: "needs-human", MaxRetries: 2}
			},
			escalate: func(state *State, p ReconcileParams, pending *PendingReaction, m domain.Metrics) {
				pending.Kind = ReactionKindCI
				result := domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}
				escalateCIFailure(state, p, pending, result, "abc1234", 3, trigger, goldenNow, time.Second, log, ctx, m)
			},
		},
		{
			name:    "review",
			event:   domain.EventEscalationReviewComments,
			counter: "review",
			configure: func(p *ReconcileParams, mode string) {
				p.ReviewConfig = ReviewReactionConfig{Escalation: mode, EscalationLabel: "needs-human"}
			},
			escalate: func(state *State, p ReconcileParams, pending *PendingReaction, m domain.Metrics) {
				pending.Kind = ReactionKindReview
				data := &ReviewReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets"}
				escalateReviewFailure(state, p, pending, 3, trigger, data, log, ctx, m)
			},
		},
		{
			name:    "bot review",
			event:   domain.EventEscalationBotReview,
			counter: "bot_review",
			configure: func(p *ReconcileParams, mode string) {
				p.BotReviewConfig = BotReviewReactionConfig{Escalation: mode, EscalationLabel: "needs-human"}
			},
			escalate: func(state *State, p ReconcileParams, pending *PendingReaction, m domain.Metrics) {
				pending.Kind = ReactionKindBotReview
				data := &BotReviewReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets"}
				escalateBotReviewFailure(state, p, pending, 3, trigger, data, log, ctx, m)
			},
		},
		{
			name:    "merge conflict",
			event:   domain.EventEscalationMergeConflicts,
			counter: "merge_conflict",
			configure: func(p *ReconcileParams, mode string) {
				p.MergeConflictConfig = MergeConflictReactionConfig{Escalation: mode, EscalationLabel: "needs-human"}
			},
			escalate: func(state *State, p ReconcileParams, pending *PendingReaction, m domain.Metrics) {
				pending.Kind = ReactionKindMergeConflict
				data := &MergeConflictReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets"}
				escalateMergeConflictFailure(state, p, pending, 2, trigger, data, log, ctx, m)
			},
		},
	}
}

func TestNotifyEvents_EscalationCounters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mode         string
		noTracker    bool
		commentErr   error
		explicit     bool
		subscriber   bool
		subscriberEr error
		want         []string
		wantComments int
		wantLabels   int
		wantSlack    int
	}{
		{name: "comment mode posts the comment", mode: "comment", want: []string{"comment"}, wantComments: 1},
		{name: "comment mode with a failing tracker", mode: "comment", commentErr: errors.New("tracker down"), want: []string{"error"}, wantComments: 1},
		{name: "comment mode without a tracker", mode: "comment", noTracker: true, want: []string{"none"}},
		{
			name: "comment mode with a failing subscriber", mode: "comment",
			subscriber: true, subscriberEr: errors.New("endpoint refused"),
			want: []string{"comment"}, wantComments: 1, wantSlack: 1,
		},
		{name: "none mode posts nothing", mode: "none", want: []string{"none"}},
		{name: "none mode with an explicit tracker comment subscription", mode: "none", explicit: true, want: []string{"comment"}, wantComments: 1},
		{name: "none mode still reaches a subscriber", mode: "none", subscriber: true, want: []string{"none"}, wantSlack: 1},
		{name: "label mode labels once and reaches a subscriber", mode: "label", subscriber: true, want: []string{"label"}, wantLabels: 1, wantSlack: 1},
	}

	for _, kind := range escalationKinds() {
		for _, tt := range tests {
			t.Run(kind.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				ops := &opsTracker{mockTrackerAdapter: &mockTrackerAdapter{}, commentErr: tt.commentErr}
				var tracker domain.TrackerAdapter = ops
				if tt.noTracker {
					tracker = nil
				}
				slack := &notifierSpy{err: tt.subscriberEr}
				in := route.Inputs{}
				if tt.mode == "comment" {
					in.CommentEscalations = []domain.EventType{kind.event}
				}
				if tt.explicit {
					in.Entries = append(in.Entries, subscribe(domain.TrackerCommentKind, kind.event))
				}
				if tt.subscriber {
					in.Entries = append(in.Entries, subscribe("slack", kind.event))
				}

				params := ReconcileParams{
					TrackerAdapter: tracker,
					Store:          &reviewReconcileStore{},
					Router:         mustRouter(t, tracker, spyLookup(slack), in),
					Logger:         discardLogger(),
				}
				kind.configure(&params, tt.mode)
				counters := &escalationCounters{}
				state := NewState(5000, 4, 0, nil, AgentTotals{})
				pending := &PendingReaction{IssueID: goldenIssueID, Identifier: goldenIssueID, CreatedAt: goldenNow}
				state.Claimed[goldenIssueID] = struct{}{}

				kind.escalate(state, params, pending, counters)
				state.TrackerOpsWg.Wait()
				CancelRetry(state, goldenIssueID)

				if got := counters.recorded(kind.counter); !slices.Equal(got, tt.want) {
					t.Errorf("%s escalation counter actions = %v, want %v", kind.name, got, tt.want)
				}
				if got := len(ops.comments); got != tt.wantComments {
					t.Errorf("tracker comments = %d, want %d", got, tt.wantComments)
				}
				if got := len(ops.labels); got != tt.wantLabels {
					t.Errorf("tracker labels = %d, want %d", got, tt.wantLabels)
				}
				sent := slack.notifications()
				if len(sent) != tt.wantSlack {
					t.Fatalf("subscriber notifications = %d, want %d", len(sent), tt.wantSlack)
				}
				for _, n := range sent {
					if n.Envelope.EventType != kind.event {
						t.Errorf("subscriber EventType = %q, want %q", n.Envelope.EventType, kind.event)
					}
				}
			})
		}
	}
}

func TestNotifyEvents_AutoMergeEscalationCountsOnce(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"label", "comment", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			ops := &opsTracker{mockTrackerAdapter: &mockTrackerAdapter{}}
			params := ReconcileParams{
				TrackerAdapter:  ops,
				Store:           &reviewReconcileStore{},
				AutoMergeConfig: AutoMergeReactionConfig{Escalation: mode, EscalationLabel: "needs-human"},
				Router:          escalationRouter(t, ops, domain.EventEscalationAutoMerge, mode),
				Logger:          discardLogger(),
			}
			counters := &escalationCounters{}
			state := NewState(5000, 4, 0, nil, AgentTotals{})
			pending := &PendingReaction{IssueID: goldenIssueID, Identifier: goldenIssueID, Kind: ReactionKindAutoMerge, CreatedAt: goldenNow}
			data := &AutoMergeReactionData{PRNumber: 42, Owner: "acme", Repo: "widgets"}

			escalateAutoMergeFailure(state, params, pending, 3, data, discardLogger(), context.Background(), counters)
			state.TrackerOpsWg.Wait()

			if got := counters.recorded("auto_merge"); !slices.Equal(got, []string{"escalated"}) {
				t.Errorf(`IncAutoMergeReactions actions = %v, want ["escalated"]`, got)
			}
			wantComments, wantLabels := 0, 0
			switch mode {
			case "comment":
				wantComments = 1
			case "label":
				wantLabels = 1
			}
			if len(ops.comments) != wantComments || len(ops.labels) != wantLabels {
				t.Errorf("tracker writes = comments:%d labels:%d, want comments:%d labels:%d", len(ops.comments), len(ops.labels), wantComments, wantLabels)
			}
		})
	}
}

func TestNotifyEvents_MissingSHAMarkedDeliveredPerRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		mode          string
		labelErr      error
		commentErr    error
		slackErr      error
		wantDelivered bool
		wantLabels    int
		wantComments  int
		wantSlack     int
	}{
		{name: "label accepted", mode: "label", wantDelivered: true, wantLabels: 1, wantSlack: 1},
		{name: "label accepted and subscriber failing", mode: "label", slackErr: errors.New("refused"), wantDelivered: true, wantLabels: 1, wantSlack: 1},
		{name: "label failed stays undispatched and sends nothing", mode: "label", labelErr: errors.New("tracker down"), wantLabels: 1},
		{name: "comment accepted", mode: "comment", wantDelivered: true, wantComments: 1, wantSlack: 1},
		{name: "comment accepted and subscriber failing", mode: "comment", slackErr: errors.New("refused"), wantDelivered: true, wantComments: 1, wantSlack: 1},
		{name: "comment failed beside a successful subscriber stays undispatched", mode: "comment", commentErr: errors.New("tracker down"), wantComments: 1, wantSlack: 1},
		{name: "none mode counts as delivered on a subscriber outcome", mode: "none", wantDelivered: true, wantSlack: 1},
		{name: "none mode counts as delivered with a failing subscriber", mode: "none", slackErr: errors.New("refused"), wantDelivered: true, wantSlack: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issueID := "MGC-SHA-RULE"
			state := mgcStateWithPending(issueID, 36)
			store := newMGCStore()
			tracker := &mgcTrackerFake{
				states:      map[string]string{issueID: "In Review"},
				addLabelErr: tt.labelErr,
				commentErr:  tt.commentErr,
			}
			scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedMissingSHAStatus(), nil }}
			slack := &notifierSpy{err: tt.slackErr}
			in := route.Inputs{Entries: []config.NotificationBackend{subscribe("slack", domain.EventEscalationMergeCompletion)}}
			if tt.mode == "comment" {
				in.CommentEscalations = []domain.EventType{domain.EventEscalationMergeCompletion}
			}
			params := mgcParams(store, scm, tracker)
			params.MergeCompletionConfig.Escalation = tt.mode
			params.Router = mustRouter(t, tracker, spyLookup(slack), in)
			now := mgcBaseTime
			params.NowFunc = func() time.Time { return now }

			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			now = now.Add(31 * time.Minute)
			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			state.TrackerOpsWg.Wait()

			observation, ok := store.missingSHAObservation(issueID)
			if !ok || observation.dispatched != tt.wantDelivered {
				t.Errorf("missing-SHA observation = %+v, ok=%v, want dispatched=%v", observation, ok, tt.wantDelivered)
			}
			if got := len(tracker.addLabelCalls); got != tt.wantLabels {
				t.Errorf("AddLabel calls = %d, want %d", got, tt.wantLabels)
			}
			if got := len(tracker.commentCalls); got != tt.wantComments {
				t.Errorf("CommentIssue calls = %d, want %d", got, tt.wantComments)
			}
			if got := len(slack.notifications()); got != tt.wantSlack {
				t.Errorf("subscriber notifications = %d, want %d", got, tt.wantSlack)
			}
		})
	}
}

func TestNotifyEvents_RouteInputsFollowProducerHeldModes(t *testing.T) {
	t.Parallel()

	held := func(mode string) *Orchestrator {
		return &Orchestrator{
			reviewConfig:          ReviewReactionConfig{Escalation: mode},
			botReviewConfig:       BotReviewReactionConfig{Escalation: mode},
			mergeConflictConfig:   MergeConflictReactionConfig{Escalation: mode},
			autoMergeConfig:       AutoMergeReactionConfig{Escalation: mode},
			mergeCompletionConfig: MergeCompletionReactionConfig{Escalation: mode},
		}
	}
	reloaded := func(ciMode string) config.ServiceConfig {
		return config.ServiceConfig{
			CIFeedback: config.CIFeedbackConfig{Escalation: ciMode},
			Tracker:    config.TrackerConfig{Comments: config.TrackerCommentsConfig{OnFailure: true}},
			Notifications: config.NotificationsConfig{
				Backends: []config.NotificationBackend{subscribe("slack", domain.EventBudgetHeld)},
			},
		}
	}

	tests := []struct {
		name  string
		held  string
		ciNow string
		want  []domain.EventType
	}{
		{
			name: "every held producer on comment", held: "comment", ciNow: "comment",
			want: []domain.EventType{
				domain.EventEscalationCIFailure, domain.EventEscalationReviewComments, domain.EventEscalationBotReview,
				domain.EventEscalationMergeConflicts, domain.EventEscalationAutoMerge, domain.EventEscalationMergeCompletion,
			},
		},
		{
			name: "held producers on comment while the reload turned CI to label", held: "comment", ciNow: "label",
			want: []domain.EventType{
				domain.EventEscalationReviewComments, domain.EventEscalationBotReview,
				domain.EventEscalationMergeConflicts, domain.EventEscalationAutoMerge, domain.EventEscalationMergeCompletion,
			},
		},
		{name: "held producers on label while the reload turned CI to comment", held: "label", ciNow: "comment", want: []domain.EventType{domain.EventEscalationCIFailure}},
		{name: "held producers on none", held: "none", ciNow: "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := reloaded(tt.ciNow)

			got := held(tt.held).routeInputs(cfg)

			if !slices.Equal(got.CommentEscalations, tt.want) {
				t.Errorf("routeInputs().CommentEscalations = %v, want %v", got.CommentEscalations, tt.want)
			}
			if got.Comments != cfg.Tracker.Comments {
				t.Errorf("routeInputs().Comments = %+v, want %+v", got.Comments, cfg.Tracker.Comments)
			}
			if !slices.EqualFunc(got.Entries, cfg.Notifications.Backends, func(a, b config.NotificationBackend) bool { return a.Kind == b.Kind }) {
				t.Errorf("routeInputs().Entries = %+v, want the configured notifications", got.Entries)
			}
		})
	}
}

func TestNotifyEvents_ReloadKeepsHeldEscalationPosting(t *testing.T) {
	t.Parallel()

	ops := &opsTracker{mockTrackerAdapter: &mockTrackerAdapter{}}
	o := &Orchestrator{
		router:       route.NewRouter(ops, nil),
		logger:       discardLogger(),
		reviewConfig: ReviewReactionConfig{Escalation: "comment"},
	}
	o.updateRouter(config.ServiceConfig{})

	o.updateRouter(config.ServiceConfig{CIFeedback: config.CIFeedbackConfig{Escalation: "label"}})

	delivery := o.router.Route(reactionNotification(domain.EventEscalationReviewComments, &PendingReaction{IssueID: "ISS-1", Identifier: "ISS-1"}, "needs a human"))
	if _, err := delivery.Deliver(context.Background()).TrackerComment(); err != nil {
		t.Fatalf("Deliver(): %v", err)
	}
	if !slices.Equal(ops.comments, []string{"needs a human"}) {
		t.Errorf("tracker comments after a reload = %v, want the held comment escalation still posted", ops.comments)
	}
}

func TestNotifyEvents_RoutingUpdateFailureKeepsInstalledTable(t *testing.T) {
	t.Parallel()

	ops := &opsTracker{mockTrackerAdapter: &mockTrackerAdapter{}}
	lookup := func(string) (registry.NotifierConstructor, error) { return nil, errors.New("unknown kind") }
	var logs bytes.Buffer
	o := &Orchestrator{
		router: mustRouter(t, ops, lookup, route.Inputs{Comments: config.TrackerCommentsConfig{OnFailure: true}}),
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}

	o.updateRouter(config.ServiceConfig{Notifications: config.NotificationsConfig{
		Backends: []config.NotificationBackend{subscribe("bogus", domain.EventBudgetHeld)},
	}})

	if !strings.Contains(logs.String(), "notification routing update failed") {
		t.Errorf("log = %q, want an ERROR record for the rejected routing update", logs.String())
	}
	if o.router.Route(reactionNotification(domain.EventSessionFailed, &PendingReaction{IssueID: "ISS-1"}, "x")).Empty() {
		t.Error("session.failed is no longer routed after a failed update, want the installed table kept")
	}
}

func TestNotifyEvents_DeliverEventLogsOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		trackerErr   error
		slackErr     error
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "a failing destination logs a warning",
			slackErr:     errors.New("endpoint refused"),
			wantContains: []string{"level=WARN", `msg="notification delivery failed"`, "event_type=budget.held", "destination=notifications[1]", "notifier_kind=slack", "endpoint refused"},
		},
		{
			name:         "a delivered destination logs at debug",
			wantContains: []string{"level=DEBUG", `msg="notification delivered"`, "event_type=budget.held", "destination=notifications[1]", "notifier_kind=slack"},
			wantAbsent:   []string{"level=WARN"},
		},
		{
			name:       "the tracker comment outcome is left to its producer",
			trackerErr: errors.New("tracker down"),
			wantAbsent: []string{"destination=tracker_comment", "tracker down"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ops := &opsTracker{mockTrackerAdapter: &mockTrackerAdapter{}, commentErr: tt.trackerErr}
			slack := &notifierSpy{err: tt.slackErr}
			router := mustRouter(t, ops, spyLookup(slack), route.Inputs{Entries: []config.NotificationBackend{
				subscribe(domain.TrackerCommentKind, domain.EventBudgetHeld),
				subscribe("slack", domain.EventBudgetHeld),
			}})
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			delivery := router.Route(budgetHeldNotification("ISS-1", &BudgetExhaustedEntry{Reason: budgetReasonSession, ExhaustedAt: goldenNow}))

			outcomes := deliverEvent(context.Background(), delivery, log)

			if len(outcomes) != 2 {
				t.Fatalf("deliverEvent() outcomes = %d, want 2", len(outcomes))
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log = %q, want it to contain %q", logs.String(), want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(logs.String(), absent) {
					t.Errorf("log = %q, want it to omit %q", logs.String(), absent)
				}
			}
		})
	}
}

func TestNotifyEvents_DeliverDetachedOutlivesCallerAndIsCounted(t *testing.T) {
	t.Parallel()

	slack := &notifierSpy{entered: make(chan struct{}, 1), release: make(chan struct{})}
	router := mustRouter(t, nil, spyLookup(slack), route.Inputs{Entries: []config.NotificationBackend{subscribe("slack", domain.EventBudgetHeld)}})
	delivery := router.Route(budgetHeldNotification("ISS-1", &BudgetExhaustedEntry{Reason: budgetReasonSession, ExhaustedAt: goldenNow}))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	deliverDetached(ctx, &wg, delivery, discardLogger(), nil)
	<-slack.entered
	cancel()

	waited := make(chan struct{})
	go func() {
		wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("WaitGroup.Wait returned while the delivery was still sending")
	default:
	}
	close(slack.release)
	<-waited

	deadlines := slack.sendDeadlines()
	if len(deadlines) != 1 {
		t.Fatalf("Send deadlines = %d, want 1", len(deadlines))
	}
	if errs := slack.contextErrsAtReturn(); len(errs) != 1 || errs[0] != nil {
		t.Errorf("Send context error after the caller was cancelled = %v, want nil (the delivery outlives its caller)", errs)
	}
	if remaining := time.Until(deadlines[0]); remaining <= 0 || remaining > outboundDeliveryTimeout {
		t.Errorf("Send deadline in %v, want within (0, %v]", remaining, outboundDeliveryTimeout)
	}
	if outboundDeliveryTimeout >= trackerOpsDrainTimeout {
		t.Errorf("outboundDeliveryTimeout = %v, want below trackerOpsDrainTimeout %v so shutdown drains it", outboundDeliveryTimeout, trackerOpsDrainTimeout)
	}
}

func TestNotifyEvents_DeliverDetachedEmptyDeliveryStartsNothing(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	called := false

	deliverDetached(context.Background(), &wg, route.Delivery{}, discardLogger(), func(bool, error) { called = true })
	wg.Wait()

	if called {
		t.Error("onTrackerComment called for an empty delivery, want no goroutine started")
	}
}

func TestNotifyEvents_NotificationEnvelopes(t *testing.T) {
	t.Parallel()

	attempt := 3
	session := sessionEvent{
		IssueID: "id-1", Identifier: "PROJ-1", DisplayID: "PROJ-One", DispatchID: "dispatch-1",
		SessionID: "sess-1", Attempt: &attempt, Agent: "claude-code",
	}

	tests := []struct {
		name         string
		got          domain.Notification
		wantType     domain.EventType
		wantSeverity string
		wantTitle    string
		wantEnvelope domain.NotificationEnvelope
	}{
		{
			name:         "session event carries the run identity",
			got:          session.notification(domain.EventSessionCompleted, "", "done"),
			wantType:     domain.EventSessionCompleted,
			wantSeverity: "info",
			wantTitle:    "PROJ-One: session.completed",
			wantEnvelope: domain.NotificationEnvelope{
				IssueID: "id-1", Identifier: "PROJ-1", DispatchID: "dispatch-1", SessionID: "sess-1",
				Attempt: &attempt, Agent: "claude-code", EventType: domain.EventSessionCompleted,
			},
		},
		{
			name:         "a severity override replaces the catalog severity",
			got:          session.notification(domain.EventSessionStopped, "info", "nothing to change"),
			wantType:     domain.EventSessionStopped,
			wantSeverity: "info",
			wantTitle:    "PROJ-One: session.stopped",
			wantEnvelope: domain.NotificationEnvelope{
				IssueID: "id-1", Identifier: "PROJ-1", DispatchID: "dispatch-1", SessionID: "sess-1",
				Attempt: &attempt, Agent: "claude-code", EventType: domain.EventSessionStopped,
			},
		},
		{
			name:         "the title falls back to the identifier and then the issue id",
			got:          sessionEvent{IssueID: "id-2"}.notification(domain.EventSessionFailed, "", "failed"),
			wantType:     domain.EventSessionFailed,
			wantSeverity: "warning",
			wantTitle:    "id-2: session.failed",
			wantEnvelope: domain.NotificationEnvelope{IssueID: "id-2", EventType: domain.EventSessionFailed},
		},
		{
			name: "reaction event carries only the issue",
			got: reactionNotification(domain.EventEscalationBotReview,
				&PendingReaction{IssueID: "id-3", Identifier: "PROJ-3", DisplayID: "PROJ-Three"}, "escalated"),
			wantType:     domain.EventEscalationBotReview,
			wantSeverity: "warning",
			wantTitle:    "PROJ-Three: escalation.bot_review",
			wantEnvelope: domain.NotificationEnvelope{IssueID: "id-3", Identifier: "PROJ-3", EventType: domain.EventEscalationBotReview},
		},
		{
			name: "budget hold carries the issue and the notice text",
			got: budgetHeldNotification("id-4", &BudgetExhaustedEntry{
				Identifier: "PROJ-4", Reason: budgetReasonSession, ExhaustedAt: goldenNow,
			}),
			wantType:     domain.EventBudgetHeld,
			wantSeverity: "warning",
			wantTitle:    "PROJ-4: budget.held",
			wantEnvelope: domain.NotificationEnvelope{IssueID: "id-4", Identifier: "PROJ-4", EventType: domain.EventBudgetHeld},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.got

			if got.Message.Severity != tt.wantSeverity {
				t.Errorf("Message.Severity = %q, want %q", got.Message.Severity, tt.wantSeverity)
			}
			if got.Message.Title != tt.wantTitle {
				t.Errorf("Message.Title = %q, want %q", got.Message.Title, tt.wantTitle)
			}
			if got.Message.Category != "" {
				t.Errorf("Message.Category = %q, want empty", got.Message.Category)
			}
			want := tt.wantEnvelope
			gotAttempt, wantAttempt := got.Envelope.Attempt, want.Attempt
			got.Envelope.Attempt, want.Attempt = nil, nil
			if got.Envelope != want {
				t.Errorf("Envelope = %+v, want %+v", got.Envelope, want)
			}
			if (gotAttempt == nil) != (wantAttempt == nil) || (gotAttempt != nil && *gotAttempt != *wantAttempt) {
				t.Errorf("Envelope.Attempt = %v, want %v", gotAttempt, wantAttempt)
			}
		})
	}
}

func TestNotifyEvents_WorkerExitPublishesSessionEvents(t *testing.T) {
	t.Parallel()

	attempt := 4
	tests := []struct {
		name         string
		result       WorkerResult
		withheld     bool
		wantType     domain.EventType
		wantSeverity string
		wantNone     bool
	}{
		{
			name:         "normal exit",
			result:       WorkerResult{ExitKind: WorkerExitNormal, TurnsCompleted: 2},
			wantType:     domain.EventSessionCompleted,
			wantSeverity: "info",
		},
		{
			name:         "soft stop with a blocker",
			result:       WorkerResult{ExitKind: WorkerExitNormal, SoftStop: true, SoftStopReason: string(workspace.StatusBlocked)},
			wantType:     domain.EventSessionStopped,
			wantSeverity: "warning",
		},
		{
			name:         "soft stop with nothing to change",
			result:       WorkerResult{ExitKind: WorkerExitNormal, SoftStop: true, SoftStopReason: string(workspace.StatusNoChangeNeeded)},
			wantType:     domain.EventSessionStopped,
			wantSeverity: "info",
		},
		{
			name:         "error exit",
			result:       WorkerResult{ExitKind: WorkerExitError, Error: &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "turn timed out"}},
			wantType:     domain.EventSessionFailed,
			wantSeverity: "warning",
		},
		{
			name:         "normal exit whose handoff evidence withheld the transition",
			result:       WorkerResult{ExitKind: WorkerExitNormal, TurnsCompleted: 1},
			withheld:     true,
			wantType:     domain.EventSessionFailed,
			wantSeverity: "warning",
		},
		{
			name:     "cancelled exit publishes nothing",
			result:   WorkerResult{ExitKind: WorkerExitCancelled},
			wantNone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "EX-1"
			slack := &notifierSpy{}
			router := mustRouter(t, nil, spyLookup(slack), route.Inputs{Entries: []config.NotificationBackend{
				subscribe("slack", domain.EventSessionCompleted, domain.EventSessionStopped, domain.EventSessionFailed),
			}})
			state := exitStateWithIssue(t, issueID, "In Progress")
			state.Running[issueID].DispatchID = "dispatch-1"
			state.Running[issueID].SessionID = "entry-session"
			params := handoffEvidenceExitParams(t, &mockExitStore{}, &mockTrackerAdapter{}, &domain.NoopMetrics{})
			params.Router = router
			result := tt.result
			result.IssueID = issueID
			result.Identifier = issueID + "-ident"
			result.AgentAdapter = "mock"
			result.Attempt = &attempt
			result.SessionID = "result-session"
			if tt.withheld {
				dir, baseline := handoffEvidenceGitWorkspace(t)
				result.WorkspacePath = dir
				result.HandoffEvidencePolicy = config.HandoffEvidenceObserved
				result.HandoffEvidenceBaseline = baseline
			}

			HandleWorkerExit(state, result, params)
			state.TrackerOpsWg.Wait()
			t.Cleanup(func() { CancelRetry(state, issueID) })

			sent := slack.notifications()
			if tt.wantNone {
				if len(sent) != 0 {
					t.Fatalf("notifications = %+v, want none", sent)
				}
				return
			}
			if len(sent) != 1 {
				t.Fatalf("notifications = %d, want exactly 1", len(sent))
			}
			envelope := sent[0].Envelope
			if envelope.EventType != tt.wantType {
				t.Errorf("EventType = %q, want %q", envelope.EventType, tt.wantType)
			}
			if sent[0].Message.Severity != tt.wantSeverity {
				t.Errorf("Severity = %q, want %q", sent[0].Message.Severity, tt.wantSeverity)
			}
			if envelope.SessionID != "result-session" || envelope.DispatchID != "dispatch-1" || envelope.Agent != "mock" {
				t.Errorf("Envelope = %+v, want session result-session, dispatch dispatch-1, agent mock", envelope)
			}
			if envelope.Attempt == nil || *envelope.Attempt != attempt {
				t.Errorf("Envelope.Attempt = %v, want %d", envelope.Attempt, attempt)
			}
			if sent[0].Message.Body == "" {
				t.Error("Message.Body is empty, want the comment text")
			}
		})
	}
}

func TestNotifyEvents_OrchestratorEventsCreateNoSlotFiles(t *testing.T) {
	t.Parallel()

	const issueID = "SLOT-1"
	workspaceDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspaceDir, ".sortie"), 0o755); err != nil {
		t.Fatalf("Mkdir(.sortie): %v", err)
	}
	slack := &notifierSpy{}
	router := mustRouter(t, nil, spyLookup(slack), route.Inputs{Entries: []config.NotificationBackend{
		subscribe("slack", domain.EventSessionCompleted, domain.EventEscalationCIFailure),
	}})
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, &mockExitStore{}, &mockTrackerAdapter{}, &domain.NoopMetrics{})
	params.Router = router

	HandleWorkerExit(state, WorkerResult{
		IssueID:       issueID,
		Identifier:    issueID + "-ident",
		ExitKind:      WorkerExitNormal,
		WorkspacePath: workspaceDir,
		AgentAdapter:  "mock",
	}, params)
	state.TrackerOpsWg.Wait()
	t.Cleanup(func() { CancelRetry(state, issueID) })

	if len(slack.notifications()) != 1 {
		t.Fatalf("notifications = %d, want the exit event delivered", len(slack.notifications()))
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, ".sortie", "notification_slots")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(.sortie/notification_slots) error = %v, want not-exist", err)
	}
}
