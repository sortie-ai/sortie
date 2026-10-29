package orchestrator

import (
	"path/filepath"
	"strconv"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/registry"
)

// AttemptSettings is what one attempt reads from the configuration in
// force at its start.
type AttemptSettings struct {
	Settings         config.AgentSettings
	UsageArrival     registry.UsageArrival
	UsageAttribution registry.UsageAttribution

	// CarriesTokenFigure reports whether the kind's sources can carry a
	// token figure on this attempt's launch.
	CarriesTokenFigure bool

	// Refusals are the error-severity checks the block fails; an attempt
	// with any starts no session.
	Refusals []PreflightError
}

// resolveAttemptSettings resolves the settings an attempt of selection
// runs with from cfg; sshHost is empty for a local launch. Call it once
// per attempt on the event loop, so a reload reaches a claim already held.
func (o *Orchestrator) resolveAttemptSettings(cfg config.ServiceConfig, selection DispatchResolution, sshHost string) AttemptSettings {
	settings := config.ResolveAgentSettings(cfg,
		config.SettingsSelection{Kind: selection.AgentKind, RuleName: selection.RuleName},
		filepath.Dir(o.workflowManager.WorkflowAbsPath()))
	meta, registered := o.preflightParams.AgentRegistry.Meta(selection.AgentKind)

	arrival, attribution, carriesTokenFigure := usageDisposition(meta, registered, sshHost != "")
	refusals, _ := settingsDiagnostics(cfg, meta, registered, settings, sshHost != "")

	return AttemptSettings{
		Settings:           settings,
		UsageArrival:       arrival,
		UsageAttribution:   attribution,
		CarriesTokenFigure: carriesTokenFigure,
		Refusals:           refusals,
	}
}

// usageDisposition returns the usage arrival and attribution derived from
// meta's sources and whether a session on the given launch can carry a token
// figure, the undeclared pair and false for a kind the registry does not hold.
func usageDisposition(meta registry.AgentMeta, registered bool, remote bool) (registry.UsageArrival, registry.UsageAttribution, bool) {
	if !registered {
		return registry.UsageArrivalUndeclared, registry.UsageAttributionUndeclared, false
	}
	arrival, attribution := meta.UsageDisposition()
	return arrival, attribution, meta.UsageSources.CarriesTokenFigure(remote)
}

// settingsDiagnostics runs every check that reads a resolved settings
// block. Preflight and every attempt share it, so they cannot disagree
// about a block. An unregistered kind draws nothing.
func settingsDiagnostics(cfg config.ServiceConfig, meta registry.AgentMeta, registered bool, settings config.AgentSettings, remote bool) ([]PreflightError, []PreflightWarning) {
	if !registered {
		return nil, nil
	}

	var errs []PreflightError
	var warns []PreflightWarning
	kind := settings.Kind

	if meta.MCPDialect == registry.MCPDialectNone && settings.MCPConfigPath != "" {
		warns = append(warns, PreflightWarning{
			Check:   "agent.mcp_config",
			Message: "mcp_config in the " + strconv.Quote(kind) + " block cannot reach the agent: this agent kind receives no MCP configuration",
		})
	}

	if !meta.UsageSources.CarriesTokenFigure(remote) {
		if cfg.Agent.MaxTokens != 0 {
			warns = append(warns, PreflightWarning{
				Check: "agent.kind.no_usage_reporting",
				Message: "agent.max_tokens is set but agent kind " + strconv.Quote(kind) +
					" reports no token usage for the sessions this configuration produces: the per-issue token ceiling can never be reached for it",
			})
		}
		for _, setting := range moneyCeilingSettings(cfg.Agent) {
			warns = append(warns, PreflightWarning{
				Check: setting.Key + ".no_usage_reporting",
				Message: setting.Key + " is set but agent kind " + strconv.Quote(kind) +
					" reports no token usage for the sessions this configuration produces: " + setting.NoUsage,
			})
		}
		if rates, _ := pricing.ParseTokenRates(cfg.ExtensionValue("token_rates")); rates[kind].Complete() {
			warns = append(warns, PreflightWarning{
				Check: "agent.kind.no_cost_estimate",
				Message: "token_rates prices agent kind " + strconv.Quote(kind) +
					", which reports no token usage for the sessions this configuration produces: no cost can be estimated for it",
			})
		}
	}

	// A continuation, including one restored after a restart, a stall retry
	// that fires before any restart, and the first run after a person
	// releases a park resume the earlier session, so a kind that cannot
	// resume fails every such turn.
	if meta.SessionResumeBlockedBy != nil {
		if key := meta.SessionResumeBlockedBy(settings.Passthrough); key != "" {
			errs = append(errs, PreflightError{
				Check: "agent.kind.session_resume",
				Message: kind + "." + key + " stops this agent kind from resuming a session across separate agent launches, " +
					"but Sortie resumes an issue's earlier session on a continuation, including one queued before a restart, on the retry after a stall while Sortie keeps running, and on the first run after a person releases a park, " +
					"and every such turn fails. Change " + kind + "." + key + ", or use an agent kind that can resume a session.",
			})
		}
	}

	if meta.ValidateAgentConfig == nil {
		return errs, warns
	}
	fields := registry.AgentConfigFields{
		Kind:        kind,
		Passthrough: settings.Passthrough,
	}
	for _, d := range meta.ValidateAgentConfig(fields) {
		switch d.Severity {
		case "warning":
			warns = append(warns, PreflightWarning{Check: d.Check, Message: d.Message})
		default:
			errs = append(errs, PreflightError{Check: d.Check, Message: d.Message})
		}
	}
	return errs, warns
}
