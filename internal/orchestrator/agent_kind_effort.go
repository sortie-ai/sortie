package orchestrator

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// AgentKindEffortAdvisories returns one advisory per distinct agent kind
// cfg reaches whose registration declares [registry.EffortNotForwarded]
// and whose settings block sets [registry.EffortKey], in the order
// [orderedUniqueAgentKinds] yields, plus one per dispatch rule of such a
// kind that resolves a level. A non-string value draws the advisory too.
// It returns nil when nothing is drawn.
func AgentKindEffortAdvisories(cfg config.ServiceConfig, metaOf func(kind string) (registry.AgentMeta, bool)) []config.Advisory {
	var advisories []config.Advisory
	for _, ref := range orderedUniqueAgentKinds(cfg) {
		meta, registered := metaOf(ref.Kind)
		if !registered || meta.EffortForwarding != registry.EffortNotForwarded {
			continue
		}
		level, fault := registry.EffortSetting(config.ResolveAgentSettings(cfg, config.SettingsSelection{Kind: ref.Kind}, "").Passthrough)
		if level == "" && fault == nil {
			continue
		}
		advisories = append(advisories, config.Advisory{
			Check:   "agent.effort.not_forwarded",
			Text:    effortNotForwardedText(ref.Kind, meta),
			Message: "effort setting has no effect for this agent kind",
			Attrs:   []slog.Attr{slog.String("agent_kind", ref.Kind)},
		})
	}

	for i, rule := range cfg.Dispatch.Rules {
		if rule.SettingsKind == "" {
			continue
		}
		meta, registered := metaOf(rule.SettingsKind)
		if !registered || meta.EffortForwarding != registry.EffortNotForwarded {
			continue
		}
		settings := config.ResolveAgentSettings(cfg, config.SettingsSelection{Kind: rule.SettingsKind, RuleName: rule.Name}, "")
		level, fault := registry.EffortSetting(settings.Passthrough)
		if level == "" && fault == nil {
			continue
		}
		advisories = append(advisories, config.Advisory{
			Check:   "agent.effort.not_forwarded",
			Text:    ruleSettingsPrefix(i, rule) + effortNotForwardedText(rule.SettingsKind, meta),
			Message: "effort setting has no effect for this agent kind",
			Attrs: []slog.Attr{
				slog.String("agent_kind", rule.SettingsKind),
				slog.String("rule_name", rule.Name),
			},
		})
	}
	return advisories
}

// DispatchRuleEffortAdvisories returns one advisory per dispatch rule that
// sets [registry.ModelKey] (null included), omits [registry.EffortKey] and
// so inherits a non-empty level from its kind's top-level block. Level
// names depend on the model. It returns nil when nothing is drawn.
func DispatchRuleEffortAdvisories(cfg config.ServiceConfig) []config.Advisory {
	var advisories []config.Advisory
	for i, rule := range cfg.Dispatch.Rules {
		if rule.SettingsKind == "" {
			continue
		}
		_, setsModel := rule.Settings[registry.ModelKey]
		_, setsEffort := rule.Settings[registry.EffortKey]
		if !setsModel || setsEffort {
			continue
		}
		inherited := config.ResolveAgentSettings(cfg, config.SettingsSelection{Kind: rule.SettingsKind}, "").Effort
		if inherited == "" {
			continue
		}
		advisories = append(advisories, config.Advisory{
			Check:   "agent.effort.inherited",
			Message: "dispatch rule sets a model and inherits an effort level chosen for another model",
			Text: fmt.Sprintf("dispatch rule %s (dispatch.rules[%d].%s) sets model and inherits effort %s from the top-level %s block; level names depend on the model, so write effort in the rule to choose the level for its model, or effort: null to clear it",
				strconv.Quote(rule.Name), i, rule.SettingsKind, strconv.Quote(inherited), rule.SettingsKind),
			Attrs: []slog.Attr{
				slog.String("rule_name", rule.Name),
				slog.String("agent_kind", rule.SettingsKind),
				slog.String(registry.EffortKey, inherited),
			},
		})
	}
	return advisories
}

func effortNotForwardedText(kind string, meta registry.AgentMeta) string {
	prefix := fmt.Sprintf("%s.%s has no effect: agent kind %s ", kind, registry.EffortKey, strconv.Quote(kind))
	if meta.RequiresCommand || meta.DefaultCommand != "" {
		return prefix + "passes no reasoning level to its agent; where the agent takes a reasoning option on its command line, write it in agent.command"
	}
	return prefix + "launches no agent"
}
