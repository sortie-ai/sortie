package orchestrator

import (
	"context"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
)

type holdLane struct {
	pass      func()
	subscribe func(entries []config.NotificationBackend)
	state     *State
	rows      func() int
}

func rebuildHoldLane(t *testing.T, routerHasTracker bool, entries []config.NotificationBackend, slack *notifierSpy) holdLane {
	t.Helper()

	issue := domain.Issue{ID: "iss-hold", Identifier: "PROJ-HOLD", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	wm.config.Notifications.Backends = entries
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	var routerTracker domain.TrackerAdapter
	if routerHasTracker {
		routerTracker = tracker
	}
	regs := passingPreflightRegistries()
	regs.ReloadWorkflow = func() error { return nil }
	regs.ConfigFunc = wm.Config
	orch := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		Router:          route.NewRouter(routerTracker, spyLookup(slack)),
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: regs,
	})

	return holdLane{
		pass: func() {
			orch.handleTick(context.Background())
			state.TrackerOpsWg.Wait()
		},
		subscribe: func(entries []config.NotificationBackend) {
			wm.mu.Lock()
			defer wm.mu.Unlock()
			wm.config.Notifications.Backends = entries
		},
		state: state,
		rows:  func() int { return len(store.budgetHoldNotices) },
	}
}

func retryHoldLane(t *testing.T, routerHasTracker bool, entries []config.NotificationBackend, slack *notifierSpy) holdLane {
	t.Helper()

	const id = "ISS-HOLD"
	store := &mockRetryStore{runHistoryCount: 3}
	tracker := &mockRetryTracker{}
	state := retryState(t, id, "PROJ-HOLD", 1)
	var routerTracker domain.TrackerAdapter
	if routerHasTracker {
		routerTracker = tracker
	}
	router := mustRouter(t, routerTracker, spyLookup(slack), route.Inputs{Entries: entries})
	params := defaultRetryParams(t, store, tracker)
	params.MaxSessions = 3
	params.Router = router

	return holdLane{
		pass: func() {
			state.RetryAttempts[id] = &RetryEntry{IssueID: id, Identifier: "PROJ-HOLD", Attempt: 1}
			state.Claimed[id] = struct{}{}
			HandleRetryTimer(state, id, params)
			state.TrackerOpsWg.Wait()
		},
		subscribe: func(entries []config.NotificationBackend) {
			if err := router.Update(route.Inputs{Entries: entries}); err != nil {
				t.Errorf("Router.Update: %v", err)
			}
		},
		state: state,
		rows:  func() int { return len(store.budgetHoldNotices) },
	}
}

func TestBudgetHoldRouting_EmptyDeliveryRecordsNothingUntilSubscribed(t *testing.T) {
	t.Parallel()

	lanes := []struct {
		name string
		make func(t *testing.T, routerHasTracker bool, entries []config.NotificationBackend, slack *notifierSpy) holdLane
	}{
		{"rebuild lane", rebuildHoldLane},
		{"retry lane", retryHoldLane},
	}
	scenarios := []struct {
		name             string
		routerHasTracker bool
		entries          []config.NotificationBackend
	}{
		{name: "no tracker", routerHasTracker: false},
		{
			name:             "explicit tracker comment entry that omits budget.held",
			routerHasTracker: true,
			entries:          []config.NotificationBackend{subscribe(domain.TrackerCommentKind)},
		},
	}

	for _, lane := range lanes {
		for _, sc := range scenarios {
			t.Run(lane.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()

				slack := &notifierSpy{}
				l := lane.make(t, sc.routerHasTracker, sc.entries, slack)

				l.pass()
				l.pass()

				if l.rows() != 0 {
					t.Errorf("budget_hold_notices rows = %d, want none for an empty delivery", l.rows())
				}
				if len(l.state.BudgetHoldNoticed) != 0 {
					t.Errorf("BudgetHoldNoticed = %v, want no entry for an empty delivery", l.state.BudgetHoldNoticed)
				}
				if l.state.BudgetHoldNoticesInWindow != 0 {
					t.Errorf("BudgetHoldNoticesInWindow = %d, want 0 (an empty delivery spends no pacing slot)", l.state.BudgetHoldNoticesInWindow)
				}
				if got := len(slack.notifications()); got != 0 {
					t.Errorf("subscriber notifications = %d, want 0", got)
				}

				l.subscribe(append(sc.entries, subscribe("slack", domain.EventBudgetHeld)))
				l.pass()
				l.pass()

				if got := len(slack.notifications()); got != 1 {
					t.Fatalf("subscriber notifications after subscribing = %d, want exactly 1", got)
				}
				if n := slack.notifications()[0]; n.Envelope.EventType != domain.EventBudgetHeld {
					t.Errorf("subscriber EventType = %q, want %q", n.Envelope.EventType, domain.EventBudgetHeld)
				}
				if l.rows() != 1 {
					t.Errorf("budget_hold_notices rows after subscribing = %d, want 1", l.rows())
				}
				if l.state.BudgetHoldNoticesInWindow != 1 {
					t.Errorf("BudgetHoldNoticesInWindow after subscribing = %d, want 1", l.state.BudgetHoldNoticesInWindow)
				}
			})
		}
	}
}

func TestBudgetHoldRouting_FailedSendKeepsItsRow(t *testing.T) {
	t.Parallel()

	lanes := []struct {
		name string
		make func(t *testing.T, routerHasTracker bool, entries []config.NotificationBackend, slack *notifierSpy) holdLane
	}{
		{"rebuild lane", rebuildHoldLane},
		{"retry lane", retryHoldLane},
	}

	for _, lane := range lanes {
		t.Run(lane.name, func(t *testing.T) {
			t.Parallel()

			slack := &notifierSpy{err: context.DeadlineExceeded}
			l := lane.make(t, false, []config.NotificationBackend{subscribe("slack", domain.EventBudgetHeld)}, slack)

			l.pass()
			l.pass()

			if l.rows() != 1 {
				t.Errorf("budget_hold_notices rows after a failed send = %d, want the row kept", l.rows())
			}
			if got := len(slack.notifications()); got != 1 {
				t.Errorf("subscriber notifications = %d, want 1 (a failed send is not retried)", got)
			}
		})
	}
}
