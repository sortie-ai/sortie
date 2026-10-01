package route_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
)

const deprecatedMessage = "deprecated configuration in use"

type wantAdvisory struct {
	check       string
	text        string
	setting     string
	replacement []string
}

func mustConfig(t *testing.T, raw map[string]any) config.ServiceConfig {
	t.Helper()

	cfg, err := config.NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}
	return cfg
}

func trackerRaw(extra map[string]any) map[string]any {
	tracker := map[string]any{"kind": "file"}
	maps.Copy(tracker, extra)
	return map[string]any{"tracker": tracker}
}

func attrString(a config.Advisory, key string) string {
	for _, attr := range a.Attrs {
		if attr.Key == key {
			return attr.Value.String()
		}
	}
	return ""
}

func assertAdvisories(t *testing.T, got []config.Advisory, want []wantAdvisory) {
	t.Helper()

	checks := make([]string, len(got))
	for i, advisory := range got {
		checks[i] = advisory.Check
	}
	wantChecks := make([]string, len(want))
	for i, advisory := range want {
		wantChecks[i] = advisory.check
	}
	if !slices.Equal(checks, wantChecks) {
		t.Fatalf("Advisories() checks = %v, want %v", checks, wantChecks)
	}

	for i, advisory := range got {
		if advisory.Message != deprecatedMessage {
			t.Errorf("Advisories()[%d].Message = %q, want %q", i, advisory.Message, deprecatedMessage)
		}
		if advisory.Text != want[i].text {
			t.Errorf("Advisories()[%d].Text = %q, want %q", i, advisory.Text, want[i].text)
		}
		if setting := attrString(advisory, "setting"); setting != want[i].setting {
			t.Errorf("Advisories()[%d] setting = %q, want %q", i, setting, want[i].setting)
		}
		replacement := attrString(advisory, "replacement")
		for _, fragment := range want[i].replacement {
			if !strings.Contains(replacement, fragment) {
				t.Errorf("Advisories()[%d] replacement = %q, want it to contain %q", i, replacement, fragment)
			}
		}
	}
}

func flagAdvisory(flag string, events ...string) wantAdvisory {
	setting := "tracker.comments." + flag
	return wantAdvisory{
		check: setting + ".deprecated",
		text: setting + " is deprecated and keeps posting until it is removed; list " + strings.Join(events, " and ") +
			" in the events of a notifications entry of kind tracker_comment, then remove the key",
		setting:     setting,
		replacement: events,
	}
}

func escalationAdvisory(key string) wantAdvisory {
	setting := "reactions." + key + ".escalation"
	return wantAdvisory{
		check: setting + ".deprecated",
		text: setting + ": comment is deprecated; set escalation: none and list escalation." + key +
			" in the events of a notifications entry of kind tracker_comment",
		setting:     setting,
		replacement: []string{"escalation: none", "escalation." + key},
	}
}

func implicitAdvisory(check, comment, event string) wantAdvisory {
	return wantAdvisory{
		check: check,
		text: comment + " is posted only because no notifications entry of kind tracker_comment exists, which is deprecated; " +
			"add one that lists " + event + ", because an entry of that kind posts only the events it lists",
		setting:     event,
		replacement: []string{event},
	}
}

func missingEventsAdvisory(index string) wantAdvisory {
	setting := "notifications[" + index + "]"
	return wantAdvisory{
		check: setting + ".events.missing",
		text: setting + " has no events and receives agent.message only, which is deprecated; " +
			"add events: [agent.message] to keep its current behavior",
		setting:     setting + ".events",
		replacement: []string{"agent.message"},
	}
}

func reaction(extra map[string]any) map[string]any {
	reaction := map[string]any{"provider": "github", "target_state": "Done"}
	maps.Copy(reaction, extra)
	return reaction
}

func TestAdvisories(t *testing.T) {
	t.Parallel()

	slackEntry := map[string]any{"kind": "slack", "webhook_url": "https://hooks.slack.com/T/B/SECRET"}
	webhookWithEvents := map[string]any{"kind": "webhook", "url": "https://example.com/hook", "events": []any{"agent.message"}}
	trackerCommentEntry := map[string]any{"kind": domain.TrackerCommentKind, "events": []any{}}

	tests := []struct {
		name string
		raw  map[string]any
		want []wantAdvisory
	}{
		{
			name: "a configuration with no deprecated form",
			raw:  trackerRaw(nil),
		},
		{
			name: "on_dispatch",
			raw:  trackerRaw(map[string]any{"comments": map[string]any{"on_dispatch": true}}),
			want: []wantAdvisory{flagAdvisory("on_dispatch", "session.started")},
		},
		{
			name: "on_completion",
			raw:  trackerRaw(map[string]any{"comments": map[string]any{"on_completion": true}}),
			want: []wantAdvisory{flagAdvisory("on_completion", "session.completed", "session.stopped")},
		},
		{
			name: "on_failure",
			raw:  trackerRaw(map[string]any{"comments": map[string]any{"on_failure": true}}),
			want: []wantAdvisory{flagAdvisory("on_failure", "session.failed")},
		},
		{
			name: "a flag written false draws no advisory",
			raw:  trackerRaw(map[string]any{"comments": map[string]any{"on_dispatch": false, "on_completion": false, "on_failure": false}}),
		},
		{
			name: "ci_failure escalation comment",
			raw:  map[string]any{"reactions": map[string]any{"ci_failure": reaction(map[string]any{"escalation": "comment"})}},
			want: []wantAdvisory{escalationAdvisory("ci_failure")},
		},
		{
			name: "review_comments escalation comment",
			raw:  map[string]any{"reactions": map[string]any{"review_comments": reaction(map[string]any{"escalation": "comment"})}},
			want: []wantAdvisory{escalationAdvisory("review_comments")},
		},
		{
			name: "bot_review escalation comment",
			raw:  map[string]any{"reactions": map[string]any{"bot_review": reaction(map[string]any{"escalation": "comment"})}},
			want: []wantAdvisory{escalationAdvisory("bot_review")},
		},
		{
			name: "merge_conflicts escalation comment",
			raw:  map[string]any{"reactions": map[string]any{"merge_conflicts": reaction(map[string]any{"escalation": "comment"})}},
			want: []wantAdvisory{escalationAdvisory("merge_conflicts")},
		},
		{
			name: "auto_merge escalation comment",
			raw:  map[string]any{"reactions": map[string]any{"auto_merge": reaction(map[string]any{"escalation": "comment"})}},
			want: []wantAdvisory{escalationAdvisory("auto_merge")},
		},
		{
			name: "merge_completion escalation comment",
			raw:  map[string]any{"reactions": map[string]any{"merge_completion": reaction(map[string]any{"escalation": "comment"})}},
			want: []wantAdvisory{escalationAdvisory("merge_completion")},
		},
		{
			name: "escalation label and none draw no advisory",
			raw: map[string]any{"reactions": map[string]any{
				"ci_failure":      reaction(map[string]any{"escalation": "label"}),
				"review_comments": reaction(map[string]any{"escalation": "none"}),
			}},
		},
		{
			name: "escalation comment without a provider draws no advisory",
			raw:  map[string]any{"reactions": map[string]any{"review_comments": map[string]any{"escalation": "comment"}}},
		},
		{
			name: "auto_merge with the escalation omitted draws no escalation advisory",
			raw:  map[string]any{"reactions": map[string]any{"auto_merge": reaction(nil)}},
		},
		{
			name: "implicit auto merge comment",
			raw: func() map[string]any {
				raw := trackerRaw(nil)
				raw["reactions"] = map[string]any{"auto_merge": reaction(nil)}
				return raw
			}(),
			want: []wantAdvisory{implicitAdvisory("notifications.tracker_comment.implicit_auto_merge", "the auto-merge success comment", "auto_merge.merged")},
		},
		{
			name: "implicit budget hold comment from max_sessions",
			raw: func() map[string]any {
				raw := trackerRaw(nil)
				raw["agent"] = map[string]any{"max_sessions": 3}
				return raw
			}(),
			want: []wantAdvisory{implicitAdvisory("notifications.tracker_comment.implicit_budget_hold", "the budget-hold comment", "budget.held")},
		},
		{
			name: "implicit budget hold comment from max_tokens",
			raw: func() map[string]any {
				raw := trackerRaw(nil)
				raw["agent"] = map[string]any{"max_tokens": 1000}
				return raw
			}(),
			want: []wantAdvisory{implicitAdvisory("notifications.tracker_comment.implicit_budget_hold", "the budget-hold comment", "budget.held")},
		},
		{
			name: "an explicit tracker comment entry ends both implicit advisories",
			raw: func() map[string]any {
				raw := trackerRaw(nil)
				raw["reactions"] = map[string]any{"auto_merge": reaction(nil)}
				raw["agent"] = map[string]any{"max_sessions": 3}
				raw["notifications"] = []any{trackerCommentEntry}
				return raw
			}(),
		},
		{
			name: "no tracker draws no implicit advisory",
			raw: map[string]any{
				"reactions": map[string]any{"auto_merge": reaction(nil)},
				"agent":     map[string]any{"max_sessions": 3},
			},
		},
		{
			name: "an entry without events",
			raw:  map[string]any{"notifications": []any{slackEntry}},
			want: []wantAdvisory{missingEventsAdvisory("0")},
		},
		{
			name: "entries without events are reported by index",
			raw:  map[string]any{"notifications": []any{webhookWithEvents, slackEntry, slackEntry}},
			want: []wantAdvisory{missingEventsAdvisory("1"), missingEventsAdvisory("2")},
		},
		{
			name: "every row in table order",
			raw: func() map[string]any {
				raw := trackerRaw(map[string]any{"comments": map[string]any{"on_dispatch": true, "on_completion": true, "on_failure": true}})
				raw["reactions"] = map[string]any{
					"ci_failure": reaction(map[string]any{"escalation": "comment"}),
					"auto_merge": reaction(nil),
				}
				raw["agent"] = map[string]any{"max_sessions": 3}
				raw["notifications"] = []any{slackEntry, webhookWithEvents, slackEntry}
				return raw
			}(),
			want: []wantAdvisory{
				flagAdvisory("on_dispatch", "session.started"),
				flagAdvisory("on_completion", "session.completed", "session.stopped"),
				flagAdvisory("on_failure", "session.failed"),
				escalationAdvisory("ci_failure"),
				implicitAdvisory("notifications.tracker_comment.implicit_auto_merge", "the auto-merge success comment", "auto_merge.merged"),
				implicitAdvisory("notifications.tracker_comment.implicit_budget_hold", "the budget-hold comment", "budget.held"),
				missingEventsAdvisory("0"),
				missingEventsAdvisory("2"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := route.Advisories(mustConfig(t, tt.raw))

			assertAdvisories(t, got, tt.want)
		})
	}
}

func TestAdvisories_FlagsFromEnvironment(t *testing.T) {
	t.Setenv("SORTIE_TRACKER_COMMENTS_ON_DISPATCH", "true")
	t.Setenv("SORTIE_TRACKER_COMMENTS_ON_COMPLETION", "true")
	t.Setenv("SORTIE_TRACKER_COMMENTS_ON_FAILURE", "true")

	got := route.Advisories(mustConfig(t, trackerRaw(nil)))

	assertAdvisories(t, got, []wantAdvisory{
		flagAdvisory("on_dispatch", "session.started"),
		flagAdvisory("on_completion", "session.completed", "session.stopped"),
		flagAdvisory("on_failure", "session.failed"),
	})
}

func TestAdvisories_AttributesDistinguishEveryAdvisory(t *testing.T) {
	t.Parallel()

	raw := trackerRaw(map[string]any{"comments": map[string]any{"on_dispatch": true, "on_completion": true, "on_failure": true}})
	raw["notifications"] = []any{
		map[string]any{"kind": "slack", "webhook_url": "https://hooks.slack.com/T/B/SECRET"},
		map[string]any{"kind": "slack", "webhook_url": "https://hooks.slack.com/T/B/OTHER"},
	}

	seen := make(map[string]bool)
	for _, advisory := range route.Advisories(mustConfig(t, raw)) {
		key := advisory.Check + "|" + attrString(advisory, "setting") + "|" + attrString(advisory, "replacement")
		if seen[key] {
			t.Errorf("advisory %q repeats an earlier advisory's check and attributes, want every advisory distinct", key)
		}
		seen[key] = true
	}
	if len(seen) != 5 {
		t.Errorf("distinct advisories = %d, want 5", len(seen))
	}
}

func TestAgentMessageEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []config.NotificationBackend
		want    []string
	}{
		{name: "no entries"},
		{
			name: "agent message subscribers in configuration order",
			entries: []config.NotificationBackend{
				entry("slack", domain.EventAgentMessage),
				entry("webhook", domain.EventBudgetHeld, domain.EventAgentMessage),
			},
			want: []string{"slack", "webhook"},
		},
		{
			name: "an entry that omits agent message is dropped",
			entries: []config.NotificationBackend{
				entry("slack", domain.EventSessionFailed),
				entry("webhook", domain.EventAgentMessage),
			},
			want: []string{"webhook"},
		},
		{
			name: "the tracker comment entry is never selected",
			entries: []config.NotificationBackend{
				entry(domain.TrackerCommentKind, domain.EventAgentMessage),
				entry("webhook", domain.EventAgentMessage),
			},
			want: []string{"webhook"},
		},
		{
			name:    "an entry with no events is dropped",
			entries: []config.NotificationBackend{{Kind: "slack"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []string
			for _, selected := range route.AgentMessageEntries(tt.entries) {
				got = append(got, selected.Kind)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("AgentMessageEntries() kinds = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckDestinations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		entries   []config.NotificationBackend
		setup     func(l *fakeLookup)
		wantField string
		wantKinds []string
	}{
		{
			name: "entries that receive only agent message never reach the lookup",
			entries: []config.NotificationBackend{
				entry("no-such-kind", domain.EventAgentMessage),
				{Kind: "slack"},
			},
		},
		{
			name:      "the tracker comment entry never reaches the lookup",
			entries:   []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventBudgetHeld)},
			wantKinds: nil,
		},
		{
			name: "every orchestrator destination is constructed",
			entries: []config.NotificationBackend{
				entry("slack", domain.EventAgentMessage, domain.EventBudgetHeld),
				entry("webhook", domain.EventSessionFailed),
			},
			wantKinds: []string{"slack", "webhook"},
		},
		{
			name: "lookup failure names the kind field",
			entries: []config.NotificationBackend{
				entry("webhook", domain.EventBudgetHeld),
				entry("slack", domain.EventBudgetHeld),
			},
			setup:     func(l *fakeLookup) { l.lookupErr["slack"] = errors.New("no such kind") },
			wantField: "notifications[1].kind",
			wantKinds: []string{"webhook", "slack"},
		},
		{
			name: "constructor failure names the entry",
			entries: []config.NotificationBackend{
				entry("webhook", domain.EventBudgetHeld),
				entry("slack", domain.EventBudgetHeld),
			},
			setup:     func(l *fakeLookup) { l.constructErr["slack"] = errors.New("empty webhook url") },
			wantField: "notifications[1]",
			wantKinds: []string{"webhook", "slack"},
		},
		{
			name: "the first failure is reported",
			entries: []config.NotificationBackend{
				entry("webhook", domain.EventBudgetHeld),
				entry("slack", domain.EventBudgetHeld),
			},
			setup: func(l *fakeLookup) {
				l.constructErr["webhook"] = errors.New("empty url")
				l.constructErr["slack"] = errors.New("empty webhook url")
			},
			wantField: "notifications[0]",
			wantKinds: []string{"webhook"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookup := newFakeLookup("slack", "webhook")
			if tt.setup != nil {
				tt.setup(lookup)
			}

			err := route.CheckDestinations(tt.entries, lookup.lookup)

			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("CheckDestinations() error = %v, want nil", err)
				}
			} else {
				var ce *config.ConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("CheckDestinations() error = %v, want *config.ConfigError", err)
				}
				if ce.Field != tt.wantField {
					t.Errorf("ConfigError.Field = %q, want %q", ce.Field, tt.wantField)
				}
			}
			if got := lookup.looked(); !slices.Equal(got, tt.wantKinds) {
				t.Errorf("CheckDestinations() looked up %v, want %v", got, tt.wantKinds)
			}
			for kind, notifier := range lookup.notifiers {
				if got := notifier.notifications(); len(got) != 0 {
					t.Errorf("CheckDestinations() sent %d notifications through %s, want none", len(got), kind)
				}
			}
		})
	}
}
