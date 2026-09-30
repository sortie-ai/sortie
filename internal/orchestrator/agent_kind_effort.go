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
// [orderedUniqueAgentKinds] yields. A non-string value draws the
// advisory too, since the setting is inert either way. It performs no
// I/O and no mutation, and returns nil when nothing is drawn.
func AgentKindEffortAdvisories(cfg config.ServiceConfig, metaOf func(kind string) (registry.AgentMeta, bool)) []config.Advisory {
	var advisories []config.Advisory
	for _, ref := range orderedUniqueAgentKinds(cfg) {
		meta, registered := metaOf(ref.Kind)
		if !registered || meta.EffortForwarding != registry.EffortNotForwarded {
			continue
		}
		level, fault := registry.EffortSetting(config.AgentAdapterConfig(cfg, ref.Kind))
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
	return advisories
}

func effortNotForwardedText(kind string, meta registry.AgentMeta) string {
	prefix := fmt.Sprintf("%s.%s has no effect: agent kind %s ", kind, registry.EffortKey, strconv.Quote(kind))
	if meta.RequiresCommand || meta.DefaultCommand != "" {
		return prefix + "passes no reasoning level to its agent; where the agent takes a reasoning option on its command line, write it in agent.command"
	}
	return prefix + "launches no agent"
}
