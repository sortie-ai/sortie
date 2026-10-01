package orchestrator

import (
	"cmp"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
)

// outboundDeliveryTimeout bounds one detached delivery. Shutdown waits
// trackerOpsDrainTimeout for it, which is set above this value.
const outboundDeliveryTimeout = 30 * time.Second

// sessionEvent holds the identity of the run an orchestrator session
// event describes.
type sessionEvent struct {
	IssueID    string
	Identifier string
	DisplayID  string
	DispatchID string
	SessionID  string
	Attempt    *int
	Agent      string
}

// notification builds the event for t with body as its text. severity
// overrides the catalog severity when non-empty.
func (s sessionEvent) notification(t domain.EventType, severity, body string) domain.Notification {
	return orchestratorNotification(t, severity, domain.NotificationEnvelope{
		IssueID:    s.IssueID,
		Identifier: s.Identifier,
		DispatchID: s.DispatchID,
		SessionID:  s.SessionID,
		Attempt:    s.Attempt,
		Agent:      s.Agent,
	}, cmp.Or(s.DisplayID, s.Identifier, s.IssueID), body)
}

func reactionNotification(t domain.EventType, pending *PendingReaction, body string) domain.Notification {
	return orchestratorNotification(t, "", domain.NotificationEnvelope{
		IssueID:    pending.IssueID,
		Identifier: pending.Identifier,
	}, cmp.Or(pending.DisplayID, pending.Identifier, pending.IssueID), body)
}

func budgetHeldNotification(issueID string, entry *BudgetExhaustedEntry) domain.Notification {
	return orchestratorNotification(domain.EventBudgetHeld, "", domain.NotificationEnvelope{
		IssueID:    issueID,
		Identifier: entry.Identifier,
	}, cmp.Or(entry.DisplayID, entry.Identifier, issueID), buildBudgetHoldComment(entry))
}

func orchestratorNotification(t domain.EventType, severity string, envelope domain.NotificationEnvelope, display, body string) domain.Notification {
	envelope.EventType = t
	return domain.Notification{
		Envelope: envelope,
		Message: domain.NotificationMessage{
			Severity: cmp.Or(severity, t.Severity()),
			Title:    display + ": " + string(t),
			Body:     body,
		},
	}
}

// deliverEvent sends d to every destination and logs the outcome of each
// one except tracker_comment, whose outcome belongs to the producer that
// owns its metric and log line.
func deliverEvent(ctx context.Context, d route.Delivery, log *slog.Logger) route.Outcomes {
	outcomes := d.Deliver(ctx)
	for _, outcome := range outcomes {
		if outcome.Kind == domain.TrackerCommentKind {
			continue
		}
		if outcome.Err != nil {
			log.Warn("notification delivery failed",
				slog.String("event_type", string(d.EventType())),
				slog.String("destination", outcome.Destination),
				slog.String("notifier_kind", outcome.Kind),
				slog.Any("error", outcome.Err),
			)
			continue
		}
		log.Debug("notification delivered",
			slog.String("event_type", string(d.EventType())),
			slog.String("destination", outcome.Destination),
			slog.String("notifier_kind", outcome.Kind),
		)
	}
	return outcomes
}

// deliverDetached sends d from a goroutine counted by wg, under one
// deadline that outlives ctx's cancellation so a shutdown drains it.
// onTrackerComment, when non-nil, receives the tracker_comment outcome
// after every send returned; received is false when tracker_comment was
// not a target. An empty delivery starts no goroutine and never calls
// onTrackerComment.
func deliverDetached(ctx context.Context, wg *sync.WaitGroup, d route.Delivery, log *slog.Logger, onTrackerComment func(received bool, err error)) {
	if d.Empty() {
		return
	}
	wg.Go(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outboundDeliveryTimeout)
		defer cancel()

		outcomes := deliverEvent(dctx, d, log)
		if onTrackerComment == nil {
			return
		}
		onTrackerComment(outcomes.TrackerComment())
	})
}

// publishEscalation delivers an escalation event detached. logFailure
// receives a failed tracker comment. record, when non-nil, receives the
// escalation counter action: comment on a posted comment, error on a
// failed one, and none when tracker_comment was no target, including an
// empty delivery; a producer that applied a label passes nil because the
// label owns the counter.
func publishEscalation(ctx context.Context, wg *sync.WaitGroup, d route.Delivery, log *slog.Logger, record func(action string), logFailure func(err error)) {
	if d.Empty() {
		if record != nil {
			record(escalationActionNone)
		}
		return
	}
	deliverDetached(ctx, wg, d, log, func(received bool, err error) {
		if received && err != nil {
			logFailure(err)
		}
		if record == nil {
			return
		}
		switch {
		case !received:
			record(escalationActionNone)
		case err != nil:
			record(outcomeError)
		default:
			record(escalationActionComment)
		}
	})
}

const (
	escalationActionNone    = "none"
	escalationActionComment = "comment"
)

// routeInputs derives the routing inputs from cfg and from the
// escalation modes the producers hold. Five reaction blocks are frozen at
// construction, so their modes come from the producer-held configuration
// rather than from the reloaded file; otherwise a reload could leave an
// escalation with neither a label nor a comment.
func (o *Orchestrator) routeInputs(cfg config.ServiceConfig) route.Inputs {
	var escalations []domain.EventType
	for _, producer := range []struct {
		event domain.EventType
		mode  string
	}{
		{domain.EventEscalationCIFailure, cfg.CIFeedback.Escalation},
		{domain.EventEscalationReviewComments, o.reviewConfig.Escalation},
		{domain.EventEscalationBotReview, o.botReviewConfig.Escalation},
		{domain.EventEscalationMergeConflicts, o.mergeConflictConfig.Escalation},
		{domain.EventEscalationAutoMerge, o.autoMergeConfig.Escalation},
		{domain.EventEscalationMergeCompletion, o.mergeCompletionConfig.Escalation},
	} {
		if producer.mode == "comment" {
			escalations = append(escalations, producer.event)
		}
	}
	return route.Inputs{
		Entries:            cfg.Notifications.Backends,
		Comments:           cfg.Tracker.Comments,
		CommentEscalations: escalations,
	}
}

// updateRouter installs the routing table for cfg. A failure keeps the
// installed table.
func (o *Orchestrator) updateRouter(cfg config.ServiceConfig) {
	if o.router == nil {
		return
	}
	if err := o.router.Update(o.routeInputs(cfg)); err != nil {
		o.logger.Error("notification routing update failed", slog.Any("error", err))
	}
}

// publishSessionStarted routes the session.started event and delivers it.
// Every destination but tracker_comment is sent detached, and only while
// ctx is live, so a slow operator endpoint never delays the agent's start;
// the tracker comment is then posted on ctx, as the dispatch comment
// always was. The tracker outcome is logged and counted before the caller
// goes on.
func publishSessionStarted(ctx context.Context, issue domain.Issue, attempt *int, agentKind string, deps WorkerDeps, log *slog.Logger) {
	event := sessionEvent{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		DisplayID:  issue.DisplayID,
		DispatchID: deps.DispatchID,
		Attempt:    attempt,
		Agent:      agentKind,
	}.notification(domain.EventTypeSessionStarted, "", dispatchComment)

	trackerDelivery, others := deps.Router.Route(event).SplitTrackerComment()

	if ctx.Err() == nil {
		wg := deps.TrackerOpsWg
		if wg == nil {
			wg = new(sync.WaitGroup)
		}
		deliverDetached(ctx, wg, others, log, nil)
	}

	received, err := deliverEvent(ctx, trackerDelivery, log).TrackerComment()
	switch {
	case !received:
	case err != nil:
		log.Warn("dispatch comment failed", slog.Any("error", err))
		deps.Metrics.IncTrackerComments("dispatch", "error")
	default:
		log.Info("dispatch comment posted")
		deps.Metrics.IncTrackerComments("dispatch", "success")
	}
}
