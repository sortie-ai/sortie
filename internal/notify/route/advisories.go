package route

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

const deprecatedAdvisoryMessage = "deprecated configuration in use"

// AgentMessageEntries returns the entries, in configuration order, that
// receive agent.message. The tracker_comment entry never does.
func AgentMessageEntries(entries []config.NotificationBackend) []config.NotificationBackend {
	var selected []config.NotificationBackend
	for _, entry := range entries {
		if entry.Kind != domain.TrackerCommentKind && slices.Contains(entry.Events, domain.EventAgentMessage) {
			selected = append(selected, entry)
		}
	}
	return selected
}

// CheckDestinations constructs every main-process destination with
// lookup and discards it, returning the first failure as a
// [*config.ConfigError]. It performs no I/O.
func CheckDestinations(entries []config.NotificationBackend, lookup NotifierLookup) error {
	_, err := buildDestinations(entries, lookup)
	return err
}

// isMainProcessDestination reports whether the main process constructs
// the entry: a registered kind that receives at least one orchestrator
// event.
func isMainProcessDestination(entry config.NotificationBackend) bool {
	if entry.Kind == domain.TrackerCommentKind {
		return false
	}
	return slices.ContainsFunc(entry.Events, domain.EventType.FromOrchestrator)
}

func buildDestinations(entries []config.NotificationBackend, lookup NotifierLookup) ([]domain.Notifier, error) {
	destinations := make([]domain.Notifier, len(entries))
	for i, entry := range entries {
		if !isMainProcessDestination(entry) {
			continue
		}
		if lookup == nil {
			return nil, &config.ConfigError{
				Field:   fmt.Sprintf("notifications[%d].kind", i),
				Message: "no notifier lookup is available",
			}
		}
		constructor, err := lookup(entry.Kind)
		if err != nil {
			return nil, &config.ConfigError{
				Field:   fmt.Sprintf("notifications[%d].kind", i),
				Message: err.Error(),
			}
		}
		notifier, err := constructor(entry.Config)
		if err != nil {
			return nil, &config.ConfigError{
				Field:   fmt.Sprintf("notifications[%d]", i),
				Message: err.Error(),
			}
		}
		destinations[i] = notifier
	}
	return destinations, nil
}

// commentEscalationReactions lists the reaction keys whose escalation
// has an event, in advisory order.
var commentEscalationReactions = []struct {
	key   string
	event domain.EventType
}{
	{"ci_failure", domain.EventEscalationCIFailure},
	{"review_comments", domain.EventEscalationReviewComments},
	{"bot_review", domain.EventEscalationBotReview},
	{"merge_conflicts", domain.EventEscalationMergeConflicts},
	{"auto_merge", domain.EventEscalationAutoMerge},
	{"merge_completion", domain.EventEscalationMergeCompletion},
}

// Advisories reports every deprecated form cfg relies on, in a fixed
// order: the three tracker.comments flags, the commenting reactions, the
// two implicit tracker comments, then the notifications entries without
// events by index. It is pure.
func Advisories(cfg config.ServiceConfig) []config.Advisory {
	var advisories []config.Advisory

	comments := cfg.Tracker.Comments
	for _, flag := range []struct {
		enabled bool
		key     string
		events  []domain.EventType
	}{
		{comments.OnDispatch, "on_dispatch", []domain.EventType{domain.EventTypeSessionStarted}},
		{comments.OnCompletion, "on_completion", []domain.EventType{domain.EventSessionCompleted, domain.EventSessionStopped}},
		{comments.OnFailure, "on_failure", []domain.EventType{domain.EventSessionFailed}},
	} {
		if !flag.enabled {
			continue
		}
		setting := "tracker.comments." + flag.key
		advisories = append(advisories, config.Advisory{
			Check: setting + ".deprecated",
			Text: setting + " is deprecated and keeps posting until it is removed; list " + joinEventTypes(" and ", flag.events...) +
				" in the events of a notifications entry of kind tracker_comment, then remove the key",
			Message: deprecatedAdvisoryMessage,
			Attrs: []slog.Attr{
				slog.String("setting", setting),
				slog.String("replacement", eventsList(flag.events...)),
			},
		})
	}

	for _, reaction := range commentEscalationReactions {
		if !commentsOnEscalation(cfg, reaction.key) {
			continue
		}
		setting := "reactions." + reaction.key + ".escalation"
		advisories = append(advisories, config.Advisory{
			Check: setting + ".deprecated",
			Text: setting + ": comment is deprecated; set escalation: none and list escalation." + reaction.key +
				" in the events of a notifications entry of kind tracker_comment",
			Message: deprecatedAdvisoryMessage,
			Attrs: []slog.Attr{
				slog.String("setting", setting),
				slog.String("replacement", "escalation: none; "+eventsList(reaction.event)),
			},
		})
	}

	if cfg.Tracker.Kind != "" && !hasTrackerCommentEntry(cfg.Notifications.Backends) {
		if cfg.Reactions["auto_merge"].Provider != "" {
			advisories = append(advisories, implicitAdvisory(
				"notifications.tracker_comment.implicit_auto_merge",
				"the auto-merge success comment", domain.EventAutoMergeMerged))
		}
		if cfg.Agent.MaxSessions > 0 || cfg.Agent.MaxTokens > 0 {
			advisories = append(advisories, implicitAdvisory(
				"notifications.tracker_comment.implicit_budget_hold",
				"the budget-hold comment", domain.EventBudgetHeld))
		}
	}

	for i, entry := range cfg.Notifications.Backends {
		if entry.Kind == domain.TrackerCommentKind || entry.EventsDeclared {
			continue
		}
		setting := fmt.Sprintf("notifications[%d]", i)
		advisories = append(advisories, config.Advisory{
			Check: setting + ".events.missing",
			Text: setting + " has no events and receives agent.message only, which is deprecated; " +
				"add events: [agent.message] to keep its current behavior",
			Message: deprecatedAdvisoryMessage,
			Attrs: []slog.Attr{
				slog.String("setting", setting+".events"),
				slog.String("replacement", eventsList(domain.EventAgentMessage)),
			},
		})
	}
	return advisories
}

func commentsOnEscalation(cfg config.ServiceConfig, key string) bool {
	if key == "ci_failure" {
		return cfg.CIFeedback.Kind != "" && cfg.CIFeedback.Escalation == "comment"
	}
	reaction, ok := cfg.Reactions[key]
	return ok && reaction.Provider != "" && reaction.Escalation == "comment"
}

func hasTrackerCommentEntry(entries []config.NotificationBackend) bool {
	return slices.ContainsFunc(entries, func(entry config.NotificationBackend) bool {
		return entry.Kind == domain.TrackerCommentKind
	})
}

func implicitAdvisory(check, comment string, event domain.EventType) config.Advisory {
	return config.Advisory{
		Check: check,
		Text: comment + " is posted only because no notifications entry of kind tracker_comment exists, which is deprecated; " +
			"add one that lists " + string(event) + ", because an entry of that kind posts only the events it lists",
		Message: deprecatedAdvisoryMessage,
		Attrs: []slog.Attr{
			slog.String("setting", string(event)),
			slog.String("replacement", eventsList(event)),
		},
	}
}

// eventsList renders the events value an operator writes.
func eventsList(events ...domain.EventType) string {
	return "events: [" + joinEventTypes(", ", events...) + "]"
}

func joinEventTypes(separator string, events ...domain.EventType) string {
	names := make([]string, len(events))
	for i, event := range events {
		names[i] = string(event)
	}
	return strings.Join(names, separator)
}
